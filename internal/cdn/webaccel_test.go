package cdn

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// freshSetClient returns a client for a site not yet on WebAccel, plus the slot
// capturing what the set call received.
func freshSetClient(t *testing.T) (*mockClient, *string, *string) {
	t.Helper()
	var capturedToken, capturedSecret string
	client := &mockClient{
		setCDNWebAccelFn: func(_, token, secret string) (*api.CDNSetResponse, error) {
			capturedToken = token
			capturedSecret = secret
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
	}
	return client, &capturedToken, &capturedSecret
}

func TestSetWebAccelNonInteractiveHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client, capturedToken, capturedSecret := freshSetClient(t)

	opts := WebAccelOptions{
		Token:  "wa-token",
		Secret: "wa-secret",
	}

	var out bytes.Buffer
	if err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}

	if *capturedToken != "wa-token" {
		t.Errorf("token = %q", *capturedToken)
	}
	if *capturedSecret != "wa-secret" {
		t.Errorf("secret = %q", *capturedSecret)
	}
	if !strings.Contains(out.String(), "CDN set to WebAccel") {
		t.Errorf("output = %q", out.String())
	}
}

func TestSetWebAccelEnvVarFallback(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "env-token")
	t.Setenv(EnvWebAccelSecret, "env-secret")

	client, capturedToken, capturedSecret := freshSetClient(t)

	var out bytes.Buffer
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
	if *capturedToken != "env-token" {
		t.Errorf("token from env = %q", *capturedToken)
	}
	if *capturedSecret != "env-secret" {
		t.Errorf("secret from env = %q", *capturedSecret)
	}
}

func TestSetWebAccelFlagsBeatEnvVars(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "env-token")
	t.Setenv(EnvWebAccelSecret, "env-secret")

	client, capturedToken, capturedSecret := freshSetClient(t)

	opts := WebAccelOptions{Token: "flag-token", Secret: "flag-secret"}
	var out bytes.Buffer
	if err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
	if *capturedToken != "flag-token" {
		t.Errorf("flags should beat env: token = %q", *capturedToken)
	}
	if *capturedSecret != "flag-secret" {
		t.Errorf("flags should beat env: secret = %q", *capturedSecret)
	}
}

func TestSetWebAccelMissingTokenNonInteractive(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	client := &mockClient{
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			t.Error("API should not be called when token is missing")
			return nil, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
	}

	opts := WebAccelOptions{Secret: "wa-secret"}

	var out bytes.Buffer
	err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSetWebAccelEagerValidationErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	cases := []struct {
		code    string
		wantMsg string
	}{
		{"invalid_webaccel_credentials", "apikeys"},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			client := &mockClient{
				setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
					return nil, &api.ErrorResponse{Code: tc.code, Message: "server says no"}
				},
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
					return &api.CDNStatusResponse{CDNMode: "none"}, nil
				},
			}
			opts := WebAccelOptions{Token: "t", Secret: "s"}
			var out bytes.Buffer
			err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("%s: error = %q, want %q in message", tc.code, err.Error(), tc.wantMsg)
			}
		})
	}
}

// Empty credentials on the wire are the signal to reuse the stored ones, so
// this path must send them empty rather than prompting.
func TestSetWebAccelReuseStoredSkipsPrompt(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	promptCalled := false
	var capturedToken, capturedSecret string

	reader := func(prompt string, _ *bufio.Reader, _ io.Reader, _ io.Writer, _ bool) (string, error) {
		promptCalled = true
		return "should-not-be-called", nil
	}

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "webaccel"}, nil
		},
		setCDNWebAccelFn: func(_, token, secret string) (*api.CDNSetResponse, error) {
			capturedToken = token
			capturedSecret = secret
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	var out bytes.Buffer
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, nil, &out, reader); err != nil {
		t.Fatal(err)
	}

	if promptCalled {
		t.Error("prompt must not be called when reusing stored creds")
	}
	if capturedToken != "" {
		t.Errorf("token must be empty (reuse signal), got %q", capturedToken)
	}
	if capturedSecret != "" {
		t.Errorf("secret must be empty (reuse signal), got %q", capturedSecret)
	}
	if !strings.Contains(out.String(), "already on file") {
		t.Errorf("expected 'already on file' message: %q", out.String())
	}
}

