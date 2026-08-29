package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndLoadLoginSecret(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	expiresAt := time.Now().Add(10 * time.Minute)
	err := SaveLoginSecret("user@example.com", "abc123token", expiresAt)
	if err != nil {
		t.Fatalf("SaveLoginSecret() error = %v", err)
	}

	ls, err := LoadLoginSecret()
	if err != nil {
		t.Fatalf("LoadLoginSecret() error = %v", err)
	}
	if ls.Email != "user@example.com" {
		t.Errorf("email = %q, want user@example.com", ls.Email)
	}
	if ls.Secret.Token != "abc123token" {
		t.Errorf("token = %q, want abc123token", ls.Secret.Token)
	}
}

func TestLoadLoginSecretMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	ls, err := LoadLoginSecret()
	if err != nil {
		t.Fatalf("LoadLoginSecret() error = %v", err)
	}
	if ls != nil {
		t.Errorf("expected nil, got %+v", ls)
	}
}

func TestLoginSecretFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveLoginSecret("user@example.com", "token", time.Now().Add(10*time.Minute))

	path := filepath.Join(tmpDir, "kamakiri", ".login_secret")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestIsValidMatchingEmail(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveLoginSecret("user@example.com", "token", time.Now().Add(10*time.Minute))

	ls, _ := LoadLoginSecret()
	if !ls.IsValid("user@example.com") {
		t.Error("IsValid() = false, want true for matching email")
	}
}

func TestIsValidWrongEmail(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveLoginSecret("user@example.com", "token", time.Now().Add(10*time.Minute))

	ls, _ := LoadLoginSecret()
	if ls.IsValid("other@example.com") {
		t.Error("IsValid() = true, want false for different email")
	}
}

func TestIsValidExpired(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveLoginSecret("user@example.com", "token", time.Now().Add(-1*time.Minute))

	ls, _ := LoadLoginSecret()
	if ls.IsValid("user@example.com") {
		t.Error("IsValid() = true, want false for expired secret")
	}
}

func TestDeleteLoginSecret(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveLoginSecret("user@example.com", "token", time.Now().Add(10*time.Minute))

	err := DeleteLoginSecret()
	if err != nil {
		t.Fatalf("DeleteLoginSecret() error = %v", err)
	}

	ls, err := LoadLoginSecret()
	if err != nil {
		t.Fatalf("LoadLoginSecret() error = %v", err)
	}
	if ls != nil {
		t.Error("expected nil after delete")
	}
}

func TestDeleteLoginSecretMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	err := DeleteLoginSecret()
	if err != nil {
		t.Errorf("DeleteLoginSecret() on missing file error = %v", err)
	}
}

func TestLoginSecretPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")

	path, err := LoginSecretPath()
	if err != nil {
		t.Fatalf("LoginSecretPath() error = %v", err)
	}
	want := "/tmp/test-config/kamakiri/.login_secret"
	if path != want {
		t.Errorf("LoginSecretPath() = %q, want %q", path, want)
	}
}
