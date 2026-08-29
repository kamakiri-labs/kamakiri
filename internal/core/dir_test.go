package core

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKamakiriConfigDirXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")

	got, err := kamakiriConfigDir()
	if err != nil {
		t.Fatalf("kamakiriConfigDir() error = %v", err)
	}
	want := "/tmp/xdg-test/kamakiri"
	if got != want {
		t.Errorf("kamakiriConfigDir() = %q, want %q", got, want)
	}
}

func TestKamakiriConfigDirFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")

	got, err := kamakiriConfigDir()
	if err != nil {
		t.Fatalf("kamakiriConfigDir() error = %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".config", "kamakiri")
	if got != want {
		t.Errorf("kamakiriConfigDir() = %q, want %q", got, want)
	}
}

func TestKamakiriConfigDirNoHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	_, err := kamakiriConfigDir()
	if err == nil {
		t.Fatal("expected error when HOME is unset")
	}
}

// Every error this package returns puts a localized frame in front of the one
// the operating system gave, and callers still have to be able to look through
// that frame with errors.Is and errors.As. A frame joined with %s instead of %w
// renders the same text and breaks both, so the seam is pinned here rather than
// left to a reader to spot. The frames themselves are asserted too: unwrapping
// alone passes just as well with the wrong message in front of it.
//
// The frames live in credentials.go, login_secret.go and settings.go, and the
// test lives here because everything it drives funnels through this file: the
// three writes through atomicWriteFile, and all four paths through
// kamakiriConfigDir.
func TestFramedErrorsStillUnwrap(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("preparing the config directory: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", root)

	t.Run("writes", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a directory whatever its mode says, so no write here would fail")
		}

		// A directory with no write permission, so every write below is refused.
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatalf("sealing the config directory: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0700) })

		writes := []struct {
			name  string
			frame string
			err   error
		}{
			{"credentials", "write credentials", SaveCredentials("kk_live_x", "a@example.com")},
			{"login secret", "write login secret", SaveLoginSecret("a@example.com", "tok", time.Time{})},
			{"settings", "write settings", SaveSettings("ja")},
		}
		for _, write := range writes {
			if write.err == nil {
				t.Fatalf("writing %s into a read-only directory: expected an error", write.name)
			}
			if !strings.Contains(write.err.Error(), write.frame) {
				t.Errorf("writing %s: error %q does not carry the frame %q", write.name, write.err, write.frame)
			}
			if !errors.Is(write.err, fs.ErrPermission) {
				t.Errorf("writing %s: errors.Is(err, fs.ErrPermission) = false through %q", write.name, write.err)
			}
			var pathErr *fs.PathError
			if !errors.As(write.err, &pathErr) {
				t.Errorf("writing %s: errors.As did not reach a *fs.PathError through %q", write.name, write.err)
			}
		}
	})

	// The read side, over a file that is a directory: unreadable, and reported
	// through the same kind of frame. It fails on EISDIR rather than on
	// permissions, so it binds for root too.
	t.Run("read", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(dir, "credentials.json"), 0700); err != nil {
			t.Fatalf("planting a directory where the credentials file goes: %v", err)
		}
		_, err := LoadCredentials()
		if err == nil {
			t.Fatal("reading a credentials file that is a directory: expected an error")
		}
		const frame = "read credentials"
		if !strings.Contains(err.Error(), frame) {
			t.Errorf("reading credentials: error %q does not carry the frame %q", err, frame)
		}
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("reading credentials: errors.As did not reach a *fs.PathError through %q", err)
		}
	})
}
