package status

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// testLang is the catalog every test here renders against. A test that switches
// away restores this rather than a literal of its own, so the pin moves in one
// place.
const testLang = "en"

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under: every label, value and per-domain row this page prints
// comes from the message catalog. Load rather than Setup: nothing here reports
// which language is in force, only renders in it.
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
	getSiteFn       func(id string) (*api.Site, error)
	listDomainsFn   func(siteID string) (*api.DomainList, error)
	cdnStatusFn     func(siteID string) (*api.CDNStatusResponse, error)
	recheckDomainFn func(domain string) (*api.Domain, error)
}

func (m *mockClient) RecheckDomain(domain string) (*api.Domain, error) {
	if m.recheckDomainFn != nil {
		return m.recheckDomainFn(domain)
	}
	return nil, errors.New("RecheckDomain not stubbed")
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	return m.getSiteFn(id)
}

func (m *mockClient) ListDomains(siteID string) (*api.DomainList, error) {
	if m.listDomainsFn != nil {
		return m.listDomainsFn(siteID)
	}
	return &api.DomainList{}, nil
}

func (m *mockClient) CDNStatus(siteID string) (*api.CDNStatusResponse, error) {
	if m.cdnStatusFn != nil {
		return m.cdnStatusFn(siteID)
	}
	return &api.CDNStatusResponse{CDNMode: "none", Provider: "none"}, nil
}

func TestStatusNoCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	if want := i18n.T("common.err_not_logged_in"); err == nil || err.Error() != want {
		t.Fatalf("Run() error = %v, want %q", err, want)
	}

	out := buf.String()

	assertContains(t, out, "v0.1.0")
	assertContains(t, out, "(not found)")
	assertContains(t, out, "Logged in:    no")
	assertContains(t, out, "https://api.kamakiri-labs.jp/cloud")
	assertContains(t, out, "Site:         (none)")
}

// A credentials file holding no key is not a login. The page golden below
// pins what the reader sees over it; this pins the verdict the run hands back,
// which the golden runner discards.
func TestStatusKeylessFileIsNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	dir := filepath.Join(home, "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"api_key":"","email":"file@example.com"}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	if want := i18n.T("common.err_not_logged_in"); err == nil || err.Error() != want {
		t.Fatalf("Run() error = %v, want %q", err, want)
	}
}

// Matched by containment: the message ends in the operating system's own reason
// for having no home directory to fall back on.
func TestStatusNoConfigDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	if want := "determine config directory"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Run() error = %v, want it to carry %q", err, want)
	}

	assertContains(t, buf.String(), "Logged in:    no")
}

func TestStatusWithCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")

	var buf bytes.Buffer
	Run(&buf, "v0.2.0", "http://localhost:4000", nil, false)

	out := buf.String()

	assertContains(t, out, "v0.2.0")
	assertContains(t, out, "credentials.json")
	assertContains(t, out, "user@example.com")
	assertContains(t, out, "http://localhost:4000")
	assertContains(t, out, "Site:         (none)")
}

func TestStatusWithLegacyCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "")

	var buf bytes.Buffer
	Run(&buf, "(dev)", "https://api.kamakiri-labs.jp/cloud", nil, false)

	out := buf.String()

	assertContains(t, out, "Logged in:    yes")
}

// The variable outranks the file, and the page says so rather than naming a
// file the run never opened. `yes` on the logged-in line is all the variable
// can support: it carries a key and no email. The page names the variable and
// never the key it holds: a CI run prints that page into a log anyone can read.
func TestStatusWithAPIKeyFromEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	out := buf.String()

	assertContains(t, out, "Credentials:  KAMAKIRI_API_KEY (environment)")
	assertContains(t, out, "Logged in:    yes")
	assertNotContains(t, out, "credentials.json")
	assertNotContains(t, out, "(not found)")
	assertNotContains(t, out, "kk_live_fromenv")
}

// A file present under the variable is still not read, which is what the absent
// email proves: the file holds one, and the page would print it if it had been
// opened.
func TestStatusWithAPIKeyFromEnvironmentIgnoresTheFile(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := core.SaveCredentials("kk_live_fromfile", "user@example.com"); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	out := buf.String()

	assertContains(t, out, "Credentials:  KAMAKIRI_API_KEY (environment)")
	assertContains(t, out, "Logged in:    yes")
	assertNotContains(t, out, "user@example.com")
	assertNotContains(t, out, "credentials.json")
	assertNotContains(t, out, "kk_live_fromenv")
}

func TestStatusDevVersion(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var buf bytes.Buffer
	Run(&buf, "(dev)", "https://api.kamakiri-labs.jp/cloud", nil, false)

	assertContains(t, buf.String(), "(dev)")
}

func TestStatusCorruptedCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	credDir := filepath.Join(tmpDir, "kamakiri")
	os.MkdirAll(credDir, 0700)
	os.WriteFile(filepath.Join(credDir, "credentials.json"), []byte("{bad json"), 0600)

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	if err == nil || !strings.Contains(err.Error(), "parse credentials") {
		t.Fatalf("Run() error = %v, want it to carry %q", err, "parse credentials")
	}

	out := buf.String()
	assertContains(t, out, "credentials.json")
	assertContains(t, out, "error")
}

func TestStatusWithSite(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Subdomain:    my-site.kamakiri-pages.jp")
	assertContains(t, out, "Domain:       (none)")
}

func TestStatusWithQuota(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:               id,
				Subdomain:        "my-site",
				SubdomainEnabled: true,
				Quota: &api.Quota{
					SitesUsed:        2,
					SitesMax:         5,
					SnapshotsUsed:    3,
					SnapshotsMax:     5,
					MaxSnapshotBytes: 100 * 1024 * 1024,
				},
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Sites:        2 / 5")
	assertContains(t, out, "Snapshots:    3 / 5 (this site)")
	assertContains(t, out, "Max deploy:   100 MB")
}

func TestFormatMaxDeploy(t *testing.T) {
	cases := map[int64]string{
		100 * 1024 * 1024:       "100 MB",
		25 * 1024 * 1024:        "25 MB",
		25*1024*1024 + 512*1024: "25.5 MB", // a non-MB-aligned override keeps one decimal
	}

	for bytes, want := range cases {
		if got := formatMaxDeploy(bytes); got != want {
			t.Errorf("formatMaxDeploy(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestStatusQuotaOmittedWhenAbsent(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			// No Quota block (an older server): status must skip the lines, not crash.
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertNotContains(t, out, "Sites:")
	assertNotContains(t, out, "Max deploy:")
}

func TestStatusWithSiteAPIError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(_ string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
	}

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	if err == nil || !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("Run() error = %v, want it to carry %q", err, "invalid API key")
	}

	out := buf.String()
	assertContains(t, out, "Site:         site123")
}

// Every read hands its failure to MapError, so a server code arrives as the copy
// every other command prints rather than as the server's own message. The site
// read is pinned by TestStatusWithSiteAPIError; the tables that drive these two
// feed them a plain error and the upgrade sentinel, both of which MapError
// returns unchanged, so neither would catch a raw return here.
func TestStatusMapsAnErrorResponseFromTheDomainAndCDNReads(t *testing.T) {
	site := func(id string) (*api.Site, error) {
		return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
	}
	notFound := &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}

	cases := []struct {
		name   string
		client *mockClient
	}{
		{
			name: "domain list",
			client: &mockClient{
				getSiteFn:     site,
				listDomainsFn: func(string) (*api.DomainList, error) { return nil, notFound },
			},
		},
		{
			name: "cdn status",
			client: &mockClient{
				getSiteFn:   site,
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) { return nil, notFound },
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			core.SaveCredentials("kk_live_test", "user@example.com")
			core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

			var buf bytes.Buffer
			_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", tc.client, false)

			if want := i18n.T("api.err_site_not_found"); err == nil || err.Error() != want {
				t.Fatalf("Run() error = %v, want %q", err, want)
			}
		})
	}
}

