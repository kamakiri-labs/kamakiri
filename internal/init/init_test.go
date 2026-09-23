package initcmd

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

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: every line init prints renders from the
// message catalog. Load rather than Setup: nothing here reports which
// language is in force, only renders in it.
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
	createSiteFn func(subdomain string) (*api.Site, error)
	getSiteFn    func(id string) (*api.Site, error)
	getSiteCalls int
}

func (m *mockClient) CreateSite(subdomain string) (*api.Site, error) {
	return m.createSiteFn(subdomain)
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

func setupCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
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

// flunkGetSite fails the test if the site is read at all.
func flunkGetSite(t *testing.T) func(string) (*api.Site, error) {
	t.Helper()
	return func(_ string) (*api.Site, error) {
		t.Error("GetSite should not be called on this path")
		return nil, errors.New("flunk")
	}
}

func TestInitNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			t.Error("CreateSite should not be called")
			return nil, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

// The wait has to be started at the attempt id the creation returned, or a
// reconcile that ran before it would satisfy it. The script answers ok one
// below that id first and at it second, so the poll count is what proves which
// id was awaited.
func TestInitHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	client := &mockClient{
		createSiteFn: func(subdomain string) (*api.Site, error) {
			if subdomain != "my-site" {
				t.Errorf("subdomain = %q, want my-site", subdomain)
			}
			return &api.Site{
				ID:            "site123",
				Subdomain:     "my-site",
				URL:           "https://my-site.kamakiri-pages.jp",
				SyncAttemptID: 42,
			}, nil
		},
	}
	client.getSiteFn = func(id string) (*api.Site, error) {
		if id != "site123" {
			t.Errorf("GetSite id = %q, want site123", id)
		}
		attempt := int64(42)
		if client.getSiteCalls == 1 {
			attempt = 41
		}
		return &api.Site{
			ID: "site123", Subdomain: "my-site",
			Sync: &api.Sync{Outcome: "ok", LatestAttemptID: attempt},
		}, nil
	}

	stdin := strings.NewReader("my-site\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Creating site... done.") {
		t.Errorf("output missing 'Creating site... done.': %q", output)
	}
	if !strings.Contains(output, ".kamakiri/config.json") {
		t.Errorf("output missing config path: %q", output)
	}
	if client.getSiteCalls != 2 {
		t.Errorf("GetSite called %d times, want 2: the attempt at 41 must not satisfy a wait for 42",
			client.getSiteCalls)
	}

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil, want config")
	}
	if config.ID != "site123" {
		t.Errorf("config.ID = %q, want site123", config.ID)
	}
}

// --no-wait must not poll.
func TestInitNoWaitSkipsPolling(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{
				ID: "siteNW", Subdomain: "nowait", URL: "https://nowait.kamakiri-pages.jp",
				SyncAttemptID: 7,
			}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	stdin := strings.NewReader("nowait\n")
	var stdout bytes.Buffer

	err := Run(client, true, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "queued") {
		t.Errorf("output missing 'queued': %q", output)
	}
	if !strings.Contains(output, "Saved to .kamakiri/config.json") {
		t.Errorf("output missing 'Saved to .kamakiri/config.json': %q", output)
	}
	if client.getSiteCalls != 0 {
		t.Errorf("the site was read %d times on the --no-wait path, want 0", client.getSiteCalls)
	}

	config, err := core.LoadProject()
	if err != nil || config == nil || config.ID != "siteNW" {
		t.Errorf("config not written or wrong id: config=%v err=%v", config, err)
	}
}

// A failed reconcile propagates its reason, and still leaves the local config
// on disk so the user can retry or tear down.
func TestInitReconcileErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "siteErr", Subdomain: "errsite", SyncAttemptID: 11}, nil
		},
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "errsite",
				Sync: &api.Sync{
					Outcome:         "error",
					LatestAttemptID: 11,
					Error:           "caddy push failed: connection refused",
				},
			}, nil
		},
	}

	stdin := strings.NewReader("errsite\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected the reconcile failure, got nil")
	}
	if err.Error() != "reconcile failed: caddy push failed: connection refused" {
		t.Errorf("Run() err = %q, want the reconciler's own reason framed", err.Error())
	}

	output := stdout.String()
	// The reason belongs on stdout, where the person running init is looking;
	// the command wrapper repeats it on stderr for CI logs.
	if !strings.Contains(output, "Creating site... failed: reconcile failed: caddy push failed: connection refused") {
		t.Errorf("output missing 'Creating site... failed: <reason>': %q", output)
	}
	if !strings.Contains(output, "Saved to .kamakiri/config.json") {
		t.Errorf("output missing 'Saved to .kamakiri/config.json': %q", output)
	}

	config, err := core.LoadProject()
	if err != nil || config == nil || config.ID != "siteErr" {
		t.Errorf("config not written or wrong id: config=%v err=%v", config, err)
	}
}

