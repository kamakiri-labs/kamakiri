package cdn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/cliflags"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// WebAccelOptions captures the inputs for `kamakiri cdn webaccel`. Empty
// fields are resolved from the environment, then interactively.
type WebAccelOptions struct {
	Token  string
	Secret string
	// Yes is accepted but changes nothing: running the command is the consent,
	// and there is no confirmation gate for it to skip.
	Yes bool
}

// EnvWebAccelToken and EnvWebAccelSecret name the environment variables read
// when the matching flag is absent. Flags win over the environment, which wins
// over prompting.
const (
	EnvWebAccelToken  = "KAMAKIRI_CDN_TOKEN"
	EnvWebAccelSecret = "KAMAKIRI_CDN_SECRET"
)

// SecretReader reads one prompted value, masking it when masked is set.
//
// raw is the original reader, needed to recognise a real terminal; reader wraps
// it and is shared across every prompt in one flow, so buffered bytes are not
// lost between calls.
//
// Masking engages only when raw is the process's own stdin. Anything wrapping
// stdin, a tee to a log for instance, defeats the check and the value is read
// and echoed in the clear.
type SecretReader func(prompt string, reader *bufio.Reader, raw io.Reader, out io.Writer, masked bool) (string, error)

// DefaultSecretReader is the implementation SetWebAccel uses. An unmasked value
// echoes as typed, deliberately, so the user can catch a bad paste. Anything
// that is not a terminal falls back to a plain line read either way.
func DefaultSecretReader(prompt string, reader *bufio.Reader, raw io.Reader, out io.Writer, masked bool) (string, error) {
	fmt.Fprint(out, prompt)

	if masked {
		if file, ok := raw.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			value, err := readMasked(int(file.Fd()), out)
			fmt.Fprintln(out)
			if err != nil {
				return "", err
			}
			return value, nil
		}
	}

	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readMasked reads one line from a terminal in raw mode, printing a star per
// keystroke. It returns ErrCancelled on Ctrl-C or EOF, and an error when raw
// mode is unavailable, which aborts the prompt rather than echoing the secret.
func readMasked(fd int, out io.Writer) (string, error) {
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer term.Restore(fd, oldState) //nolint:errcheck

	f := os.NewFile(uintptr(fd), "stdin")
	var buf []byte
	oneByte := make([]byte, 1)
	for {
		n, err := f.Read(oneByte)
		if err != nil || n == 0 {
			return "", ErrCancelled
		}
		b := oneByte[0]
		switch {
		case b == '\r' || b == '\n':
			return string(buf), nil
		case b == 0x03: // Ctrl-C
			return "", ErrCancelled
		case b == 0x7f || b == 0x08: // backspace / DEL
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				// Erase the last star: move back, overwrite, move back again.
				fmt.Fprint(out, "\b \b")
			}
		default:
			buf = append(buf, b)
			fmt.Fprint(out, "*")
		}
	}
}

// preflightText explains what the token will be used for before anything asks
// for it. It states the ownership TXT step up front rather than promising no
// registrar work, because the flow cannot complete without that record.
//
// A package-level value cannot hold it: package initialization runs before the
// language is resolved, so the block would freeze in whatever the environment
// suggested and the saved preference would never reach it.
func preflightText() string {
	return i18n.T("cdn.preflight")
}

// SetWebAccel is the `kamakiri cdn webaccel` entry point.
//
// Token and secret resolve in this order:
//
//  1. the flag values on opts
//  2. the environment variables named above
//  3. when steps 1 and 2 together did not supply both values, the credentials
//     already stored server-side, if the site is on WebAccel already and the
//     stored token is not being rejected; both values then go out empty, which
//     is what tells the server to reuse what it holds
//  4. an interactive prompt for whatever is still missing
//
// in is the reader prompts are served from; a nil reader disables prompting
// altogether, which makes a missing credential an error rather than a hang.
//
// Unless noWait is set, the command blocks until every content-serving domain
// is live. The wait is not passive: WebAccel needs a one-time ownership record
// per domain, so the watch guides the user through publishing it. Switching off
// Cloudflare is the exception: that route needs a terminal to stream both of
// its legs, and without one it queues the exit to direct serving alone and
// names the command to run next.
func SetWebAccel(client APIClient, opts WebAccelOptions, noWait bool, in io.Reader, out io.Writer) error {
	return setWebAccelWithReader(client, opts, noWait, in, out, DefaultSecretReader)
}

