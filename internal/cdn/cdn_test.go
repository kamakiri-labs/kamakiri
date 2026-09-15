package cdn

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
	setCDNFn         func(siteID, mode string) (*api.CDNSetResponse, error)
	setCDNWebAccelFn func(siteID, token, secret string) (*api.CDNSetResponse, error)
	rotateCredsFn    func(siteID, token, secret string) (*api.CDNSetResponse, error)
	cleanupCDNFn     func(siteID string) (*api.CDNCleanupResponse, error)
	cdnStatusFn      func(siteID string) (*api.CDNStatusResponse, error)
	purgeCDNFn       func(siteID string) (*api.CDNPurgeResponse, error)
	getSiteFn        func(id string) (*api.Site, error)
	waitForSyncFn    func(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
	listDomainsFn    func(siteID string) (*api.DomainList, error)
}

func (m *mockClient) SetCDN(siteID, mode string) (*api.CDNSetResponse, error) {
	return m.setCDNFn(siteID, mode)
}

func (m *mockClient) CleanupCDN(siteID string) (*api.CDNCleanupResponse, error) {
	return m.cleanupCDNFn(siteID)
}

func (m *mockClient) SetCDNWebAccel(siteID, token, secret string) (*api.CDNSetResponse, error) {
	return m.setCDNWebAccelFn(siteID, token, secret)
}

func (m *mockClient) RotateCDNCredentials(siteID, token, secret string) (*api.CDNSetResponse, error) {
	return m.rotateCredsFn(siteID, token, secret)
}

func (m *mockClient) CDNStatus(siteID string) (*api.CDNStatusResponse, error) {
	if m.cdnStatusFn == nil {
		// Several paths make a best-effort status read they tolerate any answer
		// to, so an empty frame spares their tests from wiring one up.
		return &api.CDNStatusResponse{}, nil
	}
	return m.cdnStatusFn(siteID)
}

func (m *mockClient) PurgeCDN(siteID string) (*api.CDNPurgeResponse, error) {
	return m.purgeCDNFn(siteID)
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	if m.getSiteFn == nil {
		return nil, errors.New("GetSite: no mock configured")
	}
	return m.getSiteFn(id)
}

func (m *mockClient) WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error) {
	if m.waitForSyncFn == nil {
		return &api.Site{ID: siteID}, nil
	}
	return m.waitForSyncFn(siteID, sinceAttemptID, timeout)
}

// The exits type-assert their client to the domain package's interface to run
// the reverse cutover, so this mock has to satisfy that whole interface too.
// Only ListDomains below carries behaviour; the rest exist to conform.

func (m *mockClient) ListDomains(siteID string) (*api.DomainList, error) {
	if m.listDomainsFn == nil {
		return &api.DomainList{}, nil
	}
	return m.listDomainsFn(siteID)
}

func (m *mockClient) SetCanonical(siteID, domainName string) (*api.Domain, error) {
	return nil, nil
}

func (m *mockClient) UnsetCanonical(siteID string) (*api.UnsetCanonicalResult, error) {
	return nil, nil
}

func (m *mockClient) AddDomain(siteID, domainName, role string, redirectStatus int) (*api.Domain, error) {
	return nil, nil
}

func (m *mockClient) RemoveDomain(domainName string) (*api.RemoveDomainResult, error) {
	return nil, nil
}

func (m *mockClient) RecheckDomain(domainName string) (*api.Domain, error) {
	return nil, nil
}

func (m *mockClient) RegisterDomain(string) (*api.Registration, error) { return nil, nil }
func (m *mockClient) UnregisterDomain(string) error                    { return nil }
func (m *mockClient) ListRegistrations() (*api.RegistrationList, error) {
	return &api.RegistrationList{}, nil
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

func TestStatusNone(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(siteID string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none", Provider: "none"}, nil
		},
	}

	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CDN:  none") {
		t.Errorf("output = %q", out.String())
	}
}

func TestStatusNoneSurfacesCleanupOrphans(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none", CleanupOrphanCount: 2, CleanupBlockedCount: 1}, nil
		},
	}

	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"CDN:  none",
		"2 CDN resource(s) awaiting cleanup; run `kamakiri cdn cleanup`.",
		"1 blocked: credentials no longer valid; remove them in your Sakura panel.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %q", want, got)
		}
	}
}

func TestStatusCloudflare(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(siteID string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{
				CDNMode:  "cloudflare",
				Provider: "cloudflare",
				Domains:  []api.CDNDomainStatus{{Domain: "example.com"}},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	if !strings.Contains(output, "CDN:  cloudflare") {
		t.Errorf("output = %q", output)
	}
}

func TestStatusWebAccelShowsCDNID(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(_ string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{
				CDNMode:  "webaccel",
				Provider: "webaccel",
				Domains: []api.CDNDomainStatus{
					{Domain: "altstack.jp", CDNID: "1001"},
				},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	if !strings.Contains(output, "CDN:  webaccel") {
		t.Errorf("missing webaccel label, output = %q", output)
	}
	if !strings.Contains(output, "altstack.jp") {
		t.Errorf("missing domain, output = %q", output)
	}
	if !strings.Contains(output, "1001") {
		t.Errorf("missing cdn_id, output = %q", output)
	}
}

// A failing token gates purge, probe and rotate but not the edge, so the rows
// below the site-level note must still show the domains serving.
func TestStatusWebAccelCredentialsFailing(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{
				CDNMode:                    "webaccel",
				Provider:                   "webaccel",
				CdnCredentialsHealth:       "failing",
				CdnCredentialsErrorReason:  "http_401",
				CdnCredentialsLastFailedAt: time.Now().Add(-4 * time.Minute).Format(time.RFC3339),
				Domains: []api.CDNDomainStatus{
					{Domain: "example.com", CDNID: "1001", CdnState: CdnStateActive},
				},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// Both lines are matched whole, trailing newline included, so that a change
	// to the wording fails here rather than sliding past a loose substring.
	if !strings.Contains(got, "  WebAccel API credentials are failing (last checked ~4m ago).\n") {
		t.Errorf("creds-failing line does not match the expected transcript: %q", got)
	}
	if !strings.Contains(got, "  Run `kamakiri cdn credentials` to update them.\n") {
		t.Errorf("missing exact remediation line: %q", got)
	}
	if strings.Contains(got, "only gates") || strings.Contains(got, "edge keeps serving") {
		t.Errorf("the gate clause belongs in prose, not in the CLI line: %q", got)
	}
	if !strings.Contains(got, "via WebAccel") {
		t.Errorf("per-domain row should still show via WebAccel: %q", got)
	}
	if strings.Contains(got, "Purges:") {
		t.Errorf("creds-failing must not involve the purge axis: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestStatusNoProject(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{}
	var out bytes.Buffer
	err := Status(client, &out)
	if err == nil {
		t.Fatal("expected error")
	}
}

// The credential-health age floor rounds a fresh failure up to a minute rather
// than reporting a number the observation cannot support, and renders it through
// the ordinary elapsed copy.
func TestCredentialsHealthAgeFloorRendersThroughItsOwnCopy(t *testing.T) {
	var out bytes.Buffer
	WriteCredentialsHealthLine(&out, &api.CDNStatusResponse{
		CdnCredentialsHealth:       "failing",
		CdnCredentialsLastFailedAt: time.Now().UTC().Format(time.RFC3339),
	}, "")
	want := i18n.Tf("cdn.credentials_failing_aged", i18n.Tf("cdn.elapsed_minutes", 1))
	if !strings.Contains(out.String(), want) {
		t.Errorf("credentials line = %q, want it to carry %q", out.String(), want)
	}
}
