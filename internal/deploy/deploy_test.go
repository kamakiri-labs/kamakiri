package deploy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// mockClient tells apart the two reasons Run reads the site. Every GetSite
// before Deploy is the pre-read that sets a directory's size limit, answered by
// preReadFn and recorded in preReads; every one after it is a freshness poll,
// answered by getSiteFn and counted in polls. Kept apart, a test can pin "read
// once before the upload, never polled" without one answer standing in for the
// other.
type mockClient struct {
	deployFn  func(config []byte, tarball io.Reader) (*api.DeployResult, error)
	getSiteFn func(id string) (*api.Site, error)
	// preReadFn left nil answers a site with no quota block, which is what an
	// older server returns, so the size check falls back to MaxTotalSize.
	preReadFn func(id string) (*api.Site, error)

	deployed bool
	preReads []string
	polls    int
}

func (m *mockClient) Deploy(config []byte, tarball io.Reader) (*api.DeployResult, error) {
	m.deployed = true
	return m.deployFn(config, tarball)
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	if !m.deployed {
		m.preReads = append(m.preReads, id)
		if m.preReadFn == nil {
			return &api.Site{ID: id}, nil
		}
		return m.preReadFn(id)
	}
	m.polls++
	if m.getSiteFn == nil {
		return nil, errors.New("GetSite: no mock configured")
	}
	return m.getSiteFn(id)
}

// assertReadOnceBeforeDeploy checks the run read the linked site exactly once,
// before any upload, and made no further reads.
func assertReadOnceBeforeDeploy(t *testing.T, m *mockClient) {
	t.Helper()
	if len(m.preReads) != 1 || m.preReads[0] != "site123" {
		t.Errorf("site reads before Deploy = %q, want exactly one, of site123", m.preReads)
	}
	if m.polls != 0 {
		t.Errorf("site reads after Deploy = %d, want none on this path", m.polls)
	}
}

// flunkGetSite fails the test if GetSite is called at all. As getSiteFn it
// forbids the freshness wait's polls; as preReadFn, the read before packing.
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

func createFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDeployHappyPath drives the wait from pending to fresh and pins the
// ordering: the deploy id lands before the wait's own lines, and "✓ live" only
// on fresh. It really sleeps one 2s poll cadence, since the exported Wait takes
// no schedule.
func TestDeployHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{
		"index.html": "<html>hello</html>",
		"style.css":  "body {}",
	})

	calls := 0
	client := &mockClient{
		deployFn: func(config []byte, tarball io.Reader) (*api.DeployResult, error) {
			if !strings.Contains(string(config), "site123") {
				t.Errorf("config missing site ID: %s", config)
			}
			if _, err := io.ReadAll(tarball); err != nil {
				t.Errorf("reading tarball: %v", err)
			}
			return &api.DeployResult{
				URL:      "https://shop.example.com",
				DeployID: "20260401-120000-000",
			}, nil
		},
		getSiteFn: func(id string) (*api.Site, error) {
			if id != "site123" {
				t.Errorf("GetSite id = %q, want site123 (from local config)", id)
			}
			calls++
			if calls == 1 {
				return &api.Site{Freshness: &api.Freshness{
					State:   "pending",
					Pending: []api.PendingTarget{{Axis: "edge"}},
				}}, nil
			}
			return freshSite("https://shop.example.com"), nil
		},
	}

	var stdout bytes.Buffer
	if err := Run(client, dir, false, &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	out := stdout.String()

	// The run's whole narrative, compared as one block. Every line of it is a
	// separate catalog lookup, and a substring check on one phrase passes just as
	// happily when a neighbouring line reads the wrong key or the lines swap
	// order. Only the measured upload duration and the tarball's compressed size
	// are masked, since neither is the same twice.
	masked := regexp.MustCompile(`\(\d+\.\d+s\)`).ReplaceAllString(out, "(Ns)")
	masked = regexp.MustCompile(`\(\d+\.\d+ KB\)`).ReplaceAllString(masked, "(N KB)")
	wantBlock := "Uploading 2 files (N KB)... done (Ns).\n" +
		"Deploying...\n" +
		"\n" +
		"Deploy: 20260401-120000-000\n" +
		"  ⧗ publishing…\n" +
		"✓ live: https://shop.example.com\n"
	if masked != wantBlock {
		t.Errorf("deploy narrative =\n%q\nwant:\n%q", masked, wantBlock)
	}

	if len(client.preReads) != 1 || client.preReads[0] != "site123" {
		t.Errorf("site reads before Deploy = %q, want exactly one, of site123", client.preReads)
	}
	if client.polls == 0 {
		t.Error("GetSite was not called on the wait path")
	}
}

