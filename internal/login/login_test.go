package login

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/keyname"
)

type mockClient struct {
	registerFn func(email string, tosAccepted bool, secretHash string, resend bool) (*api.RegisterResponse, error)
	verifyFn   func(email, code, secret, name string) (*api.VerifyResponse, error)

	// names holds the key name each Verify call carried, in order.
	names []string
}

func (m *mockClient) Register(email string, tosAccepted bool, secretHash string, resend bool) (*api.RegisterResponse, error) {
	return m.registerFn(email, tosAccepted, secretHash, resend)
}

func (m *mockClient) Verify(email, code, secret, name string, _ bool) (*api.VerifyResponse, error) {
	m.names = append(m.names, name)
	return m.verifyFn(email, code, secret, name)
}

// mintingClient answers the two calls of a successful login, checking nothing:
// it serves the tests that hold the closing lines and the saved file rather
// than the flow itself.
func mintingClient(apiKey string) *mockClient {
	return &mockClient{
		registerFn: func(string, bool, string, bool) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(string, string, string, string) (*api.VerifyResponse, error) {
			return &api.VerifyResponse{APIKey: apiKey}, nil
		},
	}
}

// testLang is the catalog every test here renders against. A test that switches
// away restores this rather than a literal of its own, so the pin moves in one
// place.
const testLang = "en"

// The catalog is pinned so the assertions below hold whatever locale the suite
// runs under. Load rather than Setup: nothing here reports which language is in
// force, only renders in it. The tests that check the Japanese copy swap the
// catalog themselves and swap it back; none of them is parallel, since the
// catalog is an unsynchronized map.
//
// KAMAKIRI_API_KEY is cleared for a reason of its own: it outranks the
// credentials file, so on a machine that exports it a test that writes a key
// into its own config home would still load the developer's. There is no
// *testing.T here, hence os.Unsetenv rather than t.Setenv.
//
// The sleep seam is replaced with one that panics, so a test that reaches a
// countdown without stubbing the wait through stubWaits fails at once rather
// than sleeping for real.
//
// The hostname seam answers testHost, so no test sends the real machine's name
// and every expectation about the default name holds on any machine.
func TestMain(m *testing.M) {
	i18n.Load(testLang)
	os.Unsetenv(core.EnvAPIKey)
	sleep = func(d time.Duration) {
		panic(fmt.Sprintf("login test slept for real (%s); stub the wait with stubWaits", d))
	}
	hostname = func() (string, error) { return testHost, nil }
	os.Exit(m.Run())
}

const testHost = "test-host"

func TestLoginSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(email string, _ bool, secretHash string, _ bool) (*api.RegisterResponse, error) {
			if email != "user@example.com" {
				t.Errorf("email = %q", email)
			}
			if secretHash == "" {
				t.Error("secretHash should not be empty")
			}
			return &api.RegisterResponse{Message: "Confirmation code sent.", Status: "sent", ExpiresIn: 600}, nil
		},
		verifyFn: func(email, code, secret, _ string) (*api.VerifyResponse, error) {
			if email != "user@example.com" {
				t.Errorf("email = %q", email)
			}
			if code != "ABC123" {
				t.Errorf("code = %q", code)
			}
			if secret == "" {
				t.Error("secret should not be empty")
			}
			return &api.VerifyResponse{APIKey: "kk_live_testkey123"}, nil
		},
	}

	stdin := strings.NewReader("user@example.com\nY\nABC123\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Prompts do not echo what the reader supplies, so each answer is absent
	// and the line that follows a prompt starts right after it.
	credPath, err := core.CredentialsPath()
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	want := "Email: Terms of Service: https://kamakiri-labs.jp/en/terms/\n" +
		"Privacy Policy:   https://kamakiri-labs.jp/en/privacy/\n" +
		"Accept Terms of Service? (Y/n) Confirmation code sent to user@example.com.\n" +
		"To get a new code, press Enter without typing one.\n" +
		"Code: API key saved to " + credPath + "\n"
	if got := stdout.String(); got != want {
		t.Errorf("output:\n got %q\nwant %q", got, want)
	}

	creds, err := core.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials() error = %v", err)
	}
	if creds.APIKey != "kk_live_testkey123" {
		t.Errorf("saved api_key = %q, want kk_live_testkey123", creds.APIKey)
	}
	if creds.Email != "user@example.com" {
		t.Errorf("saved email = %q, want user@example.com", creds.Email)
	}

	ls, _ := core.LoadLoginSecret()
	if ls != nil {
		t.Error("login secret should be deleted after successful login")
	}
}

// Under the variable the login still mints and saves a key; the note is what
// tells the user why nothing about the run changes until the variable is unset.
// The file is read with os.ReadFile rather than through core.LoadCredentials,
// which under the variable hands back the variable's key and would prove
// nothing about what was written.
func TestLoginUnderTheEnvironmentVariableSavesAndSaysSo(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")

	stdin := strings.NewReader("user@example.com\nY\nABC123\n")
	var stdout bytes.Buffer

	if err := Run(mintingClient("kk_live_minted"), stdin, &stdout, Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	saved := strings.Index(output, "API key saved")
	note := strings.Index(output, "KAMAKIRI_API_KEY is set, so it is the key every command uses for now; the saved key applies once it is unset.")
	if saved < 0 {
		t.Fatalf("output missing save message: %q", output)
	}
	if note < 0 {
		t.Fatalf("output missing the override note: %q", output)
	}
	if note < saved {
		t.Errorf("the override note came before the save message: %q", output)
	}

	credPath, err := core.CredentialsPath()
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading the credentials file: %v", err)
	}
	var written struct {
		APIKey string `json:"api_key"`
		Email  string `json:"email"`
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("decoding the credentials file: %v", err)
	}
	if written.APIKey != "kk_live_minted" {
		t.Errorf("saved api_key = %q, want the key the server minted", written.APIKey)
	}
	if written.Email != "user@example.com" {
		t.Errorf("saved email = %q, want user@example.com", written.Email)
	}
}

func TestLoginWithoutTheEnvironmentVariablePrintsNoNote(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KAMAKIRI_API_KEY", "")

	stdin := strings.NewReader("user@example.com\nY\nABC123\n")
	var stdout bytes.Buffer

	if err := Run(mintingClient("kk_live_minted"), stdin, &stdout, Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := stdout.String(); strings.Contains(got, "KAMAKIRI_API_KEY") {
		t.Errorf("output mentions the variable with none set: %q", got)
	}
}

func TestLoginTosRejected(t *testing.T) {
	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			t.Error("Register should not be called")
			return nil, nil
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			t.Error("Verify should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("user@example.com\nn\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, Options{})
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}

	want := "Email: Terms of Service: https://kamakiri-labs.jp/en/terms/\n" +
		"Privacy Policy:   https://kamakiri-labs.jp/en/privacy/\n" +
		"Accept Terms of Service? (Y/n) Error: You must accept the Terms of Service to continue.\n"
	if got := stdout.String(); got != want {
		t.Errorf("output:\n got %q\nwant %q", got, want)
	}
}

func TestLoginInvalidEmail(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "invalid_email", Message: "Invalid email address."}
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			t.Error("Verify should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("bad\nY\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, Options{})
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}

	// The address is refused before the Terms and Privacy lines print.
	want := "Email: Error: Invalid email address.\n"
	if got := stdout.String(); got != want {
		t.Errorf("output:\n got %q\nwant %q", got, want)
	}
}

