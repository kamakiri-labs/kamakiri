//go:build linux

package login

import (
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPty returns both ends of a fresh pseudoterminal.
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudoterminal available: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlocking the slave: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("reading the slave number: %v", err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("opening the slave: %v", err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// readable reports whether a read of f would find input, without reading. Go
// switches a descriptor to blocking mode when its Fd is taken, so a read that
// expects EAGAIN cannot be relied on to return.
func readable(t *testing.T, f *os.File) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		t.Fatalf("polling the slave: %v", err)
	}
	return n > 0 && fds[0].Revents&unix.POLLIN != 0
}

// queued reports how many bytes are waiting to be read from the terminal f.
func queued(t *testing.T, f *os.File) int {
	t.Helper()
	n, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCINQ)
	if err != nil {
		t.Fatalf("reading the input queue length: %v", err)
	}
	return n
}

// An Enter typed while a countdown runs sits in the terminal's input queue. The
// default flush must empty that queue, so the prompt that follows finds nothing
// to read.
func TestDiscardTypeaheadEmptiesATerminalsInputQueue(t *testing.T) {
	master, slave := openPty(t)

	if _, err := master.WriteString("typed\n"); err != nil {
		t.Fatal(err)
	}
	// The line discipline takes the master's bytes on a worker, so they reach the
	// slave's queue a moment after the write returns.
	deadline := time.Now().Add(2 * time.Second)
	for queued(t, slave) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !readable(t, slave) {
		t.Fatal("nothing queued on the slave before the flush; the test proves nothing")
	}

	discardTypeahead(slave)

	if got := queued(t, slave); got != 0 {
		t.Errorf("%d bytes still queued after the flush", got)
	}
	if readable(t, slave) {
		t.Error("the slave is still readable after the flush")
	}
}