// A token the provider is actively rejecting must not be reused: resuming with
// it would loop forever, so this path asks for a fresh one.
func TestSetWebAccelResumeRePromptsOnFailingCreds(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	promptCalls := 0
	reader := func(prompt string, _ *bufio.Reader, _ io.Reader, w io.Writer, _ bool) (string, error) {
		promptCalls++
		// Echo the prompt, as the real reader does, so ordering is observable.
		fmt.Fprint(w, prompt)
		if strings.Contains(prompt, "Secret") {
			return "fresh-secret", nil
		}
		return "fresh-token", nil
	}

	var capturedToken, capturedSecret string
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "webaccel", CdnCredentialsHealth: "failing"}, nil
		},
		setCDNWebAccelFn: func(_, token, secret string) (*api.CDNSetResponse, error) {
			capturedToken = token
			capturedSecret = secret
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	var out bytes.Buffer
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, strings.NewReader(""), &out, reader); err != nil {
		t.Fatal(err)
	}
	if promptCalls == 0 {
		t.Error("failing creds must re-prompt, not reuse the rejected token")
	}
	if capturedToken != "fresh-token" || capturedSecret != "fresh-secret" {
		t.Errorf("expected fresh creds sent, got token=%q secret=%q", capturedToken, capturedSecret)
	}
	if strings.Contains(out.String(), "already on file") {
		t.Errorf("must not claim reuse when creds are failing: %q", out.String())
	}
	// The re-enter line promises a prompt, so it must be followed by one.
	got := out.String()
	reenter := strings.Index(got, i18n.T("cdn.token_rejected_reenter"))
	prompt := strings.Index(got, i18n.T("cdn.prompt_access_token"))
	if reenter < 0 || prompt < reenter {
		t.Errorf("want the re-enter line followed by the token prompt:\n%s", got)
	}
	if strings.Contains(got, i18n.T("cdn.token_rejected_on_file")) {
		t.Errorf("the no-prompt rejection line must not appear when a prompt follows:\n%s", got)
	}
}

// Without a reader nothing will be asked, so the rejected-token line must not
// ask for a new token: it states the rejection and the missing-value error
// names the variable and the flag that supply one.
func TestSetWebAccelFailingCredsWithoutReaderReportsMissingValue(t *testing.T) {
	cases := []struct {
		name    string
		opts    WebAccelOptions
		wantErr string
	}{
		{"nothing supplied", WebAccelOptions{}, i18n.Tf("cdn.err_token_missing", EnvWebAccelToken)},
		{"token supplied", WebAccelOptions{Token: "tok"}, i18n.Tf("cdn.err_secret_missing", EnvWebAccelSecret)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProjectConfig(t, "site123")
			t.Setenv(EnvWebAccelToken, "")
			t.Setenv(EnvWebAccelSecret, "")

			client := &mockClient{
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
					return &api.CDNStatusResponse{CDNMode: "webaccel", CdnCredentialsHealth: "failing"}, nil
				},
				setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
					t.Error("API must not be called without credentials")
					return nil, nil
				},
			}

			var out bytes.Buffer
			err := setWebAccelWithReader(client, c.opts, true, nil, &out, stubReader)
			if err == nil || err.Error() != c.wantErr {
				t.Errorf("err = %v, want %q", err, c.wantErr)
			}
			got := out.String()
			if !strings.Contains(got, i18n.T("cdn.token_rejected_on_file")+"\n") {
				t.Errorf("missing the rejection line %q in:\n%s", i18n.T("cdn.token_rejected_on_file"), got)
			}
			if strings.Contains(got, i18n.T("cdn.token_rejected_reenter")) {
				t.Errorf("the re-enter line promises a prompt that never comes:\n%s", got)
			}
		})
	}
}

// With a token already supplied only the secret is prompted, so the rejection
// line must not ask for a new token: it states the rejection, and the secret
// prompt that follows explains itself.
func TestSetWebAccelFailingCredsWithTokenSuppliedPromptsOnlyForSecret(t *testing.T) {
	cases := []struct {
		name      string
		opts      WebAccelOptions
		envToken  string
		wantToken string
	}{
		{"token flag", WebAccelOptions{Token: "flag-token"}, "", "flag-token"},
		{"token variable", WebAccelOptions{}, "env-token", "env-token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProjectConfig(t, "site123")
			t.Setenv(EnvWebAccelToken, c.envToken)
			t.Setenv(EnvWebAccelSecret, "")

			var prompts []string
			reader := func(prompt string, _ *bufio.Reader, _ io.Reader, w io.Writer, _ bool) (string, error) {
				prompts = append(prompts, prompt)
				// Echo the prompt, as the real reader does, so ordering is observable.
				fmt.Fprint(w, prompt)
				return "fresh-secret", nil
			}

			var capturedToken, capturedSecret string
			client := &mockClient{
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
					return &api.CDNStatusResponse{CDNMode: "webaccel", CdnCredentialsHealth: "failing"}, nil
				},
				setCDNWebAccelFn: func(_, token, secret string) (*api.CDNSetResponse, error) {
					capturedToken, capturedSecret = token, secret
					return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
				},
			}

			var out bytes.Buffer
			if err := setWebAccelWithReader(client, c.opts, true, strings.NewReader(""), &out, reader); err != nil {
				t.Fatal(err)
			}
			if len(prompts) != 1 || prompts[0] != i18n.T("cdn.prompt_access_token_secret") {
				t.Errorf("prompts = %q, want only %q", prompts, i18n.T("cdn.prompt_access_token_secret"))
			}
			if capturedToken != c.wantToken || capturedSecret != "fresh-secret" {
				t.Errorf("sent token/secret = %q/%q, want %s/fresh-secret", capturedToken, capturedSecret, c.wantToken)
			}
			got := out.String()
			onFile := strings.Index(got, i18n.T("cdn.token_rejected_on_file")+"\n")
			secretPrompt := strings.Index(got, i18n.T("cdn.prompt_access_token_secret"))
			if onFile < 0 || secretPrompt < onFile {
				t.Errorf("want the rejection line followed by the secret prompt:\n%s", got)
			}
			if strings.Contains(got, i18n.T("cdn.token_rejected_reenter")) {
				t.Errorf("the re-enter line asks for a token that is never prompted:\n%s", got)
			}
		})
	}
}

