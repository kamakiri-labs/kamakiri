package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// testLang is the catalog every test here renders against. A test that switches
// language to check a Japanese rendering restores this one, so the pin lives in
// one place rather than in each of them.
const testLang = "en"

// The catalog is pinned so the assertions below hold whatever locale the suite
// runs under: the copy they match on renders from the message catalog. Load
// rather than Setup: nothing here reports which language is in force, only
// renders in it.
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

type mockClient struct {
	setCanonicalFn   func(siteID, domain string) (*api.Domain, error)
	unsetCanonicalFn func(siteID string) (*api.UnsetCanonicalResult, error)
	waitForSyncFn    func(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
	addDomainFn      func(siteID, domain, role string, redirectStatus int) (*api.Domain, error)
	removeDomainFn   func(domain string) (*api.RemoveDomainResult, error)
	listDomainsFn    func(siteID string) (*api.DomainList, error)
	recheckDomainFn  func(domain string) (*api.Domain, error)

	registerDomainFn    func(name string) (*api.Registration, error)
	unregisterDomainFn  func(name string) error
	listRegistrationsFn func() (*api.RegistrationList, error)
}

func (m *mockClient) RegisterDomain(name string) (*api.Registration, error) {
	return m.registerDomainFn(name)
}

func (m *mockClient) UnregisterDomain(name string) error {
	return m.unregisterDomainFn(name)
}

func (m *mockClient) ListRegistrations() (*api.RegistrationList, error) {
	return m.listRegistrationsFn()
}

func (m *mockClient) RecheckDomain(domain string) (*api.Domain, error) {
	return m.recheckDomainFn(domain)
}

func (m *mockClient) SetCanonical(siteID, domain string) (*api.Domain, error) {
	return m.setCanonicalFn(siteID, domain)
}

func (m *mockClient) UnsetCanonical(siteID string) (*api.UnsetCanonicalResult, error) {
	return m.unsetCanonicalFn(siteID)
}

func (m *mockClient) WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error) {
	if m.waitForSyncFn != nil {
		return m.waitForSyncFn(siteID, sinceAttemptID, timeout)
	}
	return &api.Site{Sync: &api.Sync{LatestAttemptID: sinceAttemptID, Outcome: "ok"}}, nil
}

func (m *mockClient) AddDomain(siteID, domain, role string, redirectStatus int) (*api.Domain, error) {
	return m.addDomainFn(siteID, domain, role, redirectStatus)
}

func (m *mockClient) RemoveDomain(domain string) (*api.RemoveDomainResult, error) {
	return m.removeDomainFn(domain)
}

func (m *mockClient) ListDomains(siteID string) (*api.DomainList, error) {
	return m.listDomainsFn(siteID)
}

func setupCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}

func setupProjectConfig(t *testing.T, siteID string) {
	t.Helper()
	if err := os.MkdirAll(".kamakiri", 0755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: siteID}); err != nil {
		t.Fatal(err)
	}
}

func TestPointBackToOriginShowsRecordsNoWatch(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "next.example.com", Role: "canonical",
					DNSRecordsExpected: []api.DNSRecord{{Name: "next.example.com", Type: "cname", Value: "h1lc.c2.kamakiri-pages.site."}}},
				{Domain: "go.example.com", Role: "redirect"}, // not content-serving, must be ignored
			}}, nil
		},
	}

	var out bytes.Buffer
	if _, err := PointBackToOriginAndWatch(context.Background(), client, "site123", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Configure your DNS:") || !strings.Contains(got, "h1lc.c2.kamakiri-pages.site.") {
		t.Errorf("missing records block: %q", got)
	}
	if !strings.Contains(got, "kamakiri domain verify next.example.com") {
		t.Errorf("missing resume hint: %q", got)
	}
	if strings.Contains(got, "go.example.com") {
		t.Errorf("redirect domain should not be listed for repointing: %q", got)
	}
}

// The reverse cutover watches every content domain: committing live when one
// converges would under-report an alias still on its stale record.
func TestPointBackToOriginWatchesAllContentDomains(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "alias.example.com", Role: "alias",
					DNSRecordsExpected: []api.DNSRecord{{Name: "alias.example.com", Type: "cname", Value: "x.c2.kamakiri-pages.site."}}},
				{Domain: "next.example.com", Role: "canonical",
					DNSRecordsExpected: []api.DNSRecord{{Name: "next.example.com", Type: "cname", Value: "h1lc.c2.kamakiri-pages.site."}}},
				{Domain: "go.example.com", Role: "redirect"}, // not content-serving, must be ignored
			}}, nil
		},
	}

	var watched []string
	origWatch := watchAllToLiveFn
	watchAllToLiveFn = func(_ context.Context, _ APIClient, _ string, names []string, _ io.Writer) error {
		watched = names
		return nil
	}
	defer func() { watchAllToLiveFn = origWatch }()

	var out bytes.Buffer
	streamed, err := PointBackToOriginAndWatch(context.Background(), client, "site123", true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !streamed {
		t.Error("a site with content domains must report streamed=true (so the caller does not settle on top)")
	}
	want := []string{"next.example.com", "alias.example.com"} // canonical first
	if len(watched) != len(want) || watched[0] != want[0] || watched[1] != want[1] {
		t.Errorf("watched = %v, want %v (canonical first, redirect excluded)", watched, want)
	}
	got := out.String()
	if !strings.Contains(got, "alias.example.com") || !strings.Contains(got, "next.example.com") {
		t.Errorf("both content domains' records should be shown: %q", got)
	}
	if strings.Contains(got, "go.example.com") {
		t.Errorf("redirect domain must not be watched or shown: %q", got)
	}
}

// The fixture's expected records are the delivery ones alone, so what the
// assertions pin is the plain-domain table, with no CDN validation entry in it.
func TestSetHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(siteID, domain string) (*api.Domain, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q, want site123", siteID)
			}
			if domain != "example.com" {
				t.Errorf("domain = %q, want example.com", domain)
			}
			return &api.Domain{
				Domain:    "example.com",
				Role:      "canonical",
				DnsTarget: "site123-abc.c1.kamakiri-pages.site",
				// An apex domain's alternatives arrive as one group of expected
				// records, which the CLI renders as sent: it holds no apex
				// heuristic of its own.
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					{Name: "example.com", Type: "a", Value: "203.0.113.2", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					{Name: "example.com", Type: "alias_or_aname", Value: "site123-abc.c1.kamakiri-pages.site", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
				},
			}, nil
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "example.com", false, &stdout)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"Configure your DNS:",
		"example.com",
		"203.0.113.1",
		"203.0.113.2",
		"ALIAS",
		"site123-abc.c1.kamakiri-pages.site",
		"pick ONE option",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q: %q", want, output)
		}
	}
}

func TestSetNoWaitSkipsWaitForSync(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{
				Domain:        "example.com",
				Role:          "canonical",
				DnsTarget:     "site123.c1.kamakiri-pages.site",
				SyncAttemptID: 42,
			}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Fatal("WaitForSync must NOT be called when noWait=true")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Set(client, "example.com", true, &out); err != nil {
		t.Fatalf("Set(noWait=true) error = %v", err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("noWait output should mention 'queued': %q", out.String())
	}
}

func TestSetSyncTimeoutReturnsErrSyncTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{Domain: "example.com", DnsTarget: "x.kamakiri-pages.jp", SyncAttemptID: 1}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}

	// A non-TTY returns before the wait, so only the interactive core reaches
	// the timeout branch.
	var out bytes.Buffer
	err := set(client, "example.com", true, &out)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("expected api.ErrSyncTimeout, got %v", err)
	}
	// A timeout is in progress, not a failure, so it must keep the ⧗ glyph. The
	// exact wording is pinned so a revert to plain text or ✗ is caught.
	s := out.String()
	if !strings.Contains(s, "⧗ still syncing your domain on our side") {
		t.Errorf("expected the ⧗ still-syncing our-side line: %q", s)
	}
	if strings.Contains(s, "✗ couldn't") {
		t.Errorf("a timeout must NOT render as a ✗ failure: %q", s)
	}
}

func TestSetNonTimeoutWaitErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	wantErr := errors.New("reconcile failed: caddy push failed (5xx)")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{Domain: "example.com", DnsTarget: "x.kamakiri-pages.jp", SyncAttemptID: 1}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, wantErr
		},
	}

	var out bytes.Buffer
	err := set(client, "example.com", true, &out)
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("expected wrapped wantErr, got %v", err)
	}
	if !strings.Contains(out.String(), "✗ couldn't link your domain on our side") ||
		!strings.Contains(out.String(), wantErr.Error()) {
		t.Errorf("expected the ✗ our-side failure line with the reason: %q", out.String())
	}
	// Matching ErrOurSideShown is what stops the command layer echoing the
	// reason a second time, and Unwrap must still yield the reason that was
	// shown on the ✗ line.
	if !errors.Is(err, ErrOurSideShown) {
		t.Errorf("non-timeout our-side failure must wrap ErrOurSideShown, got %v", err)
	}
	if u := errors.Unwrap(err); u == nil || u.Error() != wantErr.Error() {
		t.Errorf("Unwrap must yield the shown reason, got %v", u)
	}
}

// A refusal is not our side failing, and both resume pointers name commands the
// same floor refuses, so the our-side ✗ line and the already-reported wrap are
// both skipped and the sentinel reaches the command layer whole.
func TestSetOurSideAbortsOnAVersionRefusal(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{Domain: "example.com", DnsTarget: "x.kamakiri-pages.jp", SyncAttemptID: 1}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrUpgradeRequired
		},
	}

	var out bytes.Buffer
	err := set(client, "example.com", true, &out)
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	// ErrOurSideShown suppresses the stderr echo, which is the only place the
	// upgrade message would appear.
	if errors.Is(err, ErrOurSideShown) {
		t.Errorf("the refusal must not be marked as already shown: %v", err)
	}
	got := out.String()
	resume := i18n.Tf("domain.resume_verify", "example.com")
	for _, banned := range []string{
		ourSideFailedLine(syncVerbLink, i18n.T("api.err_upgrade_required"), resume),
		i18n.T("domain.resume_status"),
		resume,
		i18n.Tf("domain.our_side_still_syncing", resume),
		i18n.T("domain.our_side_linked"),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q:\n%s", banned, got)
		}
	}
	// The ⧗ line is the last thing on screen: no outcome replaces it, and off a
	// TTY nothing rewinds it either, so the upgrade message the command layer
	// prints starts on the next line.
	if !strings.HasSuffix(got, ourSideStartLine(syncVerbLink)+"\n") {
		t.Errorf("output must end on the our-side line:\n%q", got)
	}
}

// waitAndRender's outcome words are the wrong frame for a refusal: "failed."
// would read as the sync failing. The caller's open line still has to be closed,
// so the refusal writes the newline and nothing else, and the sentinel is the
// whole report.
func TestWaitAndRenderAbortsOnAVersionRefusal(t *testing.T) {
	client := &mockClient{
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrUpgradeRequired
		},
	}

	var out bytes.Buffer
	err := waitAndRender(client, "site123", 1, &out)
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	got := out.String()
	for _, banned := range []string{
		i18n.T("domain.wait_failed"),
		i18n.T("domain.wait_still_syncing"),
		i18n.T("domain.wait_done"),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q: %q", banned, got)
		}
	}
	// The callers open the line and leave it to waitAndRender to close, so the
	// refusal owes them a newline and owes the upgrade message a clean terminal.
	if got != "\n" {
		t.Errorf("output = %q, want just the newline that closes the caller's line", got)
	}
}

func TestUnsetNoWaitSkipsWaitForSync(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			return &api.UnsetCanonicalResult{SyncAttemptID: 7}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Fatal("WaitForSync must NOT be called when noWait=true")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Unset(client, true, &out); err != nil {
		t.Fatalf("Unset(noWait=true) error = %v", err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("noWait output should mention 'queued': %q", out.String())
	}
}

func TestUnsetSyncTimeoutReturnsErrSyncTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			return &api.UnsetCanonicalResult{SyncAttemptID: 7}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}

	var out bytes.Buffer
	err := Unset(client, false, &out)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("expected api.ErrSyncTimeout, got %v", err)
	}
	if !strings.Contains(out.String(), "still syncing") {
		t.Errorf("expected 'still syncing' message: %q", out.String())
	}
}

func TestSetNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			t.Error("SetCanonical should not be called")
			return nil, nil
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

func TestSetNoProject(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			t.Error("SetCanonical should not be called")
			return nil, nil
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no site linked") {
		t.Errorf("error = %q, want 'no site linked'", err.Error())
	}
}

func TestSetDomainTaken(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_taken", Message: "Domain is already attached to another site."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "taken.com", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already attached to another site") {
		t.Errorf("error = %q, want 'already attached to another site'", err.Error())
	}
}

func TestSetDomainReserved(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_reserved", Message: "That domain is part of Kamakiri's own namespace."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "kamakiri-pages.jp", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	// This fragment appears only in the mapped copy, never in the mock's raw
	// message, so the test bites only if Set maps the error.
	if !strings.Contains(err.Error(), "can't be used as a custom domain") {
		t.Errorf("error = %q, want 'can't be used as a custom domain'", err.Error())
	}
}

func TestSetSiteNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "missing")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "site not found") {
		t.Errorf("error = %q, want 'site not found'", err.Error())
	}
}

func TestSetUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

func TestUnsetHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(siteID string) (*api.UnsetCanonicalResult, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q, want site123", siteID)
			}
			return &api.UnsetCanonicalResult{SyncAttemptID: 1}, nil
		},
	}

	var stdout bytes.Buffer
	err := Unset(client, false, &stdout)
	if err != nil {
		t.Fatalf("Unset() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Removing canonical domain") {
		t.Errorf("output = %q, want 'Removing canonical domain'", output)
	}
	if !strings.Contains(output, "done") {
		t.Errorf("output = %q, want 'done'", output)
	}
}

func TestUnsetNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			t.Error("UnsetCanonical should not be called")
			return &api.UnsetCanonicalResult{SyncAttemptID: 1}, nil
		},
	}

	var stdout bytes.Buffer
	err := Unset(client, false, &stdout)
	if err == nil {
		t.Fatal("Unset() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

func TestUnsetNoProject(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			t.Error("UnsetCanonical should not be called")
			return &api.UnsetCanonicalResult{SyncAttemptID: 1}, nil
		},
	}

	var stdout bytes.Buffer
	err := Unset(client, false, &stdout)
	if err == nil {
		t.Fatal("Unset() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no site linked") {
		t.Errorf("error = %q, want 'no site linked'", err.Error())
	}
}

func TestUnsetNoCanonical(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			return nil, &api.ErrorResponse{Code: "no_canonical", Message: "Site has no canonical domain."}
		},
	}

	var stdout bytes.Buffer
	err := Unset(client, false, &stdout)
	if err == nil {
		t.Fatal("Unset() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "set a canonical domain first") {
		t.Errorf("error = %q, want 'set a canonical domain first'", err.Error())
	}
}

func TestUnsetUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Unset(client, false, &stdout)
	if err == nil {
		t.Fatal("Unset() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

// addClientWithWait builds a client whose sync wait succeeds immediately. Tests
// for the wait branch override it.
func addClientWithWait(addFn func(siteID, domain, role string, redirectStatus int) (*api.Domain, error)) *mockClient {
	return &mockClient{
		addDomainFn: addFn,
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{}, nil
		},
	}
}

// The fixture's expected records are the delivery CNAME alone, so what the
// assertions pin is the plain-domain table, with no CDN validation entry in it.
func TestAddRedirectHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := addClientWithWait(func(siteID, domain, role string, redirectStatus int) (*api.Domain, error) {
		if siteID != "site123" {
			t.Errorf("siteID = %q", siteID)
		}
		if domain != "www.example.com" {
			t.Errorf("domain = %q", domain)
		}
		if role != "redirect" {
			t.Errorf("role = %q", role)
		}
		if redirectStatus != 301 {
			t.Errorf("redirectStatus = %d", redirectStatus)
		}
		return &api.Domain{
			Domain:        "www.example.com",
			Role:          "redirect",
			DnsTarget:     "site123.c1.kamakiri-pages.site",
			SyncAttemptID: 11,
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "www.example.com", Type: "cname", Value: "site123.c1.kamakiri-pages.site", Purpose: "primary", Required: true},
			},
		}, nil
	})

	// A non-TTY writer means the fast queued return, record and resume hint
	// included.
	var stdout bytes.Buffer
	err := Add(client, "www.example.com", "redirect", 301, false, &stdout)
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	for _, want := range []string{
		"www.example.com",
		"CNAME",
		"site123.c1.kamakiri-pages.site",
		"Configure your DNS:",
		"queued",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q: %q", want, stdout.String())
		}
	}
}

func TestAddAliasHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := addClientWithWait(func(_, _, role string, redirectStatus int) (*api.Domain, error) {
		if role != "alias" {
			t.Errorf("role = %q", role)
		}
		if redirectStatus != 0 {
			t.Errorf("redirectStatus = %d, want 0", redirectStatus)
		}
		return &api.Domain{
			Domain:        "alias.example.com",
			Role:          "alias",
			DnsTarget:     "site123.c1.kamakiri-pages.site",
			SyncAttemptID: 12,
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "alias.example.com", Type: "cname", Value: "site123.c1.kamakiri-pages.site", Purpose: "primary", Required: true},
			},
		}, nil
	})

	var stdout bytes.Buffer
	err := Add(client, "alias.example.com", "alias", 0, false, &stdout)
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	for _, want := range []string{
		"alias",
		"Configure your DNS:",
		"alias.example.com",
		"CNAME",
		"site123.c1.kamakiri-pages.site",
		"queued",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q: %q", want, stdout.String())
		}
	}
}

// A domain with no expected records must suppress the whole records block,
// header included, rather than print a header over nothing.
func TestAddSuppressesDnsBlockWhenNoExpectedRecords(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := addClientWithWait(func(_, _, _ string, _ int) (*api.Domain, error) {
		return &api.Domain{
			Domain:        "x.example.com",
			Role:          "alias",
			SyncAttemptID: 99,
			// DNSRecordsExpected intentionally empty.
		}, nil
	})

	var stdout bytes.Buffer
	if err := Add(client, "x.example.com", "alias", 0, false, &stdout); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if strings.Contains(stdout.String(), "Configure your DNS:") {
		t.Errorf("DNS header should be suppressed when no expected records; got:\n%s", stdout.String())
	}
}

func TestAddNoCanonical(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "no_canonical", Message: "Set a canonical domain first."}
		},
	}

	var stdout bytes.Buffer
	err := Add(client, "www.example.com", "redirect", 301, false, &stdout)
	if err == nil {
		t.Fatal("Add() expected error")
	}
	if !strings.Contains(err.Error(), "canonical domain first") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAddDomainTaken(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_taken", Message: "Domain is already attached to another site."}
		},
	}

	var stdout bytes.Buffer
	err := Add(client, "taken.com", "alias", 0, false, &stdout)
	if err == nil {
		t.Fatal("Add() expected error")
	}
	if !strings.Contains(err.Error(), "already attached to another site") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAddDomainReserved(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_reserved", Message: "That domain is part of Kamakiri's own namespace."}
		},
	}

	var stdout bytes.Buffer
	err := Add(client, "alias.kamakiri-pages.site", "alias", 0, false, &stdout)
	if err == nil {
		t.Fatal("Add() expected error")
	}
	// Assert on a fragment present only in the mapped copy, not in the mock's
	// raw Message, so the test bites only when Add routes through MapError.
	if !strings.Contains(err.Error(), "can't be used as a custom domain") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAddUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Add(client, "example.com", "alias", 0, false, &stdout)
	if err == nil {
		t.Fatal("Add() expected error")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAddNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{}
	var stdout bytes.Buffer
	err := Add(client, "example.com", "alias", 0, false, &stdout)
	if err == nil {
		t.Fatal("Add() expected error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAddNoWaitSkipsWaitForSync(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return &api.Domain{
				Domain:        "alias.example.com",
				Role:          "alias",
				DnsTarget:     "site123.c1.kamakiri-pages.site",
				SyncAttemptID: 21,
			}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Fatal("WaitForSync must NOT be called when noWait=true")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Add(client, "alias.example.com", "alias", 0, true, &out); err != nil {
		t.Fatalf("Add(noWait=true) error = %v", err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("noWait output should mention 'queued': %q", out.String())
	}
}

func TestAddSyncTimeoutReturnsErrSyncTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return &api.Domain{Domain: "alias.example.com", DnsTarget: "x", SyncAttemptID: 22}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}

	// A non-TTY returns before the wait, so only the interactive core reaches
	// the timeout branch.
	var out bytes.Buffer
	err := add(client, "alias.example.com", "alias", 0, true, &out)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("expected api.ErrSyncTimeout, got %v", err)
	}
	if !strings.Contains(out.String(), "still syncing") {
		t.Errorf("expected 'still syncing' message: %q", out.String())
	}
}

func TestAddNonTimeoutWaitErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	wantErr := errors.New("reconcile failed: caddy push failed (5xx)")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return &api.Domain{Domain: "alias.example.com", DnsTarget: "x", SyncAttemptID: 23}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, wantErr
		},
	}

	var out bytes.Buffer
	err := add(client, "alias.example.com", "alias", 0, true, &out)
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("expected wrapped wantErr, got %v", err)
	}
	if !strings.Contains(out.String(), "✗ couldn't link your domain on our side") ||
		!strings.Contains(out.String(), wantErr.Error()) {
		t.Errorf("expected the ✗ our-side failure line with the reason: %q", out.String())
	}
}

// removeClientWithWait builds a client whose sync wait succeeds immediately and
// asserts it was waited on with the ids the server returned.
func removeClientWithWait(siteID string, syncAttemptID int64, removeFn func(domain string) (*api.RemoveDomainResult, error)) *mockClient {
	return &mockClient{
		removeDomainFn: removeFn,
		waitForSyncFn: func(gotSiteID string, gotAttemptID int64, _ time.Duration) (*api.Site, error) {
			if gotSiteID != siteID {
				return nil, fmt.Errorf("WaitForSync siteID = %q, want %q", gotSiteID, siteID)
			}
			if gotAttemptID != syncAttemptID {
				return nil, fmt.Errorf("WaitForSync sinceAttemptID = %d, want %d", gotAttemptID, syncAttemptID)
			}
			return &api.Site{}, nil
		},
	}
}

func TestRemoveHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := removeClientWithWait("site123", 31, func(domain string) (*api.RemoveDomainResult, error) {
		if domain != "www.example.com" {
			t.Errorf("domain = %q", domain)
		}
		return &api.RemoveDomainResult{SiteID: "site123", SyncAttemptID: 31}, nil
	})

	var stdout bytes.Buffer
	err := Remove(client, "www.example.com", false, &stdout)
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "www.example.com") {
		t.Errorf("output = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "done.") {
		t.Errorf("expected 'done.' marker: %q", stdout.String())
	}
}

func TestRemoveNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		removeDomainFn: func(_ string) (*api.RemoveDomainResult, error) {
			return nil, &api.ErrorResponse{Code: "domain_not_found", Message: "Domain not found."}
		},
	}

	var stdout bytes.Buffer
	err := Remove(client, "nonexistent.com", false, &stdout)
	if err == nil {
		t.Fatal("Remove() expected error")
	}
	if !strings.Contains(err.Error(), "domain not found") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemoveIsCanonical(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		removeDomainFn: func(_ string) (*api.RemoveDomainResult, error) {
			return nil, &api.ErrorResponse{Code: "is_canonical", Message: "Use unset."}
		},
	}

	var stdout bytes.Buffer
	err := Remove(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Remove() expected error")
	}
	if !strings.Contains(err.Error(), "kamakiri domain unset") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemoveUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		removeDomainFn: func(_ string) (*api.RemoveDomainResult, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Remove(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Remove() expected error")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemoveNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{}
	var stdout bytes.Buffer
	err := Remove(client, "example.com", false, &stdout)
	if err == nil {
		t.Fatal("Remove() expected error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRemoveNoWaitSkipsWaitForSync(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		removeDomainFn: func(_ string) (*api.RemoveDomainResult, error) {
			return &api.RemoveDomainResult{SiteID: "site123", SyncAttemptID: 41}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Fatal("WaitForSync must NOT be called when noWait=true")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Remove(client, "www.example.com", true, &out); err != nil {
		t.Fatalf("Remove(noWait=true) error = %v", err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("noWait output should mention 'queued': %q", out.String())
	}
}

func TestRemoveSyncTimeoutReturnsErrSyncTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		removeDomainFn: func(_ string) (*api.RemoveDomainResult, error) {
			return &api.RemoveDomainResult{SiteID: "site123", SyncAttemptID: 42}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}

	var out bytes.Buffer
	err := Remove(client, "www.example.com", false, &out)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("expected api.ErrSyncTimeout, got %v", err)
	}
	if !strings.Contains(out.String(), "still syncing") {
		t.Errorf("expected 'still syncing' message: %q", out.String())
	}
}

// The wait must key on the site id the server returned, not the one in the
// local project config. A regression here still works whenever the two match,
// and breaks the moment the linked project is not the domain's own site.
func TestRemoveResultCarriesSiteID(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "local-project-id")

	var capturedSiteID string
	var capturedAttemptID int64

	client := &mockClient{
		removeDomainFn: func(_ string) (*api.RemoveDomainResult, error) {
			return &api.RemoveDomainResult{SiteID: "remote-site-id", SyncAttemptID: 7}, nil
		},
		waitForSyncFn: func(siteID string, attemptID int64, _ time.Duration) (*api.Site, error) {
			capturedSiteID = siteID
			capturedAttemptID = attemptID
			return &api.Site{}, nil
		},
	}

	var out bytes.Buffer
	if err := Remove(client, "x.example.com", false, &out); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if capturedSiteID != "remote-site-id" {
		t.Errorf("WaitForSync siteID = %q, want %q (must use server result, not project config)", capturedSiteID, "remote-site-id")
	}
	if capturedAttemptID != 7 {
		t.Errorf("WaitForSync attemptID = %d, want 7", capturedAttemptID)
	}
}

func TestListRegistrationsHappyPath(t *testing.T) {
	// Account-scoped, so there is no project setup here, though the wrapper
	// requires a credential.
	setupCredentials(t)

	client := &mockClient{
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return &api.RegistrationList{
				Registrations: []api.Registration{
					{
						Name:   "example.com",
						Status: "verified",
						Attachments: []api.RegistrationAttachment{
							{SiteID: "s1", Host: "example.com", Role: "canonical"},
							{SiteID: "s1", Host: "www.example.com", Role: "redirect", RedirectStatus: 301},
							{SiteID: "s2", Host: "app.example.com", Role: "alias"},
						},
					},
					{Name: "pending.com", Status: "awaiting"},
				},
			}, nil
		},
	}

	var stdout bytes.Buffer
	if err := ListRegistrations(client, &stdout); err != nil {
		t.Fatalf("ListRegistrations() error = %v", err)
	}
	output := stdout.String()
	for _, want := range []string{
		"example.com",
		"verified",
		"example.com (canonical)",
		"www.example.com (redirect 301)",
		"app.example.com (alias)",
		"pending.com",
		"awaiting verification",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q: %q", want, output)
		}
	}
}

func TestListRegistrationsUnknownStatusRendersVerbatim(t *testing.T) {
	setupCredentials(t)

	// A status this CLI has no label for is shown as sent rather than swallowed.
	client := &mockClient{
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return &api.RegistrationList{
				Registrations: []api.Registration{{Name: "example.com", Status: "some_future_status"}},
			}, nil
		},
	}

	var stdout bytes.Buffer
	if err := ListRegistrations(client, &stdout); err != nil {
		t.Fatalf("ListRegistrations() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "some_future_status") {
		t.Errorf("output missing the raw status: %q", stdout.String())
	}
}

func TestListRegistrationsEmpty(t *testing.T) {
	setupCredentials(t)

	client := &mockClient{
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return &api.RegistrationList{}, nil
		},
	}

	var stdout bytes.Buffer
	if err := ListRegistrations(client, &stdout); err != nil {
		t.Fatalf("ListRegistrations() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "No domains registered") {
		t.Errorf("output = %q, want 'No domains registered'", stdout.String())
	}
}

func TestListRegistrationsUnauthorized(t *testing.T) {
	setupCredentials(t)

	client := &mockClient{
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := ListRegistrations(client, &stdout)
	if err == nil {
		t.Fatal("ListRegistrations() expected error")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestUnsetHasRedirectDomains(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		unsetCanonicalFn: func(_ string) (*api.UnsetCanonicalResult, error) {
			return nil, &api.ErrorResponse{Code: "has_redirect_domains", Message: "Remove redirect domains first."}
		},
	}

	var stdout bytes.Buffer
	err := Unset(client, false, &stdout)
	if err == nil {
		t.Fatal("Unset() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "remove redirect domains") {
		t.Errorf("error = %q, want 'remove redirect domains'", err.Error())
	}
}

func TestSetUnknownServerErrorSurfaces(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "some_server_error", Message: "server says no"}
		},
	}

	var out bytes.Buffer
	err := Set(client, "example.com", false, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server says no") {
		t.Errorf("error = %q, want server message to surface", err.Error())
	}
}

func TestAddUnknownServerErrorSurfaces(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "some_server_error", Message: "server says no"}
		},
	}

	var out bytes.Buffer
	err := Add(client, "blog.example.com", "alias", 0, false, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "server says no") {
		t.Errorf("error = %q, want server message to surface", err.Error())
	}
}

