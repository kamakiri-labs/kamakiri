package cdn

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/cliflags"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/syncwait"
)

// awaitRotation blocks until the rotation the server accepted is reconciled,
// leaving the working line open for the caller to terminate. The signal region
// is minted here rather than around the credential prompts, so Ctrl-C at a
// prompt still ends the process the ordinary way.
func awaitRotation(client APIClient, siteID string, attemptID int64, out io.Writer) error {
	ctx, stop := signalCtx()
	defer stop()

	_, err := syncwait.Sync(ctx, client, siteID, attemptID, syncwait.Options{
		Out:      out,
		Prefix:   i18n.T("cdn.rotate_working"),
		StopLine: syncwait.SharedStopLine(),
		Schedule: pollSchedule,
		Now:      time.Now,
		IsTTY:    isTerminalWriter(out),
	})
	return err
}

// RotateCredentials replaces the stored WebAccel token and secret, keeping the
// per-domain resource mapping the server already holds. It resolves the two
// values through the same flags, environment and prompts as `cdn webaccel`,
// and like it prompts only when in is a terminal: from any other reader a
// missing value is an error naming the environment variable and the flag.
//
// A rotation changes no per-domain state, so there is nothing to watch
// converge: its correctness is the outcome of the reconciler pass, where the
// new credentials are tried for the first time. Bad credentials surface as that
// pass failing, so a failure the reconciler judged is reported as a rejected
// token. Every other way the wait can end leaves that verdict unread, and the
// copy then reports what went wrong without naming the credentials.
func RotateCredentials(client APIClient, opts WebAccelOptions, noWait bool, in io.Reader, out io.Writer) error {
	// The core disables prompting on a nil reader, which has to be a nil
	// interface rather than a typed nil file.
	if !isTerminalReader(in) {
		in = nil
	}
	return rotateCredentialsWithReader(client, opts, noWait, in, out, DefaultSecretReader)
}

func rotateCredentialsWithReader(client APIClient, opts WebAccelOptions, noWait bool, in io.Reader, out io.Writer, secretReader SecretReader) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	token, secret, err := resolveCredentialsOnly(opts, in, out, secretReader)
	if err != nil {
		return err
	}

	result, err := client.RotateCDNCredentials(config.ID, token, secret)
	if err != nil {
		return api.MapError(err)
	}
	if result == nil || result.SyncAttemptID == 0 {
		return errors.New(i18n.T("cdn.err_no_sync_attempt_rotate"))
	}

	if noWait {
		fmt.Fprintln(out, i18n.T("cdn.rotate_queued"))
		return nil
	}

	if syncErr := awaitRotation(client, config.ID, result.SyncAttemptID, out); syncErr != nil {
		// The refusal stopped the wait before its verdict, if any, could be
		// read, and it says nothing about the credentials, so neither failure
		// copy below fits. The open line is closed and the sentinel travels
		// bare, unmarked, so the command layer prints the upgrade message.
		if errors.Is(syncErr, api.ErrUpgradeRequired) {
			fmt.Fprintln(out)
			return syncErr
		}
		// The wait has closed the line and said the rotation carries on, so
		// reporting a failure here would condemn a token whose verdict, if
		// any, was never read.
		if errors.Is(syncErr, syncwait.ErrInterrupted) {
			return syncErr
		}
		// The credentials are only tried during the reconciler pass, so a
		// rejected token arrives here as that pass failing: the verdict is on
		// the token and the copy says so. It is the whole report, so the error
		// is marked as already reported.
		var rejected *syncwait.ReconcileFailure
		if errors.As(syncErr, &rejected) {
			fmt.Fprintln(out, i18n.T("cdn.rotate_failed"))
			fmt.Fprintln(out, i18n.T("cdn.rotate_rejected"))
			return fmt.Errorf("%w: %w", ErrCdnReported, syncErr)
		}
		// The wait could not read the outcome past what it tolerates, so
		// whether the credentials were ever tried is unknown and blaming them
		// would misdirect. The terminator carries the reason, which is the
		// whole report, so the error is marked as already reported.
		fmt.Fprintln(out, i18n.Tf("cdn.rotate_failed_reason", syncErr))
		return fmt.Errorf("%w: %w", ErrCdnReported, syncErr)
	}
	fmt.Fprintln(out, i18n.T("cdn.rotate_done"))
	return nil
}

// resolveCredentialsOnly resolves the token and secret from flags, then the
// environment, then a prompt. Unlike resolveWebAccelCredentials it never
// reuses what the server holds: reusing the stored credentials is exactly what
// a rotation is undoing.
func resolveCredentialsOnly(opts WebAccelOptions, in io.Reader, out io.Writer, secretReader SecretReader) (string, string, error) {
	token := opts.Token
	secret := opts.Secret
	if token == "" {
		token = os.Getenv(EnvWebAccelToken)
	}
	if secret == "" {
		secret = os.Getenv(EnvWebAccelSecret)
	}

	interactive := in != nil
	var reader *bufio.Reader
	if interactive {
		reader = bufio.NewReader(in)
	}

	if interactive && token == "" {
		t, err := promptField(i18n.T("cdn.prompt_access_token"), i18n.T("cdn.err_token_empty_retry"),
			reader, in, out, secretReader, false)
		if err != nil {
			return "", "", err
		}
		token = t
	}
	if interactive && secret == "" {
		s, err := promptField(i18n.T("cdn.prompt_access_token_secret"), i18n.T("cdn.err_secret_empty_retry"),
			reader, in, out, secretReader, true)
		if err != nil {
			return "", "", err
		}
		secret = s
	}

	if token == "" {
		if interactive {
			return "", "", errors.New(i18n.T("cdn.err_token_empty"))
		}
		return "", "", errors.New(i18n.Tf("cdn.err_token_missing", EnvWebAccelToken))
	}
	if secret == "" {
		if interactive {
			return "", "", errors.New(i18n.T("cdn.err_secret_empty"))
		}
		return "", "", errors.New(i18n.Tf("cdn.err_secret_missing", EnvWebAccelSecret))
	}
	return token, secret, nil
}

// ParseCredentialsArgs parses the flag tail for `kamakiri cdn credentials`,
// which accepts only --token and --secret, in both the space-separated and
// equals forms. It rejects --domain-id by name, since a rotation is not a mode
// change, and points the caller at `kamakiri cdn webaccel` for one.
func ParseCredentialsArgs(args []string) (WebAccelOptions, error) {
	opts := WebAccelOptions{}
	for i := 0; i < len(args); i++ {
		name, value, hasValue := cliflags.SplitArg(args[i])
		switch name {
		case "--token":
			v, next, err := cliflags.Value(args, i, hasValue, value, "--token", "")
			if err != nil {
				return opts, err
			}
			opts.Token = v
			i = next
		case "--secret":
			v, next, err := cliflags.Value(args, i, hasValue, value, "--secret", "")
			if err != nil {
				return opts, err
			}
			opts.Secret = v
			i = next
		case "--domain-id":
			return WebAccelOptions{}, errors.New(i18n.T("cdn.err_credentials_domain_id"))
		default:
			return WebAccelOptions{}, errors.New(i18n.Tf("cdn.err_unknown_flag_credentials", cliflags.Redact(args[i])))
		}
	}
	return opts, nil
}
