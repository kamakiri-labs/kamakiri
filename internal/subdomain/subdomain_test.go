package subdomain

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/syncwait"
)

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under: the API error copy they match on renders from the
// message catalog. Load rather than Setup: nothing here reports which language
// is in force, only renders in it.
//
// KAMAKIRI_API_KEY is cleared for a reason of its own: it outranks the
// credentials file, so on a machine that exports it a test that writes a key
// into its own config home would still load the developer's. There is no
// *testing.T here, hence os.Unsetenv rather than t.Setenv.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Unsetenv(core.EnvAPIKey)
	os.Exit(m.Run())
}

// resolvedAttemptID sits above every attempt id these tests await, so the
// default site below resolves whichever wait reaches it.
const resolvedAttemptID = 1000

// mockClient answers GetSite from getSiteFn where a test scripts one and from a
// site whose reconcile has already succeeded otherwise. The wait has no bound,
// so a default that never resolved would hang a test rather than fail it.
type mockClient struct {
	getSiteFn          func(id string) (*api.Site, error)
	updateSubdomainFn  func(siteID, subdomain string) (*api.Site, error)
	disableSubdomainFn func(siteID string) (*api.Site, error)
	enableSubdomainFn  func(siteID string) (*api.Site, error)
	getSiteCalls       int
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	m.getSiteCalls++
	if m.getSiteFn == nil {
		return &api.Site{
			ID:        id,
			Subdomain: "my-site",
			Sync:      &api.Sync{Outcome: "ok", LatestAttemptID: resolvedAttemptID},
		}, nil
	}
	return m.getSiteFn(id)
}

// resolveAt scripts GetSite to answer ok one attempt below want first and at
// want second. A wait started at any lower id would resolve on the first
// answer, so the call count is what proves which id was awaited. The site
// handed back is what the command renders its result line from.
func (m *mockClient) resolveAt(want int64, site api.Site) func(string) (*api.Site, error) {
	return func(string) (*api.Site, error) {
		attempt := want
		if m.getSiteCalls == 1 {
			attempt = want - 1
		}
		resolved := site
		resolved.Sync = &api.Sync{Outcome: "ok", LatestAttemptID: attempt}
		return &resolved, nil
	}
}

// reconcileFailed scripts GetSite to answer with a failed attempt carrying the
// reconciler's own reason.
func reconcileFailed(reason string) func(string) (*api.Site, error) {
	return func(id string) (*api.Site, error) {
		return &api.Site{
			ID:   id,
			Sync: &api.Sync{Outcome: "error", LatestAttemptID: resolvedAttemptID, Error: reason},
		}, nil
	}
}

// flunkGetSite fails the test if the site is read at all.
func flunkGetSite(t *testing.T) func(string) (*api.Site, error) {
	t.Helper()
	return func(_ string) (*api.Site, error) {
		t.Error("GetSite should not be called on this path")
		return nil, errors.New("flunk")
	}
}

// fastPolls runs the wait on a millisecond cadence for one test, so a scripted
// sequence of answers plays out without waiting out the production bands.
func fastPolls(t *testing.T) {
	t.Helper()
	prev := pollSchedule
	t.Cleanup(func() { pollSchedule = prev })
	pollSchedule = func(time.Duration) time.Duration { return time.Millisecond }
}

// interruptBeforeWait hands the wait a context that is already cancelled, which
// is how Ctrl-C reaches it without a real signal.
func interruptBeforeWait(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prev := signalCtx
	t.Cleanup(func() { signalCtx = prev })
	signalCtx = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
}

func (m *mockClient) UpdateSubdomain(siteID, subdomain string) (*api.Site, error) {
	return m.updateSubdomainFn(siteID, subdomain)
}

func (m *mockClient) DisableSubdomain(siteID string) (*api.Site, error) {
	return m.disableSubdomainFn(siteID)
}

func (m *mockClient) EnableSubdomain(siteID string) (*api.Site, error) {
	return m.enableSubdomainFn(siteID)
}

func ptr[T any](v T) *T { return &v }

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

func TestGetNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			t.Error("GetSite should not be called")
			return nil, nil
		},
	}

	var stdout bytes.Buffer
	err := Get(client, &stdout)
	if err == nil {
		t.Fatal("Get() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

func TestSetNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			t.Error("UpdateSubdomain should not be called")
			return nil, nil
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "anything", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

func TestGetHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			if id != "site123" {
				t.Errorf("id = %q, want site123", id)
			}
			return &api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true, URL: "https://my-site.kamakiri-pages.jp"}, nil
		},
	}

	var stdout bytes.Buffer
	err := Get(client, &stdout)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	output := stdout.String()
	if output != "my-site.kamakiri-pages.jp\n" {
		t.Errorf("output = %q, want %q", output, "my-site.kamakiri-pages.jp\n")
	}
}

func TestGetNoConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			t.Error("GetSite should not be called")
			return nil, nil
		},
	}

	var stdout bytes.Buffer
	err := Get(client, &stdout)
	if err == nil {
		t.Fatal("Get() expected error, got nil")
	}
}

func TestGetSiteNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "missing-site")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
	}

	var stdout bytes.Buffer
	err := Get(client, &stdout)
	if err == nil {
		t.Fatal("Get() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "site not found") {
		t.Errorf("error = %q, want 'site not found'", err.Error())
	}
}

func TestGetUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Get(client, &stdout)
	if err == nil {
		t.Fatal("Get() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

func TestSetHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		updateSubdomainFn: func(siteID, subdomain string) (*api.Site, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q, want site123", siteID)
			}
			if subdomain != "new-name" {
				t.Errorf("subdomain = %q, want new-name", subdomain)
			}
			return &api.Site{ID: "site123", Subdomain: "new-name", URL: "https://new-name.kamakiri-pages.jp", SyncAttemptID: 42}, nil
		},
	}
	client.getSiteFn = client.resolveAt(42, api.Site{
		ID: "site123", Subdomain: "new-name", URL: "https://new-name.kamakiri-pages.jp",
	})

	var stdout bytes.Buffer
	err := Set(client, "new-name", false, &stdout)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	if client.getSiteCalls != 2 {
		t.Errorf("GetSite called %d times, want 2: the attempt at 41 must not satisfy a wait for 42",
			client.getSiteCalls)
	}
	output := stdout.String()
	if output != "Renamed: new-name.kamakiri-pages.jp\n" {
		t.Errorf("output = %q, want %q", output, "Renamed: new-name.kamakiri-pages.jp\n")
	}
}

func TestSetNoWaitSkipsPolling(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "new", SyncAttemptID: 7}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Set(client, "new", true, &stdout)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "queued") {
		t.Errorf("output = %q, want 'queued'", stdout.String())
	}
	if client.getSiteCalls != 0 {
		t.Errorf("the site was read %d times on the --no-wait path, want 0", client.getSiteCalls)
	}
}

func TestSetReconcileErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "x", SyncAttemptID: 1}, nil
		},
		getSiteFn: reconcileFailed("caddy push failed: connection refused"),
	}

	var stdout bytes.Buffer
	err := Set(client, "x", false, &stdout)
	if err == nil {
		t.Fatal("expected the reconcile failure to propagate")
	}
	if err.Error() != "reconcile failed: caddy push failed: connection refused" {
		t.Errorf("error = %q, want the reconciler's own reason framed", err.Error())
	}
	if strings.Contains(stdout.String(), "Renamed") {
		t.Errorf("stdout must not claim success on failure: %q", stdout.String())
	}
}

// On Ctrl-C the sentinel propagates, since the 130 exit depends on recognizing
// it, and the stop line lands on stdout once with no result line beside it.
func TestSetInterruptStopsAndPropagatesSentinel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)
	interruptBeforeWait(t)

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "x", SyncAttemptID: 1}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Set(client, "x", false, &stdout)
	if !errors.Is(err, syncwait.ErrInterrupted) {
		t.Fatalf("expected syncwait.ErrInterrupted, got %v", err)
	}
	if stdout.String() != syncwait.SharedStopLine()+"\n" {
		t.Errorf("output = %q, want the stop line alone", stdout.String())
	}
}

func TestSetSubdomainTaken(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "subdomain_taken", Message: "Subdomain is already taken."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "taken", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "subdomain is already taken") {
		t.Errorf("error = %q, want 'subdomain is already taken'", err.Error())
	}
}

func TestSetSubdomainReserved(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "subdomain_reserved", Message: "That subdomain is reserved."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "admin", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "that subdomain is reserved. Choose another") {
		t.Errorf("error = %q, want 'that subdomain is reserved. Choose another'", err.Error())
	}
}

func TestSetUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "new-name", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

func TestSetNoConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		updateSubdomainFn: func(_, _ string) (*api.Site, error) {
			t.Error("UpdateSubdomain should not be called")
			return nil, nil
		},
	}

	var stdout bytes.Buffer
	err := Set(client, "anything", false, &stdout)
	if err == nil {
		t.Fatal("Set() expected error, got nil")
	}
}

func TestGetObservedNilNoArrow(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{
				ID:                "site123",
				Subdomain:         "fresh-site",
				SubdomainEnabled:  true,
				SubdomainObserved: nil,
			}, nil
		},
	}

	var stdout bytes.Buffer
	if err := Get(client, &stdout); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if strings.Contains(stdout.String(), "→") {
		t.Errorf("output must not contain arrow when observed is nil; got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "fresh-site.kamakiri-pages.jp") {
		t.Errorf("output = %q, want plain subdomain", stdout.String())
	}
}

func TestGetObservedDifferentShowsArrow(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{
				ID:                "site123",
				Subdomain:         "newname",
				SubdomainEnabled:  true,
				SubdomainObserved: ptr("oldname"),
			}, nil
		},
	}

	var stdout bytes.Buffer
	if err := Get(client, &stdout); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "newname → oldname.kamakiri-pages.jp") {
		t.Errorf("output = %q, want desired → observed.<pagesDomain>", out)
	}
	if !strings.Contains(out, "⧗ syncing") {
		t.Errorf("output = %q, want ⧗ syncing", out)
	}
}

func TestGetObservedSameNoArrow(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{
				ID:                "site123",
				Subdomain:         "stable",
				SubdomainEnabled:  true,
				SubdomainObserved: ptr("stable"),
			}, nil
		},
	}

	var stdout bytes.Buffer
	if err := Get(client, &stdout); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if strings.Contains(stdout.String(), "→") {
		t.Errorf("output must not contain arrow when desired == observed; got %q", stdout.String())
	}
}

func TestGetDisabledState(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: false}, nil
		},
	}

	var stdout bytes.Buffer
	err := Get(client, &stdout)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "(disabled)") {
		t.Errorf("output = %q, want '(disabled)'", output)
	}
	if !strings.Contains(output, "my-site") {
		t.Errorf("output missing subdomain: %q", output)
	}
}

func TestDisableHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		disableSubdomainFn: func(siteID string) (*api.Site, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q", siteID)
			}
			return &api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: false, SyncAttemptID: 42}, nil
		},
	}
	client.getSiteFn = client.resolveAt(42, api.Site{
		ID: "site123", Subdomain: "my-site", SubdomainEnabled: false,
	})

	var stdout bytes.Buffer
	err := Disable(client, false, &stdout)
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if client.getSiteCalls != 2 {
		t.Errorf("GetSite called %d times, want 2: the attempt at 41 must not satisfy a wait for 42",
			client.getSiteCalls)
	}
	want := "Subdomain disabled: my-site.kamakiri-pages.jp is no longer serving.\n" +
		"The subdomain is still reserved. Use \"kamakiri subdomain enable\" to re-enable.\n"
	if stdout.String() != want {
		t.Errorf("output = %q, want %q", stdout.String(), want)
	}
}

func TestDisableNoWaitSkipsPolling(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		disableSubdomainFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "my-site", SyncAttemptID: 7}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Disable(client, true, &stdout)
	if err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "queued") {
		t.Errorf("output = %q, want 'queued'", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Disabling") {
		t.Errorf("output = %q, want 'Disabling'", stdout.String())
	}
	if client.getSiteCalls != 0 {
		t.Errorf("the site was read %d times on the --no-wait path, want 0", client.getSiteCalls)
	}
}

func TestDisableReconcileErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		disableSubdomainFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "x", SyncAttemptID: 1}, nil
		},
		getSiteFn: reconcileFailed("caddy push timed out"),
	}

	var stdout bytes.Buffer
	err := Disable(client, false, &stdout)
	if err == nil {
		t.Fatal("expected the reconcile failure to propagate")
	}
	if err.Error() != "reconcile failed: caddy push timed out" {
		t.Errorf("error = %q, want the reconciler's own reason framed", err.Error())
	}
	if strings.Contains(stdout.String(), "Subdomain disabled") {
		t.Errorf("stdout must not claim success on failure: %q", stdout.String())
	}
}