func TestFormatStatusFiveStates(t *testing.T) {
	// The unknown-state case would otherwise write to real stderr; the warning
	// itself has its own test.
	resetUnknownStateWarnings(t)
	var warnBuf bytes.Buffer
	prev := unknownStateWarnWriter
	unknownStateWarnWriter = &warnBuf
	t.Cleanup(func() { unknownStateWarnWriter = prev })

	cases := []struct {
		name   string
		domain api.Domain
		want   string
	}{
		{
			// Never-live copy is actionable: it names the verify command rather
			// than sitting on a static "awaiting your DNS".
			name:   "awaiting_dns",
			domain: api.Domain{State: "awaiting_dns", Domain: "next.altstack.jp"},
			want:   "⧗ waiting for your DNS. Add the record below, then `kamakiri domain verify next.altstack.jp`",
		},
		{
			// The "add the record" nudge would misdirect a user whose record is
			// already published, so an unreachable verdict gets its own copy.
			name: "awaiting_dns unreachable",
			domain: api.Domain{
				State:      "awaiting_dns",
				Domain:     "next.altstack.jp",
				DnsVerdict: "unreachable",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "next.altstack.jp", Type: "cname", ObserveError: "servfail", ObserveErrorAt: time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339)},
				},
			},
			want: "couldn't reach your DNS (servfail, ~4m ago)",
		},
		{
			// A nil has_live_deploy, which is what an older server sends, must
			// not flip the copy to deploy-first.
			name:   "awaiting_edge",
			domain: api.Domain{State: "awaiting_edge"},
			want:   "⧗ verifying edge",
		},
		{
			name:   "awaiting_edge with live deploy stays verifying edge",
			domain: api.Domain{State: "awaiting_edge", HasLiveDeploy: boolPtr(true)},
			want:   "⧗ verifying edge",
		},
		{
			name:   "awaiting_edge no live deploy renders deploy-first",
			domain: api.Domain{State: "awaiting_edge", Domain: "x.example.com", HasLiveDeploy: boolPtr(false)},
			want:   "this site has no published content yet. Run `kamakiri deploy <path>`",
		},
		{
			// A deploy-less domain rests in awaiting_edge, but one seen here
			// still cannot get a certificate, so it must not get the spinner.
			name:   "awaiting_cert no live deploy renders deploy-first",
			domain: api.Domain{State: "awaiting_cert", Domain: "x.example.com", HasLiveDeploy: boolPtr(false)},
			want:   "this site has no published content yet. Run `kamakiri deploy <path>`",
		},
		{
			// The deploy-first copy must not reach awaiting_dns, where the user
			// needs the record guidance first.
			name:   "awaiting_dns no live deploy keeps DNS guidance",
			domain: api.Domain{State: "awaiting_dns", Domain: "next.altstack.jp", HasLiveDeploy: boolPtr(false)},
			want:   "⧗ waiting for your DNS. Add the record below, then `kamakiri domain verify next.altstack.jp`",
		},
		{
			name:   "serving",
			domain: api.Domain{State: "serving"},
			want:   "✓ live",
		},
		{
			// An "absent" verdict regresses the DNS axis while a correct
			// certificate leaves the cert axis healthy, which isolates the
			// DNS-only variant of the degraded line.
			name: "degraded with DNS regressed",
			domain: api.Domain{
				State:        "degraded",
				Domain:       "x.example.com",
				CertRequired: true,
				DnsVerdict:   "absent",
				DnsCheckedAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
				CertVerdict:  CertVerdictCorrect,
			},
			want: "DNS record no longer visible",
		},
		{
			// "No longer visible" would fabricate a definitive miss that was
			// never observed.
			name: "degraded with DNS unreachable",
			domain: api.Domain{
				State:        "degraded",
				Domain:       "x.example.com",
				CertRequired: true,
				DnsVerdict:   "unreachable",
				CertVerdict:  CertVerdictCorrect,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "x.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "x.example.com", Type: "cname", ObserveError: "timeout", ObserveErrorAt: time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)},
				},
			},
			want: "couldn't reach your DNS (timeout, ~1m ago)",
		},
		{
			// The record is present, just pointed elsewhere, so the line names
			// the wrong target rather than calling it gone.
			name: "degraded with DNS present_but_wrong",
			domain: api.Domain{
				State:        "degraded",
				Domain:       "www.example.com",
				CertRequired: true,
				DnsVerdict:   "present_but_wrong",
				CertVerdict:  CertVerdictCorrect,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "www.example.com", Type: "cname", Values: []string{"other.example.net"}},
				},
			},
			want: "points at the wrong target",
		},
		{
			// "Add the record below" would misdirect: the record exists.
			name: "awaiting_dns present_but_wrong",
			domain: api.Domain{
				State:      "awaiting_dns",
				Domain:     "www.example.com",
				DnsVerdict: "present_but_wrong",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "www.example.com", Type: "cname", Values: []string{"other.example.net"}},
				},
			},
			want: "points at the wrong target",
		},
		{
			// Neither axis regressed, so the line must not blame DNS.
			name:   "degraded without axis attribution",
			domain: api.Domain{State: "degraded"},
			want:   "we're re-checking automatically",
		},
		{
			name:   "tearing_down (fresh)",
			domain: api.Domain{State: "tearing_down"},
			want:   "⧗ removing",
		},
		{
			name: "tearing_down (stuck >2h surfaces annotation)",
			domain: api.Domain{
				State:             "tearing_down",
				DeleteRequestedAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
			},
			want: "⧗ removing (since 3h ago)",
		},
		{
			name:   "unknown state passes through (forward-compat)",
			domain: api.Domain{State: "future_value"},
			want:   "future_value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatStatus(tc.domain)
			// The longer lines are matched on a substring, so the test is not a
			// verbatim lock on copy that may be reworded.
			if got != tc.want && !strings.Contains(got, tc.want) {
				t.Errorf("FormatStatus = %q, want %q (or containing it)", got, tc.want)
			}
			// The negative pins the wrong-target arm against a refactor that
			// folds it back into the missing-record one.
			if tc.domain.DnsVerdict == "present_but_wrong" {
				for _, banned := range []string{"no longer visible", "Add the record", "DNS record missing"} {
					if strings.Contains(got, banned) {
						t.Errorf("present_but_wrong line must not contain %q, got %q", banned, got)
					}
				}
			}
		})
	}
}

// boolPtr exists for the nil-versus-explicit-false distinction the no-deploy
// copy branches on.
func boolPtr(b bool) *bool { return &b }

// A CDN-mode domain reaches the same branch and issues no certificate of ours,
// and a site that was live can lose its deploy, so the no-deploy copy must
// never mention a certificate or a first deploy in any mode.
func TestFormatStatusNoDeployCopyIsCdnNeutral(t *testing.T) {
	for _, mode := range []string{"none", "webaccel", "cloudflare"} {
		t.Run(mode, func(t *testing.T) {
			d := api.Domain{
				State:         "awaiting_edge",
				Domain:        "x.example.com",
				CdnMode:       mode,
				CertRequired:  mode == "none",
				HasLiveDeploy: boolPtr(false),
			}
			got := FormatStatus(d)
			if !strings.Contains(got, "no published content yet") {
				t.Fatalf("expected the deploy-first line, got %q", got)
			}
			for _, banned := range []string{"SSL", "certificate", "first deploy"} {
				if strings.Contains(got, banned) {
					t.Errorf("no-deploy copy must not contain %q, got %q", banned, got)
				}
			}
		})
	}
}

