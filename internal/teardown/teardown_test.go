package teardown

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
// locale the suite runs under: every line teardown prints renders from the
// message catalog. Load rather than Setup: nothing here reports which
// language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

type mockClient struct {
	getSiteFn     func(id string) (*api.Site, error)
	deleteSiteFn  func(id string) (*api.Site, error)
	waitForSyncFn func(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	return m.getSiteFn(id)
}

func (m *mockClient) DeleteSite(id string) (*api.Site, error) {
	return m.deleteSiteFn(id)
}

func (m *mockClient) WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error) {
	return m.waitForSyncFn(siteID, sinceAttemptID, timeout)
}

func setupCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}

func setupConfig(t *testing.T, id string) {
	t.Helper()
	if err := os.MkdirAll(".kamakiri", 0755); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			t.Error("GetSite should not be called")
			return nil, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

// The happy path: the wait resolves, and the local config goes with it.
func TestTeardownHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", URL: "https://my-site.kamakiri-pages.jp"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			if id != "site123" {
				t.Errorf("id = %q, want site123", id)
			}
			return &api.Site{
				ID:             id,
				Subdomain:      "my-site",
				Status:         "deleted",
				StatusObserved: "active",
				SyncAttemptID:  42,
			}, nil
		},
		waitForSyncFn: func(siteID string, sinceAttemptID int64, _ time.Duration) (*api.Site, error) {
			if siteID != "site123" {
				t.Errorf("WaitForSync siteID = %q, want site123", siteID)
			}
			if sinceAttemptID != 42 {
				t.Errorf("WaitForSync sinceAttemptID = %d, want 42", sinceAttemptID)
			}
			return &api.Site{
				ID:             siteID,
				Status:         "deleted",
				StatusObserved: "deleted",
				Sync: &api.Sync{
					LatestAttemptID: 42,
					Outcome:         "ok",
				},
			}, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Tearing down... done.") {
		t.Errorf("output missing confirmation: %q", output)
	}
	if !strings.Contains(output, "Removed .kamakiri/config.json") {
		t.Errorf("output missing config removal: %q", output)
	}
	if strings.Contains(output, "Sakura") {
		t.Errorf("non-WebAccel teardown must not print a WebAccel notice: %q", output)
	}

	config, _ := core.LoadProject()
	if config != nil {
		t.Error("config should be removed after teardown")
	}
}

// Every failure label ends in "Check `kamakiri status`", a command the same
// floor refuses, so a refusal only closes the open line and lets the sentinel
// carry the message. The rest of the error arm is unchanged: the retained
// resource is still named and the local config still goes.
func TestTeardownAbortsOnAVersionRefusal(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:            id,
				SyncAttemptID: 42,
				RetainedWebAccelResources: []api.WebAccelResource{
					{ID: "1001", Subdomain: "abc123.user.webaccel.ne.jp"},
				},
			}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrUpgradeRequired
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer
	err := Run(client, stdin, &stdout, false)
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}

	output := stdout.String()
	for _, banned := range []string{
		i18n.T("teardown.failed"),
		i18n.T("teardown.failed_edge_delete"),
		i18n.T("teardown.done"),
		i18n.T("teardown.still_syncing"),
	} {
		if strings.Contains(output, banned) {
			t.Errorf("output must not carry %q:\n%s", banned, output)
		}
	}
	// The notice and the config line below end in newlines of their own, so the
	// newline that has to be pinned is the one closing the tearing-down line:
	// writing nothing at all would run the notice onto the end of it.
	if !strings.Contains(output, i18n.T("teardown.tearing_down")+"\n") {
		t.Errorf("the tearing-down line must be closed before the upgrade message:\n%q", output)
	}
	if !strings.Contains(output, "abc123.user.webaccel.ne.jp") {
		t.Errorf("the retained WebAccel notice must still print:\n%s", output)
	}
	if !strings.Contains(output, i18n.T("teardown.removed_config")) {
		t.Errorf("the local config must still be removed:\n%s", output)
	}
	if config, _ := core.LoadProject(); config != nil {
		t.Error("config should be removed even when the wait was refused")
	}
}

// A WebAccel resource survives teardown, so the notice must name it by its
// delivery hostname and point at the panel where the user can remove it.
func TestTeardownWebAccelNoticeNamesResource(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:            id,
				SyncAttemptID: 42,
				RetainedWebAccelResources: []api.WebAccelResource{
					{ID: "1001", Subdomain: "abc123.user.webaccel.ne.jp"},
				},
			}, nil
		},
		waitForSyncFn: func(siteID string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{
				ID:             siteID,
				StatusObserved: "deleted",
				Sync:           &api.Sync{LatestAttemptID: 42, Outcome: "ok"},
			}, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer
	if err := Run(client, stdin, &stdout, false); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "abc123.user.webaccel.ne.jp") {
		t.Errorf("output should name the retained WebAccel resource by hostname: %q", output)
	}
	if !strings.Contains(output, "Sakura control panel") {
		t.Errorf("output should point at the Sakura control panel: %q", output)
	}
	if !strings.Contains(output, "stays in your Sakura account") {
		t.Errorf("output should use singular phrasing for one resource: %q", output)
	}
}