// A terminal flush block still leaves the user the deploy id, and must never
// print "✓ live".
func TestDeployBlockedExitsWithEntry(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{"index.html": "<html></html>"})

	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			io.ReadAll(tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: func(string) (*api.Site, error) {
			return blockedSite(api.Blocker{
				Provider: "webaccel", Host: "shop.example.com", Reason: "credentials_rejected",
			}), nil
		},
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if !errors.Is(err, freshness.ErrFlushBlocked) {
		t.Fatalf("Run() err = %v, want ErrFlushBlocked", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Deploy: 20260401-120000-000") {
		t.Errorf("trailer must print before the wait, even on a blocked exit: %q", out)
	}
	if !strings.Contains(out, "✗ Deployed, but the WebAccel cache was not flushed: the stored credentials were rejected.") {
		t.Errorf("output missing the blocked entry: %q", out)
	}
	if !strings.Contains(out, "Run kamakiri cdn credentials to update them") {
		t.Errorf("output missing the remediation line: %q", out)
	}
	if strings.Contains(out, "✓ live") {
		t.Errorf("a blocked deploy must not report live: %q", out)
	}
}

// --no-wait must not poll, and its URL echo names the site's address without
// claiming the site is live.
func TestDeployNoWait(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{"index.html": "<html></html>"})

	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			io.ReadAll(tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	if err := Run(client, dir, true, &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Deploying... queued (--no-wait).") {
		t.Errorf("output missing 'queued (--no-wait)': %q", out)
	}
	if !strings.Contains(out, "Deploy: 20260401-120000-000") {
		t.Errorf("output missing deploy ID: %q", out)
	}
	if !strings.Contains(out, "→ https://shop.example.com") {
		t.Errorf("output missing the URL echo: %q", out)
	}
	if !strings.Contains(out, "Publishing continues in the background; check kamakiri status.") {
		t.Errorf("output missing the converging clause: %q", out)
	}
	if strings.Contains(out, "Cache purged.") {
		t.Errorf("output should NOT contain 'Cache purged.': %q", out)
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployUploadFailureSkipsWait(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{"index.html": "<html></html>"})

	want := errors.New("connection refused")
	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			return nil, want
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	if err := Run(client, dir, false, &stdout); err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployArchive(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	archivePath := filepath.Join(".", "dist.tar.gz")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte("fake tarball content"))
	gw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			io.ReadAll(tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: func(string) (*api.Site, error) {
			return freshSite("https://shop.example.com"), nil
		},
	}

	var stdout bytes.Buffer
	if err := Run(client, archivePath, false, &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Uploading dist.tar.gz") {
		t.Errorf("output missing archive name: %q", out)
	}
	if !strings.Contains(out, "✓ live: https://shop.example.com") {
		t.Errorf("output missing the live milestone: %q", out)
	}
}

func TestDeployNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called")
			return nil, nil
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, ".", false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %q, want 'not logged in'", err.Error())
	}
}

func TestDeployNoConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called")
			return nil, nil
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, ".", false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no site linked") {
		t.Errorf("error = %q, want 'no site linked'", err.Error())
	}
}

func TestDeployPathNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called")
			return nil, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, "/nonexistent/path", false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "path not found") {
		t.Errorf("error = %q, want 'path not found'", err.Error())
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployUnsupportedFile(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	zipPath := filepath.Join(".", "dist.zip")
	if err := os.WriteFile(zipPath, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called")
			return nil, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, zipPath, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "only .tar.gz and .tgz archives are supported") {
		t.Errorf("error = %q, want 'only .tar.gz and .tgz'", err.Error())
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployEmptyDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)

	// Only a root dotfile, which the packer skips, so the directory packs to
	// nothing.
	createFiles(t, dir, map[string]string{
		".hidden": "hidden file",
	})

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called")
			return nil, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "directory is empty") {
		t.Errorf("error = %q, want 'directory is empty'", err.Error())
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{"index.html": "<html></html>"})

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("error = %q, want 'invalid API key'", err.Error())
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeploySiteNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{"index.html": "<html></html>"})

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "site not found") {
		t.Errorf("error = %q, want 'site not found'", err.Error())
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployLintsRedirectsBeforeUpload(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{
		"index.html": "<html></html>",
		"_redirects": "/old no-slash-destination\n",
	})

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called when the lint fails")
			return nil, errors.New("flunk")
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected lint error, got nil")
	}
	if !strings.Contains(err.Error(), "to must start with /") {
		t.Errorf("error = %q, want the line-numbered lint message", err.Error())
	}
}