// The server matches addresses with their ASCII letters folded, so the CLI
// folds them the same way before anything leaves the machine: the local secret,
// both requests and the saved credentials all carry the one form.
func TestLoginFoldsTheTypedEmailToLowercase(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	const folded = "alice@example.com"
	client := &mockClient{
		registerFn: func(email string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			if email != folded {
				t.Errorf("Register email = %q, want %q", email, folded)
			}
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(email, _, _, _ string) (*api.VerifyResponse, error) {
			if email != folded {
				t.Errorf("Verify email = %q, want %q", email, folded)
			}
			return &api.VerifyResponse{APIKey: "kk_live_folded"}, nil
		},
	}

	var stdout bytes.Buffer
	if err := Run(client, strings.NewReader("  Alice@Example.COM \nY\nABC123\n"), &stdout, Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Confirmation code sent to "+folded+".") {
		t.Errorf("output does not name the folded address: %q", stdout.String())
	}
	creds, err := core.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials() error = %v", err)
	}
	if creds.Email != folded {
		t.Errorf("saved email = %q, want %q", creds.Email, folded)
	}
}

// Only A to Z fold. A full Unicode fold would disagree with the server on a
// non-ASCII capital, and the two sides must name one account.
func TestFoldEmailCaseFoldsOnlyASCIILetters(t *testing.T) {
	cases := map[string]string{
		"Alice@Example.COM":         "alice@example.com",
		"already@lower.case":        "already@lower.case",
		"\u00c4lice@Example.com":    "\u00c4lice@example.com",
		"\u0130stanbul@Example.com": "\u0130stanbul@example.com",
		"Z_Q-9+A@X.JP":              "z_q-9+a@x.jp",
		"A\xffB@Example.com":        "a\xffb@example.com",
	}
	for in, want := range cases {
		if got := foldEmailCase(in); got != want {
			t.Errorf("foldEmailCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginAuthRateLimitedAborts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "auth_rate_limited", Message: "Too many requests."}
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			t.Fatal("verify must not be reached after a register flood")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &out, Options{}); err == nil {
		t.Fatal("Run() expected the flood error, got nil")
	}

	output := out.String()
	if !strings.Contains(output, "Too many requests. Please wait a moment and try again.") {
		t.Errorf("output missing EN auth-rate-limited copy: %q", output)
	}
	// A flood must not be framed as a code already sent, which would advance the
	// user to a code prompt with no code waiting.
	if strings.Contains(output, "already sent") {
		t.Errorf("auth_rate_limited was rendered as code_already_sent: %q", output)
	}

	// The same path in Japanese.
	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	var outJA bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &outJA, Options{}); err == nil {
		t.Fatal("Run() (ja) expected the flood error, got nil")
	}
	if !strings.Contains(outJA.String(), "リクエストが多すぎます") {
		t.Errorf("output missing JA auth-rate-limited copy: %q", outJA.String())
	}
}

func TestLoginInviteRequiredAborts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "invite_required", Message: "private beta"}
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			t.Fatal("verify must not be reached when the email is not invited")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &out, Options{}); err == nil {
		t.Fatal("Run() expected the invite-required error, got nil")
	}

	output := out.String()
	if !strings.Contains(output, "Kamakiri Pages is in private beta. To request an invitation, email beta@kamakiri-labs.jp.") {
		t.Errorf("output missing EN invite-required copy: %q", output)
	}
	// A closed-beta refusal is not a code-was-sent state, so nothing here may
	// frame it as one or prompt for a code.
	if strings.Contains(output, "already sent") {
		t.Errorf("invite_required was rendered as code_already_sent: %q", output)
	}
	// A key that is not in the catalog renders as itself, which no output
	// contains, so the absence assertion below would hold for any reason at all.
	codePrompt := i18n.T("login.prompt_code")
	if codePrompt == "login.prompt_code" {
		t.Fatal("login.prompt_code is missing from the catalog")
	}
	if strings.Contains(output, codePrompt) {
		t.Errorf("invite_required must not prompt for a code: %q", output)
	}

	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	var outJA bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &outJA, Options{}); err == nil {
		t.Fatal("Run() (ja) expected the invite-required error, got nil")
	}
	// The assertion needs a substring unique to Japanese: the address is
	// byte-identical across locales, so matching it alone would pass on the
	// English copy too.
	if !strings.Contains(outJA.String(), "プライベートベータ") {
		t.Errorf("output missing localized JA invite-required copy: %q", outJA.String())
	}
	if !strings.Contains(outJA.String(), "beta@kamakiri-labs.jp") {
		t.Errorf("output missing JA invite-required contact address: %q", outJA.String())
	}
}

func TestLoginVerifyAuthRateLimitedAborts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// A flood on verify must render the same way it does on register.
	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			return nil, &api.ErrorResponse{Code: "auth_rate_limited", Message: "Too many requests."}
		},
	}

	var out bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\nABC123\n"), &out, Options{}); err == nil {
		t.Fatal("Run() expected the verify flood error, got nil")
	}
	if !strings.Contains(out.String(), "Too many requests. Please wait a moment and try again.") {
		t.Errorf("verify-flood output missing auth-rate-limited copy: %q", out.String())
	}
}

func TestLoginEmptyTosAccepted(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			return &api.VerifyResponse{APIKey: "kk_live_abc"}, nil
		},
	}

	// An empty answer to the terms prompt means yes.
	stdin := strings.NewReader("user@example.com\n\nABC123\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestLoginSecretReusedOnRateLimit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	var capturedHash string
	client := &mockClient{
		registerFn: func(_ string, _ bool, secretHash string, _ bool) (*api.RegisterResponse, error) {
			capturedHash = secretHash
			return nil, &api.ErrorResponse{Code: "rate_limited", Message: "Please wait."}
		},
		verifyFn: func(_, _, secret, _ string) (*api.VerifyResponse, error) {
			// The secret sent to verify must hash to what register was given,
			// or the channel binding proves nothing.
			got := hashSecret(secret)
			if got != capturedHash {
				t.Errorf("secret hash mismatch: verify secret hashes to %q, register sent %q", got, capturedHash)
			}
			return &api.VerifyResponse{APIKey: "kk_live_ok"}, nil
		},
	}

	stdin := strings.NewReader("user@example.com\nY\nABC123\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestLoginUpgradeRequiredAborts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string, _ bool) (*api.RegisterResponse, error) {
			return nil, api.ErrUpgradeRequired
		},
		verifyFn: func(_, _, _, _ string) (*api.VerifyResponse, error) {
			t.Fatal("verify must not be reached after a refused version")
			return nil, nil
		},
	}

	var out bytes.Buffer
	err := Run(client, strings.NewReader("user@example.com\nY\n"), &out, Options{})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("Run() error = %v, want the upgrade sentinel", err)
	}

	output := out.String()
	// A sentinel whose key left the catalog renders as the key, and the output
	// printed from it carries that same key, so the check below would hold on
	// nothing at all.
	upgradeCopy := api.ErrUpgradeRequired.Error()
	if upgradeCopy == "api.err_upgrade_required" {
		t.Fatal("api.err_upgrade_required is missing from the catalog")
	}
	if !strings.Contains(output, upgradeCopy) {
		t.Errorf("output missing the upgrade copy: %q", output)
	}
	// The sentinel is not a coded error response, so without its own check it
	// would render as a network failure and send the user chasing their
	// connection.
	networkCopy := i18n.T("login.err_network")
	if networkCopy == "login.err_network" {
		t.Fatal("login.err_network is missing from the catalog")
	}
	if strings.Contains(output, networkCopy) {
		t.Errorf("a refused version was rendered as a network error: %q", output)
	}
}

// scriptedClient answers Register and Verify from queues, in order, and logs
// every call it receives, so a test can assert the exact sequence of requests a
// run made as well as what it printed. A call past the end of a queue fails the
// test: the run asked the server for something the scenario does not expect.
type scriptedClient struct {
	t         *testing.T
	registers []registerAnswer
	verifies  []verifyAnswer

	// onRegister and onVerify, when set, run at the start of the matching call,
	// so a test can change the world between two steps of the run.
	onRegister func()
	onVerify   func()

	calls        []string
	hashes       []string
	names        []string
	revokeOthers []bool
}

type registerAnswer struct {
	resp *api.RegisterResponse
	err  error
}

type verifyAnswer struct {
	resp *api.VerifyResponse
	err  error
}

