package cdn

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
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
		Yes:    true,
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
	if err := setWebAccelWithReader(client, WebAccelOptions{Yes: true}, true, nil, &out, stubReader); err != nil {
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

	opts := WebAccelOptions{Token: "flag-token", Secret: "flag-secret", Yes: true}
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

	opts := WebAccelOptions{Secret: "wa-secret", Yes: true}

	var out bytes.Buffer
	err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSetWebAccelMapsServerErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return nil, &api.ErrorResponse{Code: "invalid_credentials", Message: "WebAccel API rejected the provided credentials."}
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
	}

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret", Yes: true}

	var out bytes.Buffer
	err := setWebAccelWithReader(client, opts, true, nil, &out, stubReader)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "credentials are invalid") {
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
			opts := WebAccelOptions{Token: "t", Secret: "s", Yes: true}
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
	reader := func(prompt string, _ *bufio.Reader, _ io.Reader, _ io.Writer, _ bool) (string, error) {
		promptCalls++
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

	opts := WebAccelOptions{Token: "t", Secret: "s"} // no --yes needed
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
		"--yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "wa-token" || opts.Secret != "wa-secret" {
		t.Errorf("token/secret = %q/%q", opts.Token, opts.Secret)
	}
	if !opts.Yes {
		t.Errorf("Yes should be true")
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

func TestReadMaskedNonTTY(t *testing.T) {
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

func TestReadUnmaskedNonTTY(t *testing.T) {
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
	t.Setenv(EnvSkipCertCheck, "1")

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

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret", Yes: true}

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

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret", Yes: true}
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
		opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret", Yes: true}
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

	opts := WebAccelOptions{Token: "wa-token", Secret: "wa-secret", Yes: true}
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
