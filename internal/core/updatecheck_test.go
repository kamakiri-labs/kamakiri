package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateCheckPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")

	path, err := updateCheckPath()
	if err != nil {
		t.Fatalf("updateCheckPath() error = %v", err)
	}
	want := "/tmp/test-config/kamakiri/update-check.json"
	if path != want {
		t.Errorf("updateCheckPath() = %q, want %q", path, want)
	}
}

// Both halves are silent here, which is where this file parts company with the
// settings one: nobody asked for either, so with nowhere to write there is
// nothing to report.
func TestUpdateCheckWithNoResolvableConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	if _, err := updateCheckPath(); err == nil {
		t.Fatal("updateCheckPath() error = nil; this test needs an environment where the home directory cannot be resolved")
	}

	if state := LoadUpdateCheck(); state != nil {
		t.Errorf("LoadUpdateCheck() = %+v, want nil", state)
	}
	SaveUpdateCheck(time.Now())
}

func TestSaveAndLoadUpdateCheck(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	last := time.Now().Add(-90 * time.Minute)
	SaveUpdateCheck(last)

	state := LoadUpdateCheck()
	if state == nil {
		t.Fatal("LoadUpdateCheck() = nil, want the saved state")
	}
	if state.Version != 1 {
		t.Errorf("version = %d, want 1", state.Version)
	}
	// A round trip through RFC 3339 keeps the second but not the monotonic
	// clock reading, so the two are compared to the second.
	if !state.LastNudge.Truncate(time.Second).Equal(last.Truncate(time.Second)) {
		t.Errorf("last_nudge = %v, want %v", state.LastNudge, last)
	}
}

func TestSaveUpdateCheckOverwritesPrevious(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	last := time.Now()
	SaveUpdateCheck(last.Add(-48 * time.Hour))
	SaveUpdateCheck(last)

	state := LoadUpdateCheck()
	if state == nil {
		t.Fatal("LoadUpdateCheck() = nil, want the saved state")
	}
	if !state.LastNudge.Truncate(time.Second).Equal(last.Truncate(time.Second)) {
		t.Errorf("last_nudge = %v, want %v", state.LastNudge, last)
	}
}

// The timestamp is written as RFC 3339 rather than as a number of seconds, so a
// file anyone opens says when in a form they can read.
func TestSaveUpdateCheckJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	last := time.Now()
	SaveUpdateCheck(last)

	data, err := os.ReadFile(filepath.Join(tmpDir, "kamakiri", "update-check.json"))
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
	written, ok := raw["last_nudge"].(string)
	if !ok {
		t.Fatalf("last_nudge = %v, want a string", raw["last_nudge"])
	}
	parsed, err := time.Parse(time.RFC3339, written)
	if err != nil {
		t.Fatalf("last_nudge = %q, want an RFC 3339 timestamp: %v", written, err)
	}
	if !parsed.Truncate(time.Second).Equal(last.Truncate(time.Second)) {
		t.Errorf("last_nudge = %v, want %v", parsed, last)
	}
}

func TestSaveUpdateCheckFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveUpdateCheck(time.Now())

	info, err := os.Stat(filepath.Join(tmpDir, "kamakiri", "update-check.json"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("permissions = %o, want 644", perm)
	}
}

// The nudge can be the first thing that ever writes to the config directory, so
// creating it 0755 here would leave the credentials file's directory
// world-readable for good on that machine.
func TestSaveUpdateCheckCreatesConfigDir0700(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveUpdateCheck(time.Now())

	info, err := os.Stat(filepath.Join(tmpDir, "kamakiri"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("config dir permissions = %o, want 700", perm)
	}
}

func TestSaveUpdateCheckIsSilentWhenItCannotWrite(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// A regular file where the config directory belongs, so MkdirAll fails.
	if err := os.WriteFile(filepath.Join(tmpDir, "kamakiri"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	SaveUpdateCheck(time.Now())

	if state := LoadUpdateCheck(); state != nil {
		t.Errorf("LoadUpdateCheck() = %+v, want nil after a write that could not happen", state)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%s holds %d entries, want only the file standing in for the config directory", tmpDir, len(entries))
	}
}

func TestLoadUpdateCheckMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if state := LoadUpdateCheck(); state != nil {
		t.Errorf("LoadUpdateCheck() = %+v, want nil", state)
	}
}

func TestLoadUpdateCheckUnreadable(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, "kamakiri"), 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path := filepath.Join(tmpDir, "kamakiri", "update-check.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"last_nudge":"2026-01-01T00:00:00Z"}`), 0000); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless of its mode")
	}

	if state := LoadUpdateCheck(); state != nil {
		t.Errorf("LoadUpdateCheck() = %+v, want nil", state)
	}
}

// A hand-edited or truncated file, and a timestamp that is not RFC 3339, land
// in the same place: the unmarshal fails, and the fail-open rule covers both
// with no branch of their own.
func TestLoadUpdateCheckMalformed(t *testing.T) {
	cases := []struct {
		name     string
		contents string
	}{
		{"not JSON at all", "{"},
		{"a timestamp nothing can parse", `{"version":1,"last_nudge":"yesterday"}`},
		{"a timestamp of the wrong type", `{"version":1,"last_nudge":1767225600}`},
		{"a newer file version", `{"version":2,"last_nudge":"2026-01-01T00:00:00Z"}`},
		{"no file version", `{"last_nudge":"2026-01-01T00:00:00Z"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmpDir)

			if err := os.MkdirAll(filepath.Join(tmpDir, "kamakiri"), 0700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			path := filepath.Join(tmpDir, "kamakiri", "update-check.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			if state := LoadUpdateCheck(); state != nil {
				t.Errorf("LoadUpdateCheck() = %+v, want nil", state)
			}
		})
	}
}
