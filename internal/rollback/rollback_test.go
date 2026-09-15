package rollback

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
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

// mockClient scripts the freshness verdict through getSiteFn; a path that must
// not poll sets it to flunkGetSite so a regression fails loudly.
type mockClient struct {
	rollbackFn func(siteID, deployID string) (*api.DeployResult, error)
	getSiteFn  func(id string) (*api.Site, error)
	getSiteHit bool
}

func (m *mockClient) Rollback(siteID, deployID string) (*api.DeployResult, error) {
	return m.rollbackFn(siteID, deployID)
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	m.getSiteHit = true
	if m.getSiteFn == nil {
		return nil, errors.New("GetSite: no mock configured")
	}
	return m.getSiteFn(id)
}

func flunkGetSite(t *testing.T) func(string) (*api.Site, error) {
	t.Helper()
	return func(string) (*api.Site, error) {
		t.Error("GetSite should not be called on this path")
		return nil, errors.New("flunk")
	}
}

func freshSite(url string) *api.Site {
	return &api.Site{Freshness: &api.Freshness{State: "fresh", LiveURL: url}}
}

func blockedSite(bs ...api.Blocker) *api.Site {
	return &api.Site{Freshness: &api.Freshness{State: "blocked", Blockers: bs}}
}

func setupCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}

func setupProject(t *testing.T) {
	t.Helper()
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
		t.Fatal(err)
	}
}

// The deploy id lands before the wait's own lines, and "✓ live" only on fresh.
func TestRollbackHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(siteID, deployID string) (*api.DeployResult, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q, want site123", siteID)
			}
			if deployID != "20260401-100000-000" {
				t.Errorf("deployID = %q, want 20260401-100000-000", deployID)
			}
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-100000-000"}, nil
		},
		getSiteFn: func(id string) (*api.Site, error) {
			if id != "site123" {
				t.Errorf("GetSite id = %q, want site123 (from local config)", id)
			}
			return freshSite("https://shop.example.com"), nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, "20260401-100000-000", false, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Rolled back to 20260401-100000-000") {
		t.Errorf("missing rollback confirmation: %q", output)
	}
	if !strings.Contains(output, "✓ live: https://shop.example.com") {
		t.Errorf("missing the live milestone: %q", output)
	}
	if strings.Contains(output, "Rolling back... done.") {
		t.Errorf("the old 'Rolling back... done.' idiom must be gone: %q", output)
	}
	if strings.Contains(output, "Cache purged.") {
		t.Errorf("the 'Cache purged.' emit must be gone: %q", output)
	}
	if !client.getSiteHit {
		t.Error("GetSite was not called on the wait path")
	}
}

// A terminal flush block still leaves the user the deploy id.
func TestRollbackBlockedExitsWithEntry(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-100000-000"}, nil
		},
		getSiteFn: func(string) (*api.Site, error) {
			return blockedSite(api.Blocker{
				Provider: "webaccel", Host: "shop.example.com", Reason: "credentials_rejected",
			}), nil
		},
	}

	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if !errors.Is(err, freshness.ErrFlushBlocked) {
		t.Fatalf("Run() err = %v, want ErrFlushBlocked", err)
	}
	output := out.String()
	if !strings.Contains(output, "Rolled back to 20260401-100000-000") {
		t.Errorf("trailer must print before the wait, even on a blocked exit: %q", output)
	}
	if !strings.Contains(output, "✗ Rolled back, but the WebAccel cache was not flushed: the stored credentials were rejected.") {
		t.Errorf("output missing the blocked entry with the rollback lead: %q", output)
	}
	if strings.Contains(output, "✓ live") {
		t.Errorf("a blocked rollback must not report live: %q", output)
	}
}

// --no-wait must not poll.
func TestRollbackNoWait(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-100000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var out bytes.Buffer
	if err := Run(client, "20260401-100000-000", true, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Rolling back to 20260401-100000-000 (queued, --no-wait).") {
		t.Errorf("output missing the queued line: %q", output)
	}
	if !strings.Contains(output, "→ https://shop.example.com") {
		t.Errorf("output missing the URL echo: %q", output)
	}
	if !strings.Contains(output, "Rollback continues in the background; check kamakiri status.") {
		t.Errorf("output missing the converging clause: %q", output)
	}
	if strings.Contains(output, "Cache purged.") {
		t.Errorf("output should NOT contain 'Cache purged.' on --no-wait: %q", output)
	}
	if client.getSiteHit {
		t.Error("GetSite was called on --no-wait path")
	}
}

func TestRollbackNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	client := &mockClient{getSiteFn: flunkGetSite(t)}
	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("expected 'not logged in', got: %q", err.Error())
	}
}

func TestRollbackNoConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{getSiteFn: flunkGetSite(t)}
	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no site linked") {
		t.Errorf("expected 'no site linked', got: %q", err.Error())
	}
}

func TestRollbackDeployNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "deploy_not_found", Message: "Deploy not found."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var out bytes.Buffer
	err := Run(client, "99991231-235959-000", false, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Deploy not found: 99991231-235959-000") {
		t.Errorf("expected deploy ID in error, got: %q", err.Error())
	}
}

// A deploy that is already live must not enter the wait: with nothing enqueued,
// the wait would settle on an unrelated earlier reconcile and report success for
// work that never ran.
func TestRollbackAlreadyLive(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "already_live", Message: "Deploy is already live."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if err != nil {
		t.Fatalf("expected nil error (success), got: %v", err)
	}
	if !strings.Contains(out.String(), "Deploy 20260401-100000-000 is already live.") {
		t.Errorf("expected already-live message on stdout, got: %q", out.String())
	}
	if client.getSiteHit {
		t.Error("GetSite was called on already_live short-circuit")
	}
}

func TestRollbackSiteNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "site not found") {
		t.Errorf("expected mapped error, got: %q", err.Error())
	}
}

func TestRollbackForbidden(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "forbidden", Message: "Not your site."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "don't own") {
		t.Errorf("expected mapped error, got: %q", err.Error())
	}
}

func TestRollbackUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		rollbackFn: func(_, _ string) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Invalid API key."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var out bytes.Buffer
	err := Run(client, "20260401-100000-000", false, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("expected mapped error, got: %q", err.Error())
	}
}