func (c *scriptedClient) Register(email string, tosAccepted bool, secretHash string, resend bool) (*api.RegisterResponse, error) {
	c.calls = append(c.calls, fmt.Sprintf("register resend=%t", resend))
	if email != "user@example.com" || !tosAccepted {
		c.t.Errorf("Register(email=%q, tosAccepted=%t), want user@example.com and true", email, tosAccepted)
	}
	if c.onRegister != nil {
		c.onRegister()
	}
	c.hashes = append(c.hashes, secretHash)
	if len(c.registers) == 0 {
		c.t.Fatalf("unexpected Register(resend=%t); calls so far: %q", resend, c.calls)
	}
	next := c.registers[0]
	c.registers = c.registers[1:]
	return next.resp, next.err
}

func (c *scriptedClient) Verify(email, code, secret, name string, revokeOthers bool) (*api.VerifyResponse, error) {
	c.calls = append(c.calls, "verify "+code)
	c.names = append(c.names, name)
	c.revokeOthers = append(c.revokeOthers, revokeOthers)
	if email != "user@example.com" {
		c.t.Errorf("Verify(email=%q), want user@example.com", email)
	}
	if c.onVerify != nil {
		c.onVerify()
	}
	if hashSecret(secret) != c.hashes[len(c.hashes)-1] {
		c.t.Errorf("verify %s: the secret does not hash to the one register was given", code)
	}
	if len(c.verifies) == 0 {
		c.t.Fatalf("unexpected Verify(%q); calls so far: %q", code, c.calls)
	}
	next := c.verifies[0]
	c.verifies = c.verifies[1:]
	return next.resp, next.err
}

func sent(expiresIn int) registerAnswer {
	return registerAnswer{resp: &api.RegisterResponse{Message: "Confirmation code sent.", Status: "sent", ExpiresIn: expiresIn}}
}

func pending(expiresIn int) registerAnswer {
	return registerAnswer{resp: &api.RegisterResponse{Message: "A confirmation code is already pending.", Status: "pending", ExpiresIn: expiresIn}}
}

func cooldown(retryAfter int) registerAnswer {
	return registerAnswer{err: &api.ErrorResponse{Code: "rate_limited", Message: "Please wait before requesting another code.", RetryAfter: retryAfter}}
}

func registerFails(err error) registerAnswer { return registerAnswer{err: err} }

func minted() verifyAnswer { return verifyAnswer{resp: &api.VerifyResponse{APIKey: "kk_live_minted"}} }

func refused(code string, attemptsLeft *int) verifyAnswer {
	return verifyAnswer{err: &api.ErrorResponse{Code: code, Message: "Invalid or expired code.", AttemptsLeft: attemptsLeft}}
}

func left(n int) *int { return &n }

// stubWaits swaps the countdown's two seams for the length of one test: the
// sleep records what it was asked for instead of waiting, and the terminal
// check answers as the test says. It returns the recorded sleeps.
func stubWaits(t *testing.T, terminal bool) *[]time.Duration {
	prevSleep, prevTerminal := sleep, isTerminal
	t.Cleanup(func() { sleep, isTerminal = prevSleep, prevTerminal })
	slept := []time.Duration{}
	sleep = func(d time.Duration) { slept = append(slept, d) }
	isTerminal = func(io.Writer) bool { return terminal }
	return &slept
}

// traceWaits logs the countdown's sleeps and the typeahead flush into the
// client's call log, so a test can assert their order against the requests. It
// returns the readers the flush was given. Call it after stubWaits.
func traceWaits(t *testing.T, client *scriptedClient) *[]io.Reader {
	t.Helper()
	prevSleep, prevDiscard := sleep, discardTypeahead
	t.Cleanup(func() { sleep, discardTypeahead = prevSleep, prevDiscard })
	sleep = func(d time.Duration) {
		prevSleep(d)
		client.calls = append(client.calls, "sleep "+d.String())
	}
	flushed := []io.Reader{}
	discardTypeahead = func(in io.Reader) {
		flushed = append(flushed, in)
		client.calls = append(client.calls, "discard")
	}
	return &flushed
}

// runScripted drives one login for user@example.com in a config home of its
// own, answering the Terms prompt with Y and then feeding codeLines, one per
// Code: prompt. It returns what the run printed and the error it ended with.
func runScripted(t *testing.T, client *scriptedClient, codeLines ...string) (string, error) {
	t.Helper()
	return runScriptedWith(t, client, Options{}, codeLines...)
}

// runScriptedWith is runScripted with the command-line options given.
func runScriptedWith(t *testing.T, client *scriptedClient, opts Options, codeLines ...string) (string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	input := "user@example.com\nY\n"
	for _, line := range codeLines {
		input += line + "\n"
	}
	var out bytes.Buffer
	err := Run(client, strings.NewReader(input), &out, opts)
	return out.String(), err
}

func savedLine(t *testing.T) string {
	t.Helper()
	credPath, err := core.CredentialsPath()
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	return "API key saved to " + credPath + "\n"
}

func assertCalls(t *testing.T, client *scriptedClient, want ...string) {
	t.Helper()
	if strings.Join(client.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n got %q\nwant %q", client.calls, want)
	}
}

func assertOutput(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("output:\n got %q\nwant %q", got, want)
	}
}

// The lines the run prints, kept literal so each test compares the whole block
// against the copy itself rather than against the catalog it is checking.
const (
	header      = "Email: Terms of Service: https://kamakiri-labs.jp/en/terms/\nPrivacy Policy:   https://kamakiri-labs.jp/en/privacy/\nAccept Terms of Service? (Y/n) "
	sentLine    = "Confirmation code sent to user@example.com.\n"
	pendingLine = "A confirmation code for user@example.com was already requested and is still valid.\n"
	hintLine    = "To get a new code, press Enter without typing one.\n"
	prompt      = "Code: "
	unconfirmed = "Error: Could not confirm that a new code was sent. Press Enter to try again.\n"
	alreadySent = "A confirmation code was already sent. Check your email.\n"
)

// The first request lets a same-machine re-run resume its code, and only an
// Enter asks for a new one. The request after a countdown repeats the value of
// the one that was refused, so a first request refused during a cooldown still
// resumes rather than replacing a code.
func TestLoginSendsResendFalseFirstAndTrueOnEnter(t *testing.T) {
	stubWaits(t, false)
	client := &scriptedClient{
		t: t,
		registers: []registerAnswer{
			cooldown(1), sent(600),
			sent(600),
			cooldown(1), sent(600),
		},
		verifies: []verifyAnswer{minted()},
	}

	_, err := runScripted(t, client, "", "", "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertCalls(t, client,
		"register resend=false", "register resend=false",
		"register resend=true",
		"register resend=true", "register resend=true",
		"verify ABC123")
}

// A pending code is the one the user already holds, so the run goes straight to
// verifying it and sends nothing more.
func TestLoginPendingResumesWithoutASecondRegister(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{pending(300)}, verifies: []verifyAnswer{minted()}}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+pendingLine+hintLine+prompt+savedLine(t))
	assertCalls(t, client, "register resend=false", "verify ABC123")
}

// An older server's 200 carries no status, and such a server sent a code.
func TestLoginAnswerWithoutStatusReadsAsSent(t *testing.T) {
	client := &scriptedClient{
		t:         t,
		registers: []registerAnswer{{resp: &api.RegisterResponse{Message: "Confirmation code sent."}}},
		verifies:  []verifyAnswer{minted()},
	}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t))
}