func TestDeployIncludesRedirectsAndOmitsConfigKey(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{
		"index.html": "<html></html>",
		"_redirects": "/old/ /new/ 301\n",
	})

	client := &mockClient{
		deployFn: func(config []byte, tarball io.Reader) (*api.DeployResult, error) {
			if strings.Contains(string(config), "redirects") {
				t.Errorf("config should not carry a redirects key: %s", config)
			}
			entries := extractTarballEntries(t, tarball)
			found := false
			for _, name := range entries {
				if name == "_redirects" {
					found = true
				}
			}
			if !found {
				t.Errorf("tarball entries %v missing _redirects (CLI must not strip it)", entries)
			}
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	if err := Run(client, dir, true, &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployArchiveSkipsLint(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	src := t.TempDir()
	createFiles(t, src, map[string]string{
		"index.html": "<html></html>",
		"_redirects": "/old no-slash-destination\n",
	})
	archive := writeArchive(t, src)

	deployed := false
	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			deployed = true
			io.Copy(io.Discard, tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	if err := Run(client, archive, true, &stdout); err != nil {
		t.Fatalf("Run(archive) error = %v, want nil (lint skipped for archives)", err)
	}
	if !deployed {
		t.Error("Deploy was not called; the archive lint-skip path did not reach upload")
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployLintsHeadersBeforeUpload(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{
		"index.html": "<html></html>",
		"_headers":   "/*\n  Set-Cookie: a=b\n",
	})

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called when the _headers lint fails")
			return nil, errors.New("flunk")
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected lint error, got nil")
	}
	if !strings.Contains(err.Error(), "Set-Cookie is not supported") {
		t.Errorf("error = %q, want the line-numbered _headers lint message", err.Error())
	}
}

func TestDeployIncludesHeadersOmitsConfigKeyAndWarnsOnDrop(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{
		"index.html": "<html></html>",
		"_headers":   "/*\n  X-Custom: v\n  Server: leak\n",
	})

	client := &mockClient{
		deployFn: func(config []byte, tarball io.Reader) (*api.DeployResult, error) {
			if strings.Contains(string(config), "headers") {
				t.Errorf("config should not carry a headers key: %s", config)
			}
			entries := extractTarballEntries(t, tarball)
			found := false
			for _, name := range entries {
				if name == "_headers" {
					found = true
				}
			}
			if !found {
				t.Errorf("tarball entries %v missing _headers (CLI must not strip it)", entries)
			}
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	if err := Run(client, dir, true, &stdout); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Server is managed by the platform and was dropped.") {
		t.Errorf("stdout = %q, want the dropped-header warning", stdout.String())
	}
	assertReadOnceBeforeDeploy(t, client)
}

func TestDeployArchiveSkipsHeadersLint(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	src := t.TempDir()
	createFiles(t, src, map[string]string{
		"index.html": "<html></html>",
		"_headers":   "/*\n  Set-Cookie: a=b\n",
	})
	archive := writeArchive(t, src)

	deployed := false
	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			deployed = true
			io.Copy(io.Discard, tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
	}

	var stdout bytes.Buffer
	if err := Run(client, archive, true, &stdout); err != nil {
		t.Fatalf("Run(archive) error = %v, want nil (lint skipped for archives)", err)
	}
	if !deployed {
		t.Error("Deploy was not called; the archive lint-skip path did not reach upload")
	}
	assertReadOnceBeforeDeploy(t, client)
}

func quotaSite(maxSnapshotBytes int64) *api.Site {
	return &api.Site{ID: "site123", Quota: &api.Quota{MaxSnapshotBytes: maxSnapshotBytes}}
}

// A directory over the plan's per-deploy limit is refused on the machine, before
// anything is uploaded, and the line names the limit the site read gave.
func TestDeployRefusesADirectoryOverThePlanLimit(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{
		"index.html": "<html></html>",
		"big.bin":    strings.Repeat("x", 1<<20),
	})

	client := &mockClient{
		deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
			t.Error("Deploy should not be called for a directory over the limit")
			return nil, errors.New("flunk")
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: func(string) (*api.Site, error) { return quotaSite(1 << 20), nil },
	}

	var stdout bytes.Buffer
	err := Run(client, dir, false, &stdout)
	if err == nil {
		t.Fatal("Run() expected the size refusal, got nil")
	}
	if want := "upload exceeds 1 MB limit. Reduce your build output and try again"; err.Error() != want {
		t.Errorf("Run() error = %q, want %q", err.Error(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: the refusal comes before the upload line", stdout.String())
	}
	assertReadOnceBeforeDeploy(t, client)
}

// A directory under the plan's limit uploads, which a check against the fixed
// ceiling alone would also allow; together with the refusal above it shows the
// limit comes from the site read.
func TestDeployUploadsADirectoryUnderThePlanLimit(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)
	createFiles(t, dir, map[string]string{"index.html": strings.Repeat("x", 1024)})

	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			io.Copy(io.Discard, tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: func(string) (*api.Site, error) { return quotaSite(1024), nil },
	}

	var stdout bytes.Buffer
	if err := Run(client, dir, true, &stdout); err != nil {
		t.Fatalf("Run() error = %v, want nil at exactly the limit", err)
	}
	if !client.deployed {
		t.Error("Deploy was not called for a directory at the limit")
	}
	assertReadOnceBeforeDeploy(t, client)
}

// When the site read yields no usable limit, the deploy goes ahead against the
// fixed ceiling rather than failing: the server applies the plan's limit to the
// upload either way. A zero or negative limit is treated as absent, since a
// quota block without the field decodes to zero.
func TestDeployFallsBackToTheFixedCeiling(t *testing.T) {
	cases := []struct {
		name string
		read func(string) (*api.Site, error)
	}{
		{"read fails in transport", func(string) (*api.Site, error) { return nil, errors.New("connection refused") }},
		{"read gets a 500", func(string) (*api.Site, error) {
			return nil, errors.New(i18n.Tf("api.err_server_returned", 500))
		}},
		{"read gets a coded 5xx", func(string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "internal_error", Message: "Internal server error."}
		}},
		{"read is forbidden", func(string) (*api.Site, error) {
			return nil, &api.ErrorResponse{Code: "forbidden", Message: "You do not own this site."}
		}},
		{"no quota block", func(string) (*api.Site, error) { return &api.Site{ID: "site123"}, nil }},
		{"zero limit", func(string) (*api.Site, error) { return quotaSite(0), nil }},
		{"negative limit", func(string) (*api.Site, error) { return quotaSite(-1), nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProject(t)

			dir := filepath.Join(".", "dist")
			os.MkdirAll(dir, 0755)
			createFiles(t, dir, map[string]string{"index.html": strings.Repeat("x", 2048)})

			client := &mockClient{
				deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
					io.Copy(io.Discard, tarball)
					return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
				},
				getSiteFn: flunkGetSite(t),
				preReadFn: tc.read,
			}

			var stdout bytes.Buffer
			if err := Run(client, dir, true, &stdout); err != nil {
				t.Fatalf("Run() error = %v, want the deploy to go ahead", err)
			}
			if !client.deployed {
				t.Error("Deploy was not called")
			}
			assertReadOnceBeforeDeploy(t, client)

			site, _ := tc.read("site123")
			if got := sizeLimit(site); got != MaxTotalSize {
				t.Errorf("sizeLimit() = %d, want MaxTotalSize (%d)", got, int64(MaxTotalSize))
			}
		})
	}
}

// A positive limit from the site read is used as given, above the fixed ceiling
// as well as below it: the ceiling is only the stand-in for a missing limit.
func TestSizeLimitUsesThePlanLimit(t *testing.T) {
	for _, limit := range []int64{1, 100 << 20, 2 * MaxTotalSize} {
		if got := sizeLimit(quotaSite(limit)); got != limit {
			t.Errorf("sizeLimit(quota %d) = %d, want %d", limit, got, limit)
		}
	}
}

// An archive still gets the site read, which every deploy makes, but it is
// never measured against the limit: that would mean decompressing it, and the
// server checks it anyway.
func TestDeployArchiveReadsTheSiteButIsNotSizeChecked(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	src := t.TempDir()
	createFiles(t, src, map[string]string{"index.html": strings.Repeat("x", 4096)})
	archive := writeArchive(t, src)

	client := &mockClient{
		deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
			io.Copy(io.Discard, tarball)
			return &api.DeployResult{URL: "https://shop.example.com", DeployID: "20260401-120000-000"}, nil
		},
		getSiteFn: flunkGetSite(t),
		preReadFn: func(string) (*api.Site, error) { return quotaSite(1), nil },
	}

	var stdout bytes.Buffer
	if err := Run(client, archive, true, &stdout); err != nil {
		t.Fatalf("Run(archive) error = %v, want nil (archives are not size-checked)", err)
	}
	if !client.deployed {
		t.Error("Deploy was not called for the archive")
	}
	assertReadOnceBeforeDeploy(t, client)
}

// The refusal names the limit the way `kamakiri status` does: a whole number
// of MB with no decimal, one decimal otherwise. Each directory holds a single
// sparse file one byte over the limit, which the packer refuses from its size
// alone, before opening it.
func TestDeployTooLargeNamesTheLimitAsTheServerDoes(t *testing.T) {
	cases := []struct {
		name  string
		read  func(string) (*api.Site, error)
		limit int64
		want  string
	}{
		{"plan limit of 100 MiB", func(string) (*api.Site, error) { return quotaSite(100 << 20), nil }, 100 << 20, "100 MB"},
		{"fallback", func(string) (*api.Site, error) { return &api.Site{ID: "site123"}, nil }, MaxTotalSize, "500 MB"},
		{"plan limit of 1.5 MiB", func(string) (*api.Site, error) { return quotaSite(3 << 19), nil }, 3 << 19, "1.5 MB"},
		{"plan limit of 512 KiB", func(string) (*api.Site, error) { return quotaSite(512 << 10), nil }, 512 << 10, "0.5 MB"},
		{"plan limit of 0.25 MiB rounds half up", func(string) (*api.Site, error) { return quotaSite(1 << 18), nil }, 1 << 18, "0.3 MB"},
		{"plan limit just under 1 MiB", func(string) (*api.Site, error) { return quotaSite(1<<20 - 1), nil }, 1<<20 - 1, "1 MB"},
		{"plan limit of 40,000 bytes", func(string) (*api.Site, error) { return quotaSite(40000), nil }, 40000, "0 MB"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProject(t)

			dir := filepath.Join(".", "dist")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(dir, "big.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(tc.limit + 1); err != nil {
				t.Fatal(err)
			}
			f.Close()

			client := &mockClient{
				deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
					t.Error("Deploy should not be called for a directory over the limit")
					return nil, errors.New("flunk")
				},
				getSiteFn: flunkGetSite(t),
				preReadFn: tc.read,
			}

			var stdout bytes.Buffer
			err = Run(client, dir, false, &stdout)
			want := "upload exceeds " + tc.want + " limit. Reduce your build output and try again"
			if err == nil || err.Error() != want {
				t.Errorf("Run() error = %v, want %q", err, want)
			}
			assertReadOnceBeforeDeploy(t, client)
		})
	}
}

