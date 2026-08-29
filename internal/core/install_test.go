package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallMarkerPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")

	path, err := installMarkerPath()
	if err != nil {
		t.Fatalf("installMarkerPath() error = %v", err)
	}
	want := "/tmp/test-config/kamakiri/install.json"
	if path != want {
		t.Errorf("installMarkerPath() = %q, want %q", path, want)
	}
}

// The least obvious fold of the fail-open contract: with no config directory to
// resolve there is not even a path to read, and the read still reports nothing
// rather than an error.
func TestLoadInstallMarkerWithNoResolvableConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	if _, err := installMarkerPath(); err == nil {
		t.Fatal("installMarkerPath() error = nil; this test needs an environment where the home directory cannot be resolved")
	}

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

func TestLoadInstallMarkerMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

func TestLoadInstallMarkerUnreadable(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// A directory at the marker path: unreadable whatever the user's
	// privileges, unlike a 0000 file, which root reads anyway.
	if err := os.MkdirAll(filepath.Join(tmpDir, "kamakiri", "install.json"), 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

func TestLoadInstallMarkerCorrupt(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeInstallMarkerFile(t, tmpDir, "{not json")

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

func TestLoadInstallMarkerUnknownVersion(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeInstallMarkerFile(t, tmpDir, `{"version": 2, "method": "script", "path": "/usr/local/bin/kamakiri"}`)

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

// A file with no version field at all is the shape a draft installer writes,
// and its zero version is not the version this CLI reads, so it is ignored like
// any other unreadable marker rather than taken for a version 1 file.
func TestLoadInstallMarkerMissingVersion(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeInstallMarkerFile(t, tmpDir, `{"method": "script", "path": "/usr/local/bin/kamakiri"}`)

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

// A method this CLI does not know reads as no marker at all, which is what
// makes an unrecognized channel fall back to replacing the binary itself
// rather than to a method whose meaning this build cannot know.
func TestLoadInstallMarkerUnknownMethod(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeInstallMarkerFile(t, tmpDir, `{"version": 1, "method": "deb", "path": "/usr/bin/kamakiri"}`)

	if marker := LoadInstallMarker(); marker != nil {
		t.Errorf("LoadInstallMarker() = %+v, want nil", marker)
	}
}

func TestLoadInstallMarkerScript(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	writeInstallMarkerFile(t, tmpDir, `{"version": 1, "method": "script", "path": "/home/user/.local/bin/kamakiri"}`)

	marker := LoadInstallMarker()
	if marker == nil {
		t.Fatal("LoadInstallMarker() = nil, want the written marker")
	}
	if marker.Version != installMarkerVersion {
		t.Errorf("version = %d, want %d", marker.Version, installMarkerVersion)
	}
	if marker.Method != InstallMethodScript {
		t.Errorf("method = %q, want %q", marker.Method, InstallMethodScript)
	}
	if marker.Path != "/home/user/.local/bin/kamakiri" {
		t.Errorf("path = %q, want /home/user/.local/bin/kamakiri", marker.Path)
	}
}

func writeInstallMarkerFile(t *testing.T, configHome, content string) {
	t.Helper()
	dir := filepath.Join(configHome, "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.json"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