// deliveryUnreachable builds a domain whose delivery record's last probe was a
// resolver error, which is the per-record source the unreachable line reads.
func deliveryUnreachable(name, reason, at string) api.Domain {
	return api.Domain{
		Domain: name,
		DNSRecordsExpected: []api.DNSRecord{
			{Name: name, Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: name, Type: "cname", ObserveError: reason, ObserveErrorAt: at},
		},
	}
}

// The line names only the observed error and when, across every combination of
// the two the server can send.
func TestDnsUnreachableStatusLine(t *testing.T) {
	fourMinAgo := time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339)

	cases := []struct {
		name   string
		domain api.Domain
		want   string
	}{
		{
			name:   "error and anchor",
			domain: deliveryUnreachable("www.example.com", "servfail", fourMinAgo),
			want:   "⚠ couldn't reach your DNS (servfail, ~4m ago); re-checking automatically.",
		},
		{
			// A sub-minute anchor renders empty, so the line drops the age
			// rather than emitting "~0m ago".
			name:   "error only (sub-minute anchor)",
			domain: deliveryUnreachable("www.example.com", "servfail", time.Now().UTC().Format(time.RFC3339)),
			want:   "⚠ couldn't reach your DNS (servfail); re-checking automatically.",
		},
		{
			// With no error to name, the line falls back to the last-checked
			// anchor alone.
			name:   "anchor only via checked_at",
			domain: api.Domain{DnsCheckedAt: fourMinAgo},
			want:   "⚠ couldn't reach your DNS (last attempt ~4m ago); re-checking automatically.",
		},
		{
			name:   "neither error nor anchor",
			domain: api.Domain{},
			want:   "⚠ couldn't reach your DNS; re-checking automatically.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dnsUnreachableStatusLine(tc.domain); got != tc.want {
				t.Errorf("dnsUnreachableStatusLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// The line names expected against observed, folds in the diagnosis when one
// matches, and falls back to a generic line where there is no diff to show.
func TestDnsWrongTargetStatusLine(t *testing.T) {
	cname := []api.DNSRecord{
		{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
	}

	t.Run("names expected and observed", func(t *testing.T) {
		d := api.Domain{
			Domain:             "www.example.com",
			DNSRecordsExpected: cname,
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "www.example.com", Type: "cname", Values: []string{"other.example.net"}},
			},
		}
		got := dnsWrongTargetStatusLine(d)
		for _, want := range []string{"points at the wrong target", "site-abc.kamakiri-pages.jp.", "other.example.net", "Update the record below"} {
			if !strings.Contains(got, want) {
				t.Errorf("line missing %q: %q", want, got)
			}
		}
	})

	t.Run("folds in the orange-cloud diagnosis", func(t *testing.T) {
		d := api.Domain{
			Domain:             "www.example.com",
			DNSRecordsExpected: cname,
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "www.example.com", Type: "cname", Values: []string{"104.21.5.5"}},
			},
		}
		if got := dnsWrongTargetStatusLine(d); !strings.Contains(got, "orange cloud") {
			t.Errorf("expected the Cloudflare-proxy diagnosis, got: %q", got)
		}
	})

	t.Run("generic fallback when no CNAME-shaped diff", func(t *testing.T) {
		// The only expected primary is the `alias_or_aname` placeholder,
		// which the diff skips, so there is no expected/got pair to name.
		d := api.Domain{
			Domain: "example.com",
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "example.com", Type: "alias_or_aname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", AlternativeGroup: "apex_primary"},
			},
		}
		got := dnsWrongTargetStatusLine(d)
		if !strings.Contains(got, "points at the wrong target") || strings.Contains(got, "expected ") {
			t.Errorf("expected the generic no-diff fallback, got: %q", got)
		}
	})
}

// With both axes regressed and DNS unreachable, the line must still name both
// without claiming a definitive miss that was never observed.
func TestFormatStatusDegradedBothAxesUnreachable(t *testing.T) {
	d := api.Domain{
		State:              "degraded",
		Domain:             "x.example.com",
		CertRequired:       true,
		DnsVerdict:         "unreachable",
		CertVerdict:        CertVerdictWrong,
		CertObservedLastAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "x.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "x.example.com", Type: "cname", ObserveError: "timeout", ObserveErrorAt: time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339)},
		},
	}
	got := FormatStatus(d)
	if strings.Contains(got, "DNS record missing") {
		t.Errorf("combined unreachable line must not fabricate a definitive miss, got %q", got)
	}
	if !strings.Contains(got, "couldn't reach your DNS") || !strings.Contains(got, "TLS certificate") {
		t.Errorf("combined unreachable line must name both axes, got %q", got)
	}
}

// With both axes regressed and the record present, the line must name the wrong
// target rather than a missing record the observation contradicts.
func TestFormatStatusDegradedBothAxesPresentButWrong(t *testing.T) {
	d := api.Domain{
		State:              "degraded",
		Domain:             "www.example.com",
		CertRequired:       true,
		DnsVerdict:         "present_but_wrong",
		CertVerdict:        CertVerdictWrong,
		CertObservedLastAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "www.example.com", Type: "cname", Values: []string{"other.example.net"}},
		},
	}
	got := FormatStatus(d)
	if strings.Contains(got, "DNS record missing") {
		t.Errorf("combined present_but_wrong line must not claim the record is missing, got %q", got)
	}
	if !strings.Contains(got, "wrong target") || !strings.Contains(got, "TLS certificate") {
		t.Errorf("combined present_but_wrong line must name both axes, got %q", got)
	}
}

func TestIsRouted(t *testing.T) {
	// awaiting_cert counts as routed, since the route has to be in place before
	// issuance starts.
	for _, state := range []string{"awaiting_edge", "awaiting_cert", "serving", "degraded"} {
		if !IsRouted(state) {
			t.Errorf("IsRouted(%q) = false, want true", state)
		}
	}

	for _, state := range []string{"awaiting_dns", "tearing_down", ""} {
		if IsRouted(state) {
			t.Errorf("IsRouted(%q) = true, want false", state)
		}
	}
}

// The four named categories carry the same diagnosis here as the watch's
// certCategoryCopy gives them, so one observed fact does not reach the user two
// different ways. The empty category is outside that set: it falls back to the
// bare line, while the watch's fallback adds "still working".
func TestFormatStatusAwaitingCertNamesObservedCategory(t *testing.T) {
	for _, tc := range []struct {
		name, category, wantSubstring string
	}{
		{"bare (no category yet)", "", "provisioning SSL certificate"},
		{"cert_unavailable", CertErrorCertUnavailable, "our edge reports no certificate yet"},
		{"wrong_subject", CertErrorWrongSubject, "different hostname"},
		{"chain_invalid", CertErrorChainInvalid, "chain failed validation"},
		{"expired", CertErrorExpired, "past its expiry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := api.Domain{State: StateAwaitingCert, CertError: tc.category}
			got := FormatStatus(d)
			if !strings.Contains(got, tc.wantSubstring) {
				t.Errorf("category %q: FormatStatus=%q, want substring %q",
					tc.category, got, tc.wantSubstring)
			}
		})
	}
}

// resetUnknownStateWarnings clears the once-per-state guard so a test can
// observe the warning deterministically.
func resetUnknownStateWarnings(t *testing.T) {
	t.Helper()
	unknownStateWarnedMu.Lock()
	defer unknownStateWarnedMu.Unlock()
	unknownStateWarned = map[string]bool{}
}