// A missing value names the environment variable before the flag, in both
// languages and from both commands. The literal copy is pinned, since
// comparing against the catalog would pass whatever order it held.
func TestMissingValueErrorsNameTheVariableFirst(t *testing.T) {
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")
	cases := []struct {
		lang       string
		wantToken  string
		wantSecret string
	}{
		{"en", "missing WebAccel token (set KAMAKIRI_CDN_TOKEN or --token)",
			"missing WebAccel secret (set KAMAKIRI_CDN_SECRET or --secret)"},
		{"ja", "WebAccel のトークンがありません（KAMAKIRI_CDN_TOKEN または --token を指定してください）",
			"WebAccel のシークレットがありません（KAMAKIRI_CDN_SECRET または --secret を指定してください）"},
	}
	for _, c := range cases {
		t.Run(c.lang, func(t *testing.T) {
			i18n.Load(c.lang)
			t.Cleanup(func() { i18n.Load(testLang) })

			var out bytes.Buffer
			if _, _, err := resolveCredentialsOnly(WebAccelOptions{}, nil, &out, stubReader); err == nil || err.Error() != c.wantToken {
				t.Errorf("missing token: err = %v, want %q", err, c.wantToken)
			}
			if _, _, err := resolveCredentialsOnly(WebAccelOptions{Token: "t"}, nil, &out, stubReader); err == nil || err.Error() != c.wantSecret {
				t.Errorf("missing secret: err = %v, want %q", err, c.wantSecret)
			}

			client := &mockClient{
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
					return &api.CDNStatusResponse{CDNMode: "none"}, nil
				},
			}
			if _, _, _, err := resolveWebAccelCredentials(client, "site123", WebAccelOptions{}, nil, &out, stubReader); err == nil || err.Error() != c.wantToken {
				t.Errorf("cdn webaccel missing token: err = %v, want %q", err, c.wantToken)
			}
			if _, _, _, err := resolveWebAccelCredentials(client, "site123", WebAccelOptions{Token: "t"}, nil, &out, stubReader); err == nil || err.Error() != c.wantSecret {
				t.Errorf("cdn webaccel missing secret: err = %v, want %q", err, c.wantSecret)
			}
		})
	}
}

// stubTerminalReader makes the entry points' terminal gate answer isTTY for
// the duration of the test.
func stubTerminalReader(t *testing.T, isTTY bool) {
	t.Helper()
	orig := isTerminalReader
	isTerminalReader = func(io.Reader) bool { return isTTY }
	t.Cleanup(func() { isTerminalReader = orig })
}

