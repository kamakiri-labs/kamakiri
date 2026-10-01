package login

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/streamui"
)

// secretTTL is how long a fresh login secret lasts, and the code lifetime
// assumed when the server does not report one.
const secretTTL = 10 * time.Minute

// sleep, isTerminal and discardTypeahead are what the cooldown wait takes from
// its surroundings. They are variables so a test can count the wait without
// waiting, choose which form of it the run prints, and observe the flush;
// nothing else reassigns them.
var (
	sleep      = time.Sleep
	isTerminal = streamui.IsTerminalWriter
)

// discardTypeahead drops input the user has typed but the prompt has not read.
// It runs once the request that follows a countdown has returned: an Enter
// pressed during the wait, or during that request's round trip, would otherwise
// sit in the terminal's input queue and, as soon as the new code is sent, be read
// as a request for yet another one, which would replace the code just mailed.
// It acts only on a terminal, and a failure to flush is ignored, since the flush
// is a courtesy and the worst outcome without it is the extra request.
var discardTypeahead = func(in io.Reader) {
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return
	}
	flushInput(f)
}

// APIClient is the API surface the login flow needs.
type APIClient interface {
	Register(email string, tosAccepted bool, secretHash string, resend bool) (*api.RegisterResponse, error)
	Verify(email, code, secret string) (*api.VerifyResponse, error)
}

// Run executes the login flow, reading from in and writing to out.
func Run(client APIClient, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)

	fmt.Fprint(out, i18n.T("login.prompt_email"))
	email, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	email = foldEmailCase(strings.TrimSpace(email))

	if _, parseErr := mail.ParseAddress(email); parseErr != nil {
		fmt.Fprintln(out, i18n.T("login.err_invalid_email"))
		return errors.New(i18n.T("login.err_aborted_invalid_email"))
	}

	fmt.Fprintln(out, i18n.T("login.terms_line"))
	fmt.Fprintln(out, i18n.T("login.privacy_line"))
	fmt.Fprint(out, i18n.T("login.prompt_tos"))
	tosInput, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	tosInput = strings.TrimSpace(strings.ToLower(tosInput))
	if tosInput != "y" && tosInput != "" {
		fmt.Fprintln(out, i18n.T("login.err_tos_required"))
		return errors.New(i18n.T("login.err_aborted_tos_rejected"))
	}

	// The channel-binding secret ties this client to the verify step: the
	// server stores its hash at register and checks it at verify.
	secret, err := getOrCreateSecret(email)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("login.err_login_secret"), err)
	}

	codes := codeRequester{client: client, in: in, out: out, email: email, secret: secret, secretHash: hashSecret(secret)}

	// The first request asks to resume rather than for a new code: a re-run on
	// this machine holds the secret of a code that may still be valid, and a new
	// one would replace it.
	if err := codes.request(false); err != nil {
		return handleAPIError(out, err)
	}

	for {
		fmt.Fprint(out, i18n.T("login.prompt_code"))
		code, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		code = strings.TrimSpace(code)

		if code == "" {
			if err := codes.request(true); err != nil {
				if !retryableNewCodeFailure(err) {
					return handleAPIError(out, err)
				}
				fmt.Fprintln(out, i18n.T("login.err_new_code_unconfirmed"))
			}
			continue
		}

		resp, err := client.Verify(email, code, secret)
		if err != nil {
			if line, ok := rejectedCodeLine(err); ok {
				fmt.Fprintln(out, line)
				continue
			}
			return handleAPIError(out, err)
		}

		// Delete the local secret before saving credentials: the server has
		// already consumed it on verify, so it serves no further purpose.
		_ = core.DeleteLoginSecret()

		if err := core.SaveCredentials(resp.APIKey, email); err != nil {
			return fmt.Errorf("%s: %w", i18n.T("login.err_save_credentials"), err)
		}

		credPath, _ := core.CredentialsPath()
		fmt.Fprintln(out, i18n.Tf("login.api_key_saved", credPath))
		// The key was minted and saved, and yet nothing about the next command
		// changes while the environment holds a key of its own, so say so rather
		// than leave the user to work out why the account did not switch.
		if _, fromEnvironment := core.APIKeyFromEnvironment(); fromEnvironment {
			fmt.Fprintln(out, i18n.T("login.env_override_note"))
		}
		return nil
	}
}