// On Ctrl-C the sentinel must propagate, since the 130 exit depends on
// recognizing it, the stop line must reach stdout once, and the config line has
// to follow it: the site exists and the user is owed its handle.
func TestInitInterruptStopsAndKeepsConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)
	interruptBeforeWait(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "siteTO", Subdomain: "tosite", SyncAttemptID: 99}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	stdin := strings.NewReader("tosite\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if !errors.Is(err, syncwait.ErrInterrupted) {
		t.Fatalf("Run() err = %v, want syncwait.ErrInterrupted", err)
	}
	output := stdout.String()
	stop := syncwait.SharedStopLine()
	if got := strings.Count(output, stop); got != 1 {
		t.Errorf("stop line printed %d times, want exactly 1:\n%q", got, output)
	}
	if !strings.Contains(output, stop+"\n"+i18n.T("init.saved_config")+"\n") {
		t.Errorf("the config line must follow the stop line:\n%q", output)
	}

	config, err := core.LoadProject()
	if err != nil || config == nil || config.ID != "siteTO" {
		t.Errorf("config not written or wrong id: config=%v err=%v", config, err)
	}
}

// Losing the local config on a failed reconcile is the worst outcome there is:
// it leaves the user a server-side site they cannot address, and a re-run only
// collides with its own subdomain.
func TestInitConfigSavedEvenOnReconcileError(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "siteUX", Subdomain: "uxsite", SyncAttemptID: 1}, nil
		},
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id,
				Sync: &api.Sync{
					Outcome: "error", LatestAttemptID: 1, Error: "caddy unreachable",
				},
			}, nil
		},
	}

	stdin := strings.NewReader("uxsite\n")
	var stdout bytes.Buffer

	_ = Run(client, false, stdin, &stdout)

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil; the config file must exist after a failed reconcile")
	}
	if config.ID != "siteUX" {
		t.Errorf("config.ID = %q, want siteUX", config.ID)
	}
}

func TestInitAlreadyInitialized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	if err := os.MkdirAll(".kamakiri", 0755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "existing123"}); err != nil {
		t.Fatal(err)
	}

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			t.Error("CreateSite should not be called")
			return nil, nil
		},
		// Re-init looks the subdomain up by id, since the local config has only
		// the id.
		getSiteFn: func(id string) (*api.Site, error) {
			if id != "existing123" {
				t.Errorf("GetSite id = %q, want existing123", id)
			}
			return &api.Site{ID: "existing123", Subdomain: "my-site"}, nil
		},
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Already initialized: my-site.kamakiri-pages.jp") {
		t.Errorf("output missing friendly already-initialized message: %q", output)
	}
	if client.getSiteCalls != 1 {
		t.Errorf("the site was read %d times, want 1: the short-circuit runs no wait", client.getSiteCalls)
	}
}

func TestInitAlreadyInitializedFallsBackToIDWhenOffline(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	if err := os.MkdirAll(".kamakiri", 0755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "existing123"}); err != nil {
		t.Fatal(err)
	}

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			t.Error("CreateSite should not be called")
			return nil, nil
		},
		// Unreachable API: re-init still reports, falling back to the id.
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, errors.New("network unreachable")
		},
	}

	var stdout bytes.Buffer
	if err := Run(client, false, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Already initialized: existing123") {
		t.Errorf("offline re-init should fall back to the id: %q", stdout.String())
	}
}

func TestInitAlreadyInitializedFallsBackToIDWhenSubdomainEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	if err := os.MkdirAll(".kamakiri", 0755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "existing123"}); err != nil {
		t.Fatal(err)
	}

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			t.Error("CreateSite should not be called")
			return nil, nil
		},
		// An empty subdomain also falls back to the id, rather than print a
		// bare suffix with nothing in front of it.
		getSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "existing123", Subdomain: ""}, nil
		},
	}

	var stdout bytes.Buffer
	if err := Run(client, false, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Already initialized: existing123") {
		t.Errorf("empty-subdomain re-init should fall back to the id: %q", stdout.String())
	}
}

func TestInitSubdomainTakenReprompts(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	attempt := 0
	client := &mockClient{
		createSiteFn: func(subdomain string) (*api.Site, error) {
			attempt++
			if attempt == 1 {
				if subdomain != "taken" {
					t.Errorf("first subdomain = %q, want taken", subdomain)
				}
				return nil, &api.ErrorResponse{Code: "subdomain_taken", Message: "Subdomain is already taken."}
			}
			if subdomain != "available" {
				t.Errorf("second subdomain = %q, want available", subdomain)
			}
			return &api.Site{
				ID: "site456", Subdomain: "available",
				URL: "https://available.kamakiri-pages.jp", SyncAttemptID: 5,
			}, nil
		},
	}

	stdin := strings.NewReader("taken\navailable\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "already taken") {
		t.Errorf("output missing taken message: %q", output)
	}

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil, want config")
	}
	if config.ID != "site456" {
		t.Errorf("config.ID = %q, want site456", config.ID)
	}
	if attempt != 2 {
		t.Errorf("CreateSite called %d times, want 2", attempt)
	}
}

