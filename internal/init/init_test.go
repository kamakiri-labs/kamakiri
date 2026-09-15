package initcmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
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

// mockClient defaults waitForSyncFn to a flunk, so a path that regresses into
// polling fails loudly rather than quietly.
type mockClient struct {
	createSiteFn   func(subdomain string) (*api.Site, error)
	getSiteFn      func(id string) (*api.Site, error)
	waitForSyncFn  func(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
	waitForSyncHit bool
}

func (m *mockClient) CreateSite(subdomain string) (*api.Site, error) {
	return m.createSiteFn(subdomain)
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	if m.getSiteFn == nil {
		return nil, errors.New("GetSite: no mock configured")
	}
	return m.getSiteFn(id)
}

func (m *mockClient) WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error) {
	m.waitForSyncHit = true
	if m.waitForSyncFn == nil {
		return nil, errors.New("WaitForSync: no mock configured")
	}
	return m.waitForSyncFn(siteID, sinceAttemptID, timeout)
}

func setupCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}

// flunkWait fails the test if WaitForSync is called at all.
func flunkWait(t *testing.T) func(string, int64, time.Duration) (*api.Site, error) {
	t.Helper()
	return func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
		t.Error("WaitForSync should not be called on this path")
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
		waitForSyncFn: flunkWait(t),
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

func TestInitHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

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
		waitForSyncFn: func(siteID string, sinceAttemptID int64, _ time.Duration) (*api.Site, error) {
			if siteID != "site123" {
				t.Errorf("WaitForSync siteID = %q, want site123", siteID)
			}
			if sinceAttemptID != 42 {
				t.Errorf("WaitForSync sinceAttemptID = %d, want 42 (matches 202 sync_attempt_id)", sinceAttemptID)
			}
			return &api.Site{
				ID: "site123", Subdomain: "my-site",
				Sync: &api.Sync{Outcome: "ok", LatestAttemptID: 42},
			}, nil
		},
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
	if !client.waitForSyncHit {
		t.Error("WaitForSync was not called on the sync-default path")
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
		waitForSyncFn: flunkWait(t),
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
	if client.waitForSyncHit {
		t.Error("WaitForSync was called on the --no-wait path")
	}

	config, err := core.LoadProject()
	if err != nil || config == nil || config.ID != "siteNW" {
		t.Errorf("config not written or wrong id: config=%v err=%v", config, err)
	}
}

// A reconcile failure other than a timeout propagates, and still leaves the
// local config on disk so the user can retry or tear down.
func TestInitWaitForSyncErrorPropagates(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	want := errors.New("reconcile failed: caddy push failed: connection refused")
	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "siteErr", Subdomain: "errsite", SyncAttemptID: 11}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, want
		},
	}

	stdin := strings.NewReader("errsite\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if !errors.Is(err, want) {
		t.Fatalf("Run() err = %v, want %v", err, want)
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

// On timeout the sentinel must propagate, since suppressing the duplicate
// stderr echo depends on recognizing it, and the "still syncing" hint must
// reach stdout.
func TestInitTimeoutPrintsHintAndPropagatesSentinel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "siteTO", Subdomain: "tosite", SyncAttemptID: 99}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}

	stdin := strings.NewReader("tosite\n")
	var stdout bytes.Buffer

	err := Run(client, false, stdin, &stdout)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("Run() err = %v, want ErrSyncTimeout", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "still syncing") {
		t.Errorf("output missing 'still syncing': %q", output)
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

	client := &mockClient{
		createSiteFn: func(_ string) (*api.Site, error) {
			return &api.Site{ID: "siteUX", Subdomain: "uxsite", SyncAttemptID: 1}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, errors.New("reconcile failed: caddy unreachable")
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
		waitForSyncFn: flunkWait(t),
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
	if client.waitForSyncHit {
		t.Error("WaitForSync called on already-initialized short-circuit")
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
		waitForSyncFn: flunkWait(t),
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
		waitForSyncFn: flunkWait(t),
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
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{Sync: &api.Sync{Outcome: "ok"}}, nil
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
		waitForSyncFn: flunkWait(t),
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
	if client.waitForSyncHit {
		t.Error("WaitForSync called on the adoption path, which is sync-only")
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
		// The reprompt after a rejected answer ends in a real creation, so one
		// wait is expected here.
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{Sync: &api.Sync{Outcome: "ok"}}, nil
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
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{Sync: &api.Sync{Outcome: "ok"}}, nil
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
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{Sync: &api.Sync{Outcome: "ok"}}, nil
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
		waitForSyncFn: flunkWait(t),
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
		waitForSyncFn: flunkWait(t),
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