func TestStatusSkipsTheLookupWithoutAKey(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(string) (*api.Site, error) {
			t.Fatal("GetSite must not be called without a key")
			return nil, nil
		},
	}

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	// Equality rather than containment: a wrapped or prefixed error at the
	// boundary between the page and its caller is the thing worth catching.
	if want := i18n.T("common.err_not_logged_in"); err == nil || err.Error() != want {
		t.Fatalf("Run() error = %v, want %q", err, want)
	}

	out := buf.String()
	assertContains(t, out, "Logged in:    no")
	assertContains(t, out, "Site:         site123")
}

func TestStatusWithSiteNoClient(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	var buf bytes.Buffer
	_, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", nil, false)

	// No credential is saved here, so the nil-client return is the one that
	// carries the not-logged-in error out.
	if want := i18n.T("common.err_not_logged_in"); err == nil || err.Error() != want {
		t.Fatalf("Run() error = %v, want %q", err, want)
	}

	out := buf.String()
	assertContains(t, out, "Site:         site123")
}

func TestStatusWithCanonicalDomain(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{
				Domains: []api.Domain{
					{Domain: "example.com", Role: "canonical"},
					{Domain: "www.example.com", Role: "redirect", RedirectStatus: 301},
				},
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Domain:       example.com")
	assertContains(t, out, "Domains:      1 additional")
}

func TestStatusSubdomainDisabled(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: false}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Subdomain:    my-site.kamakiri-pages.jp (disabled)")
}

func TestStatusNoDomains(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Domain:       (none)")
}

func TestStatusWithCDNNone(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "CDN:          none")
}

func TestStatusDeletedActive_PendingTeardown(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:               id,
				Subdomain:        "td",
				SubdomainEnabled: true,
				Status:           "deleted",
				StatusObserved:   "active",
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "pending teardown")
	// The top-level branch supersedes the per-projection rows.
	if strings.Contains(out, "Subdomain:") {
		t.Errorf("(deleted, active) must skip per-projection rows; got:\n%s", out)
	}
}

// (deleted, soft_deleted) renders the "tearing down" line and then falls
// through to the per-projection rows, so the user sees which externals are
// still in flight.
func TestStatusDeletedSoftDeleted_TearingDown(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:             id,
				Subdomain:      "td",
				Status:         "deleted",
				StatusObserved: "soft_deleted",
			}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "example.com", Role: "canonical"},
			}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "tearing down")
	assertContains(t, out, "Subdomain:")
}

// An unset observed value with a queued or running sync row reads as "never
// reconciled", which is a different situation from an ongoing drift-sync.
func TestStatusActiveActive_NeverReconciled(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:               id,
				Subdomain:        "fresh",
				SubdomainEnabled: true,
				Status:           "active",
				StatusObserved:   "active",
				Sync:             &api.Sync{LatestAttemptID: 1, Outcome: "running"},
			}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "never reconciled")
}

func TestStatusDeletedDeleted_Terminal(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:             id,
				Subdomain:      "td",
				Status:         "deleted",
				StatusObserved: "deleted",
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "deleted")
	if strings.Contains(out, "Subdomain:") {
		t.Errorf("(deleted, deleted) must skip per-projection rows; got:\n%s", out)
	}
}

// A combination the server cannot emit: surfaces the raw values for forensics.
func TestStatusActiveSoftDeleted_Stuck(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID:             id,
				Subdomain:      "td",
				Status:         "active",
				StatusObserved: "soft_deleted",
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "stuck")
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("output does not contain %q:\n%s", substr, s)
	}
}

func assertNotContains(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("output unexpectedly contains %q:\n%s", substr, s)
	}
}

// The two axes are independent columns: a domain can be serving for DNS while
// still awaiting_cf_validation for CDN.
func TestStatusPerDomainBlockShowsDNSAndCDNStates(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain:   "example.com",
					Role:     "canonical",
					State:    "serving",
					CdnState: "active",
				},
				{
					Domain:   "blog.example.com",
					Role:     "alias",
					State:    "awaiting_dns",
					CdnState: "off",
				},
			}}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Per-domain status:")
	assertContains(t, out, "example.com")
	assertContains(t, out, "✓ live")           // DNS serving
	assertContains(t, out, "✓ via Cloudflare") // CDN active
	assertContains(t, out, "blog.example.com")
	// A never-live awaiting_dns row points at the verify command rather than
	// showing a static waiting indicator.
	assertContains(t, out, "⧗ waiting for your DNS")
	assertContains(t, out, "domain verify blog.example.com")
}

// Header rows must align across the whole block even when a records table is
// interleaved below one of them. A regression here collapses the columns to
// one-space-separated values.
func TestStatusPerDomainBlockColumnsAlignAcrossRows(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "a.example.com", Role: "canonical", State: "serving", CdnState: "active"},
				{Domain: "much-longer.example.com", Role: "alias", State: "serving", CdnState: "active"},
			}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	// Scan only the Per-domain block: the top-level "Domain:" summary line also
	// names the canonical domain and would false-positive.
	perDomainBlock := ""
	if i := strings.Index(out, "Per-domain status:\n"); i >= 0 {
		perDomainBlock = out[i:]
	}
	if perDomainBlock == "" {
		t.Fatalf("Per-domain block not found in output:\n%s", out)
	}
	// Finding the row is half the assertion: a changed indent would leave the
	// loop below matching nothing and reporting success on an empty scan.
	found := false
	for _, line := range strings.Split(perDomainBlock, "\n") {
		if !strings.HasPrefix(line, "  a.example.com") {
			continue
		}
		found = true
		// perDomainLines sizes the domain column from the widest domain in the
		// whole block, not from this row alone, so the short domain here is
		// padded well past a two-space gap. The threshold of 4 catches that
		// wider padding without pinning the exact column width.
		if !strings.Contains(line, "a.example.com    ") { // 4 spaces
			t.Errorf("domain column not padded to the block's widest domain; got: %q", line)
		}
	}
	if !found {
		t.Errorf("no row for a.example.com at the block's own indent:\n%s", perDomainBlock)
	}
}

func TestStatusRendersRecordsTableForBrokenRow(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{{
				Domain:   "www.example.com",
				Role:     "canonical",
				State:    "degraded",
				CdnState: "off",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "site123.c1.kamakiri-pages.site", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "www.example.com", Type: "cname", Values: []string{"wrong.example.org"}},
				},
			}}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "site123.c1.kamakiri-pages.site") // expected value rendered
	assertContains(t, out, "✗ got wrong.example.org")        // observed mismatch
}