func TestInitSubdomainTakenOwnAccountAccept(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{
				Code:    "subdomain_taken_own_account",
				Message: "You already own this subdomain.",
				SiteID:  "existing789",
			}
		},
		// Adopting an existing site enqueues no reconcile, so there is nothing
		// for a wait to poll.
		getSiteFn: flunkGetSite(t),
	}

	stdin := strings.NewReader("my-blog\nY\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "already used by another site on your account") {
		t.Errorf("output missing adoption prompt: %q", output)
	}
	if !strings.Contains(output, ".kamakiri/config.json") {
		t.Errorf("output missing config path: %q", output)
	}
	if client.getSiteCalls != 0 {
		t.Errorf("the site was read %d times on the adoption path, want 0", client.getSiteCalls)
	}

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil, want config")
	}
	if config.ID != "existing789" {
		t.Errorf("config.ID = %q, want existing789", config.ID)
	}
}

func TestInitSubdomainTakenOwnAccountReject(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	attempt := 0
	client := &mockClient{
		createSiteFn: func(subdomain string) (*api.Site, error) {
			attempt++
			if attempt == 1 {
				return nil, &api.ErrorResponse{
					Code:    "subdomain_taken_own_account",
					Message: "You already own this subdomain.",
					SiteID:  "existing789",
				}
			}
			return &api.Site{
				ID: "newsite", Subdomain: subdomain,
				URL: "https://" + subdomain + ".kamakiri-pages.jp", SyncAttemptID: 13,
			}, nil
		},
	}

	stdin := strings.NewReader("my-blog\nn\nother-site\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil, want config")
	}
	if config.ID != "newsite" {
		t.Errorf("config.ID = %q, want newsite", config.ID)
	}

	if attempt != 2 {
		t.Errorf("CreateSite called %d times, want 2", attempt)
	}
}

func TestInitInvalidSubdomainReprompts(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	attempt := 0
	client := &mockClient{
		createSiteFn: func(subdomain string) (*api.Site, error) {
			attempt++
			if attempt == 1 {
				if subdomain != "BAD!" {
					t.Errorf("first subdomain = %q, want BAD!", subdomain)
				}
				return nil, &api.ErrorResponse{Code: "invalid_subdomain", Message: "Invalid subdomain."}
			}
			return &api.Site{
				ID: "site789", Subdomain: subdomain,
				URL: "https://" + subdomain + ".kamakiri-pages.jp", SyncAttemptID: 21,
			}, nil
		},
	}

	stdin := strings.NewReader("BAD!\ngood-name\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Invalid subdomain") {
		t.Errorf("output missing invalid subdomain message: %q", output)
	}

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil, want config")
	}
	if config.ID != "site789" {
		t.Errorf("config.ID = %q, want site789", config.ID)
	}
	if attempt != 2 {
		t.Errorf("CreateSite called %d times, want 2", attempt)
	}
}

func TestInitReservedSubdomainReprompts(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	fastPolls(t)

	attempt := 0
	client := &mockClient{
		createSiteFn: func(subdomain string) (*api.Site, error) {
			attempt++
			if attempt == 1 {
				if subdomain != "admin" {
					t.Errorf("first subdomain = %q, want admin", subdomain)
				}
				return nil, &api.ErrorResponse{Code: "subdomain_reserved", Message: "That subdomain is reserved."}
			}
			return &api.Site{
				ID: "site789", Subdomain: subdomain,
				URL: "https://" + subdomain + ".kamakiri-pages.jp", SyncAttemptID: 21,
			}, nil
		},
	}

	stdin := strings.NewReader("admin\ngood-name\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "That subdomain is reserved. Choose another.") {
		t.Errorf("output missing reserved subdomain message: %q", output)
	}

	config, err := core.LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config == nil {
		t.Fatal("LoadProject() returned nil, want config")
	}
	if config.ID != "site789" {
		t.Errorf("config.ID = %q, want site789", config.ID)
	}
	if attempt != 2 {
		t.Errorf("CreateSite called %d times, want 2", attempt)
	}
}

func TestInitUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	stdin := strings.NewReader("my-site\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

func TestInitAdoptionEmptySiteID(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{
				Code:    "subdomain_taken_own_account",
				Message: "You already own this subdomain.",
				SiteID:  "",
			}
		},
	}

	stdin := strings.NewReader("my-blog\ny\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "server returned empty site ID") {
		t.Errorf("error = %q, want 'server returned empty site ID'", err.Error())
	}
}