func setWebAccelWithReader(client APIClient, opts WebAccelOptions, noWait bool, in io.Reader, out io.Writer, secretReader SecretReader) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	// The server rejects a direct provider flip, so a site already on Cloudflare
	// has to route through none. The read is best-effort: if it fails the
	// single-leg path runs and the server's own guard still refuses the flip.
	var priorStatus *api.CDNStatusResponse
	if st, sErr := client.CDNStatus(config.ID); sErr == nil {
		priorStatus = st
	}
	isSwitch := priorStatus != nil && priorStatus.CDNMode == "cloudflare"

	// Before credentials are resolved: this path defers the provider command, so
	// prompting for a token here would ask for one it never uses. Unlike the
	// switch to Cloudflare there is no apex pre-check, since an apex on WebAccel
	// is a warning rather than a blocker.
	if isSwitch && !switchStreamable(noWait, out) {
		return switchNoWaitDegrade(client, config.ID, "webaccel", out)
	}

	token, secret, _, err := resolveWebAccelCredentials(client, config.ID, opts, in, out, secretReader)
	if err != nil {
		return err
	}

	if isSwitch {
		return switchToWebAccel(client, config.ID, priorStatus, token, secret, out)
	}

	// Empty credentials here are not a bug: they tell the server to reuse the
	// ones it already holds.
	result, err := client.SetCDNWebAccel(config.ID, token, secret)
	if err != nil {
		return api.MapError(err)
	}
	if result == nil || result.SyncAttemptID == 0 {
		return errNoSyncAttempt()
	}

	fmt.Fprintln(out, i18n.T("cdn.set_webaccel_done"))

	WriteApexARecordsBlock(out, result.ApexARecordsToUpdate)
	WriteApexARecordsPendingBlock(out, result.ApexARecordsPending)

	// Warn before the user invests in a flow their registrar may not let them
	// finish. Best-effort: the per-domain delivery block warns again later.
	if status, sErr := client.CDNStatus(config.ID); sErr == nil {
		warnApexEligibility(out, status)
	}

	if noWait {
		fmt.Fprintln(out, i18n.T("cdn.change_queued_verify"))
		return nil
	}

	return WatchToLive(client, config.ID, "webaccel", out)
}

// switchToWebAccel composes a Cloudflare to WebAccel switch as one streamed
// flow: the exit back to live direct serving, then the WebAccel go-live. It
// prints no cleanup line at the end because the orphaned Cloudflare hostname is
// ours and is deleted automatically.
func switchToWebAccel(client APIClient, siteID string, priorStatus *api.CDNStatusResponse, token, secret string, out io.Writer) error {
	return composeSwitch(client, siteID, priorStatus, "cloudflare", "webaccel",
		func(ctx context.Context) error {
			result, err := client.SetCDNWebAccel(siteID, token, secret)
			if err != nil {
				return api.MapError(err)
			}
			if result == nil || result.SyncAttemptID == 0 {
				return errNoSyncAttempt()
			}
			fmt.Fprintln(out, i18n.T("cdn.set_webaccel_done"))
			WriteApexARecordsBlock(out, result.ApexARecordsToUpdate)
			WriteApexARecordsPendingBlock(out, result.ApexARecordsPending)
			if status, sErr := client.CDNStatus(siteID); sErr == nil {
				warnApexEligibility(out, status)
			}
			return WatchToLiveCtx(ctx, client, siteID, "webaccel", out)
		}, out)
}

// warnApexEligibility notes any content domain sitting at an apex. WebAccel
// needs an ALIAS or ANAME there, which plenty of registrars cannot do, so the
// user hears it before entering a flow they may not be able to finish.
func warnApexEligibility(out io.Writer, status *api.CDNStatusResponse) {
	var apexes []string
	for _, d := range status.Domains {
		if !isContentRow(d) {
			continue
		}
		if isApexDomain(d.Domain) {
			apexes = append(apexes, d.Domain)
		}
	}
	if len(apexes) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.Tf("cdn.apex_heads_up", strings.Join(apexes, ", ")))
	fmt.Fprintln(out, i18n.T("cdn.apex_webaccel_note"))
}