// Each wrong code says how many attempts the code has left, and at none the run
// stays at the prompt until an Enter asks for a new code.
func TestLoginWrongCodesCountTheAttemptsLeft(t *testing.T) {
	client := &scriptedClient{
		t:         t,
		registers: []registerAnswer{sent(600), sent(600)},
		verifies: []verifyAnswer{
			refused("invalid_code", left(4)),
			refused("invalid_code", left(3)),
			refused("invalid_code", left(2)),
			refused("invalid_code", left(1)),
			refused("invalid_code", left(0)),
			minted(),
		},
	}

	out, err := runScripted(t, client, "WRONG1", "WRONG2", "WRONG3", "WRONG4", "WRONG5", "", "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+
		prompt+"Error: Incorrect code. 4 attempts left.\n"+
		prompt+"Error: Incorrect code. 3 attempts left.\n"+
		prompt+"Error: Incorrect code. 2 attempts left.\n"+
		prompt+"Error: Incorrect code. 1 attempt left.\n"+
		prompt+"Error: Incorrect code. No attempts are left for this code; press Enter for a new one.\n"+
		prompt+sentLine+hintLine+
		prompt+savedLine(t))
	assertCalls(t, client,
		"register resend=false",
		"verify WRONG1", "verify WRONG2", "verify WRONG3", "verify WRONG4", "verify WRONG5",
		"register resend=true",
		"verify ABC123")
}

// An expired code and a code out of attempts are reported and the run waits at
// the prompt; a new code goes out only once an empty line asks.
func TestLoginExpiredAndUsedUpCodesWaitForEnter(t *testing.T) {
	cases := []struct {
		code string
		line string
	}{
		{"code_expired", "Error: This code has expired. Press Enter for a new code.\n"},
		{"too_many_attempts", "Error: Too many failed attempts for this code. Press Enter for a new code.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			client := &scriptedClient{
				t:         t,
				registers: []registerAnswer{sent(600), sent(600)},
				verifies:  []verifyAnswer{refused(tc.code, nil), refused(tc.code, nil), minted()},
			}

			out, err := runScripted(t, client, "OLD111", "OLD222", "", "ABC123")
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertOutput(t, out, header+sentLine+hintLine+
				prompt+tc.line+
				prompt+tc.line+
				prompt+sentLine+hintLine+
				prompt+savedLine(t))
			assertCalls(t, client,
				"register resend=false",
				"verify OLD111", "verify OLD222",
				"register resend=true",
				"verify ABC123")
		})
	}
}

// Without attempts_left the server either predates the field or no longer
// holds this run's secret, so the line fits both and points at Enter.
func TestLoginWrongCodeWithoutAttemptsLeftAsksAgain(t *testing.T) {
	client := &scriptedClient{
		t:         t,
		registers: []registerAnswer{sent(600)},
		verifies:  []verifyAnswer{refused("invalid_code", nil), minted()},
	}

	out, err := runScripted(t, client, "WRONG1", "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+
		prompt+"Error: Incorrect or expired code. Press Enter for a new code.\n"+
		prompt+savedLine(t))
	assertCalls(t, client, "register resend=false", "verify WRONG1", "verify ABC123")
}

// The cooldown off a terminal: one line, one sleep of the whole wait, then the
// request again.
func TestLoginCooldownWaitsThenSendsOffATerminal(t *testing.T) {
	slept := stubWaits(t, false)
	client := &scriptedClient{t: t, registers: []registerAnswer{cooldown(2), sent(600)}, verifies: []verifyAnswer{minted()}}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+
		"A code was requested for this address recently. Sending a new one in 2s...\n"+
		sentLine+hintLine+prompt+savedLine(t))
	if want := []time.Duration{2 * time.Second}; fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Errorf("slept %v, want %v", *slept, want)
	}
}

// The cooldown on a terminal: the row is rewritten once a second, cleared, and
// the answer to the repeated request lands on the cleared row.
func TestLoginCooldownCountsDownOnATerminal(t *testing.T) {
	slept := stubWaits(t, true)
	client := &scriptedClient{t: t, registers: []registerAnswer{cooldown(2), sent(600)}, verifies: []verifyAnswer{minted()}}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+
		"\r\033[KA code was requested for this address recently. Sending a new one in 2s..."+
		"\r\033[KA code was requested for this address recently. Sending a new one in 1s..."+
		"\r\033[K"+
		sentLine+hintLine+prompt+savedLine(t))
	if want := []time.Duration{time.Second, time.Second}; fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Errorf("slept %v, want %v", *slept, want)
	}
}

// An older server's cooldown refusal carries no retry_after: the run says a
// code was already sent and goes to the prompt, first request and Enter alike,
// without waiting.
func TestLoginCooldownWithoutRetryAfterGoesToThePrompt(t *testing.T) {
	slept := stubWaits(t, false)
	client := &scriptedClient{t: t, registers: []registerAnswer{cooldown(0), cooldown(0)}, verifies: []verifyAnswer{minted()}}

	out, err := runScripted(t, client, "", "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+alreadySent+prompt+alreadySent+prompt+savedLine(t))
	assertCalls(t, client, "register resend=false", "register resend=true", "verify ABC123")
	if len(*slept) != 0 {
		t.Errorf("slept %v, want no wait", *slept)
	}
}

// The secret on disk is re-dated to the code's own expiry, so a re-run can
// resume the code for as long as the server still holds it. The run ends at end
// of input so the secret is still there to read.
func TestLoginRedatesTheSecretToTheCodesExpiry(t *testing.T) {
	cases := []struct {
		name   string
		answer registerAnswer
		want   time.Duration
	}{
		{"sent", sent(900), 900 * time.Second},
		{"pending", pending(120), 120 * time.Second},
		{"no expires_in", registerAnswer{resp: &api.RegisterResponse{Message: "Confirmation code sent."}}, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{tc.answer}}

			before := time.Now()
			if _, err := runScripted(t, client); err == nil {
				t.Fatal("Run() ended without an error at end of input")
			}

			ls, err := core.LoadLoginSecret()
			if err != nil || ls == nil {
				t.Fatalf("LoadLoginSecret() = %v, %v; want the saved secret", ls, err)
			}
			if got := ls.Secret.ExpiresAt.Sub(before); got < tc.want-5*time.Second || got > tc.want+5*time.Second {
				t.Errorf("secret expires %s after the run started, want about %s", got, tc.want)
			}
			if hashSecret(ls.Secret.Token) != client.hashes[0] {
				t.Error("the saved secret is not the one whose hash went to register")
			}
			if ls.Email != "user@example.com" {
				t.Errorf("saved email = %q, want user@example.com", ls.Email)
			}
		})
	}
}

// An Enter request refused by the flood guard, failing to send, failing on the
// server or getting no answer at all says the new code is unconfirmed and keeps
// the run at the prompt; the same failure on the first request ends the run,
// since there is no prompt yet to go back to.
func TestLoginNewCodeRequestFailuresKeepTheRunAlive(t *testing.T) {
	transport := errors.New("request failed: dial tcp 127.0.0.1:4000: connect: connection refused")
	cases := []struct {
		name      string
		err       error
		firstLine string
	}{
		{"auth_rate_limited", &api.ErrorResponse{Code: "auth_rate_limited", Message: "Too many requests."}, "Error: Too many requests. Please wait a moment and try again.\n"},
		{"email_error", &api.ErrorResponse{Code: "email_error", Message: "Failed to send confirmation email. Try again."}, "Error: Server error. Try again. If the problem persists, email support@kamakiri-labs.jp.\n"},
		{"server_error", &api.ErrorResponse{Code: "server_error", Message: "Registration failed. Try again."}, "Error: Server error. Try again. If the problem persists, email support@kamakiri-labs.jp.\n"},
		{"transport", transport, "Error: Could not connect to the server. Check your network and try again.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" on Enter", func(t *testing.T) {
			client := &scriptedClient{
				t:         t,
				registers: []registerAnswer{sent(600), registerFails(tc.err)},
				verifies:  []verifyAnswer{minted()},
			}

			out, err := runScripted(t, client, "", "ABC123")
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertOutput(t, out, header+sentLine+hintLine+prompt+unconfirmed+prompt+savedLine(t))
			assertCalls(t, client, "register resend=false", "register resend=true", "verify ABC123")
		})
		t.Run(tc.name+" on the first request", func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{registerFails(tc.err)}}

			out, err := runScripted(t, client, "ABC123")
			if err != tc.err {
				t.Fatalf("Run() error = %v, want %v", err, tc.err)
			}
			assertOutput(t, out, header+tc.firstLine)
		})
	}
}