// codeRequester asks the server for a confirmation code on behalf of one login
// run and prints the outcome. The first request and every Enter go through it,
// so the cooldown and the re-dating of the secret are handled in one place.
type codeRequester struct {
	client     APIClient
	in         io.Reader
	out        io.Writer
	email      string
	secret     string
	secretHash string
}

// request asks for a code with the given resend and prints what happened. A
// cooldown refusal that says how long it lasts is waited out and the same
// request made again, for as long as the server keeps refusing; each wait is at
// least a second and Ctrl-C ends the run. A cooldown refusal without a length
// comes from an older server, whose code for this address is still out, so it
// is reported as already sent. Any other failure is returned unprinted, for the
// caller to render. After a countdown, whatever the user typed since the wait
// began, including during the repeated request's round trip, is discarded as
// soon as that request returns and before anything is printed, so an Enter
// pressed while nothing was on screen to answer does not ask for another code.
// Input typed once the prompt appears is left alone.
func (r codeRequester) request(resend bool) error {
	waited := false
	for {
		resp, err := r.client.Register(r.email, true, r.secretHash, resend)
		if waited {
			discardTypeahead(r.in)
		}
		if err == nil {
			r.issued(resp)
			return nil
		}
		apiErr, ok := err.(*api.ErrorResponse)
		if !ok || apiErr.Code != "rate_limited" {
			return err
		}
		if apiErr.RetryAfter < 1 {
			fmt.Fprintln(r.out, i18n.T("login.code_already_sent"))
			return nil
		}
		r.countDown(apiErr.RetryAfter)
		waited = true
	}
}

func (r codeRequester) issued(resp *api.RegisterResponse) {
	if resp.Status == "pending" {
		fmt.Fprintln(r.out, i18n.Tf("login.code_pending", r.email))
	} else {
		fmt.Fprintln(r.out, i18n.Tf("login.confirmation_sent", r.email))
	}
	fmt.Fprintln(r.out, i18n.T("login.new_code_hint"))

	// The secret file is dated from when it was made, so a code sent or resumed
	// later would outlive it, and a re-run after the file lapsed could not
	// resume that code. Moving the file's expiry to the code's keeps the two
	// together. A failed write is not reported: this run holds the secret in
	// memory and can finish without the file, which only a later re-run reads.
	ttl := secretTTL
	if resp.ExpiresIn > 0 {
		ttl = time.Duration(resp.ExpiresIn) * time.Second
	}
	_ = core.SaveLoginSecret(r.email, r.secret, time.Now().Add(ttl))
}

const maxCooldownSeconds = 3600

// countDown waits out a cooldown of the given seconds. On a terminal the time
// left is rewritten in place once a second and the row is cleared at the end,
// so what the repeated request prints takes its place. Elsewhere the rewrite
// would print raw escape codes, so one line goes out and the whole wait is
// slept at once. The wait is capped at maxCooldownSeconds: a larger value
// would overflow the conversion to a Duration, return the sleep at once, and
// spin the request loop, and no real cooldown comes near it.
func (r codeRequester) countDown(seconds int) {
	seconds = min(seconds, maxCooldownSeconds)
	if !isTerminal(r.out) {
		fmt.Fprintln(r.out, i18n.Tf("login.resend_wait", seconds))
		sleep(time.Duration(seconds) * time.Second)
		return
	}
	for remaining := seconds; remaining >= 1; remaining-- {
		streamui.RewriteRow(r.out, i18n.Tf("login.resend_wait", remaining))
		sleep(time.Second)
	}
	streamui.RewriteRow(r.out, "")
}