func TestDisableInterruptStopsAndPropagatesSentinel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)
	interruptBeforeWait(t)

	client := &mockClient{
		disableSubdomainFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "x", SyncAttemptID: 1}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Disable(client, false, &stdout)
	if !errors.Is(err, syncwait.ErrInterrupted) {
		t.Fatalf("expected syncwait.ErrInterrupted, got %v", err)
	}
	if stdout.String() != syncwait.SharedStopLine()+"\n" {
		t.Errorf("output = %q, want the stop line alone", stdout.String())
	}
}

func TestDisableNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{
		disableSubdomainFn: func(_ string) (*api.Site, error) {
			t.Error("DisableSubdomain should not be called")
			return nil, nil
		},
	}
	var stdout bytes.Buffer
	err := Disable(client, false, &stdout)
	if err == nil {
		t.Fatal("Disable() expected error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestDisableUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		disableSubdomainFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Disable(client, false, &stdout)
	if err == nil {
		t.Fatal("Disable() expected error")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestEnableHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		enableSubdomainFn: func(siteID string) (*api.Site, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q", siteID)
			}
			return &api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true, SyncAttemptID: 99}, nil
		},
	}
	client.getSiteFn = client.resolveAt(99, api.Site{
		ID: "site123", Subdomain: "my-site", SubdomainEnabled: true,
	})

	var stdout bytes.Buffer
	err := Enable(client, false, &stdout)
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if client.getSiteCalls != 2 {
		t.Errorf("GetSite called %d times, want 2: the attempt at 98 must not satisfy a wait for 99",
			client.getSiteCalls)
	}
	want := "Subdomain enabled: my-site.kamakiri-pages.jp\n"
	if stdout.String() != want {
		t.Errorf("output = %q, want %q", stdout.String(), want)
	}
}

func TestEnableNoWaitSkipsPolling(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		enableSubdomainFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "my-site", SyncAttemptID: 8}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Enable(client, true, &stdout)
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "queued") {
		t.Errorf("output = %q, want 'queued'", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Enabling") {
		t.Errorf("output = %q, want 'Enabling'", stdout.String())
	}
	if client.getSiteCalls != 0 {
		t.Errorf("the site was read %d times on the --no-wait path, want 0", client.getSiteCalls)
	}
}

func TestEnableReconcileErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		enableSubdomainFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "x", SyncAttemptID: 1}, nil
		},
		getSiteFn: reconcileFailed("caddy push timed out"),
	}

	var stdout bytes.Buffer
	err := Enable(client, false, &stdout)
	if err == nil {
		t.Fatal("expected the reconcile failure to propagate")
	}
	if err.Error() != "reconcile failed: caddy push timed out" {
		t.Errorf("error = %q, want the reconciler's own reason framed", err.Error())
	}
	if strings.Contains(stdout.String(), "Subdomain enabled") {
		t.Errorf("stdout must not claim success on failure: %q", stdout.String())
	}
}

func TestEnableInterruptStopsAndPropagatesSentinel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)
	interruptBeforeWait(t)

	client := &mockClient{
		enableSubdomainFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "site123", Subdomain: "x", SyncAttemptID: 1}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Enable(client, false, &stdout)
	if !errors.Is(err, syncwait.ErrInterrupted) {
		t.Fatalf("expected syncwait.ErrInterrupted, got %v", err)
	}
	if stdout.String() != syncwait.SharedStopLine()+"\n" {
		t.Errorf("output = %q, want the stop line alone", stdout.String())
	}
}

func TestEnableNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupProjectConfig(t, "site123")

	client := &mockClient{
		enableSubdomainFn: func(_ string) (*api.Site, error) {
			t.Error("EnableSubdomain should not be called")
			return nil, nil
		},
	}
	var stdout bytes.Buffer
	err := Enable(client, false, &stdout)
	if err == nil {
		t.Fatal("Enable() expected error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestEnableUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		enableSubdomainFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var stdout bytes.Buffer
	err := Enable(client, false, &stdout)
	if err == nil {
		t.Fatal("Enable() expected error")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q", err.Error())
	}
}