// On an Enter request, a version refusal and a refusal of the request itself
// still end the run, as they do on the first request. The refused request is
// reported with the server's own message, which says what was wrong with it.
func TestLoginNewCodeRequestRefusalsEndTheRun(t *testing.T) {
	cases := []struct {
		name string
		err  error
		line string
	}{
		{"upgrade required", api.ErrUpgradeRequired, api.ErrUpgradeRequired.Error() + "\n"},
		{"invalid_request", &api.ErrorResponse{Code: "invalid_request", Message: "resend must be true or false."}, "Error: resend must be true or false.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600), registerFails(tc.err)}}

			out, err := runScripted(t, client, "", "ABC123")
			if !errors.Is(err, tc.err) {
				t.Fatalf("Run() error = %v, want %v", err, tc.err)
			}
			assertOutput(t, out, header+sentLine+hintLine+prompt+tc.line)
			assertCalls(t, client, "register resend=false", "register resend=true")
		})
	}
}

// End of input at the code prompt ends the run with an error, whether or not an
// Enter came first.
func TestLoginEndOfInputAtTheCodePromptIsAnError(t *testing.T) {
	cases := map[string][]string{
		"at the first prompt": nil,
		"after an Enter":      {""},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600), sent(600)}}

			_, err := runScripted(t, client, lines...)
			if !errors.Is(err, io.EOF) {
				t.Errorf("Run() error = %v, want io.EOF", err)
			}
		})
	}
}

// Two refusals in a row count down twice, with no cap: a mutant that retries
// only once would end the run on the second refusal. Every repeat carries the
// resend value of the request that was refused, on the first request and on an
// Enter alike.
func TestLoginRepeatedCooldownsCountDownEachTime(t *testing.T) {
	wait := func(n int) string {
		return fmt.Sprintf("A code was requested for this address recently. Sending a new one in %ds...\n", n)
	}
	t.Run("first request", func(t *testing.T) {
		slept := stubWaits(t, false)
		client := &scriptedClient{t: t, registers: []registerAnswer{cooldown(1), cooldown(2), sent(600)}, verifies: []verifyAnswer{minted()}}

		out, err := runScripted(t, client, "ABC123")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		assertOutput(t, out, header+wait(1)+wait(2)+sentLine+hintLine+prompt+savedLine(t))
		assertCalls(t, client, "register resend=false", "register resend=false", "register resend=false", "verify ABC123")
		if want := []time.Duration{time.Second, 2 * time.Second}; fmt.Sprint(*slept) != fmt.Sprint(want) {
			t.Errorf("slept %v, want %v", *slept, want)
		}
	})
	t.Run("on Enter", func(t *testing.T) {
		slept := stubWaits(t, false)
		client := &scriptedClient{t: t, registers: []registerAnswer{sent(600), cooldown(1), cooldown(2), sent(600)}, verifies: []verifyAnswer{minted()}}

		out, err := runScripted(t, client, "", "ABC123")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		assertOutput(t, out, header+sentLine+hintLine+prompt+wait(1)+wait(2)+sentLine+hintLine+prompt+savedLine(t))
		assertCalls(t, client, "register resend=false", "register resend=true", "register resend=true", "register resend=true", "verify ABC123")
		if want := []time.Duration{time.Second, 2 * time.Second}; fmt.Sprint(*slept) != fmt.Sprint(want) {
			t.Errorf("slept %v, want %v", *slept, want)
		}
	})
}

// The countdown on a terminal also runs for an Enter: the prompt stays on its
// row, the wait rewrites the row after it, the clearing sequence hands the row
// back and the answer to the repeated request lands on it.
func TestLoginCooldownCountsDownOnATerminalAfterAnEnter(t *testing.T) {
	slept := stubWaits(t, true)
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600), cooldown(2), sent(600)}, verifies: []verifyAnswer{minted()}}

	out, err := runScripted(t, client, "", "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+
		"\r\033[KA code was requested for this address recently. Sending a new one in 2s..."+
		"\r\033[KA code was requested for this address recently. Sending a new one in 1s..."+
		"\r\033[K"+
		sentLine+hintLine+prompt+savedLine(t))
	assertCalls(t, client, "register resend=false", "register resend=true", "register resend=true", "verify ABC123")
	if want := []time.Duration{time.Second, time.Second}; fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Errorf("slept %v, want %v", *slept, want)
	}
}

// Whatever was typed since a wait began is discarded once, after the request
// that follows the wait has returned and before the run prints anything more, so
// an Enter pressed during the wait or during that request does not ask for a
// further code. The flush is given the reader the run reads its input from. A
// run with no countdown never discards.
func TestLoginDiscardsTypeaheadOnceAfterEachCountdown(t *testing.T) {
	cases := []struct {
		name      string
		terminal  bool
		registers []registerAnswer
		lines     []string
		want      []string
		flushes   int
		// reported is what the repeated request's answer prints, which must
		// still be to come at the last flush.
		reported string
	}{
		{"first request off a terminal", false, []registerAnswer{cooldown(2), sent(600)}, []string{"ABC123"},
			[]string{"register resend=false", "sleep 2s", "register resend=false", "discard", "verify ABC123"}, 1, sentLine},
		{"first request on a terminal", true, []registerAnswer{cooldown(2), sent(600)}, []string{"ABC123"},
			[]string{"register resend=false", "sleep 1s", "sleep 1s", "register resend=false", "discard", "verify ABC123"}, 1, sentLine},
		{"Enter off a terminal", false, []registerAnswer{sent(600), cooldown(2), sent(600)}, []string{"", "ABC123"},
			[]string{"register resend=false", "register resend=true", "sleep 2s", "register resend=true", "discard", "verify ABC123"}, 1, sentLine},
		{"Enter on a terminal", true, []registerAnswer{sent(600), cooldown(2), sent(600)}, []string{"", "ABC123"},
			[]string{"register resend=false", "register resend=true", "sleep 1s", "sleep 1s", "register resend=true", "discard", "verify ABC123"}, 1, sentLine},
		{"two countdowns in a row", false, []registerAnswer{cooldown(1), cooldown(1), sent(600)}, []string{"ABC123"},
			[]string{"register resend=false", "sleep 1s", "register resend=false", "discard", "sleep 1s", "register resend=false", "discard", "verify ABC123"}, 2, sentLine},
		{"Enter then a transport failure", false, []registerAnswer{sent(600), cooldown(2), registerFails(errors.New("request failed: connection refused"))}, []string{"", "ABC123"},
			[]string{"register resend=false", "register resend=true", "sleep 2s", "register resend=true", "discard", "verify ABC123"}, 1, unconfirmed},
		{"Enter then server_error", false, []registerAnswer{sent(600), cooldown(2), registerFails(&api.ErrorResponse{Code: "server_error", Message: "Registration failed. Try again."})}, []string{"", "ABC123"},
			[]string{"register resend=false", "register resend=true", "sleep 2s", "register resend=true", "discard", "verify ABC123"}, 1, unconfirmed},
		{"Enter then a cooldown without retry_after", false, []registerAnswer{sent(600), cooldown(2), cooldown(0)}, []string{"", "ABC123"},
			[]string{"register resend=false", "register resend=true", "sleep 2s", "register resend=true", "discard", "verify ABC123"}, 1, alreadySent},
		{"no countdown", false, []registerAnswer{sent(600), sent(600), cooldown(0)}, []string{"", "", "ABC123"},
			[]string{"register resend=false", "register resend=true", "register resend=true", "verify ABC123"}, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			stubWaits(t, tc.terminal)
			client := &scriptedClient{t: t, registers: tc.registers, verifies: []verifyAnswer{minted()}}
			flushed := traceWaits(t, client)

			var out bytes.Buffer
			printedAtFlush := []int{}
			traced := discardTypeahead
			discardTypeahead = func(in io.Reader) {
				printedAtFlush = append(printedAtFlush, out.Len())
				traced(in)
			}
			input := "user@example.com\nY\n" + strings.Join(tc.lines, "\n") + "\n"
			in := strings.NewReader(input)

			if err := Run(client, in, &out, Options{}); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertCalls(t, client, tc.want...)
			if len(*flushed) != tc.flushes {
				t.Fatalf("flushed %d times, want %d", len(*flushed), tc.flushes)
			}
			for i, got := range *flushed {
				if got != io.Reader(in) {
					t.Errorf("flush %d was given %v, want the reader the run reads from", i, got)
				}
			}
			// Nothing the repeated request reports is on screen at the last flush,
			// so a line typed in reaction to it could not have been dropped.
			if n := len(printedAtFlush); n > 0 {
				if rest := out.String()[printedAtFlush[n-1]:]; !strings.HasPrefix(rest, tc.reported) {
					t.Errorf("output after the last flush = %q, want it to begin with %q", rest, tc.reported)
				}
			}
		})
	}
}

