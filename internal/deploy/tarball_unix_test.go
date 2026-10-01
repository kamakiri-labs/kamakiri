//go:build unix

package deploy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
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

// createWithin runs Create on dir on its own goroutine and returns its error.
// Opening a named pipe for reading blocks until a writer appears, so a walk
// that reaches the open would hang the whole package; past the deadline the
// test fails instead. Before failing it opens fifo for writing, without
// blocking, until Create returns, so the stuck open ends and no goroutine
// outlives the test.
func createWithin(t *testing.T, dir, fifo string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		r, _, err := Create(dir, MaxTotalSize)
		if r != nil {
			r.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
	}

	giveUp := time.After(5 * time.Second)
	for {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		select {
		case <-done:
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

	err := createWithin(t, dir, fifo)

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

	err := createWithin(t, dir, fifo)

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