// retryableNewCodeFailure reports whether a failed request for a new code
// leaves the run at the prompt, where another Enter tries again: a throttled
// request, a failed send, a server error, or no answer from the API at all. The
// line printed for these claims nothing about the earlier code, since the
// server replaces it before it sends the mail and a send reported as failed
// can still arrive. A version refusal, or a refusal the loop has no copy for,
// ends the run instead.
func retryableNewCodeFailure(err error) bool {
	if errors.Is(err, api.ErrUpgradeRequired) {
		return false
	}
	apiErr, ok := err.(*api.ErrorResponse)
	if !ok {
		return true
	}
	switch apiErr.Code {
	case "auth_rate_limited", "email_error", "server_error":
		return true
	}
	return false
}

// rejectedCodeLine renders a verify refusal that leaves the run at the prompt,
// and reports false for any other error. None of these asks for a new code on
// its own: the user asks, with Enter.
func rejectedCodeLine(err error) (string, bool) {
	apiErr, ok := err.(*api.ErrorResponse)
	if !ok {
		return "", false
	}
	switch apiErr.Code {
	case "invalid_code":
		switch left := apiErr.AttemptsLeft; {
		case left == nil:
			// The count is missing either because the server predates it or
			// because the code no longer belongs to this run's secret, a request
			// from elsewhere having replaced it. Only a new code helps in the
			// second case, so the line points at Enter.
			return i18n.T("login.err_incorrect_or_expired_code"), true
		case *left > 1:
			return i18n.Tf("login.err_incorrect_code_attempts_left_many", *left), true
		case *left == 1:
			return i18n.T("login.err_incorrect_code_attempts_left_one"), true
		default:
			return i18n.T("login.err_incorrect_code_no_attempts_left"), true
		}
	case "code_expired":
		return i18n.T("login.err_code_expired"), true
	case "too_many_attempts":
		return i18n.T("login.err_too_many_attempts"), true
	}
	return "", false
}

func getOrCreateSecret(email string) (string, error) {
	ls, err := core.LoadLoginSecret()
	if err == nil && ls != nil && ls.IsValid(email) {
		return ls.Secret.Token, nil
	}

	secret, err := generateSecret()
	if err != nil {
		return "", err
	}

	expiresAt := time.Now().Add(secretTTL)
	if err := core.SaveLoginSecret(email, secret, expiresAt); err != nil {
		return "", err
	}

	return secret, nil
}

func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("%s: %w", i18n.T("login.err_generate_secret"), err)
	}
	return hex.EncodeToString(b), nil
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func handleAPIError(out io.Writer, err error) error {
	// Ahead of the type assertion below: a version refusal is a sentinel rather
	// than a coded error response, so it would otherwise fall through to the
	// network copy and tell the user something false.
	if errors.Is(err, api.ErrUpgradeRequired) {
		fmt.Fprintln(out, api.ErrUpgradeRequired)
		return err
	}

	apiErr, ok := err.(*api.ErrorResponse)
	if !ok {
		fmt.Fprintln(out, i18n.T("login.err_network"))
		return err
	}

	switch apiErr.Code {
	case "invalid_email":
		fmt.Fprintln(out, i18n.T("login.err_invalid_email"))
	case "tos_required":
		fmt.Fprintln(out, i18n.T("login.err_tos_required"))
	case "invite_required":
		// The flow aborts here without ever prompting for a code, so the copy
		// must not send the user looking for a mail that was never sent.
		fmt.Fprintln(out, i18n.T("login.err_invite_required"))
	case "auth_rate_limited":
		// A flood refusal, kept apart from the per-email cooldown the code
		// request counts down: rendering a throttled request as that cooldown
		// would tell the user a code is out when none was sent.
		fmt.Fprintln(out, i18n.T("login.err_auth_rate_limited"))
	default:
		fmt.Fprintln(out, i18n.T("login.err_server"))
	}

	return err
}

// foldEmailCase lowercases the ASCII letters A to Z and copies every other byte
// as it is, including bytes that are not valid UTF-8, so address validation
// still sees and refuses such a byte. The server matches account emails with
// exactly this fold, so strings.ToLower, which also maps non-ASCII capitals,
// could name an address the server never stored.
func foldEmailCase(email string) string {
	folded := []byte(email)
	for i, b := range folded {
		if 'A' <= b && b <= 'Z' {
			folded[i] = b + ('a' - 'A')
		}
	}
	return string(folded)
}