// A site whose desired status is deleted is being torn down, or was and still
// has this directory linked because a teardown failed. Either way nothing may
// be uploaded to it: the refusal comes straight after the site read, before
// the path is even looked at, for a directory and an archive alike.
func TestDeployRefusesATornDownSite(t *testing.T) {
	const want = `this site is being torn down or has been torn down. Run "kamakiri teardown" to retry or finish the teardown`

	cases := []struct {
		name string
		path func(t *testing.T) string
	}{
		{"directory", func(t *testing.T) string {
			dir := filepath.Join(".", "dist")
			createFiles(t, dir, map[string]string{"index.html": "<html></html>"})
			return dir
		}},
		{"archive", func(t *testing.T) string {
			src := t.TempDir()
			createFiles(t, src, map[string]string{"index.html": "<html></html>"})
			return writeArchive(t, src)
		}},
		// A path that does not exist still gets the torn-down line, which is
		// what shows the guard runs before anything is opened or packed.
		{"missing path", func(*testing.T) string { return "/nonexistent/path" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProject(t)
			path := tc.path(t)

			client := &mockClient{
				deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
					t.Error("Deploy should not be called for a torn-down site")
					return nil, errors.New("flunk")
				},
				getSiteFn: flunkGetSite(t),
				preReadFn: func(id string) (*api.Site, error) {
					// A quota of one byte would refuse any directory had it
					// been packed, so the torn-down line also proves it was not.
					site := quotaSite(1)
					site.Status = "deleted"
					site.StatusObserved = "active"
					return site, nil
				},
			}

			var stdout bytes.Buffer
			err := Run(client, path, false, &stdout)
			if err == nil || err.Error() != want {
				t.Errorf("Run() error = %v, want %q", err, want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing before the refusal", stdout.String())
			}
			assertReadOnceBeforeDeploy(t, client)
		})
	}
}

