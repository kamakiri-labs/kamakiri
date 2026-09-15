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

type mockClient struct {
	deployFn   func(config []byte, tarball io.Reader) (*api.DeployResult, error)
	getSiteFn  func(id string) (*api.Site, error)
	getSiteHit bool
}

func (m *mockClient) Deploy(config []byte, tarball io.Reader) (*api.DeployResult, error) {
	return m.deployFn(config, tarball)
}

func (m *mockClient) GetSite(id string) (*api.Site, error) {
	m.getSiteHit = true
	if m.getSiteFn == nil {
		return nil, errors.New("GetSite: no mock configured")
	}
	return m.getSiteFn(id)
}

// flunkGetSite fails the test if the freshness wait polls at all.
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

	if !client.getSiteHit {
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
	if client.getSiteHit {
		t.Error("GetSite was called on the --no-wait path")
	}
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
	if client.getSiteHit {
		t.Error("GetSite was called even though Deploy returned an error")
	}
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
}

func TestDeployEmptyDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	dir := filepath.Join(".", "dist")
	os.MkdirAll(dir, 0755)

	// Only dotfiles, which the packer skips, so the directory packs to nothing.
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
}

func writeArchive(t *testing.T, srcDir string) string {
	t.Helper()
	r, _, err := Create(srcDir)
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