func TestFormatStatusUnknownStateWarnsOnce(t *testing.T) {
	resetUnknownStateWarnings(t)

	var buf bytes.Buffer
	prev := unknownStateWarnWriter
	unknownStateWarnWriter = &buf
	t.Cleanup(func() { unknownStateWarnWriter = prev })

	// Only the first of the three should write to stderr, so a view of many
	// rows in one unknown state writes one warning rather than one per row.
	for i := 0; i < 3; i++ {
		got := FormatStatus(api.Domain{State: "future_value"})
		if got != "future_value" {
			t.Errorf("call #%d: FormatStatus = %q, want raw passthrough", i+1, got)
		}
	}

	if got := strings.Count(buf.String(), "future_value"); got != 1 {
		t.Errorf("expected exactly 1 warning line for the unknown state, got %d (output: %q)", got, buf.String())
	}
	if !strings.Contains(buf.String(), "Consider upgrading") {
		t.Errorf("warning should suggest upgrading the CLI: %q", buf.String())
	}

	// A different unknown state warns again: the dedupe is per state, not once
	// for the whole run.
	if got := FormatStatus(api.Domain{State: "another_future_value"}); got != "another_future_value" {
		t.Errorf("second unknown state passthrough = %q", got)
	}
	if got := strings.Count(buf.String(), "another_future_value"); got != 1 {
		t.Errorf("expected per-state dedupe; got %d warnings for second state (output: %q)", got, buf.String())
	}
}

func TestFormatStatusKnownStatesDoNotWarn(t *testing.T) {
	resetUnknownStateWarnings(t)

	var buf bytes.Buffer
	prev := unknownStateWarnWriter
	unknownStateWarnWriter = &buf
	t.Cleanup(func() { unknownStateWarnWriter = prev })

	for _, state := range KnownStates {
		_ = FormatStatus(api.Domain{State: state})
	}

	if buf.Len() != 0 {
		t.Errorf("KnownStates should never warn; got: %q", buf.String())
	}
}

func TestStateConstantsMatchRoutedAndKnown(t *testing.T) {
	wantKnown := []string{
		StateAwaitingDNS,
		StateAwaitingEdge,
		StateAwaitingCert,
		StateServing,
		StateDegraded,
		StateTearingDown,
	}
	if len(KnownStates) != len(wantKnown) {
		t.Fatalf("KnownStates length = %d, want %d", len(KnownStates), len(wantKnown))
	}
	for i, s := range wantKnown {
		if KnownStates[i] != s {
			t.Errorf("KnownStates[%d] = %q, want %q", i, KnownStates[i], s)
		}
	}

	wantRouted := []string{StateAwaitingEdge, StateAwaitingCert, StateServing, StateDegraded}
	if len(RoutedStates) != len(wantRouted) {
		t.Fatalf("RoutedStates length = %d, want %d", len(RoutedStates), len(wantRouted))
	}
	for i, s := range wantRouted {
		if RoutedStates[i] != s {
			t.Errorf("RoutedStates[%d] = %q, want %q", i, RoutedStates[i], s)
		}
	}
}

func TestVerifyHappyPathCallsRecheckAndRendersStatus(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var recheckedWith string
	client := &mockClient{
		recheckDomainFn: func(d string) (*api.Domain, error) {
			recheckedWith = d
			return &api.Domain{
				Domain: "next.altstack.jp",
				Role:   "canonical",
				State:  StateAwaitingDNS,
				// An absent verdict is what makes this never-live, which is what
				// puts the nudge line in the output.
				DnsVerdict: "absent",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "0gbr-1.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			t.Fatal("verify must not call ListDomains")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Verify(client, "next.altstack.jp", false, &out); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if recheckedWith != "next.altstack.jp" {
		t.Errorf("RecheckDomain called with %q, want next.altstack.jp", recheckedWith)
	}
	s := out.String()
	for _, want := range []string{
		"next.altstack.jp",
		"Configure your DNS:",
		"0gbr-1.c2.kamakiri-pages.site.", // one trailing dot, neither stripped nor doubled
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// This call just asked for a fresh check, so the small ETA is truthful here.
	if !strings.Contains(s, "~60s") {
		t.Errorf("expected in-burst ~60s ETA, got:\n%s", s)
	}
	if strings.Contains(s, "sub-minute") {
		t.Errorf("must never say 'sub-minute':\n%s", s)
	}
	if strings.Contains(s, "kamakiri-pages.site..") {
		t.Errorf("FQDN double-dotted:\n%s", s)
	}
}

// The verify watch keeps asking for fresh checks until live, on its own floor
// rather than once per poll.
func TestVerifyWatchReissuesRecheckUntilLive(t *testing.T) {
	// A poll step under the re-issue floor, so re-issues land every third poll.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 10 * time.Second}
	rows := []api.Domain{
		cnameRowWrong("old.example.net."),
		cnameRowWrong("old.example.net."),
		cnameRowWrong("old.example.net."),
		cnameRowWrong("old.example.net."),
		cnameRowWrong("old.example.net."),
		cnameRowWrong("old.example.net."),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}
	idx, polls, rechecks := 0, 0, 0
	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			polls++
			row := rows[idx]
			if idx < len(rows)-1 {
				idx++
			}
			return &api.DomainList{Domains: []api.Domain{row}}, nil
		},
		recheckDomainFn: func(string) (*api.Domain, error) {
			rechecks++
			return &api.Domain{}, nil
		},
	}

	opts := testOpts(clk, context.Background())
	opts.reissue = func() { _, _ = client.RecheckDomain("next.altstack.jp") }

	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, opts); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	if rechecks == 0 {
		t.Fatal("the verify watch must re-issue the recheck")
	}
	// The six non-live polls span 60s at this step, so the 30s floor allows
	// exactly two re-issues; the live poll returns before its own check.
	// Without the floor the hook would fire six times, which is what makes this
	// count bite.
	const wantRechecks = 2
	if rechecks != wantRechecks {
		t.Errorf("re-issue must fire on the %ds floor, not every poll: rechecks=%d polls=%d want=%d",
			int(verifyReissueInterval/time.Second), rechecks, polls, wantRechecks)
	}
	if !strings.Contains(out.String(), "live: https://next.altstack.jp") {
		t.Errorf("watch must reach live:\n%s", out.String())
	}
}

// A rate-limited or failed re-issue must not fail the watch: the hook discards
// the error and the watch keeps polling to live.
func TestVerifyWatchSwallowsRecheckRateLimit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 40 * time.Second}
	rows := []api.Domain{
		cnameRowWrong("old.example.net."),
		cnameRowWrong("old.example.net."),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}
	idx := 0
	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			row := rows[idx]
			if idx < len(rows)-1 {
				idx++
			}
			return &api.DomainList{Domains: []api.Domain{row}}, nil
		},
		recheckDomainFn: func(string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "rate_limited", Message: "Please wait before re-checking."}
		},
	}

	opts := testOpts(clk, context.Background())
	reissues := 0
	opts.reissue = func() {
		reissues++
		_, _ = client.RecheckDomain("next.altstack.jp")
	}

	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, opts); err != nil {
		t.Fatalf("a rate-limited re-issue must not fail the watch; got %v", err)
	}
	if reissues == 0 {
		t.Fatal("expected at least one re-issue (each returning rate_limited)")
	}
	if !strings.Contains(out.String(), "live: https://next.altstack.jp") {
		t.Errorf("watch must still reach live despite rate-limited re-issues:\n%s", out.String())
	}
}