// The entry points prompt only on a terminal. A pipe carrying answers is not
// read at all: the missing-value error is the whole outcome, with no explainer
// and no prompt printed ahead of it.
func TestSetWebAccelPromptsOnlyOnATerminal(t *testing.T) {
	setup := func(t *testing.T) (*mockClient, *string, *string) {
		t.Helper()
		t.Chdir(t.TempDir())
		setupCredentials(t)
		setupProjectConfig(t, "site123")
		t.Setenv(EnvWebAccelToken, "")
		t.Setenv(EnvWebAccelSecret, "")
		return freshSetClient(t)
	}

	t.Run("not a terminal", func(t *testing.T) {
		stubTerminalReader(t, false)
		client, capturedToken, _ := setup(t)
		in := strings.NewReader("tok\nsec\n")
		var out bytes.Buffer
		err := SetWebAccel(client, WebAccelOptions{}, true, in, &out)
		if want := i18n.Tf("cdn.err_token_missing", EnvWebAccelToken); err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
		if in.Len() != len("tok\nsec\n") {
			t.Errorf("%d bytes of the piped answers were read", len("tok\nsec\n")-in.Len())
		}
		if *capturedToken != "" {
			t.Errorf("a piped token reached the server: %q", *capturedToken)
		}
		got := out.String()
		for _, bad := range []string{i18n.T("cdn.prompt_access_token"), i18n.T("cdn.preflight")} {
			if strings.Contains(got, bad) {
				t.Errorf("found %q in output with no terminal to answer it:\n%s", bad, got)
			}
		}
	})

	t.Run("terminal", func(t *testing.T) {
		stubTerminalReader(t, true)
		client, capturedToken, capturedSecret := setup(t)
		var out bytes.Buffer
		if err := SetWebAccel(client, WebAccelOptions{}, true, strings.NewReader("tok\nsec\n"), &out); err != nil {
			t.Fatal(err)
		}
		if *capturedToken != "tok" || *capturedSecret != "sec" {
			t.Errorf("sent token/secret = %q/%q, want the prompted tok/sec", *capturedToken, *capturedSecret)
		}
		if !strings.Contains(out.String(), i18n.T("cdn.prompt_access_token")) {
			t.Errorf("missing the token prompt:\n%s", out.String())
		}
	})
}

// A Ctrl-C at a prompt is not the input ending: it keeps its own sentinel so
// the command exits 130, and it is not framed as a read failure.
func TestPromptInterruptPassesThrough(t *testing.T) {
	interrupted := func(string, *bufio.Reader, io.Reader, io.Writer, bool) (string, error) {
		return "", ErrInterrupted
	}
	var out bytes.Buffer
	_, err := promptField("p: ", "retry", nil, nil, &out, interrupted, true)
	if !errors.Is(err, ErrInterrupted) {
		t.Errorf("promptField error = %v, want ErrInterrupted", err)
	}
	if errors.Is(err, ErrCancelled) {
		t.Errorf("promptField turned an interrupt into ErrCancelled: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), i18n.T("cdn.err_read_input")) {
		t.Errorf("promptField framed an interrupt as a read failure: %v", err)
	}
}

// --yes is not a flag of this command, so it is refused like any unknown flag.
func TestParseWebAccelArgsRejectsYes(t *testing.T) {
	for _, arg := range []string{"--yes", "--yes=true"} {
		_, err := ParseWebAccelArgs([]string{"--token", "t", arg})
		if want := i18n.Tf("cdn.err_unknown_flag_webaccel", "--yes"); err == nil || err.Error() != want {
			t.Errorf("ParseWebAccelArgs(%s) error = %v, want %q", arg, err, want)
		}
	}
}

func TestSetWebAccelReuseStoredSkipsConsent(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "webaccel"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	var out bytes.Buffer
	// A nil reader cannot answer a prompt, so any prompt here would error.
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
}

// Running the command is the consent, so a run with credentials supplied has
// nothing left to confirm.
func TestSetWebAccelFreshPathNonInteractiveWithCreds(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var apiCalled bool
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			apiCalled = true
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	opts := WebAccelOptions{Token: "t", Secret: "s"}
	var out bytes.Buffer
	if err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
	if !apiCalled {
		t.Error("API should be called: creds supplied, no gate")
	}
	if strings.Contains(out.String(), "Mint a token") {
		t.Errorf("pre-flight text should not appear when creds are supplied: %q", out.String())
	}
}

func TestSetWebAccelFreshPathNonInteractiveMissingCreds(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			t.Error("API should not be called without creds")
			return nil, nil
		},
	}

	var out bytes.Buffer
	err := setWebAccelWithReader(client, WebAccelOptions{}, true, nil, &out, stubReader)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want missing-token message", err.Error())
	}
}