// A serving row hides the records table; `--verbose` renders it anyway.
func TestStatusRecordsTableHiddenForServingExceptVerbose(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	makeClient := func() *mockClient {
		return &mockClient{
			getSiteFn: func(id string) (*api.Site, error) {
				return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
			},
			listDomainsFn: func(_ string) (*api.DomainList, error) {
				return &api.DomainList{Domains: []api.Domain{{
					Domain:   "www.example.com",
					Role:     "canonical",
					State:    "serving",
					CdnState: "active",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "www.example.com", Type: "cname", Value: "site123.c1.kamakiri-pages.site", Purpose: "primary", Required: true},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{Name: "www.example.com", Type: "cname", Values: []string{"site123.c1.kamakiri-pages.site"}},
					},
				}}}, nil
			},
		}
	}

	var quiet bytes.Buffer
	Run(&quiet, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", makeClient(), false)
	if strings.Contains(quiet.String(), "site123.c1.kamakiri-pages.site") {
		t.Errorf("non-verbose should NOT render records table for serving rows; got:\n%s", quiet.String())
	}

	var verbose bytes.Buffer
	Run(&verbose, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", makeClient(), true)
	assertContains(t, verbose.String(), "site123.c1.kamakiri-pages.site")
	assertContains(t, verbose.String(), "✓ matches")
}

// awaiting_cf_validation rows render the records table even when DNS is
// serving, because a user mid-DCV-setup needs to see the validation records
// regardless of the DNS-axis state.
func TestStatusRecordsTableForAwaitingCFValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{{
				Domain:   "www.example.com",
				Role:     "canonical",
				State:    "serving",
				CdnState: "awaiting_cf_validation",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "abc.cloudflare.net", Purpose: "primary", Required: true},
					{Name: "_cf-custom-hostname.www.example.com", Type: "txt", Value: "ca3-token", Purpose: "validation", Required: false},
				},
			}}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	out := buf.String()
	assertContains(t, out, "Validation records (add temporarily):")
	assertContains(t, out, "_cf-custom-hostname.www.example.com")
}

// A site whose domains carry no state gets no Per-domain block: an empty table
// is just noise.
func TestStatusOmitsPerDomainBlockWhenAllStatesBlank(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "blank.example.com", Role: "canonical"}, // no state, no cdn_state
			}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)

	if strings.Contains(buf.String(), "Per-domain status:") {
		t.Errorf("Per-domain block should be omitted when no rows carry state; got:\n%s", buf.String())
	}
}

func newNudgeClient(t *testing.T, recheckShouldBeCalled bool) *mockClient {
	t.Helper()
	return &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain: "next.altstack.jp",
					Role:   "canonical",
					State:  "awaiting_dns",
					// Never-live: the server-derived verdict is "absent" (no record
					// observed yet), which IsNeverLive keys on.
					DnsVerdict: "absent",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "next.altstack.jp", Type: "cname", Value: "t.kamakiri-pages.site.", Purpose: "primary", Required: true},
					},
				},
			}}, nil
		},
		recheckDomainFn: func(d string) (*api.Domain, error) {
			if !recheckShouldBeCalled {
				t.Fatalf("RecheckDomain must NOT be called by a pure `status` read (called with %q)", d)
			}
			return &api.Domain{
				Domain: d, Role: "canonical", State: "awaiting_dns",
				DnsVerdict: "absent",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: d, Type: "cname", Value: "t.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
	}
}

func TestStatusNudgeBandAwareNoProbeByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	// recheck must not be called by plain `status`.
	client := newNudgeClient(t, false)

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()

	// Deliberate-backoff framing (out of burst, no misleading small ETA).
	assertContains(t, out, "slowed down")
	assertContains(t, out, "domain verify next.altstack.jp")
	if strings.Contains(out, "~60s") || strings.Contains(out, "~25s") {
		t.Errorf("pure `status` must not print a small ETA (the relocated 'within ~1 min' lie):\n%s", out)
	}
	if strings.Contains(out, "sub-minute") {
		t.Errorf("must never say 'sub-minute':\n%s", out)
	}
}

func TestStatusRecheckOptInCallsRecheckAndUsesInBurstFraming(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	called := false
	client := newNudgeClient(t, true)
	inner := client.recheckDomainFn
	client.recheckDomainFn = func(d string) (*api.Domain, error) {
		called = true
		return inner(d)
	}

	var buf, errBuf bytes.Buffer
	RunWithRecheck(&buf, &errBuf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()

	if !called {
		t.Fatal("`status --recheck` must call RecheckDomain")
	}
	// The recheck just opened the probe window, so in-burst framing
	// (with a small ETA) is truthful here.
	assertContains(t, out, "~60s")
	assertContains(t, out, "domain verify next.altstack.jp")
	if strings.Contains(out, "sub-minute") {
		t.Errorf("must never say 'sub-minute':\n%s", out)
	}
}

func TestStatusRecheckRateLimitedIsNonFatal(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := newNudgeClient(t, true)
	client.recheckDomainFn = func(string) (*api.Domain, error) {
		return nil, &api.ErrorResponse{Code: "rate_limited", Message: "Too many requests."}
	}

	var buf, errBuf bytes.Buffer
	_, err := RunWithRecheck(&buf, &errBuf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	// Still renders status (falls back to the pre-recheck row).
	assertContains(t, out, "next.altstack.jp")
	assertContains(t, out, "Per-domain status:")
	if err != nil {
		t.Errorf("a rate-limited recheck must not fail the command, got %v", err)
	}
	if errBuf.String() != "" {
		t.Errorf("a rate limit says nothing the page does not, want empty stderr, got %q", errBuf.String())
	}
}

func TestStatusRecheckTransientFailureWarnsAndStillRenders(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := newNudgeClient(t, true)
	client.recheckDomainFn = func(string) (*api.Domain, error) {
		return nil, errors.New("connection refused")
	}

	var buf, errBuf bytes.Buffer
	_, err := RunWithRecheck(&buf, &errBuf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	if err != nil {
		t.Errorf("a failed recheck must not fail the command, got %v", err)
	}
	assertContains(t, out, "next.altstack.jp")
	assertContains(t, out, "Per-domain status:")
	assertContains(t, errBuf.String(), "recheck of next.altstack.jp not performed (connection refused). The server keeps checking on its own.")
}

// A recheck the server did perform is the freshest view of the domain, so the
// page must render the row it returned and not the one the listing carried.
func TestStatusRecheckReplacesTheRowWithTheFreshOne(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := newNudgeClient(t, true)
	client.recheckDomainFn = func(d string) (*api.Domain, error) {
		return &api.Domain{Domain: d, Role: "canonical", State: "serving"}, nil
	}

	var buf, errBuf bytes.Buffer
	_, err := RunWithRecheck(&buf, &errBuf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	if err != nil {
		t.Errorf("a successful recheck must not fail the command, got %v", err)
	}
	if errBuf.String() != "" {
		t.Errorf("a successful recheck warns about nothing, want empty stderr, got %q", errBuf.String())
	}
	assertContains(t, out, "next.altstack.jp")
	assertContains(t, out, "✓ live")
	// Both belong to the stale awaiting_dns row: its status line and the nudge
	// that follows it.
	assertNotContains(t, out, "waiting for your DNS")
	assertNotContains(t, out, "domain verify next.altstack.jp")
}

// When an apex's own A records no longer match the per-mode expected set (a CDN
// flip the customer's DNS has not followed), status renders a ✗ block and exits
// non-zero.
func TestStatusApexARecordsDriftSurfacesAsFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain: "example.com",
					Role:   "canonical",
					State:  "serving",
					// The server-derived verdict says the customer publishes direct
					// apex A records, so the drift check applies.
					DnsMatchedAlternative: "a",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "example.com", Type: "alias_or_aname", Value: "x.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "173.245.48.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "103.21.244.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{
							Name: "example.com", Type: "a",
							// The old cluster IPs, against the post-flip anycast
							// set expected above.
							Values:           []string{"198.51.100.10", "198.51.100.11"},
							AlternativeGroup: "apex_primary",
						},
					},
				},
			}}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}, nil
		},
	}

	var buf bytes.Buffer
	drift, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	out := buf.String()

	if !drift {
		t.Fatalf("expected drift=true so CLI exits non-zero; output:\n%s", out)
	}
	assertContains(t, out, "✗ A records do not match the current edge for cloudflare mode")
	assertContains(t, out, "expected: 173.245.48.0, 103.21.244.0")
	assertContains(t, out, "observed: 198.51.100.10, 198.51.100.11")
	assertContains(t, out, "Update your A records at your registrar")
}

