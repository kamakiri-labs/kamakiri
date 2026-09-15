package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// TestRequireCredentials pins the three branches of RequireCredentials, the
// nil branch twice, over no file and over a file that holds no key, since the
// load makes no difference between them: it is the credential half of every
// site-scoped command's check, and the three account-scoped domain verbs call
// it directly, with no dispatch test of their own reaching its load-error
// branch, since the dispatcher's own load exits before RequireProject or
// RequireCredentials ever runs.
func TestRequireCredentials(t *testing.T) {
	t.Run("a key from the environment", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(EnvAPIKey, "kk_live_fromenv")

		if err := RequireCredentials(); err != nil {
			t.Fatalf("RequireCredentials() error = %v", err)
		}
	})

	t.Run("nothing anywhere", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())

		err := RequireCredentials()
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if err.Error() != i18n.T("common.err_not_logged_in") {
			t.Errorf("error = %q, want %q", err.Error(), i18n.T("common.err_not_logged_in"))
		}
	})

	t.Run("an unparsable file", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}

		err := RequireCredentials()
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "parse credentials") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "parse credentials")
		}
	})

	t.Run("a file holding no key", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"api_key":""}`), 0600); err != nil {
			t.Fatal(err)
		}

		err := RequireCredentials()
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if err.Error() != i18n.T("common.err_not_logged_in") {
			t.Errorf("error = %q, want %q", err.Error(), i18n.T("common.err_not_logged_in"))
		}
	})
}

func TestProjectPath(t *testing.T) {
	path := ProjectPath()
	want := filepath.Join(".kamakiri", "config.json")
	if path != want {
		t.Errorf("ProjectPath() = %q, want %q", path, want)
	}
}

func TestSaveAndLoadProject(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	config := &ProjectConfig{
		Version: 1,
		Kind:    "pages",
		ID:      "abc123",
	}

	err := SaveProject(config)
	if err != nil {
		t.Fatalf("SaveProject() error = %v", err)
	}

	loaded, err := LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadProject() returned nil")
	}
	if loaded.Version != 1 {
		t.Errorf("version = %d, want 1", loaded.Version)
	}
	if loaded.Kind != "pages" {
		t.Errorf("kind = %q, want pages", loaded.Kind)
	}
	if loaded.ID != "abc123" {
		t.Errorf("id = %q, want abc123", loaded.ID)
	}
}

func TestLoadProjectMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	config, err := LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if config != nil {
		t.Errorf("expected nil, got %+v", config)
	}
}

func TestSaveProjectCreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	config := &ProjectConfig{Version: 1, Kind: "pages", ID: "test"}
	if err := SaveProject(config); err != nil {
		t.Fatalf("SaveProject() error = %v", err)
	}

	path := filepath.Join(tmpDir, ".kamakiri", "config.json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created at %s", path)
	}
}

func TestSaveProjectJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	config := &ProjectConfig{Version: 1, Kind: "pages", ID: "abc"}
	SaveProject(config)

	path := filepath.Join(tmpDir, ".kamakiri", "config.json")
	data, _ := os.ReadFile(path)

	var raw map[string]any
	json.Unmarshal(data, &raw)

	if raw["version"] != float64(1) {
		t.Errorf("version = %v, want 1", raw["version"])
	}
	if raw["kind"] != "pages" {
		t.Errorf("kind = %v, want pages", raw["kind"])
	}
	if raw["id"] != "abc" {
		t.Errorf("id = %v, want abc", raw["id"])
	}
}

func TestSaveProjectOmitsRedirectsKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	config := &ProjectConfig{Version: 1, Kind: "pages", ID: "abc"}
	SaveProject(config)

	path := filepath.Join(tmpDir, ".kamakiri", "config.json")
	data, _ := os.ReadFile(path)

	var raw map[string]any
	json.Unmarshal(data, &raw)

	// The config carries no `redirects` key: those live in the deploy-root
	// `_redirects` file.
	if _, ok := raw["redirects"]; ok {
		t.Errorf("config.json should not contain a redirects key, got %v", raw["redirects"])
	}
}

func TestLoadProjectIgnoresStaleRedirectsKey(t *testing.T) {
	t.Chdir(t.TempDir())
	os.MkdirAll(".kamakiri", 0755)

	// A config carrying the key must still load, dropping it.
	old := `{"version":1,"kind":"pages","id":"abc","redirects":{"paths":[{"from":"/a","to":"/b","status":301}]}}`
	os.WriteFile(".kamakiri/config.json", []byte(old), 0644)

	loaded, err := LoadProject()
	if err != nil {
		t.Fatalf("LoadProject() error = %v", err)
	}
	if loaded == nil || loaded.ID != "abc" {
		t.Fatalf("LoadProject() = %+v, want id abc", loaded)
	}
}

func TestLoadProjectMalformedJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	os.MkdirAll(".kamakiri", 0755)
	os.WriteFile(".kamakiri/config.json", []byte("{bad"), 0644)

	_, err := LoadProject()
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestSaveProjectRejectsSymlinkedDir(t *testing.T) {
	t.Chdir(t.TempDir())

	// Writing through a symlinked .kamakiri could let a hostile working
	// directory redirect the config write to an attacker-chosen location.
	target := t.TempDir()
	os.Symlink(target, ".kamakiri")

	config := &ProjectConfig{Version: 1, Kind: "pages", ID: "test"}
	err := SaveProject(config)
	if err == nil {
		t.Fatal("expected error for symlinked .kamakiri dir")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %q, want symlink error", err.Error())
	}
}

func TestSaveProjectRejectsSymlinkedFile(t *testing.T) {
	t.Chdir(t.TempDir())
	os.MkdirAll(".kamakiri", 0755)

	// The same guard at the file level.
	target := filepath.Join(t.TempDir(), "target.json")
	os.WriteFile(target, []byte("{}"), 0644)
	os.Symlink(target, ".kamakiri/config.json")

	config := &ProjectConfig{Version: 1, Kind: "pages", ID: "test"}
	err := SaveProject(config)
	if err == nil {
		t.Fatal("expected error for symlinked config.json")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %q, want symlink error", err.Error())
	}
}