// Explain first, prompt last: the user learns what the token is for before
// being asked for one. The explainer also has to own the ownership-record step
// rather than promise no registrar work.
func TestSetWebAccelPreflightExplainerBeforePrompt(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	var apiCalled bool
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			apiCalled = true
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	reader := func(prompt string, _ *bufio.Reader, _ io.Reader, w io.Writer, masked bool) (string, error) {
		// Echo the prompt, as the real reader does, so ordering is observable.
		fmt.Fprint(w, prompt)
		if masked {
			return "secret", nil
		}
		return "token", nil
	}

	in := strings.NewReader("")
	var out bytes.Buffer
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, in, &out, reader); err != nil {
		t.Fatal(err)
	}
	if !apiCalled {
		t.Error("API should be called after prompting")
	}
	got := out.String()
	for _, want := range []string{
		"Managed WebAccel will, using the token you provide:",
		"add one DNS TXT record per domain to prove ownership",
		"Mint a token at https://secure.sakura.ad.jp/cloud/#/apikeys",
		"Kamakiri never deletes resources unless explicitly asked to with the kamakiri cdn cleanup command.",
		"Access Token:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "Managed WebAccel will") > strings.Index(got, "Access Token:") {
		t.Errorf("explainer must precede the prompt:\n%s", got)
	}
	for _, bad := range []string{
		"Continue?",
		"no registrar change needed",
		"short\ncutover",
		"short cutover",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("found removed copy %q in:\n%s", bad, got)
		}
	}
	assertCleanCopy(t, got)
}

func TestSetWebAccelInteractiveTokenPrompt(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	var capturedToken, capturedSecret string
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, token, secret string) (*api.CDNSetResponse, error) {
			capturedToken = token
			capturedSecret = secret
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	callCount := 0
	reader := func(prompt string, _ *bufio.Reader, _ io.Reader, _ io.Writer, masked bool) (string, error) {
		callCount++
		switch callCount {
		case 1:
			if masked {
				t.Errorf("Access Token prompt must NOT be masked")
			}
			if !strings.Contains(prompt, "Access Token") {
				t.Errorf("prompt 1 = %q, want Access Token label", prompt)
			}
			return "interactive-token", nil
		case 2:
			if !masked {
				t.Errorf("Access Token Secret prompt must be masked")
			}
			if !strings.Contains(prompt, "Access Token Secret") {
				t.Errorf("prompt 2 = %q, want Access Token Secret label", prompt)
			}
			return "interactive-secret", nil
		default:
			t.Errorf("unexpected prompt call #%d: %q", callCount, prompt)
			return "", nil
		}
	}

	in := strings.NewReader("y\n")
	var out bytes.Buffer
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, in, &out, reader); err != nil {
		t.Fatal(err)
	}
	if capturedToken != "interactive-token" {
		t.Errorf("token = %q", capturedToken)
	}
	if capturedSecret != "interactive-secret" {
		t.Errorf("secret = %q", capturedSecret)
	}
}

func TestSetWebAccelInteractiveRePromptsOnEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	var capturedToken, capturedSecret string
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, token, secret string) (*api.CDNSetResponse, error) {
			capturedToken = token
			capturedSecret = secret
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}

	callCount := 0
	var out bytes.Buffer
	reader := func(prompt string, _ *bufio.Reader, _ io.Reader, w io.Writer, _ bool) (string, error) {
		callCount++
		switch callCount {
		case 1:
			fmt.Fprintln(w, "Token cannot be empty. Try again.")
			return "", nil // empty → re-prompt
		case 2:
			return "actual-token", nil
		case 3:
			fmt.Fprintln(w, "Secret cannot be empty. Try again.")
			return "", nil // empty → re-prompt
		case 4:
			return "actual-secret", nil
		default:
			t.Errorf("unexpected call #%d", callCount)
			return "", nil
		}
	}

	in := strings.NewReader("y\n")
	if err := setWebAccelWithReader(client, WebAccelOptions{}, true, in, &out, reader); err != nil {
		t.Fatal(err)
	}
	if capturedToken != "actual-token" {
		t.Errorf("token = %q, want actual-token", capturedToken)
	}
	if capturedSecret != "actual-secret" {
		t.Errorf("secret = %q, want actual-secret", capturedSecret)
	}
	if !strings.Contains(out.String(), "Token cannot be empty") {
		t.Errorf("expected token re-prompt message: %q", out.String())
	}
	if !strings.Contains(out.String(), "Secret cannot be empty") {
		t.Errorf("expected secret re-prompt message: %q", out.String())
	}
}

func TestSetWebAccelInteractiveEOFAtTokenPromptReturnsCancelled(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvWebAccelToken, "")
	t.Setenv(EnvWebAccelSecret, "")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			t.Error("API should not be called")
			return nil, nil
		},
	}

	eofReader := func(_ string, _ *bufio.Reader, _ io.Reader, _ io.Writer, _ bool) (string, error) {
		return "", io.EOF
	}

	in := strings.NewReader("y\n")
	var out bytes.Buffer
	err := setWebAccelWithReader(client, WebAccelOptions{}, true, in, &out, eofReader)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("expected ErrCancelled, got %v", err)
	}
	if strings.Contains(err.Error(), "EOF") {
		t.Errorf("error message should not surface raw EOF: %q", err.Error())
	}
}