// none mode has no CDN edge, the apex pointing straight at the cluster, so the
// headline names the cluster rather than "the current edge for none mode".
func TestStatusApexARecordsDriftNoneModeNamesClusterIPs(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain: "example.com",
					Role:   "canonical",
					State:  "serving",
					// The server-derived verdict says the customer publishes direct
					// apex A records, so the drift check applies.
					DnsMatchedAlternative: "a",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "example.com", Type: "alias_or_aname", Value: "x.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{
							Name:             "example.com",
							Type:             "a",
							Values:           []string{"198.51.100.10"},
							AlternativeGroup: "apex_primary",
						},
					},
				},
			}}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none", Provider: "none"}, nil
		},
	}

	var buf bytes.Buffer
	drift, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	out := buf.String()

	if !drift {
		t.Fatalf("expected drift=true; output:\n%s", out)
	}
	assertContains(t, out, "✗ A records do not match the cluster IPs")
	// The "current edge for none mode" phrasing must never appear.
	if strings.Contains(out, "current edge for none mode") {
		t.Errorf("none mode must not use 'current edge for none mode' framing: %s", out)
	}
}

// `kamakiri status` surfaces the awaiting-cleanup line (shared renderer with
// `kamakiri cdn`) when the site carries orphaned CDN resources.
func TestStatusSurfacesCleanupOrphans(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none", CleanupOrphanCount: 1}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	assertContains(t, buf.String(), "1 CDN resource(s) awaiting cleanup; run `kamakiri cdn cleanup`.")
}

func TestStatusApexARecordsMatchExpectedNoDrift(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain: "example.com",
					Role:   "canonical",
					State:  "serving",
					// The drift check runs and finds observed equal to expected.
					DnsMatchedAlternative: "a",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "example.com", Type: "alias_or_aname", Value: "x.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "173.245.48.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "103.21.244.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{
							Name:             "example.com",
							Type:             "a",
							Values:           []string{"103.21.244.0", "173.245.48.0"}, // same set, different order
							AlternativeGroup: "apex_primary",
						},
					},
				},
			}}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}, nil
		},
	}

	var buf bytes.Buffer
	drift, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	out := buf.String()

	if drift {
		t.Fatalf("expected drift=false (observed matches expected as set); output:\n%s", out)
	}
	if strings.Contains(out, "A records do not match") {
		t.Errorf("must not surface drift line when sets match: %s", out)
	}
}

// An ALIAS/ANAME customer resolves through the dns_target indirection, which the
// reconciler rewrites for them, so a mismatched observed A set is no drift.
func TestStatusApexOnAliasNoDriftEverEvenIfADifferent(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain: "example.com",
					Role:   "canonical",
					State:  "serving",
					// The verdict is alias_or_aname, so the apex A drift advisory
					// does not apply.
					DnsMatchedAlternative: "alias_or_aname",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "example.com", Type: "alias_or_aname", Value: "x.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "173.245.48.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{
							Name:             "example.com",
							Type:             "a",
							Values:           []string{"198.51.100.10"}, // wouldn't match expected
							AlternativeGroup: "apex_primary",
						},
					},
				},
			}}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}, nil
		},
	}

	var buf bytes.Buffer
	drift, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	out := buf.String()

	if drift {
		t.Fatalf("ALIAS customers never see drift; output:\n%s", out)
	}
	if strings.Contains(out, "A records do not match") {
		t.Errorf("ALIAS-mode apex must not see drift line: %s", out)
	}
}

// A pending site whose edge axis is not the bottleneck (edge already flushed,
// something else catching up) renders the generic converging line, driven off
// the server's `axes` verdict, never re-derived from timestamps.
func TestStatusFreshnessGenericConvergingLine(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Freshness: &api.Freshness{
					State: "pending",
					Axes:  api.FreshnessAxes{Origin: "pending", Edge: "flushed", Cdn: "flushed"},
				},
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	assertContains(t, out, "⧗ still going live")
	if strings.Contains(out, "Edge cache:") {
		t.Errorf("edge line must not render when axes.edge is not flushing:\n%s", out)
	}
}

// When the edge axis is the bottleneck, the edge-flush line renders with the
// "since <age>" anchored on edge_purge_last_ok_at. A fresh site renders no
// freshness line at all.
func TestStatusFreshnessEdgeFlushingLine(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	lastOk := time.Now().Add(-3*time.Minute - 30*time.Second).UTC().Format(time.RFC3339)
	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Freshness: &api.Freshness{
					State: "pending",
					Axes:  api.FreshnessAxes{Origin: "satisfied", Edge: "flushing", Cdn: "flushed"},
				},
				EdgePurgeHealth:   "ok",
				EdgePurgeLastOkAt: lastOk,
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	assertContains(t, out, "Edge cache: still flushing (since 3m)")
	if strings.Contains(out, "still going live") {
		t.Errorf("edge-flushing site must render the edge line, not the generic one:\n%s", out)
	}
}

// A failing edge purge appends on-us copy (never a user action), because that
// cache is ours end to end with nothing a user could act on.
func TestStatusFreshnessEdgeFailingHealth(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Freshness: &api.Freshness{
					State: "pending",
					Axes:  api.FreshnessAxes{Origin: "satisfied", Edge: "flushing", Cdn: "flushed"},
				},
				EdgePurgeHealth:      "failing",
				EdgePurgeErrorReason: "http_503",
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	assertContains(t, out, "our flush is failing (http_503)")
	assertContains(t, out, "we keep retrying")
	if strings.Contains(out, "—") {
		t.Errorf("on-us edge copy must not use an em-dash:\n%s", out)
	}
}

// A fresh site renders no site-level freshness line.
func TestStatusFreshnessFreshRendersNoLine(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Freshness: &api.Freshness{State: "fresh", Axes: api.FreshnessAxes{Origin: "satisfied", Edge: "flushed", Cdn: "flushed"}},
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	if strings.Contains(out, "still going live") || strings.Contains(out, "Edge cache:") {
		t.Errorf("a fresh site must render no freshness line:\n%s", out)
	}
}

// A terminal flush block renders through FormatBlocker with the non-verb "The"
// lead: status deployed nothing, so it never claims "Deployed, but".
func TestStatusFreshnessBlockerEntryNonVerbLead(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Freshness: &api.Freshness{
					State: "blocked",
					Axes:  api.FreshnessAxes{Origin: "satisfied", Edge: "flushed", Cdn: "blocked"},
					Blockers: []api.Blocker{
						{Provider: "webaccel", Host: "shop.example.com", Reason: "credentials_rejected"},
					},
				},
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	assertContains(t, out, "✗ The WebAccel cache is not flushed: the stored credentials were rejected.")
	assertContains(t, out, "Run kamakiri cdn credentials to update them")
	if strings.Contains(out, "Deployed, but") {
		t.Errorf("status deployed nothing, so it must never claim 'Deployed, but':\n%s", out)
	}
	// The verdict is blocked, so the ✗ entry is the whole message: no converging
	// line may sit above it claiming the site is still on its way up.
	if strings.Contains(out, "still going live") || strings.Contains(out, "Edge cache:") {
		t.Errorf("a blocked verdict must not print a converging line above its ✗ entry:\n%s", out)
	}
}

