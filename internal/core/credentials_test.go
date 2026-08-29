package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: the frame on every error this package wraps
// renders from the message catalog. Load rather than Setup: nothing here
// reports which language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

func TestSaveAndLoadCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	err := SaveCredentials("kk_live_testkey123", "test@example.com")
	if err != nil {
		t.Fatalf("SaveCredentials() error = %v", err)
	}

	creds, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials() error = %v", err)
	}
	if creds.Version != 1 {
		t.Errorf("version = %d, want 1", creds.Version)
	}
	if creds.APIKey != "kk_live_testkey123" {
		t.Errorf("api_key = %q, want kk_live_testkey123", creds.APIKey)
	}
	if creds.Email != "test@example.com" {
		t.Errorf("email = %q, want test@example.com", creds.Email)
	}
}

func TestLoadCredentialsMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	creds, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials() error = %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil credentials, got %+v", creds)
	}
}

func TestSaveCredentialsFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveCredentials("kk_live_testkey123", "test@example.com")

	path := filepath.Join(tmpDir, "kamakiri", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestSaveCredentialsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveCredentials("kk_live_abc", "abc@example.com")

	path := filepath.Join(tmpDir, "kamakiri", "credentials.json")
	data, _ := os.ReadFile(path)

	var raw map[string]any
	json.Unmarshal(data, &raw)

	if raw["version"] != float64(1) {
		t.Errorf("version = %v, want 1", raw["version"])
	}
	if raw["api_key"] != "kk_live_abc" {
		t.Errorf("api_key = %v, want kk_live_abc", raw["api_key"])
	}
	if raw["email"] != "abc@example.com" {
		t.Errorf("email = %v, want abc@example.com", raw["email"])
	}
}

func TestCredentialsPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")

	path, err := CredentialsPath()
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	want := "/tmp/test-config/kamakiri/credentials.json"
	if path != want {
		t.Errorf("CredentialsPath() = %q, want %q", path, want)
	}
}