// The default flush does not fail on input that is not a terminal, and leaves a
// regular file's contents in place. It is a no-crash check: flushing a file is
// harmless in itself, so this does not prove the terminal guard.
func TestDiscardTypeaheadToleratesInputThatIsNotATerminal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("typed\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	discardTypeahead(file)
	discardTypeahead(strings.NewReader("typed\n"))

	rest, err := io.ReadAll(file)
	if err != nil || string(rest) != "typed\n" {
		t.Errorf("file after the flush = %q, %v; want it untouched", rest, err)
	}
}

// The Japanese countdown is redrawn in place on an 80-column terminal, and a row
// that wraps leaves the head of the wrapped line standing on every tick. 999 is
// the widest number the line renders.
func TestResendWaitFitsAnEightyColumnRowInBothCatalogs(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		i18n.Load(lang)
		t.Cleanup(func() { i18n.Load(testLang) })

		line := i18n.Tf("login.resend_wait", 999)
		if line == "login.resend_wait" {
			t.Fatalf("%s: login.resend_wait is missing from the catalog", lang)
		}
		if width := i18n.Width(line); width >= 80 {
			t.Errorf("%s: %q is %d columns wide, want under 80", lang, line, width)
		}
	}
}

// A failed re-save of the secret is neither reported nor fatal: the run holds
// the secret in memory. The config directory is swapped for a file while the
// register call is in flight, which fails the write whoever runs the tests, and
// put back before verify so the closing save can succeed.
func TestLoginCarriesOnWhenTheSecretCannotBeResaved(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{minted()}}
	configDir := func() string {
		path, err := core.LoginSecretPath()
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Dir(path)
	}
	client.onRegister = func() {
		dir := configDir()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("in the way"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client.onVerify = func() {
		dir := configDir()
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t))
}

// A count below zero is a server gone wrong, and reads as none left.
func TestLoginNegativeAttemptsLeftReadsAsNoneLeft(t *testing.T) {
	client := &scriptedClient{
		t:         t,
		registers: []registerAnswer{sent(600)},
		verifies:  []verifyAnswer{refused("invalid_code", left(-1)), minted()},
	}

	out, err := runScripted(t, client, "WRONG1", "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+
		prompt+"Error: Incorrect code. No attempts are left for this code; press Enter for a new one.\n"+
		prompt+savedLine(t))
}

// A verify failure that is not one of the rejections the prompt handles ends the
// run through the shared error rendering.
func TestLoginVerifyServerErrorEndsTheRun(t *testing.T) {
	serverErr := &api.ErrorResponse{Code: "server_error", Message: "Verification failed. Try again."}
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{{err: serverErr}}}

	out, err := runScripted(t, client, "ABC123")
	if err != serverErr {
		t.Fatalf("Run() error = %v, want %v", err, serverErr)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+
		"Error: Server error. Try again. If the problem persists, email support@kamakiri-labs.jp.\n")
}

// A retry_after far beyond any real cooldown must not overflow the conversion
// to a Duration, which would make the sleep return at once and the request
// loop spin. The wait is clamped to an hour instead, on a terminal or off one.
func TestCountDownClampsHugeCooldown(t *testing.T) {
	for _, seconds := range []int{math.MaxInt64, 1 << 62} {
		slept := stubWaits(t, false)
		var out bytes.Buffer
		codeRequester{out: &out}.countDown(seconds)
		if len(*slept) != 1 || (*slept)[0] != time.Hour {
			t.Errorf("countDown(%d) slept %v, want one %v", seconds, *slept, time.Hour)
		}
	}
}

// The server stores only the hash of this secret, so its strength is the whole
// channel binding: it must stay 32 random bytes, hex encoded.
func TestGenerateSecretIs32RandomBytesAsLowercaseHex(t *testing.T) {
	a, err := generateSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || a != strings.ToLower(a) {
		t.Fatalf("secret = %q, want 64 lowercase hex characters", a)
	}
	raw, err := hex.DecodeString(a)
	if err != nil {
		t.Fatalf("secret is not hex: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded length = %d, want 32", len(raw))
	}
	if a == b {
		t.Fatal("two calls returned the same secret")
	}
}

// The copy of the --name refusal, kept literal for the same reason as the
// block of lines above.
const invalidNameLine = "Error: --name must be 1 to 64 characters with no control characters.\n"

// keyed is a successful verify whose answer names the key it issued.
func keyed(name string) verifyAnswer {
	return verifyAnswer{resp: &api.VerifyResponse{
		APIKey: "kk_live_minted",
		Key:    &api.KeyInfo{ID: "0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59", Name: name},
	}}
}

// stubHostname makes the hostname read answer as given for one test.
func stubHostname(t *testing.T, name string, err error) {
	t.Helper()
	prev := hostname
	t.Cleanup(func() { hostname = prev })
	hostname = func() (string, error) { return name, err }
}

// forbidHostname fails the test if the run reads the hostname at all.
func forbidHostname(t *testing.T) {
	t.Helper()
	prev := hostname
	t.Cleanup(func() { hostname = prev })
	hostname = func() (string, error) {
		t.Error("the run read the hostname, want it left alone")
		return "read-anyway", nil
	}
}

func assertNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("names sent to verify = %q, want %q", got, want)
	}
}

// A valid --name is what verify carries, with its surrounding whitespace
// trimmed, and the hostname is never consulted. The rule refuses control
// characters only, so the tilde (just below DEL), the no-break space (just
// above the C1 range), format characters and the line separator all pass, as
// they do on the server.
func TestLoginSendsTheNameGiven(t *testing.T) {
	cases := []struct {
		name  string
		given string
		sent  string
	}{
		{"plain", "ci-box", "ci-box"},
		{"an inner space", "ci box", "ci box"},
		{"NEL at the edges trimmed", "\u0085ci-box\u0085", "ci-box"},
		{"surrounding whitespace trimmed", " \t ci-box \n", "ci-box"},
		{"64 non-ASCII code points", strings.Repeat("名", 64), strings.Repeat("名", 64)},
		{"a tilde", "ci~box", "ci~box"},
		{"a no-break space", "ci\u00a0box", "ci\u00a0box"},
		{"a zero-width joiner", "ci\u200dbox", "ci\u200dbox"},
		{"a bidi override", "ci\u202ebox", "ci\u202ebox"},
		{"a line separator", "ci\u2028box", "ci\u2028box"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forbidHostname(t)
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{minted()}}

			if _, err := runScriptedWith(t, client, Options{Name: tc.given, NameGiven: true}, "ABC123"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertNames(t, client.names, tc.sent)
		})
	}
}