// A resource whose delivery hostname was never captured is labeled by its id,
// and --no-wait surfaces it just the same.
func TestTeardownWebAccelNoticeNoWaitFallbackToID(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:            id,
				SyncAttemptID: 42,
				RetainedWebAccelResources: []api.WebAccelResource{
					{ID: "1001"},
					{ID: "1002", Subdomain: "two.user.webaccel.ne.jp"},
				},
			}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync must not be called when --no-wait is set")
			return nil, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer
	if err := Run(client, stdin, &stdout, true); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "1001") {
		t.Errorf("output should fall back to the resource id when no subdomain: %q", output)
	}
	if !strings.Contains(output, "two.user.webaccel.ne.jp") {
		t.Errorf("output should name a resource that has a subdomain by hostname: %q", output)
	}
	if !strings.Contains(output, "stay in your Sakura account") {
		t.Errorf("output should use plural phrasing for multiple resources: %q", output)
	}
}

// --no-wait must not poll, and still removes the local config: what it points
// at is being deleted either way.
func TestTeardownNoWait(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, SyncAttemptID: 42}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync must not be called when --no-wait is set")
			return nil, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	start := time.Now()
	err := Run(client, stdin, &stdout, true)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("--no-wait took %v; expected <2s", elapsed)
	}

	output := stdout.String()
	if !strings.Contains(output, "queued") {
		t.Errorf("output should mention 'queued': %q", output)
	}
	if !strings.Contains(output, "Removed .kamakiri/config.json") {
		t.Errorf("output should remove config: %q", output)
	}
}

// A timeout prints the "still syncing" hint and propagates the sentinel, so the
// caller can tell it apart from a real failure.
func TestTeardownWaitTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, SyncAttemptID: 42}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("Run() error = %v, want ErrSyncTimeout", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "still syncing") {
		t.Errorf("output should print 'still syncing' hint: %q", output)
	}
	// A config left behind here would point a later deploy at a site that is
	// being torn down.
	if !strings.Contains(output, "Removed .kamakiri/config.json") {
		t.Errorf("output should remove config on timeout: %q", output)
	}
	if config, _ := core.LoadProject(); config != nil {
		t.Error("config should be removed after timeout")
	}
}

// A real reconcile failure prints a step-specific label, propagates, and still
// removes the local config.
func TestTeardownReconcileError(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	reconcileErr := errors.New("reconcile failed: caddy push timed out")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, SyncAttemptID: 42}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			// No changes at all: nothing to name, so the label stays generic.
			return &api.Site{
				ID: "site123",
				Sync: &api.Sync{
					LatestAttemptID: 42,
					Outcome:         "error",
					Changes:         nil,
				},
			}, reconcileErr
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reconcile failed") {
		t.Errorf("err = %v, want propagated reconcile error", err)
	}
	if errors.Is(err, api.ErrSyncTimeout) {
		t.Errorf("err = %v, want NOT ErrSyncTimeout", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "failed") {
		t.Errorf("output should print failure label: %q", output)
	}
	if !strings.Contains(output, "kamakiri status") {
		t.Errorf("output should hint at `kamakiri status`: %q", output)
	}
	if !strings.Contains(output, "Removed .kamakiri/config.json") {
		t.Errorf("output should remove config on reconcile error: %q", output)
	}
}

// The first missing step in the reported changes is the one named.
func TestTeardownPerStepFailureLabel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	reconcileErr := errors.New("reconcile failed: cloudflare 500")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, SyncAttemptID: 42}, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{
				ID: "site123",
				Sync: &api.Sync{
					LatestAttemptID: 42,
					Outcome:         "error",
					Changes:         []string{"caddy_delete", "dns_delete:example.com"},
				},
			}, reconcileErr
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	_ = Run(client, stdin, &stdout, false)

	output := stdout.String()
	if !strings.Contains(output, "CDN deregister") {
		t.Errorf("output should name the failing step (CDN deregister): %q", output)
	}
}

func TestTeardownAbort(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("n\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Aborted") {
		t.Error("output missing aborted message")
	}

	config, _ := core.LoadProject()
	if config == nil {
		t.Error("config should not be removed on abort")
	}
}

func TestTeardownDefaultNo(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Aborted") {
		t.Error("empty input should default to abort")
	}
}

func TestTeardownNoConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			t.Error("GetSite should not be called")
			return nil, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("expected error for no config")
	}
	// The no-site error has to carry the hint that fixes it, not just state the
	// problem.
	if !strings.Contains(err.Error(), "no site linked. Run \"kamakiri init\" first") {
		t.Errorf("error = %q, want the actionable no-site message", err.Error())
	}
}

func TestTeardownSiteNotFoundCleansUp(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "gone123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Removed .kamakiri/config.json") {
		t.Error("should clean up local config even when server says not found")
	}

	config, _ := core.LoadProject()
	if config != nil {
		t.Error("config should be removed")
	}
}

func TestTeardownGetSiteForbidden(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "forbidden", Message: "You do not own this site."}
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "you don't own this site") {
		t.Errorf("error = %q, want 'you don't own this site'", err.Error())
	}
}

func TestTeardownGetSiteUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			t.Error("DeleteSite should not be called")
			return nil, nil
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

func TestTeardownDeleteForbidden(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "forbidden", Message: "You do not own this site."}
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "you don't own this site") {
		t.Errorf("error = %q, want 'you don't own this site'", err.Error())
	}
}

func TestTeardownDeleteUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
}

func TestTeardownDeleteSiteNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupConfig(t, "site123")

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site"}, nil
		},
		deleteSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
		waitForSyncFn: func(_ string, _ int64, _ time.Duration) (*api.Site, error) {
			t.Error("WaitForSync should not be called")
			return nil, nil
		},
	}

	stdin := strings.NewReader("y\n")
	var stdout bytes.Buffer

	err := Run(client, stdin, &stdout, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Removed .kamakiri/config.json") {
		t.Error("should clean up local config when delete returns not found")
	}

	config, _ := core.LoadProject()
	if config != nil {
		t.Error("config should be removed")
	}
}