// A hard reconcile push failure surfaces its reason, so it stays visible after a
// Ctrl-C during a deploy.
func TestStatusFreshnessReconcileError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Sync:      &api.Sync{Outcome: "error", Error: "caddy push rejected (4xx)"},
				Freshness: &api.Freshness{State: "pending", Axes: api.FreshnessAxes{Origin: "pending", Edge: "flushed", Cdn: "flushed"}},
			}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()
	assertContains(t, out, "Reconcile:")
	assertContains(t, out, "caddy push rejected (4xx)")
}

// A host in the site's freshness Blockers drops its per-domain Purges: line, so
// the failure is reported once, site-level, with the right remediation. A
// failing host that is not blocked keeps its line. Suppression keys off the site
// payload's blocker-host set, not the per-domain cdn_purge_blocked flag.
func TestStatusFreshnessPerDomainSuppressionViaBlockerHosts(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "my-site", SubdomainEnabled: true,
				Freshness: &api.Freshness{
					State: "blocked",
					Axes:  api.FreshnessAxes{Origin: "satisfied", Edge: "flushed", Cdn: "blocked"},
					Blockers: []api.Blocker{
						{Provider: "cloudflare", Host: "shop.example.com", Reason: "provider_rejected", Detail: "zone is paused"},
					},
				},
			}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{
					Domain: "shop.example.com", Role: "canonical", State: "serving", CdnState: "active",
					CdnPurgeHealth: "failing", CdnPurgeErrorReason: "http_403", CdnPurgeConsecutiveFailures: 1,
					// The per-domain flag stays on the wire but must NOT be the
					// suppression authority; even set false here, the site
					// blocker-host set suppresses this row.
					CdnPurgeBlocked: false,
				},
				{
					Domain: "blog.example.com", Role: "alias", State: "serving", CdnState: "active",
					CdnPurgeHealth: "failing", CdnPurgeErrorReason: "http_401", CdnPurgeConsecutiveFailures: 2,
				},
			}}, nil
		},
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()

	// Site-level entry for the blocked host.
	assertContains(t, out, "✗ The Cloudflare cache is not flushed")
	// The blocked host's per-domain purge line is suppressed (its HTTP 403 tag
	// would otherwise render).
	if strings.Contains(out, "HTTP 403") {
		t.Errorf("blocked host's per-domain Purges: line must be suppressed:\n%s", out)
	}
	// The non-blocked failing host keeps its per-domain line.
	assertContains(t, out, "HTTP 401")
}

// The whole site-level freshness block is skipped for a torn-down site: the
// "tearing down" line above already owns that framing, and a torn-down site's
// edge facts are frozen so "still flushing" would report a growing-forever age.
// The (deleted, soft_deleted) window is the one that falls through to here.
func TestStatusFreshnessSkippedWhenTornDown(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	core.SaveCredentials("kk_live_test", "user@example.com")
	core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

	client := &mockClient{
		getSiteFn: func(id string) (*api.Site, error) {
			return &api.Site{
				ID: id, Subdomain: "td",
				Status: "deleted", StatusObserved: "soft_deleted",
				// A racing deploy could leave all of these set; none must render.
				Sync: &api.Sync{Outcome: "error", Error: "should not surface"},
				Freshness: &api.Freshness{
					State: "blocked",
					Axes:  api.FreshnessAxes{Origin: "pending", Edge: "flushing", Cdn: "flushed"},
					Blockers: []api.Blocker{
						{Reason: "site_torn_down"},
						{Provider: "webaccel", Host: "shop.example.com", Reason: "credentials_rejected"},
					},
				},
				EdgePurgeHealth: "failing", EdgePurgeErrorReason: "http_503",
			}, nil
		},
		listDomainsFn: func(_ string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "example.com", Role: "canonical"},
			}}, nil
		},
	}

	var buf bytes.Buffer
	Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", client, false)
	out := buf.String()

	assertContains(t, out, "tearing down")
	if strings.Contains(out, "Edge cache:") {
		t.Errorf("edge-flush line must be absent for a torn-down site:\n%s", out)
	}
	if strings.Contains(out, "still going live") {
		t.Errorf("generic converging line must be absent for a torn-down site:\n%s", out)
	}
	if strings.Contains(out, "cache is not flushed") || strings.Contains(out, "torn down, so the deploy") {
		t.Errorf("blocker entries must be absent for a torn-down site:\n%s", out)
	}
	if strings.Contains(out, "Reconcile:") || strings.Contains(out, "should not surface") {
		t.Errorf("reconcile-error line must be absent for a torn-down site:\n%s", out)
	}
}

func TestStatusSurfacesAVersionRefusalFromEveryRead(t *testing.T) {
	site := func(id string) (*api.Site, error) {
		return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
	}

	cases := []struct {
		name   string
		client *mockClient
	}{
		{
			name: "site read",
			client: &mockClient{
				getSiteFn: func(string) (*api.Site, error) { return nil, api.ErrUpgradeRequired },
			},
		},
		{
			name: "domain list",
			client: &mockClient{
				getSiteFn:     site,
				listDomainsFn: func(string) (*api.DomainList, error) { return nil, api.ErrUpgradeRequired },
			},
		},
		{
			name: "cdn status",
			client: &mockClient{
				getSiteFn:   site,
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) { return nil, api.ErrUpgradeRequired },
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			core.SaveCredentials("kk_live_test", "user@example.com")
			core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

			var buf bytes.Buffer
			drift, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", tc.client, false)

			if !errors.Is(err, api.ErrUpgradeRequired) {
				t.Fatalf("Run() error = %v, want the upgrade sentinel", err)
			}
			// Drift keeps meaning drift. The refused read says nothing about the
			// site's records, so claiming one would be a finding out of thin air.
			if drift {
				t.Error("drift = true, want false on a refused read")
			}
		})
	}
}

func TestStatusSurfacesAnOrdinaryAPIErrorAfterThePageItHas(t *testing.T) {
	// The same three reads as the refusal table. Each failure ends the page where
	// it stood and is returned for the caller to print, so a transient blip fails
	// `kamakiri status` the way it fails every other command, with the page it
	// managed to render still on stdout.
	site := func(id string) (*api.Site, error) {
		return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
	}
	boom := errors.New("connection refused")

	cases := []struct {
		name    string
		client  *mockClient
		lastRow string
		noRow   string
	}{
		{
			name: "site read",
			client: &mockClient{
				getSiteFn: func(string) (*api.Site, error) { return nil, boom },
			},
			lastRow: "Site:         site123",
			noRow:   "Subdomain:",
		},
		{
			name: "domain list",
			client: &mockClient{
				getSiteFn:     site,
				listDomainsFn: func(string) (*api.DomainList, error) { return nil, boom },
			},
			lastRow: "Subdomain:",
			noRow:   "CDN:",
		},
		{
			name: "cdn status",
			client: &mockClient{
				getSiteFn:   site,
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) { return nil, boom },
			},
			lastRow: "Domain:       (none)",
			noRow:   "CDN:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			core.SaveCredentials("kk_live_test", "user@example.com")
			core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"})

			var buf bytes.Buffer
			drift, err := Run(&buf, "v0.1.0", "https://api.kamakiri-labs.jp/cloud", tc.client, false)
			if err == nil || !strings.Contains(err.Error(), "connection refused") {
				t.Fatalf("Run() error = %v, want it to carry %q", err, "connection refused")
			}
			if drift {
				t.Error("drift = true, want false")
			}
			assertContains(t, buf.String(), tc.lastRow)
			assertNotContains(t, buf.String(), tc.noRow)
		})
	}
}