// A --name that breaks the rule ends the run before the email prompt: nothing is
// sent, the hostname is not read, and the refusal is reported the way an
// invalid address is. An empty value is a refusal rather than a request for the
// default, since the flag was given.
func TestLoginRefusesABadNameBeforeAnythingIsSent(t *testing.T) {
	cases := []struct {
		name  string
		given string
	}{
		{"empty", ""},
		{"whitespace only", " \t\n "},
		{"65 code points", strings.Repeat("名", 65)},
		{"an escape character", "ci\x1bbox"},
		{"DEL", "ci\x7fbox"},
		{"a newline inside", "ci\nbox"},
		{"the first C1 control", "ci\u0080box"},
		{"NEL inside", "ci\u0085box"},
		{"the 8-bit CSI", "ci\u009bbox"},
		{"the last C1 control", "ci\u009fbox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			forbidHostname(t)
			client := &scriptedClient{t: t}

			var out bytes.Buffer
			err := Run(client, strings.NewReader("user@example.com\nY\nABC123\n"), &out, Options{Name: tc.given, NameGiven: true})
			if err == nil || err.Error() != "invalid key name" {
				t.Fatalf("Run() error = %v, want invalid key name", err)
			}
			assertOutput(t, out.String(), invalidNameLine)
			assertCalls(t, client)
		})
	}
}

// Without --name the key is named after the machine: control characters are
// dropped, surrounding whitespace is trimmed and the result is cut to 64 code
// points, never inside one. A hostname that cannot be read, or that leaves
// nothing, sends no name, and the server names the key itself. Whatever is sent
// is a name the --name check would accept.
func TestLoginNamesTheKeyAfterTheHostnameByDefault(t *testing.T) {
	cases := []struct {
		name string
		host string
		err  error
		sent string
	}{
		{"plain", "build-box", nil, "build-box"},
		{"80 code points cut to 64", strings.Repeat("ホ", 80), nil, strings.Repeat("ホ", 64)},
		{"64 code points kept", strings.Repeat("ホ", 64), nil, strings.Repeat("ホ", 64)},
		{"65 code points cut to 64", strings.Repeat("ホ", 65), nil, strings.Repeat("ホ", 64)},
		{"control characters dropped", "build\x1b[31m-box\x7f", nil, "build[31m-box"},
		{"C1 controls dropped", "build\u0080-\u009bbox\u009f", nil, "build-box"},
		{"a format character kept", "build\u200d-box", nil, "build\u200d-box"},
		{"surrounding whitespace trimmed", "  build-box \n", nil, "build-box"},
		{"whitespace left by dropped controls trimmed", "\x01 build-box \x02", nil, "build-box"},
		{"only control characters", "\x01\x02\x7f", nil, ""},
		{"only C1 controls", "\u0080\u009b\u009f", nil, ""},
		{"an inner space kept", "build box", nil, "build box"},
		{"read fails", "", errors.New("hostname unavailable"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubHostname(t, tc.host, tc.err)
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{minted()}}

			if _, err := runScripted(t, client, "ABC123"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertNames(t, client.names, tc.sent)
			if tc.sent != "" && !keyname.Valid(tc.sent) {
				t.Errorf("the default name %q would be refused as a --name", tc.sent)
			}
		})
	}
}

// The stub hostname from TestMain is what a run with no flag sends.
func TestLoginWithoutTheFlagSendsTheHostname(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{minted()}}

	if _, err := runScripted(t, client, "ABC123"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertNames(t, client.names, testHost)
}

// The line names the key as the server stored it, which is not always what was
// sent, and sits right after the saved line.
func TestLoginPrintsTheNameTheServerReturned(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{keyed("ci-box (stored)")}}

	out, err := runScriptedWith(t, client, Options{Name: "ci-box", NameGiven: true}, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t)+"Key name: ci-box (stored)\n")
}

// Under the variable the name line still follows the saved line, and the note
// about the variable comes last.
func TestLoginPrintsTheNameBeforeTheEnvironmentNote(t *testing.T) {
	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{keyed("ci-box")}}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t)+
		"Key name: ci-box\n"+
		"KAMAKIRI_API_KEY is set, so it is the key every command uses for now; the saved key applies once it is unset.\n")
}

// A server that reports no key, or a key without a name, gets no name line.
func TestLoginPrintsNoNameLineWithoutOne(t *testing.T) {
	cases := []struct {
		name   string
		answer verifyAnswer
	}{
		{"no key object", minted()},
		{"an empty name", keyed("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{tc.answer}}

			out, err := runScripted(t, client, "ABC123")
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t))
		})
	}
}

// A refused verify request is reported with the server's own message, which
// says what was wrong, rather than as a server error.
func TestLoginVerifyInvalidRequestPrintsTheServersMessage(t *testing.T) {
	refusal := &api.ErrorResponse{Code: "invalid_request", Message: "name must be 1 to 64 characters with no control characters."}
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{{err: refusal}}}

	out, err := runScripted(t, client, "ABC123")
	if err != refusal {
		t.Fatalf("Run() error = %v, want %v", err, refusal)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+
		"Error: name must be 1 to 64 characters with no control characters.\n")
}

// The name comes from the server, so the line must not hand the terminal an
// 8-bit CSI or a bidi override, whatever the server sent.
func TestLoginEscapesTheNameLine(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{keyed("ci\u009b2Jbox\u202ex")}}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t)+`Key name: ci\u{009b}2Jbox\u{202e}x`+"\n")
	if strings.ContainsAny(out, "\u009b\u202e") {
		t.Errorf("output carries a raw control or bidi rune: %q", out)
	}
}

// revoking is a successful verify made with --revoke-others: the key it issued,
// named laptop, and the other keys the server reports it revoked.
func revoking(revoked ...api.KeyInfo) verifyAnswer {
	return verifyAnswer{resp: &api.VerifyResponse{
		APIKey:  "kk_live_minted",
		Key:     &api.KeyInfo{ID: "0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59", Name: "laptop"},
		Revoked: revoked,
	}}
}

var (
	oldLaptop = api.KeyInfo{ID: "5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65", Name: "old-laptop"}
	// A name only a hostile or broken server would send, which the block must
	// escape as the name line does.
	hostileKey = api.KeyInfo{ID: "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c", Name: "ci\u009b2Jbox"}
)

// The flag reaches the verify request, and without it the request asks for no
// revoke.
func TestLoginSendsRevokeOthersToVerify(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run(fmt.Sprintf("RevokeOthers=%t", want), func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{revoking()}}

			if _, err := runScriptedWith(t, client, Options{RevokeOthers: want}, "ABC123"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if !slices.Equal(client.revokeOthers, []bool{want}) {
				t.Errorf("revoke_others sent to verify = %v, want [%t]", client.revokeOthers, want)
			}
		})
	}
}

// The revoked keys are listed after the name line, each name escaped, in the
// order the server gave them; an empty list says so in a line of its own.
func TestLoginPrintsTheKeysItRevoked(t *testing.T) {
	cases := []struct {
		name    string
		revoked []api.KeyInfo
		en      string
		ja      string
	}{
		{
			"none", nil,
			"No other keys were revoked.\n",
			"他に失効させたキーはありません。\n",
		},
		{
			"one", []api.KeyInfo{oldLaptop},
			"Revoked 1 other key:\n  old-laptop (5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n",
			"他のキー1件を失効させました:\n  「old-laptop」(5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n",
		},
		{
			"two, one escaped", []api.KeyInfo{oldLaptop, hostileKey},
			"Revoked 2 other keys:\n  old-laptop (5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n" +
				`  ci\u{009b}2Jbox (c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c)` + "\n",
			"他のキー2件を失効させました:\n  「old-laptop」(5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n" +
				`  「ci\u{009b}2Jbox」(c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c)` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{revoking(tc.revoked...)}}

			out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t)+"Key name: laptop\n"+tc.en)
			if strings.ContainsRune(out, '\u009b') {
				t.Errorf("output carries a raw control rune: %q", out)
			}
		})
		t.Run(tc.name+" (ja)", func(t *testing.T) {
			i18n.Load("ja")
			t.Cleanup(func() { i18n.Load(testLang) })
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{revoking(tc.revoked...)}}

			out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if want := "キー名: laptop\n" + tc.ja; !strings.HasSuffix(out, want) {
				t.Errorf("output:\n got %q\nwant it to end with %q", out, want)
			}
		})
	}
}

