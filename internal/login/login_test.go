package login

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

type mockClient struct {
	registerFn func(email string, tosAccepted bool, secretHash string) (*api.RegisterResponse, error)
	verifyFn   func(email, code, secret string) (*api.VerifyResponse, error)
}

func (m *mockClient) Register(email string, tosAccepted bool, secretHash string) (*api.RegisterResponse, error) {
	return m.registerFn(email, tosAccepted, secretHash)
}

func (m *mockClient) Verify(email, code, secret string) (*api.VerifyResponse, error) {
	return m.verifyFn(email, code, secret)
}

// mintingClient answers the two calls of a successful login, checking nothing:
// it serves the tests that hold the closing lines and the saved file rather
// than the flow itself.
func mintingClient(apiKey string) *mockClient {
	return &mockClient{
		registerFn: func(string, bool, string) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(string, string, string) (*api.VerifyResponse, error) {
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
func TestMain(m *testing.M) {
	i18n.Load(testLang)
	os.Unsetenv(core.EnvAPIKey)
	os.Exit(m.Run())
}

func TestLoginSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(email string, _ bool, secretHash string) (*api.RegisterResponse, error) {
			if email != "user@example.com" {
				t.Errorf("email = %q", email)
			}
			if secretHash == "" {
				t.Error("secretHash should not be empty")
			}
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(email, code, secret string) (*api.VerifyResponse, error) {
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

	err := Run(client, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Confirmation code sent to user@example.com.") {
		t.Errorf("output missing confirmation message: %q", output)
	}
	if !strings.Contains(output, "API key saved") {
		t.Errorf("output missing save message: %q", output)
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

	if err := Run(mintingClient("kk_live_minted"), stdin, &stdout); err != nil {
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

	if err := Run(mintingClient("kk_live_minted"), stdin, &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := stdout.String(); strings.Contains(got, "KAMAKIRI_API_KEY") {
		t.Errorf("output mentions the variable with none set: %q", got)
	}
}

func TestLoginTosRejected(t *testing.T) {
	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			t.Error("Register should not be called")
			return nil, nil
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			t.Error("Verify should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("user@example.com\nn\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}

	output := stdout.String()
	if !strings.Contains(output, "Terms of Service") {
		t.Errorf("output missing ToS message: %q", output)
	}
}

func TestLoginInvalidEmail(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "invalid_email", Message: "Invalid email address."}
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			t.Error("Verify should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("bad\nY\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}

	output := stdout.String()
	if !strings.Contains(output, "Invalid email") {
		t.Errorf("output missing error message: %q", output)
	}
}

func TestLoginRetryOnWrongCode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	attempt := 0
	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(_, code, _ string) (*api.VerifyResponse, error) {
			attempt++
			if attempt == 1 {
				if code != "WRONG1" {
					t.Errorf("first code = %q, want WRONG1", code)
				}
				return nil, &api.ErrorResponse{Code: "invalid_code", Message: "Invalid or expired code."}
			}
			if code != "ABC123" {
				t.Errorf("second code = %q, want ABC123", code)
			}
			return &api.VerifyResponse{APIKey: "kk_live_ok"}, nil
		},
	}

	stdin := strings.NewReader("user@example.com\nY\nWRONG1\nABC123\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	// The prefix is part of the assertion: a re-prompt caught client-side is an
	// interactive error, and those carry it.
	if !strings.Contains(output, "Error: Incorrect code") {
		t.Errorf("output missing retry message: %q", output)
	}
	if !strings.Contains(output, "API key saved") {
		t.Errorf("output missing save message: %q", output)
	}
	if attempt != 2 {
		t.Errorf("verify called %d times, want 2", attempt)
	}
}

func TestLoginRateLimitedSkipsToCodePrompt(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "rate_limited", Message: "Please wait."}
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			return &api.VerifyResponse{APIKey: "kk_live_ok"}, nil
		},
	}

	stdin := strings.NewReader("user@example.com\nY\nABC123\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "already sent") {
		t.Errorf("output missing already-sent message: %q", output)
	}
	if !strings.Contains(output, "API key saved") {
		t.Errorf("output missing save message: %q", output)
	}
}

func TestLoginAuthRateLimitedAborts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "auth_rate_limited", Message: "Too many requests."}
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			t.Fatal("verify must not be reached after a register flood")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &out); err == nil {
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
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &outJA); err == nil {
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
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return nil, &api.ErrorResponse{Code: "invite_required", Message: "private beta"}
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			t.Fatal("verify must not be reached when the email is not invited")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &out); err == nil {
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
	if err := Run(client, strings.NewReader("user@example.com\nY\n"), &outJA); err == nil {
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
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			return nil, &api.ErrorResponse{Code: "auth_rate_limited", Message: "Too many requests."}
		},
	}

	var out bytes.Buffer
	if err := Run(client, strings.NewReader("user@example.com\nY\nABC123\n"), &out); err == nil {
		t.Fatal("Run() expected the verify flood error, got nil")
	}
	if !strings.Contains(out.String(), "Too many requests. Please wait a moment and try again.") {
		t.Errorf("verify-flood output missing auth-rate-limited copy: %q", out.String())
	}
}

func TestLoginTooManyAttemptsExits(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			return nil, &api.ErrorResponse{Code: "too_many_attempts", Message: "Too many attempts."}
		},
	}

	stdin := strings.NewReader("user@example.com\nY\nWRONG1\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}

	output := stdout.String()
	if !strings.Contains(output, "Too many failed attempts") {
		t.Errorf("output missing error message: %q", output)
	}
}

func TestLoginEmptyTosAccepted(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return &api.RegisterResponse{Message: "Confirmation code sent."}, nil
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			return &api.VerifyResponse{APIKey: "kk_live_abc"}, nil
		},
	}

	// An empty answer to the terms prompt means yes.
	stdin := strings.NewReader("user@example.com\n\nABC123\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestLoginSecretReusedOnRateLimit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	var capturedHash string
	client := &mockClient{
		registerFn: func(_ string, _ bool, secretHash string) (*api.RegisterResponse, error) {
			capturedHash = secretHash
			return nil, &api.ErrorResponse{Code: "rate_limited", Message: "Please wait."}
		},
		verifyFn: func(_, _, secret string) (*api.VerifyResponse, error) {
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

	err := Run(client, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestLoginUpgradeRequiredAborts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	client := &mockClient{
		registerFn: func(_ string, _ bool, _ string) (*api.RegisterResponse, error) {
			return nil, api.ErrUpgradeRequired
		},
		verifyFn: func(_, _, _ string) (*api.VerifyResponse, error) {
			t.Fatal("verify must not be reached after a refused version")
			return nil, nil
		},
	}

	var out bytes.Buffer
	err := Run(client, strings.NewReader("user@example.com\nY\n"), &out)
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