// Interactive verify asks for the fresh check first, then enters the watch with
// the project's site id and the domain.
func TestVerifyInteractiveEntersWatchAfterRecheck(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var recheckedWith string
	client := &mockClient{
		recheckDomainFn: func(d string) (*api.Domain, error) {
			recheckedWith = d
			return &api.Domain{
				Domain:     "next.altstack.jp",
				State:      StateAwaitingDNS,
				DnsVerdict: "present_but_wrong",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "h1lc.c2.kamakiri-pages.site."},
				},
			}, nil
		},
	}

	var gotSiteID, gotDomain string
	orig := watchVerifyFn
	watchVerifyFn = func(_ context.Context, _ APIClient, siteID, domainName string, _ io.Writer) error {
		gotSiteID, gotDomain = siteID, domainName
		return nil
	}
	t.Cleanup(func() { watchVerifyFn = orig })

	var out bytes.Buffer
	if err := verify(client, "next.altstack.jp", true, &out); err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if recheckedWith != "next.altstack.jp" {
		t.Errorf("interactive verify must POST the initial recheck; got %q", recheckedWith)
	}
	if gotSiteID != "site123" || gotDomain != "next.altstack.jp" {
		t.Errorf("watch entered with (siteID=%q, domain=%q), want (site123, next.altstack.jp)", gotSiteID, gotDomain)
	}
	got := out.String()
	if !strings.Contains(got, "Re-checking next.altstack.jp") {
		t.Errorf("interactive verify must print the re-checking intro:\n%s", got)
	}
	// The watch's waiting tick never names the target, so without the records up
	// front a blocking verify leaves the user nothing to act on.
	if !strings.Contains(got, "Configure your DNS:") {
		t.Errorf("interactive verify on a non-serving domain must render the DNS records block:\n%s", got)
	}
	if !strings.Contains(got, "h1lc.c2.kamakiri-pages.site.") {
		t.Errorf("the DNS records block must name the expected target:\n%s", got)
	}
}

func TestVerifyDomainNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		recheckDomainFn: func(string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_not_found", Message: "Domain not found."}
		},
		// No pending registration either, so verify falls through to the
		// not-found mapping rather than the registration branch.
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return &api.RegistrationList{}, nil
		},
	}

	var out bytes.Buffer
	err := Verify(client, "nope.example.com", false, &out)
	if err == nil {
		t.Fatal("Verify() expected error")
	}
	if !strings.Contains(err.Error(), "domain not found") {
		t.Errorf("error = %q, want 'domain not found'", err.Error())
	}
}

func TestVerifyRateLimited(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		recheckDomainFn: func(string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "rate_limited", Message: "Too many requests."}
		},
	}

	var out bytes.Buffer
	err := Verify(client, "next.altstack.jp", false, &out)
	if err == nil {
		t.Fatal("Verify() expected error")
	}
	if !strings.Contains(err.Error(), "checked very recently") ||
		!strings.Contains(err.Error(), "Automatic checks continue") {
		t.Errorf("rate-limited message not friendly: %q", err.Error())
	}
}

func TestVerifyTransportErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		recheckDomainFn: func(string) (*api.Domain, error) {
			return nil, errors.New("request failed: connection refused")
		},
	}

	var out bytes.Buffer
	err := Verify(client, "next.altstack.jp", false, &out)
	if err == nil {
		t.Fatal("Verify() expected error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("transport error not propagated: %q", err.Error())
	}
}

func TestVerifyNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{}
	var out bytes.Buffer
	err := Verify(client, "next.altstack.jp", false, &out)
	if err == nil {
		t.Fatal("Verify() expected error when no project")
	}
}

func TestVerifyNudgeInBurstListsVerifyFirstAndNoSubMinute(t *testing.T) {
	got := VerifyNudge("next.altstack.jp", true)
	verifyIdx := strings.Index(got, "domain verify")
	statusIdx := strings.Index(got, "kamakiri status")
	if verifyIdx == -1 {
		t.Fatalf("nudge missing 'domain verify': %q", got)
	}
	if statusIdx != -1 && verifyIdx > statusIdx {
		t.Errorf("'domain verify' must appear before 'kamakiri status': %q", got)
	}
	if !strings.Contains(got, "~60s") {
		t.Errorf("in-burst nudge should carry the cron-tick ETA: %q", got)
	}
	if strings.Contains(got, "sub-minute") {
		t.Errorf("nudge must never say 'sub-minute': %q", got)
	}
}

func TestVerifyNudgeOutOfBurstNoMisleadingSmallNumberDeliberateBackoff(t *testing.T) {
	got := VerifyNudge("next.altstack.jp", false)
	if !strings.Contains(got, "slowed down") {
		t.Errorf("out-of-burst nudge should use deliberate-backoff framing: %q", got)
	}
	if strings.Contains(got, "~60s") || strings.Contains(got, "~25s") {
		t.Errorf("out-of-burst nudge must NOT print a misleading small ETA: %q", got)
	}
	if !strings.Contains(got, "domain verify") {
		t.Errorf("out-of-burst nudge must still name 'domain verify': %q", got)
	}
}

func TestFormatStatusNeverLiveIsActionableNotAlarming(t *testing.T) {
	d := api.Domain{Domain: "next.altstack.jp", State: StateAwaitingDNS}
	got := FormatStatus(d)
	if strings.Contains(got, "DNS broken") {
		t.Errorf("never-live status must NOT be the alarming 'DNS broken': %q", got)
	}
	if !strings.Contains(got, "domain verify next.altstack.jp") {
		t.Errorf("never-live status must be actionable with 'domain verify <domain>': %q", got)
	}
	if !strings.Contains(got, "⧗") {
		t.Errorf("never-live status should use the waiting glyph ⧗: %q", got)
	}
}

func TestFormatStatusDegradedStillAlarming(t *testing.T) {
	// Degraded keeps its alarm marker while still naming the failing axis,
	// rather than falling back to a generic "DNS broken".
	d := api.Domain{
		Domain:       "x.example.com",
		State:        StateDegraded,
		CertRequired: true,
		DnsVerdict:   "absent",
		DnsCheckedAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
		CertVerdict:  CertVerdictCorrect,
	}
	got := FormatStatus(d)
	if !strings.Contains(got, "⚠") {
		t.Errorf("degraded must keep the ⚠ alarm marker: %q", got)
	}
	if !strings.Contains(got, "DNS record no longer visible") {
		t.Errorf("degraded with DNS regressed must name the failing axis: %q", got)
	}
}

func TestSetReplacesStaticWithinOneMinCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{
				Domain: "next.altstack.jp", Role: "canonical",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "t.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := Set(client, "next.altstack.jp", false, &out); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	s := out.String()
	if strings.Contains(s, "within ~1 min") {
		t.Errorf("Set must not keep the static 'within ~1 min' promise:\n%s", s)
	}
	if !strings.Contains(s, "domain verify next.altstack.jp") {
		t.Errorf("Set follow-up should point at `domain verify`:\n%s", s)
	}
}

// The status view's own fallback for a cert category this CLI does not know,
// which is a different line from the watch's.
func TestCertCategoryStatusLineUnknownCategoryFallsBack(t *testing.T) {
	if line := certCategoryStatusLine("a_category_from_a_newer_server"); line != i18n.T("domain.status_provisioning_cert") {
		t.Errorf("certCategoryStatusLine fallback = %q, want %q", line, i18n.T("domain.status_provisioning_cert"))
	}
}
