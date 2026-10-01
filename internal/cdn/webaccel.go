package cdn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

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
// The entry points hand the prompts a reader only when it is a terminal, so a
// terminal is what a reader serves in practice; a plain line read exists for
// tests that drive the prompts from a buffer.
type SecretReader func(prompt string, reader *bufio.Reader, raw io.Reader, out io.Writer, masked bool) (string, error)

// DefaultSecretReader is the implementation SetWebAccel uses. On a terminal
// both prompts read raw, so Ctrl-C and Ctrl-D arrive as bytes on every
// platform instead of depending on how the system treats a console interrupt;
// masked chooses the echo, and whether control bytes are kept (a secret keeps
// them, a token drops them so its value is what was shown). An unmasked value
// echoes as typed so the user can catch a bad paste, but only onto a terminal:
// the raw read echoes through the program, and a token must not land in
// redirected output. Any other reader falls back to a plain line read.
func DefaultSecretReader(prompt string, reader *bufio.Reader, raw io.Reader, out io.Writer, masked bool) (string, error) {
	fmt.Fprint(out, prompt)

	if file, ok := raw.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		value, err := readRawTerminal(file, rawEchoWriter(out, masked), out, masked)
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return value, nil
	}

	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// rawEchoWriter chooses where a raw read echoes. A masked value echoes stars to
// out, which reveal only its length. An unmasked value echoes the real bytes,
// and its erase sequences, only when out is a terminal; otherwise nowhere, so
// the token never reaches a file or pipe that stdout was redirected to.
func rawEchoWriter(out io.Writer, masked bool) io.Writer {
	if masked || isTerminalWriter(out) {
		return out
	}
	return io.Discard
}

// notifySignals, stopSignals and exitProcess are the signal and exit calls
// readRawTerminal makes. They are variables so a test can hand the handler a
// SIGTERM and observe the exit without signalling or ending the test binary.
var (
	notifySignals = signal.Notify
	stopSignals   = signal.Stop
	exitProcess   = os.Exit
)

// sigtermExitCode is the status a shell reports for a process killed by
// SIGTERM, which the prompt's own exit keeps.
const sigtermExitCode = 128 + int(syscall.SIGTERM)

// readRawTerminal puts the terminal behind file into raw mode, reads one line
// with readRawLine echoing to echo, and restores the mode. An error when raw
// mode is unavailable aborts the prompt rather than echoing a secret in the
// clear.
//
// It reads through file itself: wrapping the descriptor in a new os.File would
// close it when that wrapper is collected, taking stdin away from the next
// prompt.
//
// A SIGTERM while the terminal is raw restores it, writes to out the newline
// that closes the prompt line, the one the prompt writes after any read, and
// exits 143 with nothing on stderr. out is the caller's output rather than
// echo, which discards everything for a token typed with stdout redirected.
// The handler exits from its own goroutine because a blocked terminal read
// cannot be interrupted, so the read never returns for the caller to act on.
// The order of the steps around the read is what makes that hold:
//
//   - The handler is registered before raw mode is entered. Registered after,
//     a SIGTERM arriving just as the mode changed would take its default
//     action and leave the terminal raw; this way a terminal seen in raw mode
//     always has the handler armed.
//   - After the read, the terminal is restored and the goroutine is joined
//     before the handler is removed. The join keeps this function from
//     returning while a goroutine that took a signal is still exiting, so the
//     caller never prints a newline of its own or acts on the value read. It
//     also means a signal that lands while the handler is being removed goes
//     to the check below, since the goroutine is gone by then. At most one
//     exit is taken because the real exit never returns: a goroutine that took
//     a signal is never joined, so the check never runs after it.
//   - The check for a pending signal follows the removal. A SIGTERM that
//     arrived after the goroutine stopped selecting, or that it passed over in
//     favour of done, is still buffered, and none can arrive once stopSignals
//     has returned, so the non-blocking receive misses none.
//
// After the removal SIGTERM is back to its default action, so the signal
// regions the commands open after the prompts are unaffected.
func readRawTerminal(file *os.File, echo, out io.Writer, masked bool) (string, error) {
	fd := int(file.Fd())

	sigs := make(chan os.Signal, 1)
	notifySignals(sigs, syscall.SIGTERM)

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		stopSignals(sigs)
		exitOnPendingSignal(sigs, out)
		return "", err
	}

	done := make(chan struct{})
	var handler sync.WaitGroup
	handler.Go(func() {
		select {
		case <-sigs:
			term.Restore(fd, oldState) //nolint:errcheck
			fmt.Fprintln(out)
			exitProcess(sigtermExitCode)
		case <-done:
		}
	})

	value, err := readRawLine(file, echo, masked)

	term.Restore(fd, oldState) //nolint:errcheck
	close(done)
	handler.Wait()
	stopSignals(sigs)
	exitOnPendingSignal(sigs, out)
	return value, err
}

// exitOnPendingSignal takes the SIGTERM exit if a signal is waiting on sigs,
// and returns at once otherwise. The terminal is not raw by then, so there is
// nothing to restore.
func exitOnPendingSignal(sigs <-chan os.Signal, out io.Writer) {
	select {
	case <-sigs:
		fmt.Fprintln(out)
		exitProcess(sigtermExitCode)
	default:
	}
}

