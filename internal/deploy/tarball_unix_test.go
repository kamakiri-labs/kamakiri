//go:build unix

package deploy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// notRegularLine is the special-file refusal in full, written out rather than
// looked up so that a change to the catalog's wording fails here.
func notRegularLine(entry string) string {
	return fmt.Sprintf("%s is not a regular file or directory, so it cannot be deployed. Remove it and try again", filepath.FromSlash(entry))
}

// mkfifo makes a named pipe at root/name, written with forward slashes.
func mkfifo(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := syscall.Mkfifo(path, 0644); err != nil {
		t.Fatalf("syscall.Mkfifo: %v", err)
	}
	return path
}

// createWithin runs Create on dir on its own goroutine and returns the packed
// files' contents by name, with its error.
// Opening a named pipe for reading blocks until a writer appears, so a walk
// that reaches the open would hang the whole package; past the deadline the
// test fails instead. Before failing it opens fifo for writing, without
// blocking, until Create returns, so the stuck open ends and no goroutine
// outlives the test. A Create that still has not returned 5s later is left
// running, and the test fails either way.
func createWithin(t *testing.T, dir, fifo string) (map[string]string, error) {
	t.Helper()
	type result struct {
		r   io.ReadCloser
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, _, err := Create(dir, MaxTotalSize)
		done <- result{r, err}
	}()

	select {
	case res := <-done:
		if res.r == nil {
			return nil, res.err
		}
		// Read on the test goroutine: readTarball fails the test through
		// t.Fatalf, which must not run on any other goroutine.
		defer res.r.Close()
		_, contents := readTarball(t, res.r)
		return contents, res.err
	case <-time.After(5 * time.Second):
	}

	giveUp := time.After(5 * time.Second)
	for {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		select {
		case res := <-done:
			if res.r != nil {
				res.r.Close()
			}
			t.Fatal("Create() still packing after 5s: it opened the named pipe and waited for a writer")
		case <-giveUp:
			t.Fatal("Create() still packing after 5s and could not be unblocked: it opened the named pipe and waited for a writer")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestCreateRefusesANamedPipe(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	fifo := mkfifo(t, dir, "pipe")

	_, err := createWithin(t, dir, fifo)

	if err == nil || err.Error() != notRegularLine("pipe") {
		t.Errorf("Create() error = %v, want %q", err, notRegularLine("pipe"))
	}
}

// a sorts before z, so the pipe is first reached through the link, and the
// refusal names it by that path.
func TestCreateRefusesANamedPipeBelowALink(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"z/index.html": "hi"})
	fifo := mkfifo(t, dir, "z/pipe")
	symlinkOrSkip(t, dir, "a", "z")

	_, err := createWithin(t, dir, fifo)

	if want := notRegularLine("a/pipe"); err == nil || err.Error() != want {
		t.Errorf("Create() error = %v, want %q", err, want)
	}
}

func TestCreateRefusesASocket(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	l, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Skipf("cannot create a Unix socket here: %v", err)
	}
	defer l.Close()

	if err := createError(t, dir); err.Error() != notRegularLine("sock") {
		t.Errorf("Create() error = %q, want %q", err, notRegularLine("sock"))
	}
}

// A root .well-known that is not a directory is left out like any root dot
// entry, even a socket, which anywhere else fails the deploy.
func TestCreateSkipsARootSocketNamedWellKnown(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	l, err := net.Listen("unix", filepath.Join(dir, ".well-known"))
	if err != nil {
		t.Skipf("cannot create a Unix socket here: %v", err)
	}
	defer l.Close()

	_, contents, _ := packTree(t, dir)

	if want := map[string]string{"index.html": "hi"}; !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// A named pipe called .well-known is not a directory, so the root branch
// leaves it out without opening it: no error, no wait for a writer.
func TestCreateLeavesOutANamedPipeCalledWellKnown(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	fifo := mkfifo(t, dir, ".well-known")

	contents, err := createWithin(t, dir, fifo)

	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if want := map[string]string{"index.html": "hi"}; !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

func TestCreateRefusesASocketInsideTheRootWellKnown(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi", ".well-known/security.txt": "contact"})
	l, err := net.Listen("unix", filepath.Join(dir, ".well-known", "sock"))
	if err != nil {
		t.Skipf("cannot create a Unix socket here: %v", err)
	}
	defer l.Close()

	if err, want := createError(t, dir), notRegularLine(".well-known/sock"); err.Error() != want {
		t.Errorf("Create() error = %q, want %q", err, want)
	}
}

// Once entered, the root .well-known/ is listed like any directory, so one the
// walk cannot read fails the deploy rather than leaving it out unannounced.
func TestCreateRefusesAnUnreadableRootWellKnown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi", ".well-known/security.txt": "contact"})
	wkPath := filepath.Join(dir, ".well-known")
	if err := os.Chmod(wkPath, 0); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(wkPath, 0755) })

	err := createError(t, dir)

	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pathErr) || filepath.Base(pathErr.Path) != ".well-known" {
		t.Errorf("Create() error = %v, want the permission error listing .well-known", err)
	}
}