// resolveWebAccelCredentials resolves the token and secret in the order
// SetWebAccel documents. The third return reports that the server's stored
// credentials are being reused, in which case both strings come back empty.
func resolveWebAccelCredentials(client APIClient, siteID string, opts WebAccelOptions, in io.Reader, out io.Writer, secretReader SecretReader) (string, string, bool, error) {
	token := opts.Token
	secret := opts.Secret

	if token == "" {
		token = os.Getenv(EnvWebAccelToken)
	}
	if secret == "" {
		secret = os.Getenv(EnvWebAccelSecret)
	}

	if token != "" && secret != "" {
		return token, secret, false, nil
	}

	// A site already on WebAccel has credentials on file, so re-running the
	// command resumes the go-live instead of asking again. The exception is a
	// token the provider is actively rejecting: reusing that would loop forever,
	// so ask for a fresh one.
	if status, err := client.CDNStatus(siteID); err == nil && status.CDNMode == "webaccel" {
		if status.CdnCredentialsHealth == "failing" {
			fmt.Fprintln(out, i18n.T("cdn.token_rejected_reenter"))
		} else {
			fmt.Fprintln(out, i18n.T("cdn.token_reusing"))
			return "", "", true, nil
		}
	}

	interactive := in != nil
	if !interactive {
		if token == "" {
			return "", "", false, errors.New(i18n.Tf("cdn.err_token_missing", EnvWebAccelToken))
		}
		if secret == "" {
			return "", "", false, errors.New(i18n.Tf("cdn.err_secret_missing", EnvWebAccelSecret))
		}
	}

	var reader *bufio.Reader
	if interactive {
		reader = bufio.NewReader(in)
	}

	// Explain first, prompt last: the user learns what the token is for before
	// being asked to paste one, and only when they are about to be asked.
	if interactive && (token == "" || secret == "") {
		fmt.Fprintln(out, preflightText())
		fmt.Fprintln(out)
	}

	if interactive && token == "" {
		t, err := promptField(i18n.T("cdn.prompt_access_token"), i18n.T("cdn.err_token_empty_retry"),
			reader, in, out, secretReader, false)
		if err != nil {
			return "", "", false, err
		}
		token = t
	}
	if interactive && secret == "" {
		s, err := promptField(i18n.T("cdn.prompt_access_token_secret"), i18n.T("cdn.err_secret_empty_retry"),
			reader, in, out, secretReader, true)
		if err != nil {
			return "", "", false, err
		}
		secret = s
	}

	if token == "" {
		return "", "", false, errors.New(i18n.T("cdn.err_token_empty"))
	}
	if secret == "" {
		return "", "", false, errors.New(i18n.T("cdn.err_secret_empty"))
	}

	return token, secret, false, nil
}

// ErrCancelled reports that the user abandoned an interactive prompt, by
// Ctrl-C or by closing the input. It carries a readable message rather than a
// bare "EOF", since it can reach the user's terminal.
var ErrCancelled = i18n.NewError("cdn.err_cancelled")

// promptField asks for one value and keeps asking while the answer is empty.
func promptField(prompt, retryMsg string, reader *bufio.Reader, raw io.Reader, out io.Writer, secretReader SecretReader, masked bool) (string, error) {
	for {
		value, err := secretReader(prompt, reader, raw, out, masked)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, ErrCancelled) {
				return "", ErrCancelled
			}
			return "", fmt.Errorf("%s: %w", i18n.T("cdn.err_read_input"), err)
		}
		if value != "" {
			return value, nil
		}
		fmt.Fprintln(out, retryMsg)
	}
}

// ParseWebAccelArgs parses the flag tail for `kamakiri cdn webaccel`, in both
// the space-separated and equals forms. It rejects --domain-id by name so that
// an invocation carrying one is told the server now creates a resource per
// domain on its own.
func ParseWebAccelArgs(args []string) (WebAccelOptions, error) {
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
		case "--yes":
			if err := cliflags.RequireNoValue(name, hasValue); err != nil {
				return opts, err
			}
			opts.Yes = true
		case "--domain-id":
			return opts, errors.New(i18n.T("cdn.err_domain_id_removed"))
		default:
			return opts, errors.New(i18n.Tf("cdn.err_unknown_flag_webaccel", args[i]))
		}
	}
	return opts, nil
}