// readRawLine reads one line a byte at a time from a terminal in raw mode,
// where the line discipline does no editing and no signal handling, so it does
// both here. Enter returns the value. Ctrl-C returns ErrInterrupted, and
// Ctrl-D, a read error and the end of input return ErrCancelled. Backspace and
// DEL erase the last byte. Every other byte is kept and echoed to out: as a
// star when masked, as itself when printable. Unmasked, a control byte is
// dropped so the value is what the user saw. The bytes are returned as typed,
// without trimming.
//
// A \n that arrives while nothing is kept is skipped rather than ending the
// line. A value pasted with a CRLF ending ends its own prompt at the \r and
// leaves the \n queued, and the next prompt would otherwise take it as an
// empty answer. Raw mode stops the terminal turning \r into \n, so the Enter
// key still arrives as \r and an empty line can still be entered.
//
// A byte delivered together with an error is processed before the error, as the
// io.Reader contract allows, so a final Enter that arrives with end of input
// still completes the line.
func readRawLine(r io.Reader, out io.Writer, masked bool) (string, error) {
	var buf []byte
	oneByte := make([]byte, 1)
	for {
		n, err := r.Read(oneByte)
		if n == 0 {
			return "", ErrCancelled
		}
		b := oneByte[0]
		switch {
		case b == '\n' && len(buf) == 0: // the tail of a CRLF paste
		case b == '\r' || b == '\n':
			return string(buf), nil
		case b == 0x03: // Ctrl-C
			return "", ErrInterrupted
		case b == 0x04: // Ctrl-D
			return "", ErrCancelled
		case b == 0x7f || b == 0x08: // DEL / backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				// Erase the last echoed character: move back, overwrite, move back again.
				fmt.Fprint(out, "\b \b")
			}
		case masked:
			buf = append(buf, b)
			fmt.Fprint(out, "*")
		case b >= 0x20:
			buf = append(buf, b)
			out.Write([]byte{b}) //nolint:errcheck
		}
		if err != nil {
			return "", ErrCancelled
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
//  4. an interactive prompt for whatever is still missing, only when in, the
//     reader the prompts are served from, is a terminal
//
// Any other reader, a pipe or a redirected file, is never read: a credential
// still missing after step 3 is then an error naming the environment variable
// and the flag that supply it, rather than an answer taken from
// whatever bytes the pipe holds.
//
// Unless noWait is set, the command blocks until every content-serving domain
// is live. The wait is not passive: WebAccel needs a one-time ownership record
// per domain, so the watch guides the user through publishing it. Switching off
// Cloudflare is the exception: that route needs a terminal to stream both of
// its legs, and without one it queues the exit to direct serving alone and
// names the command to run next.
func SetWebAccel(client APIClient, opts WebAccelOptions, noWait bool, in io.Reader, out io.Writer) error {
	// The core disables prompting on a nil reader, which has to be a nil
	// interface rather than a typed nil file.
	if !isTerminalReader(in) {
		in = nil
	}
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
// A nil in disables prompting, so a value still missing is an error.
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
	// so the missing values are asked for afresh. The rejection line asks for a
	// new token only when the token prompt follows. Otherwise it only states the
	// rejection, and what comes next explains itself: the secret prompt when a
	// token was supplied, or the missing-value error when there is no terminal.
	if status, err := client.CDNStatus(siteID); err == nil && status.CDNMode == "webaccel" {
		if status.CdnCredentialsHealth == "failing" {
			if in != nil && token == "" {
				fmt.Fprintln(out, i18n.T("cdn.token_rejected_reenter"))
			} else {
				fmt.Fprintln(out, i18n.T("cdn.token_rejected_on_file"))
			}
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

// ErrCancelled reports that the input ended, or could not be read, before a
// prompt got its value. It carries a readable message rather than a bare "EOF",
// since it can reach the user's terminal.
var ErrCancelled = i18n.NewError("cdn.err_cancelled")

// ErrInterrupted reports a Ctrl-C at either credential prompt, both of which
// read the terminal raw so the interrupt arrives as a byte. The command exits
// 130 on it and echoes nothing: the prompt has already closed its line, and an
// interrupt the user asked for needs no message. The text is never shown.
var ErrInterrupted = errors.New("interrupted at a credential prompt")

// promptField asks for one value and keeps asking while the answer is empty
// after trimming surrounding whitespace, which it also removes from the value
// it returns, whatever the reader does.
func promptField(prompt, retryMsg string, reader *bufio.Reader, raw io.Reader, out io.Writer, secretReader SecretReader, masked bool) (string, error) {
	for {
		value, err := secretReader(prompt, reader, raw, out, masked)
		if err != nil {
			if errors.Is(err, ErrInterrupted) {
				return "", err
			}
			if errors.Is(err, io.EOF) || errors.Is(err, ErrCancelled) {
				return "", ErrCancelled
			}
			return "", fmt.Errorf("%s: %w", i18n.T("cdn.err_read_input"), err)
		}
		if value = strings.TrimSpace(value); value != "" {
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
		case "--domain-id":
			return opts, errors.New(i18n.T("cdn.err_domain_id_removed"))
		default:
			return opts, errors.New(i18n.Tf("cdn.err_unknown_flag_webaccel", cliflags.Redact(args[i])))
		}
	}
	return opts, nil
}
