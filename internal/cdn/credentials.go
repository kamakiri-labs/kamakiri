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
)

// credentialsSyncTimeout caps the wait for a credential rotation. A rotation
// is one control-plane round-trip against Sakura, not a per-domain climb, so
// its budget is much tighter than a provisioning one.
const credentialsSyncTimeout = 60 * time.Second

// RotateCredentials replaces the stored WebAccel token and secret, keeping the
// per-domain resource mapping the server already holds. It resolves the two
// values through the same flags, environment and prompts as `cdn webaccel`.
//
// A rotation changes no per-domain state, so there is nothing to watch
// converge: its correctness is the outcome of the reconciler pass, where the
// new credentials are tried for the first time. Bad credentials surface as
// that pass failing, which is why the failure copy here is about the token
// rather than about the site.
func RotateCredentials(client APIClient, opts WebAccelOptions, noWait bool, in io.Reader, out io.Writer) error {
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

	fmt.Fprint(out, i18n.T("cdn.rotate_working"))
	if _, syncErr := client.WaitForSync(config.ID, result.SyncAttemptID, credentialsSyncTimeout); syncErr != nil {
		// The refusal says nothing about the credentials, so the rejection copy
		// below would misdirect. The open line is closed and the sentinel travels
		// bare, unmarked, so the command layer prints the upgrade message.
		if errors.Is(syncErr, api.ErrUpgradeRequired) {
			fmt.Fprintln(out)
			return syncErr
		}
		if errors.Is(syncErr, api.ErrSyncTimeout) {
			fmt.Fprintln(out, i18n.T("cdn.rotate_still_applying"))
			return syncErr
		}
		// The credentials are only tried during the reconciler pass, so a
		// rejected token arrives here as a sync failure. The copy below is the
		// report, so the error is marked as already reported.
		fmt.Fprintln(out, i18n.T("cdn.rotate_failed"))
		fmt.Fprintln(out, i18n.T("cdn.rotate_rejected"))
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
			return WebAccelOptions{}, errors.New(i18n.Tf("cdn.err_unknown_flag_credentials", args[i]))
		}
	}
	return opts, nil
}
