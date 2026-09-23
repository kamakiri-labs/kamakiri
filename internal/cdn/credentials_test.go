package cdn

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/syncwait"
)

func TestRotateCredentialsSuccess(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	fastPolls(t)
	var gotTok, gotSec string
	client := &mockClient{
		rotateCredsFn: func(_, tok, sec string) (*api.CDNSetResponse, error) {
			gotTok, gotSec = tok, sec
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 7}, nil
		},
	}
	// An attempt below 7 first, so a wait keyed on a lower id would settle on
	// the wrong one and never make the second read.
	client.getSiteFn = client.resolveAt(7)

	var out bytes.Buffer
	opts := WebAccelOptions{Token: "newtok", Secret: "newsec"}
	if err := RotateCredentials(client, opts, false, nil, &out); err != nil {
		t.Fatal(err)
	}
	if gotTok != "newtok" || gotSec != "newsec" {
		t.Errorf("sent token/secret = %q/%q", gotTok, gotSec)
	}
	if client.getSiteCalls != 2 {
		t.Fatalf("GetSite called %d times, want 2: the wait must poll past the older attempt", client.getSiteCalls)
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
		getSiteFn: reconcileFailed("cdn_provider http_401"),
	}

	var out bytes.Buffer
	opts := WebAccelOptions{Token: "bad", Secret: "bad"}
	err := RotateCredentials(client, opts, false, nil, &out)
	if err == nil {
		t.Fatal("expected error for rejected credentials")
	}
	// The reconciler's own reason is what says which token was refused, so it
	// has to reach the error rather than being replaced by the rotation copy.
	if !strings.Contains(err.Error(), "cdn_provider http_401") {
		t.Errorf("the rejection reason must travel: %v", err)
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
		getSiteFn: func(string) (*api.Site, error) {
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
		i18n.T("cdn.rotate_done"),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q: %q", banned, got)
		}
	}
	// The working line is the whole of the output, off a terminal with its
	// trailing space restored, closed by the newline this arm prints so the
	// upgrade message starts on a line of its own.
	if want := i18n.T("cdn.rotate_working") + "\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	assertCleanCopy(t, got)
}

// On Ctrl-C the sentinel must propagate, since the 130 exit depends on
// recognizing it, and the stop line the wait prints is the whole report: the
// rejection copy would condemn a token the reconciler has not judged yet.
func TestRotateCredentialsInterruptStopsAndPropagatesSentinel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	interruptBeforeWait(t)

	client := &mockClient{
		rotateCredsFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 12}, nil
		},
	}

	var out bytes.Buffer
	err := RotateCredentials(client, WebAccelOptions{Token: "t", Secret: "s"}, false, nil, &out)
	if !errors.Is(err, syncwait.ErrInterrupted) {
		t.Fatalf("err = %v, want syncwait.ErrInterrupted", err)
	}
	if errors.Is(err, ErrCdnReported) {
		t.Errorf("an interrupt is not a reported failure: %v", err)
	}
	// A signal already delivered wins over a site that would answer at once, so
	// the wait never reaches its first read.
	if client.getSiteCalls != 0 {
		t.Errorf("GetSite called %d times, want none after an interrupt", client.getSiteCalls)
	}
	got := out.String()
	if !strings.Contains(got, syncwait.SharedStopLine()) {
		t.Errorf("missing the stop line: %q", got)
	}
	for _, banned := range []string{
		i18n.T("cdn.rotate_failed"),
		i18n.T("cdn.rotate_rejected"),
		i18n.T("cdn.rotate_done"),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q: %q", banned, got)
		}
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

// A site endpoint that keeps failing ends the wait once it has failed past what
// the wait tolerates, before any verdict on the new credentials can be read:
// the row carries the transport reason and the token is not blamed.
func TestRotateCredentialsFailsOnAPersistentTransportFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		rotateCredsFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 14}, nil
		},
		getSiteFn: func(string) (*api.Site, error) {
			return nil, errors.New("connection refused")
		},
	}

	var out bytes.Buffer
	err := RotateCredentials(client, WebAccelOptions{Token: "t", Secret: "s"}, false, nil, &out)
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("err = %v, want ErrCdnReported", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want the transport failure's reason carried out", err)
	}
	// Off a terminal the whole wait is the working row, its held whitespace
	// restored, and the terminator reads on from it.
	got := out.String()
	want := i18n.T("cdn.rotate_working") + i18n.Tf("cdn.rotate_failed_reason", "connection refused") + "\n"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if strings.Contains(got, i18n.T("cdn.rotate_rejected")) {
		t.Errorf("output must not blame the token: %q", got)
	}
	assertCleanCopy(t, got)
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