// The left column is derived from the widest label rather than fixed at a
// number, and this pins what the English set derives to: the page has always
// put its values at column 14, and a label added without checking would move
// every line on the page.
func TestLabelColumnIsFourteenInEnglish(t *testing.T) {
	if got := labelColumn(); got != 14 {
		t.Fatalf("labelColumn() = %d, want 14: the English page puts every value at column 14", got)
	}
	widest := ""
	for _, text := range labelTexts() {
		if i18n.Width(text) > i18n.Width(widest) {
			widest = text
		}
	}
	if i18n.Width(widest)+labelGap != 14 {
		t.Errorf("the widest English label is %q at %d columns, which does not derive 14", widest, i18n.Width(widest))
	}
}

// Every label has to leave the gap before its value in Japanese too, which a
// column measured in runes cannot promise: %-14s counts a two-column glyph as
// one, so a label of wide glyphs is padded out past the column and its value
// starts several columns right of where the lines around it start theirs.
func TestLabelColumnLeavesTheGapInJapanese(t *testing.T) {
	i18n.Load("ja")
	defer i18n.Load(testLang)

	column := labelColumn()
	for _, text := range labelTexts() {
		padded := label(text)
		if i18n.Width(padded) != column {
			t.Errorf("label(%q) is %d columns wide, want %d", text, i18n.Width(padded), column)
		}
		// Two columns is the blank this page keeps, and the check is against
		// that number rather than against labelGap: measuring the rendered
		// label against the constant that produced it passes however small the
		// constant gets, the widest label included, which is the one that would
		// butt against its value.
		const wantGap = 2
		if got := i18n.Width(padded) - i18n.Width(strings.TrimRight(padded, " ")); got < wantGap {
			t.Errorf("label(%q) = %q, leaving %d columns before its value, want at least %d", text, padded, got, wantGap)
		}
	}
}

// columnStarts reports where each of a line's cells begins, in display columns
// from the start of the line.
func columnStarts(t *testing.T, line string, cells []string) []int {
	t.Helper()

	offsets := make([]int, 0, len(cells))
	rest, consumed := line, 0
	for _, cell := range cells {
		index := strings.Index(rest, cell)
		if index < 0 {
			t.Fatalf("cell %q is missing from %q", cell, line)
		}
		offsets = append(offsets, consumed+i18n.Width(rest[:index]))
		consumed += i18n.Width(rest[:index+len(cell)])
		rest = rest[index+len(cell):]
	}
	return offsets
}

// The per-domain table's own layout, driven with the Japanese cells the DNS and
// CDN columns carry once those states render from the catalog. The CDN column
// is the one at risk: it starts wherever the DNS cell before it ends, and a
// layout counting runes would measure that cell at half the columns a terminal
// gives it and place the CDN column against the miscount.
func TestPerDomainLinesAlignJapaneseCells(t *testing.T) {
	rows := []domainRow{
		{domain: "example.com", role: "canonical", dnsStatus: "✓ 公開中", cdnStatus: "✓ WebAccel 経由"},
		{domain: "shop.example.com", role: "redirect 308", dnsStatus: "⧗ DNSの確認待ちです", cdnStatus: "⧗ 検証中"},
		{domain: "a.example.com", role: "alias", dnsStatus: "✗ 停止しています", cdnStatus: "✗ 未設定"},
	}
	lines := perDomainLines(rows)
	if len(lines) != len(rows) {
		t.Fatalf("perDomainLines returned %d lines, want %d", len(lines), len(rows))
	}

	cells := func(r domainRow) []string {
		return []string{r.domain, r.role, r.dnsStatus, r.cdnStatus}
	}
	want := columnStarts(t, lines[0], cells(rows[0]))
	for i := 1; i < len(lines); i++ {
		got := columnStarts(t, lines[i], cells(rows[i]))
		for c := range want {
			if got[c] != want[c] {
				t.Errorf("row %d column %d starts at display column %d, row 0 has it at %d:\n%q\n%q",
					i, c, got[c], want[c], lines[i], lines[0])
			}
		}
	}
}

// The drift block's two labels sit above one another, so the values beside
// them start at one column. In English the pair happen to render the same
// width, which hides every padding mistake there is; in Japanese they do not,
// so this is where a column sized from one label alone, or padded by rune
// count, starts the second value somewhere else.
func TestDriftBlockValuesAlignInJapanese(t *testing.T) {
	i18n.Load("ja")
	defer i18n.Load(testLang)

	expectedLabel := i18n.T("status.drift_label_expected")
	observedLabel := i18n.T("status.drift_label_observed")
	if i18n.Width(expectedLabel) == i18n.Width(observedLabel) {
		t.Fatalf("both labels render %d columns wide, so this fixture proves nothing about the padding",
			i18n.Width(expectedLabel))
	}

	block := apexARecordsDriftLine(api.Domain{
		Domain:                "example.com",
		Role:                  "canonical",
		State:                 "serving",
		DnsMatchedAlternative: "a",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "a", Value: "192.0.2.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "a", Values: []string{"203.0.113.9"}, AlternativeGroup: "apex_primary"},
		},
	}, "webaccel")
	lines := strings.Split(block, "\n")
	if len(lines) < 3 {
		t.Fatalf("the drift block rendered %d lines:\n%s", len(lines), block)
	}

	expectedStart := columnStarts(t, lines[1], []string{"192.0.2.1"})[0]
	observedStart := columnStarts(t, lines[2], []string{"203.0.113.9"})[0]
	if expectedStart != observedStart {
		t.Errorf("the expected value starts at column %d and the observed one at %d:\n%q\n%q",
			expectedStart, observedStart, lines[1], lines[2])
	}
}

// The teardown note reads on from the value above it, so it is indented to the
// label column rather than to a number of its own. English cannot see the
// difference, since the column comes out at the width a fixed indent would have
// been written to; Japanese labels are wider, so a fixed indent shows up here as
// a note starting left of the value it continues.
func TestTeardownNoteContinuesTheValueColumnInJapanese(t *testing.T) {
	english := labelColumn()

	i18n.Load("ja")
	defer i18n.Load(testLang)

	if labelColumn() == english {
		t.Fatalf("the Japanese label column is %d wide, the same as the English one, so an indent written as a constant would pass here", english)
	}

	var buf bytes.Buffer
	if !renderStatusBranch(&buf, &api.Site{Status: "deleted", StatusObserved: "active"}) {
		t.Fatal("a site deleted but still observed active must render its own branch")
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the branch rendered %d lines, want the value and its note:\n%s", len(lines), buf.String())
	}

	valueStart := columnStarts(t, lines[0], []string{i18n.T("status.value_pending_teardown")})[0]
	noteStart := columnStarts(t, lines[1], []string{i18n.T("status.teardown_edge_note")})[0]
	if noteStart != valueStart {
		t.Errorf("the note starts at display column %d and the value it continues at %d:\n%q\n%q",
			noteStart, valueStart, lines[0], lines[1])
	}
}

