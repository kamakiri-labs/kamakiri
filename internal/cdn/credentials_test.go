package cdn

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

func TestRotateCredentialsSuccess(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var gotTok, gotSec string
	client := &mockClient{
		rotateCredsFn: func(_, tok, sec string) (*api.CDNSetResponse, error) {
			gotTok, gotSec = tok, sec
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 7}, nil
		},
		waitForSyncFn: func(_ string, since int64, _ time.Duration) (*api.Site, error) {
			if since != 7 {
				t.Errorf("since = %d, want 7", since)
			}
			return &api.Site{ID: "site123"}, nil
		},
	}

	var out bytes.Buffer
	opts := WebAccelOptions{Token: "newtok", Secret: "newsec"}
	if err := RotateCredentials(client, opts, false, nil, &out); err != nil {
		t.Fatal(err)
	}
	if gotTok != "newtok" || gotSec != "newsec" {
		t.Errorf("sent token/secret = %q/%q", gotTok, gotSec)
	}
	if !strings.Contains(out.String(), "done.") {
		t.Errorf("missing done: %q", out.String())
	}
	assertCleanCopy(t, out.String())
}

func TestRotateCredentialsBadCreds(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		rotateCredsFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 8}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			return nil, errors.New("sync_attempt errored: cdn_provider http_401")
		},
	}

	var out bytes.Buffer
	opts := WebAccelOptions{Token: "bad", Secret: "bad"}
	err := RotateCredentials(client, opts, false, nil, &out)
	if err == nil {
		t.Fatal("expected error for rejected credentials")
	}
	// A timeout means the change is still applying and a rejected token means
	// it never will, so the two must not collapse into one classification.
	if errors.Is(err, api.ErrSyncTimeout) {
		t.Errorf("bad-creds error must not be ErrSyncTimeout: %v", err)
	}
	// The actionable copy is already on stdout, so the error must be marked as
	// reported: the user should see one report, not two.
	if !errors.Is(err, ErrCdnReported) {
		t.Errorf("bad-creds error must wrap ErrCdnReported (so cdnExit suppresses the stderr echo); got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "failed.") || !strings.Contains(got, "re-run `kamakiri cdn credentials`") {
		t.Errorf("missing actionable failure copy: %q", got)
	}
	assertCleanCopy(t, got)
}

// A refusal says nothing about the credentials, so the copy that points at the
// token is skipped, the half-written working line is closed, and the sentinel
// travels unmarked so the command layer prints the upgrade message.
func TestRotateCredentialsAbortsOnAVersionRefusal(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		rotateCredsFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 9}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			return nil, api.ErrUpgradeRequired
		},
	}

	var out bytes.Buffer
	err := RotateCredentials(client, WebAccelOptions{Token: "t", Secret: "s"}, false, nil, &out)
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	// ErrCdnReported suppresses the stderr echo, which is the only place the
	// upgrade message would appear.
	if errors.Is(err, ErrCdnReported) {
		t.Errorf("the refusal must not be marked as already reported: %v", err)
	}
	got := out.String()
	for _, banned := range []string{
		i18n.T("cdn.rotate_failed"),
		i18n.T("cdn.rotate_rejected"),
		i18n.T("cdn.rotate_still_applying"),
		i18n.T("cdn.rotate_done"),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q: %q", banned, got)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("the working line must be closed before the upgrade message: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestRotateCredentialsRequiresWebAccelMode(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		rotateCredsFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return nil, &api.ErrorResponse{Code: "requires_webaccel_mode", Message: "no webaccel config"}
		},
	}
	var out bytes.Buffer
	opts := WebAccelOptions{Token: "t", Secret: "s"}
	err := RotateCredentials(client, opts, false, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "kamakiri cdn webaccel") {
		t.Fatalf("err = %v, want a cdn-webaccel-first hint", err)
	}
}

func TestRotateCredentialsNoWaitQueued(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		rotateCredsFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 1}, nil
		},
	}
	var out bytes.Buffer
	if err := RotateCredentials(client, WebAccelOptions{Token: "t", Secret: "s"}, true, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("missing queued hint: %q", out.String())
	}
}

func TestParseCredentialsArgsRejectsDomainID(t *testing.T) {
	if _, err := ParseCredentialsArgs([]string{"--domain-id", "example.com=1001"}); err == nil {
		t.Fatal("expected --domain-id to be rejected for cdn credentials")
	}
	opts, err := ParseCredentialsArgs([]string{"--token", "t", "--secret", "s"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "t" || opts.Secret != "s" {
		t.Errorf("opts = %+v", opts)
	}
}

// The rotation command resolves its credentials through a path of its own, so
// the prompts and refusals it renders need pinning separately from the ones
// `cdn webaccel` goes through.
func TestRotationCredentialPromptsAndRefusals(t *testing.T) {
	var out bytes.Buffer
	token, secret, err := resolveCredentialsOnly(WebAccelOptions{},
		strings.NewReader("\ntok\n\nsec\n"), &out, DefaultSecretReader)
	if err != nil {
		t.Fatalf("resolveCredentialsOnly() error = %v", err)
	}
	if token != "tok" || secret != "sec" {
		t.Errorf("resolveCredentialsOnly() = %q, %q", token, secret)
	}
	for _, key := range []string{"cdn.prompt_access_token", "cdn.prompt_access_token_secret",
		"cdn.err_token_empty_retry", "cdn.err_secret_empty_retry"} {
		if !strings.Contains(out.String(), i18n.T(key)) {
			t.Errorf("the prompt flow is missing %s: %q", key, out.String())
		}
	}

	var discard bytes.Buffer
	if _, _, err := resolveCredentialsOnly(WebAccelOptions{}, nil, &discard, DefaultSecretReader); err == nil ||
		err.Error() != i18n.Tf("cdn.err_token_missing", EnvWebAccelToken) {
		t.Errorf("missing-token error = %v, want %q", err, i18n.Tf("cdn.err_token_missing", EnvWebAccelToken))
	}
	if _, _, err := resolveCredentialsOnly(WebAccelOptions{Token: "t"}, nil, &discard, DefaultSecretReader); err == nil ||
		err.Error() != i18n.Tf("cdn.err_secret_missing", EnvWebAccelSecret) {
		t.Errorf("missing-secret error = %v, want %q", err, i18n.Tf("cdn.err_secret_missing", EnvWebAccelSecret))
	}
	// The two cannot-be-empty refusals below them are defensive: promptField
	// keeps asking until it has a value, and a reader that runs out returns
	// ErrCancelled instead, so no vector reaches them.
}
