package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")

	path, err := settingsPath()
	if err != nil {
		t.Fatalf("settingsPath() error = %v", err)
	}
	want := "/tmp/test-config/kamakiri/settings.json"
	if path != want {
		t.Errorf("settingsPath() = %q, want %q", path, want)
	}
}

// The least obvious fold of the fail-open contract: with no config directory to
// resolve there is not even a path to read, and the read still reports nothing
// rather than an error, while the write reports the failure it is.
func TestSettingsWithNoResolvableConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	if _, err := settingsPath(); err == nil {
		t.Fatal("settingsPath() error = nil; this test needs an environment where the home directory cannot be resolved")
	}

	if settings := LoadSettings(); settings != nil {
		t.Errorf("LoadSettings() = %+v, want nil", settings)
	}
	if err := SaveSettings("ja"); err == nil {
		t.Error("SaveSettings() = nil, want an error when there is nowhere to write")
	}
}

func TestSaveAndLoadSettings(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if err := SaveSettings("ja"); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	settings := LoadSettings()
	if settings == nil {
		t.Fatal("LoadSettings() = nil, want the saved settings")
	}
	if settings.Version != 1 {
		t.Errorf("version = %d, want 1", settings.Version)
	}
	if settings.Language != "ja" {
		t.Errorf("language = %q, want ja", settings.Language)
	}
}

func TestSaveSettingsOverwritesPrevious(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if err := SaveSettings("ja"); err != nil {
		t.Fatalf("SaveSettings(ja) error = %v", err)
	}
	if err := SaveSettings("en"); err != nil {
		t.Fatalf("SaveSettings(en) error = %v", err)
	}

	settings := LoadSettings()
	if settings == nil {
		t.Fatal("LoadSettings() = nil, want the saved settings")
	}
	if settings.Language != "en" {
		t.Errorf("language = %q, want en", settings.Language)
	}
}

func TestSaveSettingsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if err := SaveSettings("ja"); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "kamakiri", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if raw["version"] != float64(1) {
		t.Errorf("version = %v, want 1", raw["version"])
	}
	if raw["language"] != "ja" {
		t.Errorf("language = %v, want ja", raw["language"])
	}
}

func TestSaveSettingsFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if err := SaveSettings("ja"); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(tmpDir, "kamakiri", "settings.json"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("permissions = %o, want 644", perm)
	}
}

// The directory mode matters on a machine where the language is set before the
// first login: MkdirAll never chmods an existing directory, so a 0755 creation
// here would leave the credentials file's directory world-readable for good.
func TestSaveSettingsCreatesConfigDir0700(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if err := SaveSettings("ja"); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(tmpDir, "kamakiri"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("config dir permissions = %o, want 700", perm)
	}
}

func TestSaveSettingsReportsFailure(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// A regular file where the config directory belongs, so MkdirAll fails.
	if err := os.WriteFile(filepath.Join(tmpDir, "kamakiri"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := SaveSettings("ja"); err == nil {
		t.Error("SaveSettings() = nil, want an error when the file cannot be written")
	}
}

func TestLoadSettingsMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if settings := LoadSettings(); settings != nil {
		t.Errorf("LoadSettings() = %+v, want nil", settings)
	}
}

func TestLoadSettingsCorrupt(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeSettingsFile(t, tmpDir, "{not json")

	if settings := LoadSettings(); settings != nil {
		t.Errorf("LoadSettings() = %+v, want nil", settings)
	}
}

func TestLoadSettingsUnknownVersion(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeSettingsFile(t, tmpDir, `{"version": 2, "language": "ja"}`)

	if settings := LoadSettings(); settings != nil {
		t.Errorf("LoadSettings() = %+v, want nil", settings)
	}
}

func TestLoadSettingsUnreadable(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// A directory at the settings path: unreadable whatever the user's
	// privileges, unlike a 0000 file, which root reads anyway.
	if err := os.MkdirAll(filepath.Join(tmpDir, "kamakiri", "settings.json"), 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if settings := LoadSettings(); settings != nil {
		t.Errorf("LoadSettings() = %+v, want nil", settings)
	}
}

// core stores whatever language string it is given: the set of supported
// languages belongs to exactly one package, and it is not this one.
func TestLoadSettingsKeepsUnsupportedLanguage(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeSettingsFile(t, tmpDir, `{"version": 1, "language": "fr"}`)

	settings := LoadSettings()
	if settings == nil {
		t.Fatal("LoadSettings() = nil, want the hand-edited settings")
	}
	if settings.Language != "fr" {
		t.Errorf("language = %q, want fr", settings.Language)
	}
}

func writeSettingsFile(t *testing.T, configHome, content string) {
	t.Helper()
	dir := filepath.Join(configHome, "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