// The exact bytes of a two-row table: the two-column indent, the two-column
// gutter between cells, every cell but the last padded to the widest in its
// column, and the last cell never padded. The second row is what says the last
// cell goes unpadded, since its CDN state is empty where the row above carries
// the widest one, so padding that column would leave it trailing fourteen
// spaces instead of the gutter alone. It is also what says the line is allowed
// to end in whitespace at all: the gutter ahead of an empty last cell is
// output rather than untidiness, and trimming it is an output change.
func TestPerDomainLinesAreLaidOutToTheByte(t *testing.T) {
	rows := []domainRow{
		{domain: "example.com", role: "canonical", dnsStatus: "✓ live", cdnStatus: "✓ via WebAccel"},
		{domain: "shop.example.com", role: "redirect 308", dnsStatus: "⧗ awaiting DNS", cdnStatus: ""},
	}
	want := []string{
		"  example.com       canonical     ✓ live          ✓ via WebAccel",
		"  shop.example.com  redirect 308  ⧗ awaiting DNS  ",
	}
	got := perDomainLines(rows)
	if len(got) != len(want) {
		t.Fatalf("perDomainLines returned %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
}

// pageCase is one whole `kamakiri status` page: the state it is rendered from,
// and the exact bytes it prints. The label column, the blank beside it and the
// per-domain table's indent and gutter are all layout this page has always had,
// and a golden of the whole page is what holds them: a substring assertion
// passes just as happily when a column moves.
//
// Some lines end in a space. That is not slack: the per-domain table pads every
// cell but the last, so a row whose CDN cell is empty ends in the padding of the
// cell before it. Trimming one of these lines is an output change.
type pageCase struct {
	name  string
	setup func(t *testing.T) SiteClient
	want  string
}

// linkedSite puts the run in a throwaway home with credentials and a project
// link, which is the state every page below the first few is rendered from.
func linkedSite(t *testing.T, email string) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", email); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
}

// corruptCredentials is what the unreadable-credentials page is rendered from,
// and also what its error payload is derived from below, so the two cannot
// drift apart.
const corruptCredentials = "not json"

// renderPage runs the page and replaces two things the goldens cannot own
// with stable placeholders: the run's throwaway config home, which the
// Credentials line names, and the Go standard library's own error wording,
// which three of the pages quote. Neither is CLI copy. The frames around
// them are, and they are what the goldens pin; a Go release rewording a
// message it never promised would otherwise read here as a status
// regression. Each payload is derived by reproducing the failure rather than
// spelled out, so a rewording moves the placeholder with it.
func renderPage(t *testing.T, client SiteClient) string {
	t.Helper()
	var buf bytes.Buffer
	Run(&buf, "v1.2.3", "https://api.example.test/cloud", client, false)
	out := buf.String()
	// Ahead of the <CONFIG> replacement below, since the Go error string carries
	// the raw home path and would no longer match once that has run. The guard
	// leaves the other cases alone: there the same read either succeeds or fails
	// with the not-exist error the page never prints.
	if home := os.Getenv("XDG_CONFIG_HOME"); home != "" {
		_, err := os.ReadFile(filepath.Join(home, "kamakiri", "credentials.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			out = strings.ReplaceAll(out, err.Error(), "<NOT A DIRECTORY>")
		}
	}
	if home := os.Getenv("XDG_CONFIG_HOME"); home != "" {
		out = strings.ReplaceAll(out, home, "<CONFIG>")
	}
	if _, err := os.UserHomeDir(); err != nil {
		out = strings.ReplaceAll(out, err.Error(), "<NO HOME>")
	}
	if err := json.Unmarshal([]byte(corruptCredentials), new(map[string]any)); err != nil {
		out = strings.ReplaceAll(out, err.Error(), "<BAD JSON>")
	}
	return out
}

func pageCases() []pageCase {
	return []pageCase{
		{
			name: "no credentials file",
			want: `Version:      v1.2.3
Credentials:  (not found)
Logged in:    no
API server:   https://api.example.test/cloud
Site:         (none)
`,
			setup: func(t *testing.T) SiteClient {
				t.Chdir(t.TempDir())
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				return nil
			},
		},
		{
			name: "credentials file holding no key",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    no
API server:   https://api.example.test/cloud
Site:         (none)
`,
			setup: func(t *testing.T) SiteClient {
				t.Chdir(t.TempDir())
				home := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", home)
				dir := filepath.Join(home, "kamakiri")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"api_key":"","email":"file@example.com"}`), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return nil
			},
		},
		{
			name: "no config directory to look in",
			want: `Version:      v1.2.3
Credentials:  (error: determine config directory: <NO HOME>)
Logged in:    no
API server:   https://api.example.test/cloud
Site:         (none)
`,
			setup: func(t *testing.T) SiteClient {
				t.Chdir(t.TempDir())
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("HOME", "")
				return nil
			},
		},
		{
			name: "credentials file unreadable",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    (error: parse credentials: <BAD JSON>)
API server:   https://api.example.test/cloud
Site:         (none)
`,
			setup: func(t *testing.T) SiteClient {
				t.Chdir(t.TempDir())
				home := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", home)
				dir := filepath.Join(home, "kamakiri")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(corruptCredentials), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return nil
			},
		},
		{
			name: "credentials path under a file",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    (error: read credentials: <NOT A DIRECTORY>)
API server:   https://api.example.test/cloud
Site:         (none)
`,
			setup: func(t *testing.T) SiteClient {
				t.Chdir(t.TempDir())
				home := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", home)
				// A regular file where the config directory should be, so the
				// read of the credentials path fails with an error Go does not
				// map to os.ErrNotExist.
				if err := os.WriteFile(filepath.Join(home, "kamakiri"), []byte("x"), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return nil
			},
		},
		{
			name: "logged in under no email",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    yes
API server:   https://api.example.test/cloud
Site:         (none)
`,
			setup: func(t *testing.T) SiteClient {
				t.Chdir(t.TempDir())
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				if err := core.SaveCredentials("kk_live_test", ""); err != nil {
					t.Fatalf("SaveCredentials: %v", err)
				}
				return nil
			},
		},
		{
			name: "linked site with no client to read it",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Site:         site123
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return nil
			},
		},
		{
			name: "the whole page",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Subdomain:    my-site.kamakiri-pages.jp
Domain:       example.com
Domains:      2 additional
CDN:          webaccel
Sites:        2 / 5
Snapshots:    3 / 10 (this site)
Max deploy:   100 MB

Per-domain status:
  example.com       canonical     ✓ live  ✓ via WebAccel
  www.example.com   redirect 308  ✓ live  
  shop.example.com  alias         ✓ live  ✓ via WebAccel
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{
							ID:               id,
							Subdomain:        "my-site",
							SubdomainEnabled: true,
							Quota: &api.Quota{
								SitesUsed: 2, SitesMax: 5,
								SnapshotsUsed: 3, SnapshotsMax: 10,
								MaxSnapshotBytes: 100 * 1024 * 1024,
							},
						}, nil
					},
					listDomainsFn: func(string) (*api.DomainList, error) {
						return &api.DomainList{Domains: []api.Domain{
							{Domain: "example.com", Role: "canonical", State: "serving", CdnState: "active"},
							{Domain: "www.example.com", Role: "redirect", RedirectStatus: 308, State: "serving", CdnState: "off"},
							{Domain: "shop.example.com", Role: "alias", State: "serving", CdnState: "active"},
						}}, nil
					},
					cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
						return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel"}, nil
					},
				}
			},
		},
		{
			name: "a reconcile error over a flushing edge",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Subdomain:    my-site.kamakiri-pages.jp
Domain:       (none)
CDN:          none
Reconcile:    ✗ origin push rejected
Edge cache: still flushing (since 5m); our flush is failing (timeout), we keep retrying
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{
							ID:                   id,
							Subdomain:            "my-site",
							SubdomainEnabled:     true,
							Sync:                 &api.Sync{Outcome: "error", Error: "origin push rejected"},
							EdgePurgeHealth:      "failing",
							EdgePurgeErrorReason: "timeout",
							EdgePurgeLastOkAt:    time.Now().Add(-5*time.Minute - 30*time.Second).UTC().Format(time.RFC3339),
							Freshness: &api.Freshness{
								State: "pending",
								Axes:  api.FreshnessAxes{Origin: "pending", Edge: "flushing", Cdn: "flushed"},
							},
						}, nil
					},
					listDomainsFn: func(string) (*api.DomainList, error) {
						return &api.DomainList{}, nil
					},
				}
			},
		},
		{
			name: "a blocked flush under a still-converging site",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Subdomain:    my-site.kamakiri-pages.jp
Domain:       (none)
CDN:          none
⧗ still going live
✗ The WebAccel cache is not flushed: the stored credentials were rejected.
  Run kamakiri cdn credentials to update them; once they are accepted the flush retries and completes on its own.
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{
							ID:               id,
							Subdomain:        "my-site",
							SubdomainEnabled: true,
							Freshness: &api.Freshness{
								State: "pending",
								Axes:  api.FreshnessAxes{Origin: "pending", Edge: "flushed", Cdn: "blocked"},
								Blockers: []api.Blocker{
									{Provider: "webaccel", Host: "example.com", Reason: "credentials_rejected"},
								},
							},
						}, nil
					},
					listDomainsFn: func(string) (*api.DomainList, error) {
						return &api.DomainList{}, nil
					},
				}
			},
		},
		{
			name: "apex A records drifted",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Subdomain:    my-site.kamakiri-pages.jp
Domain:       example.com
CDN:          webaccel

Per-domain status:
  example.com  canonical  ✓ live  ✓ via WebAccel
  ✗ A records do not match the current edge for webaccel mode
    expected: 192.0.2.1, 192.0.2.2
    observed: 203.0.113.9
    → Update your A records at your registrar to the expected values above.
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{ID: id, Subdomain: "my-site", SubdomainEnabled: true}, nil
					},
					listDomainsFn: func(string) (*api.DomainList, error) {
						return &api.DomainList{Domains: []api.Domain{{
							Domain:                "example.com",
							Role:                  "canonical",
							State:                 "serving",
							CdnState:              "active",
							DnsMatchedAlternative: "a",
							DNSRecordsExpected: []api.DNSRecord{
								{Name: "example.com", Type: "a", Value: "192.0.2.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
								{Name: "example.com", Type: "a", Value: "192.0.2.2", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
							},
							DNSRecordsObserved: []api.DNSRecordObserved{
								{Name: "example.com", Type: "a", Values: []string{"203.0.113.9"}, AlternativeGroup: "apex_primary"},
							},
						}}}, nil
					},
					cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
						return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel"}, nil
					},
				}
			},
		},
		{
			name: "teardown queued",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Status:       ⧗ pending teardown
              (the edge may still be serving until the removal lands.)
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{ID: id, Status: "deleted", StatusObserved: "active"}, nil
					},
				}
			},
		},
		{
			name: "teardown in flight",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Status:       ⧗ tearing down (edge removed; DNS, CDN and storage in flight)
Subdomain:    my-site.kamakiri-pages.jp (disabled)
Domain:       example.com
CDN:          none

Per-domain status:
  example.com  canonical  ✓ live  
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{ID: id, Status: "deleted", StatusObserved: "soft_deleted", Subdomain: "my-site"}, nil
					},
					listDomainsFn: func(string) (*api.DomainList, error) {
						return &api.DomainList{Domains: []api.Domain{
							{Domain: "example.com", Role: "canonical", State: "serving", CdnState: "off"},
						}}, nil
					},
				}
			},
		},
		{
			name: "torn down",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Status:       ✗ deleted
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{ID: id, Status: "deleted", StatusObserved: "deleted"}, nil
					},
				}
			},
		},
		{
			name: "a lifecycle pair the contract does not allow",
			want: `Version:      v1.2.3
Credentials:  <CONFIG>/kamakiri/credentials.json
Logged in:    user@example.com
API server:   https://api.example.test/cloud
Status:       ✗ stuck (status="active" status_observed="soft_deleted", please file a bug)
`,
			setup: func(t *testing.T) SiteClient {
				linkedSite(t, "user@example.com")
				return &mockClient{
					getSiteFn: func(id string) (*api.Site, error) {
						return &api.Site{ID: id, Status: "active", StatusObserved: "soft_deleted"}, nil
					},
				}
			},
		},
	}
}

// The page is rendered whole and compared byte for byte, because that is the
// only assertion the layout cannot slip past: a substring check passes just as
// happily when a column moves, a gutter widens or a label starts looking up a
// different catalog key. The goldens pin the published English pages, so a
// difference is a regression rather than a decision.
func TestStatusPagesRenderToTheirGoldens(t *testing.T) {
	for _, tc := range pageCases() {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.setup(t)
			got := renderPage(t, client)
			if got == tc.want {
				return
			}
			gotLines, wantLines := strings.Split(got, "\n"), strings.Split(tc.want, "\n")
			for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
				g, w := "", ""
				if i < len(gotLines) {
					g = gotLines[i]
				}
				if i < len(wantLines) {
					w = wantLines[i]
				}
				if g != w {
					t.Fatalf("line %d differs:\n got: %q\nwant: %q\n\nwhole page:\n%s", i+1, g, w, got)
				}
			}
		})
	}
}

// labelArg reduces a label() argument, and an entry in labelTexts, to one
// token: the catalog key it looks up, or the name of the constant it is. Two
// spellings of the same label therefore compare equal, and anything this does
// not recognize comes back empty rather than silently counting as a match.
func labelArg(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return "const " + v.Name
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "T" || len(v.Args) != 1 {
			return ""
		}
		lit, ok := v.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return ""
		}
		key, err := strconv.Unquote(lit.Value)
		if err != nil {
			return ""
		}
		return "key " + key
	}
	return ""
}

// The left column is only as wide as the widest label labelTexts knows about,
// so a label rendered through label() but missing from that set does not widen
// the column: in whichever language it is the widest, it collides with the
// value beside it. Nothing in the type system ties the two together, so this
// reads the source and does.
func TestEveryRenderedLabelIsOneTheColumnIsSizedFor(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "status.go", nil, 0)
	if err != nil {
		t.Fatalf("parse status.go: %v", err)
	}

	sized := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "labelTexts" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, e := range lit.Elts {
				arg := labelArg(e)
				if arg == "" {
					t.Errorf("labelTexts holds an entry this test cannot read, at %s", fset.Position(e.Pos()))
					continue
				}
				sized[arg] = true
			}
			return false
		})
		return false
	})
	if len(sized) != len(labelTexts()) {
		t.Fatalf("read %d entries out of labelTexts, which returns %d", len(sized), len(labelTexts()))
	}

	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "label" || len(call.Args) != 1 {
			return true
		}
		calls++
		arg := labelArg(call.Args[0])
		if arg == "" {
			t.Errorf("the label() at %s takes an argument this test cannot read", fset.Position(call.Pos()))
			return true
		}
		if !sized[arg] {
			t.Errorf("label(%s) at %s renders a label the column is not sized for; add it to labelTexts",
				arg, fset.Position(call.Pos()))
		}
		return true
	})
	if calls == 0 {
		t.Fatal("found no label() call in status.go, so nothing here was checked")
	}
}