// ErrCancelled looks its message up when it is printed, so the sentinel keeps
// one identity while the text follows the language actually in force. A value
// built at package initialization could not: that runs before the language is
// resolved.
func TestErrCancelledRendersFromTheCatalog(t *testing.T) {
	if got := ErrCancelled.Error(); got != i18n.T("cdn.err_cancelled") {
		t.Errorf("ErrCancelled.Error() = %q, want %q", got, i18n.T("cdn.err_cancelled"))
	}

	i18n.Load("ja")
	japanese := ErrCancelled.Error()
	i18n.Load(testLang)

	if japanese == ErrCancelled.Error() {
		t.Errorf("ErrCancelled reads the same in both languages: %q", japanese)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrCancelled), ErrCancelled) {
		t.Error("ErrCancelled lost its identity through a wrap")
	}
}

func TestParseWebAccelArgsHappyPath(t *testing.T) {
	opts, err := ParseWebAccelArgs([]string{
		"--token", "wa-token",
		"--secret", "wa-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "wa-token" || opts.Secret != "wa-secret" {
		t.Errorf("token/secret = %q/%q", opts.Token, opts.Secret)
	}
}

func TestParseWebAccelArgsEqualsForm(t *testing.T) {
	opts, err := ParseWebAccelArgs([]string{
		"--token=wa-token",
		"--secret=wa-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "wa-token" || opts.Secret != "wa-secret" {
		t.Errorf("token/secret = %q/%q", opts.Token, opts.Secret)
	}
}

func TestParseWebAccelArgsMixedForms(t *testing.T) {
	opts, err := ParseWebAccelArgs([]string{
		"--token", "wa-token",
		"--secret=wa-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "wa-token" || opts.Secret != "wa-secret" {
		t.Errorf("token/secret = %q/%q", opts.Token, opts.Secret)
	}
}

func TestParseWebAccelArgsEqualsFormEmptyValueRejected(t *testing.T) {
	_, err := ParseWebAccelArgs([]string{"--token="})
	if err == nil {
		t.Fatal("expected error for empty equals-form value")
	}
	if !strings.Contains(err.Error(), "requires a value") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestParseWebAccelArgsMissingFlagValue(t *testing.T) {
	cases := [][]string{
		{"--token"},
		{"--secret"},
	}
	for _, args := range cases {
		_, err := ParseWebAccelArgs(args)
		if err == nil {
			t.Errorf("ParseWebAccelArgs(%v) expected error", args)
		}
	}
}

func TestParseWebAccelArgsDomainIDRejected(t *testing.T) {
	_, err := ParseWebAccelArgs([]string{"--domain-id", "example.com=1001"})
	if err == nil {
		t.Fatal("expected error for --domain-id")
	}
	if !strings.Contains(err.Error(), "managed mode") {
		t.Errorf("error = %q, want mention of managed mode", err.Error())
	}
}

func TestParseWebAccelArgsUnknownFlag(t *testing.T) {
	_, err := ParseWebAccelArgs([]string{"--bogus", "x"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestParseWebAccelArgsUnknownFlagEchoesOnlyTheName(t *testing.T) {
	_, err := ParseWebAccelArgs([]string{"--secert=TOPSECRET"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "--secert") || strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("error = %q, want the flag name and not its value", err)
	}
}

func TestDefaultSecretReaderMaskedOffTerminalReadsLine(t *testing.T) {
	in := strings.NewReader("my-secret\n")
	reader := bufio.NewReader(in)
	var out bytes.Buffer
	val, err := DefaultSecretReader("Prompt: ", reader, in, &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if val != "my-secret" {
		t.Errorf("val = %q, want my-secret", val)
	}
}

func TestDefaultSecretReaderUnmaskedOffTerminalReadsLine(t *testing.T) {
	in := strings.NewReader("my-token\n")
	reader := bufio.NewReader(in)
	var out bytes.Buffer
	val, err := DefaultSecretReader("Prompt: ", reader, in, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if val != "my-token" {
		t.Errorf("val = %q, want my-token", val)
	}
}

func TestSetWebAccelNoWaitFalseBlocksUntilActive(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	// This runs the production watch, whose gate demands a WebAccel edge
	// signature no ordinary host carries, so the gate would never pass here and
	// would loop forever. This test pins the wiring, not the gate.
	skipCertGate(t)

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			// The resolver never reads status here, since the token and secret were
			// supplied; the reads are the switch check, the apex warning, and the
			// watch.
			return &api.CDNStatusResponse{
				CDNMode:  "webaccel",
				Provider: "webaccel",
				Domains:  []api.CDNDomainStatus{{Domain: "example.com", CdnState: CdnStateActive}},
			}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 77}, nil
		},
	}

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret"}

	var out bytes.Buffer
	if err := setWebAccelWithReader(client, opts, false, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "CDN set to WebAccel") {
		t.Errorf("missing set milestone: %q", got)
	}
	if !strings.Contains(got, "live via WebAccel") {
		t.Errorf("must block until live via WebAccel: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestSetWebAccelWithApexPending(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{
				CDNMode:       "webaccel",
				SyncAttemptID: 1,
				ApexARecordsPending: []api.ApexARecordsPending{
					{Domain: "example.com", Reason: "webaccel_pending"},
				},
			}, nil
		},
	}

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret"}
	var out bytes.Buffer
	if err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "CDN set to WebAccel") {
		t.Errorf("missing set milestone: %q", got)
	}
	if !strings.Contains(got, "Re-run `kamakiri status` shortly") {
		t.Errorf("missing pending nudge: %q", got)
	}
	if !strings.Contains(got, "    example.com") {
		t.Errorf("missing domain in pending list: %q", got)
	}
	assertCleanCopy(t, got)
}

// The warning has to land before the watch, so a user whose registrar cannot
// ALIAS an apex learns it before investing in a flow they cannot finish.
func TestSetWebAccelApexEligibilityWarning(t *testing.T) {
	run := func(t *testing.T, domain string) string {
		t.Helper()
		t.Chdir(t.TempDir())
		setupCredentials(t)
		setupProjectConfig(t, "site123")
		client := &mockClient{
			cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
				return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
					Domains: []api.CDNDomainStatus{{Domain: domain, CdnState: CdnStateAwaitingProvisioning}}}, nil
			},
			setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
				return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
			},
		}
		opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret"}
		var out bytes.Buffer
		if err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	t.Run("apex warns", func(t *testing.T) {
		got := run(t, "example.com")
		for _, want := range []string{"apex domain", "ALIAS/ANAME", "cdn cloudflare"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing apex eligibility note %q in:\n%s", want, got)
			}
		}
		assertCleanCopy(t, got)
	})

	t.Run("subdomain does not warn", func(t *testing.T) {
		got := run(t, "next.example.com")
		if strings.Contains(got, "apex domain") {
			t.Errorf("subdomain must not trigger the apex warning:\n%s", got)
		}
	})
}

func TestSetWebAccelWithApexIPs(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{
				CDNMode:       "webaccel",
				SyncAttemptID: 1,
				ApexARecordsToUpdate: []api.ApexARecordsToUpdate{
					{Domain: "example.com", ExpectedARecords: []string{"198.51.100.10", "198.51.100.11"}},
				},
			}, nil
		},
	}

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret"}
	var out bytes.Buffer
	if err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "example.com  A  → 198.51.100.10") {
		t.Errorf("missing WebAccel edge IP line: %q", got)
	}
	if !strings.Contains(got, "example.com  A  → 198.51.100.11") {
		t.Errorf("missing WebAccel edge IP line: %q", got)
	}
	assertCleanCopy(t, got)
}