// Under the variable the block still follows the name line, and the note about
// the variable comes last: the key the variable holds may be among those
// revoked, and the note says nothing either way.
func TestLoginPrintsTheRevokedKeysBeforeTheEnvironmentNote(t *testing.T) {
	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{revoking(oldLaptop)}}

	out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t)+
		"Key name: laptop\n"+
		"Revoked 1 other key:\n  old-laptop (5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n"+
		"KAMAKIRI_API_KEY is set, so it is the key every command uses for now; the saved key applies once it is unset.\n")
}

// The other keys are gone once verify answers, whether or not the new key can
// be saved, so the block is printed ahead of the save error.
func TestLoginPrintsTheRevokedKeysWhenSavingFails(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{revoking(oldLaptop)}}
	client.onVerify = func() {
		path, err := core.CredentialsPath()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(path)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("in the way"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
	if err == nil || !strings.HasPrefix(err.Error(), "save credentials: ") {
		t.Fatalf("Run() error = %v, want the save error", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+
		"Revoked 1 other key:\n  old-laptop (5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n")
}

// Without the flag nothing was asked to be revoked, so no block is printed,
// not even the line saying none was.
func TestLoginWithoutRevokeOthersPrintsNoRevokedBlock(t *testing.T) {
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{revoking()}}

	out, err := runScripted(t, client, "ABC123")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+savedLine(t)+"Key name: laptop\n")
}

// maybeRevoked is the line a --revoke-others run prints when its verify ended
// without an answer that says whether the other keys were revoked.
const maybeRevoked = "Your other keys may already have been revoked. Run \"kamakiri login --revoke-others\" again to finish.\n"

// A verify that fails in any way other than a refusal the CLI recognises may
// have been carried out by the server, the other keys revoked with it, so with
// --revoke-others the error line is followed by one that says so and how to
// finish. The unreadable success is the case where the server certainly did.
func TestLoginRevokeOthersVerifyWithAnUnknownOutcomeSaysTheKeysMayBeGone(t *testing.T) {
	const (
		networkLine = "Error: Could not connect to the server. Check your network and try again.\n"
		serverLine  = "Error: Server error. Try again. If the problem persists, email support@kamakiri-labs.jp.\n"
	)
	cases := []struct {
		name string
		err  error
		line string
	}{
		{"no answer", errors.New("request failed: dial tcp 127.0.0.1:4000: connect: connection refused"), networkLine},
		{"unreadable success", errors.New("parse response: unexpected end of JSON input"), networkLine},
		{"an answer that is not the API's", errors.New("server returned 413"), networkLine},
		{"server_error", &api.ErrorResponse{Code: "server_error", Message: "Verification failed. Try again."}, serverLine},
		{"a code this CLI does not know", &api.ErrorResponse{Code: "key_quota_exceeded", Message: "Too many keys."}, serverLine},
		{"an error with no code", &api.ErrorResponse{Message: "Something went wrong."}, serverLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{{err: tc.err}}}

			out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
			if err != tc.err {
				t.Fatalf("Run() error = %v, want %v", err, tc.err)
			}
			assertOutput(t, out, header+sentLine+hintLine+prompt+tc.line+maybeRevoked)
		})
	}
}

// The server refuses each of these before it changes anything, so the keys are
// known to be untouched and the run prints only the refusal's own line. Every
// recognised refusal that reaches the end of the run is here, so exempting
// only some of them from the line fails.
func TestLoginRevokeOthersVerifyRefusalPrintsOnlyItsOwnLine(t *testing.T) {
	cases := []struct {
		name string
		err  error
		line string
	}{
		{"invalid_request", &api.ErrorResponse{Code: "invalid_request", Message: "name must be 1 to 64 characters with no control characters."}, "Error: name must be 1 to 64 characters with no control characters.\n"},
		{"auth_rate_limited", &api.ErrorResponse{Code: "auth_rate_limited", Message: "Too many requests."}, "Error: Too many requests. Please wait a moment and try again.\n"},
		{"invalid_email", &api.ErrorResponse{Code: "invalid_email", Message: "Invalid email."}, "Error: Invalid email address.\n"},
		{"tos_required", &api.ErrorResponse{Code: "tos_required", Message: "Terms not accepted."}, "Error: You must accept the Terms of Service to continue.\n"},
		{"invite_required", &api.ErrorResponse{Code: "invite_required", Message: "Invitation required."}, "Kamakiri Pages is in private beta. To request an invitation, email beta@kamakiri-labs.jp.\n"},
		{"upgrade required", api.ErrUpgradeRequired, api.ErrUpgradeRequired.Error() + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{{err: tc.err}}}

			out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
			if err != tc.err {
				t.Fatalf("Run() error = %v, want %v", err, tc.err)
			}
			assertOutput(t, out, header+sentLine+hintLine+prompt+tc.line)
		})
	}
}

// Without the flag nothing was asked to be revoked, so a verify that ends
// without an answer says nothing about other keys.
func TestLoginVerifyWithAnUnknownOutcomeWithoutRevokeOthersSaysNothingOfOtherKeys(t *testing.T) {
	transport := errors.New("request failed: dial tcp 127.0.0.1:4000: connect: connection refused")
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{{err: transport}}}

	out, err := runScripted(t, client, "ABC123")
	if err != transport {
		t.Fatalf("Run() error = %v, want %v", err, transport)
	}
	assertOutput(t, out, header+sentLine+hintLine+prompt+
		"Error: Could not connect to the server. Check your network and try again.\n")
}

// Nothing is revoked before a code is accepted, so a failed code request says
// nothing about other keys, even with the flag.
func TestLoginRevokeOthersCodeRequestFailureSaysNothingOfOtherKeys(t *testing.T) {
	transport := errors.New("request failed: dial tcp 127.0.0.1:4000: connect: connection refused")
	client := &scriptedClient{t: t, registers: []registerAnswer{registerFails(transport)}}

	out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
	if err != transport {
		t.Fatalf("Run() error = %v, want %v", err, transport)
	}
	assertOutput(t, out, header+"Error: Could not connect to the server. Check your network and try again.\n")
	assertCalls(t, client, "register resend=false")
}

// The Japanese line says revoke with 失効, as the revoked-keys block does.
func TestLoginRevokeOthersVerifyWithAnUnknownOutcomeInJapanese(t *testing.T) {
	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })
	transport := errors.New("request failed: dial tcp 127.0.0.1:4000: connect: connection refused")
	client := &scriptedClient{t: t, registers: []registerAnswer{sent(600)}, verifies: []verifyAnswer{{err: transport}}}

	out, err := runScriptedWith(t, client, Options{RevokeOthers: true}, "ABC123")
	if err != transport {
		t.Fatalf("Run() error = %v, want %v", err, transport)
	}
	assertOutput(t, out, "メールアドレス: "+
		"利用規約:             https://kamakiri-labs.jp/ja/terms/\n"+
		"プライバシーポリシー: https://kamakiri-labs.jp/ja/privacy/\n"+
		"利用規約に同意しますか？ (Y/n) "+
		"user@example.com に確認コードを送信しました。\n"+
		"新しいコードを受け取るには、何も入力せずに Enter キーを押してください。\n"+
		"確認コード: "+
		"エラー: サーバーに接続できません。ネットワークを確認してもう一度お試しください。\n"+
		"他のキーはすでに失効している可能性があります。処理を完了するには \"kamakiri login --revoke-others\" を再実行してください。\n")
}