// An answer to the site read that the upload would get too (a rejected key, a
// refused CLI version, a site that does not exist) ends the deploy there, with
// the error the upload would have produced, instead of packing and uploading
// only to be told the same thing.
func TestDeployStopsOnAConclusiveSiteRead(t *testing.T) {
	cases := []struct {
		name   string
		answer error
	}{
		{"unauthorized", &api.ErrorResponse{Code: "unauthorized", Message: "Unauthorized."}},
		{"upgrade required", api.ErrUpgradeRequired},
		{"site not found", &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProject(t)

			// The same answer from the upload, after a clean read, is the
			// reference the read's refusal has to match.
			dir := filepath.Join(".", "dist")
			createFiles(t, dir, map[string]string{"index.html": "<html></html>"})
			ref := &mockClient{
				deployFn: func(_ []byte, tarball io.Reader) (*api.DeployResult, error) {
					io.Copy(io.Discard, tarball)
					return nil, tc.answer
				},
				getSiteFn: flunkGetSite(t),
			}
			wantErr := Run(ref, dir, false, io.Discard)
			if wantErr == nil {
				t.Fatal("reference Run() through Deploy returned nil")
			}

			// A missing path shows the read's refusal comes before packing.
			client := &mockClient{
				deployFn: func(_ []byte, _ io.Reader) (*api.DeployResult, error) {
					t.Error("Deploy should not be called after a conclusive site read")
					return nil, errors.New("flunk")
				},
				getSiteFn: flunkGetSite(t),
				preReadFn: func(string) (*api.Site, error) { return nil, tc.answer },
			}
			var stdout bytes.Buffer
			err := Run(client, "/nonexistent/path", false, &stdout)
			if err == nil || err.Error() != wantErr.Error() {
				t.Errorf("Run() error = %v, want %q, the error the upload gives", err, wantErr)
			}
			// A version refusal is told apart from other failures by its
			// sentinel, so the read has to hand back the value itself, as the
			// upload does, not an error carrying the same text.
			if tc.answer == api.ErrUpgradeRequired && (wantErr != api.ErrUpgradeRequired || err != api.ErrUpgradeRequired) {
				t.Errorf("Run() error = %#v through the read and %#v through the upload, want the upgrade sentinel from both", err, wantErr)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing before the refusal", stdout.String())
			}
			assertReadOnceBeforeDeploy(t, client)
		})
	}
}

func writeArchive(t *testing.T, srcDir string) string {
	t.Helper()
	r, _, err := Create(srcDir, MaxTotalSize)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	path := filepath.Join(t.TempDir(), "deploy.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		t.Fatal(err)
	}
	return path
}