func stubReader(_ string, _ *bufio.Reader, _ io.Reader, _ io.Writer, _ bool) (string, error) {
	return "", io.EOF
}

// A prompt whose reader fails for a reason other than end of input frames that
// failure rather than reporting it as a cancellation.
func TestPromptReadFailureIsFramed(t *testing.T) {
	broken := func(string, *bufio.Reader, io.Reader, io.Writer, bool) (string, error) {
		return "", errors.New("terminal exploded")
	}
	var out bytes.Buffer
	_, err := promptField("p: ", "retry", nil, nil, &out, broken, false)
	if err == nil || !strings.HasPrefix(err.Error(), i18n.T("cdn.err_read_input")+": ") ||
		errors.Unwrap(err) == nil || errors.Unwrap(err).Error() != "terminal exploded" {
		t.Errorf("promptField read error = %v, want the keyed frame over the reader's error", err)
	}
}

func TestReadRawLine(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		masked  bool
		want    string
		wantErr error
		wantOut string
	}{
		{"carriage return ends the line", "abc\r", false, "abc", nil, "abc"},
		{"newline ends the line", "abc\n", false, "abc", nil, "abc"},
		{"ctrl-c interrupts", "ab\x03", false, "", ErrInterrupted, "ab"},
		{"ctrl-c interrupts a masked read", "ab\x03", true, "", ErrInterrupted, "**"},
		{"ctrl-d cancels", "ab\x04", false, "", ErrCancelled, "ab"},
		{"ctrl-d cancels a masked read", "ab\x04", true, "", ErrCancelled, "**"},
		{"end of input cancels", "ab", false, "", ErrCancelled, "ab"},
		{"end of input before any byte cancels", "", true, "", ErrCancelled, ""},
		{"DEL erases one byte", "abc\x7fd\r", false, "abd", nil, "abc\b \bd"},
		{"backspace erases one byte", "abc\x08d\r", false, "abd", nil, "abc\b \bd"},
		{"backspace on an empty buffer does nothing", "\x7f\x08a\r", false, "a", nil, "a"},
		{"masked echoes a star per byte", "sec\r", true, "sec", nil, "***"},
		{"masked erase prints the same erase", "se\x7fc\r", true, "sc", nil, "**\b \b*"},
		{"unmasked echoes printable bytes", "a b~\r", false, "a b~", nil, "a b~"},
		{"unmasked drops control bytes from value and echo", "a\x01\x1bb\r", false, "ab", nil, "ab"},
		{"masked stars a control byte too", "\x01\r", true, "\x01", nil, "*"},
		// A pasted "token\r\n" ends the token prompt at the \r and leaves the \n
		// for the next prompt, which must not take it as an empty answer.
		{"a newline before any byte is skipped", "\nabc\r", false, "abc", nil, "abc"},
		{"a masked read skips a leading newline too", "\nsec\r", true, "sec", nil, "***"},
		{"a lone newline then end of input cancels", "\n", false, "", ErrCancelled, ""},
		{"a carriage return alone still ends an empty line", "\r", false, "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := readRawLine(strings.NewReader(tc.in), &out, tc.masked)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
			if out.String() != tc.wantOut {
				t.Errorf("echo = %q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}

// errReader yields its data, then fails with err.
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestReadRawLineNonEOFReadErrorCancels(t *testing.T) {
	var out bytes.Buffer
	got, err := readRawLine(&errReader{data: []byte("ab"), err: errors.New("device gone")}, &out, false)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}

// zeroReader returns (0, nil) up to limit times, then fails the test.
type zeroReader struct {
	t     *testing.T
	calls int
	limit int
}

func (r *zeroReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls > r.limit {
		r.t.Fatalf("readRawLine kept reading after %d zero-byte reads", r.limit)
	}
	return 0, nil
}

func TestReadRawLineZeroByteReadCancels(t *testing.T) {
	var out bytes.Buffer
	got, err := readRawLine(&zeroReader{t: t, limit: 3}, &out, false)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}

// lastByteWithErr delivers its final byte together with the error.
type lastByteWithErr struct {
	data []byte
	err  error
}

func (r *lastByteWithErr) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	if len(r.data) == 0 {
		return 1, r.err
	}
	return 1, nil
}

func TestReadRawLineProcessesByteDeliveredWithError(t *testing.T) {
	var out bytes.Buffer
	got, err := readRawLine(&lastByteWithErr{data: []byte("ab\r"), err: io.EOF}, &out, false)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if got != "ab" {
		t.Errorf("value = %q, want ab", got)
	}
}

func TestRawEchoWriterNonTerminal(t *testing.T) {
	var out bytes.Buffer
	if w := rawEchoWriter(&out, false); w != io.Discard {
		t.Errorf("unmasked echo to a non-terminal goes to %T, want io.Discard", w)
	}
	if w := rawEchoWriter(&out, true); w != &out {
		t.Errorf("masked echo goes to %T, want out", w)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	if w := rawEchoWriter(pw, false); w != io.Discard {
		t.Errorf("unmasked echo to a pipe goes to %T, want io.Discard", w)
	}
}

func TestPromptFieldTrimsAnswers(t *testing.T) {
	answers := []string{"  ", "  tok  "}
	stub := func(string, *bufio.Reader, io.Reader, io.Writer, bool) (string, error) {
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	var out bytes.Buffer
	got, err := promptField("p: ", "retry-line", nil, nil, &out, stub, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok" {
		t.Errorf("value = %q, want tok", got)
	}
	if n := strings.Count(out.String(), "retry-line"); n != 1 {
		t.Errorf("retry line printed %d times, want 1", n)
	}
}

// byteWithErrReader delivers a byte together with the same error on every
// call, up to limit calls, then fails the test. Only the check on the error
// that follows the byte can end the read, since no call ever returns zero
// bytes.
type byteWithErrReader struct {
	t     *testing.T
	err   error
	calls int
	limit int
}

func (r *byteWithErrReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls > r.limit {
		r.t.Fatalf("readRawLine kept reading after %d reads that carried an error", r.limit)
	}
	p[0] = 'a'
	return 1, r.err
}

func TestReadRawLineNonEOFErrorWithByteCancels(t *testing.T) {
	var out bytes.Buffer
	got, err := readRawLine(&byteWithErrReader{t: t, err: errors.New("device gone"), limit: 2}, &out, false)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
}
