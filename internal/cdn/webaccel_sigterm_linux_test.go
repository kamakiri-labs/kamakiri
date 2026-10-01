//go:build linux

package cdn

import (
	"bytes"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPty returns both ends of a fresh pseudoterminal. Raw mode needs a real
// terminal, which a pipe or a buffer cannot stand in for.
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

// lflags reads the local-mode flags of the terminal behind fd.
func lflags(t *testing.T, fd int) uint32 {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("reading the terminal mode: %v", err)
	}
	return tio.Lflag
}

// cooked reports whether the terminal echoes and edits lines again, the mode
// a prompt found it in.
func cooked(flags uint32) bool {
	return flags&unix.ECHO != 0 && flags&unix.ICANON != 0
}

// startPtyRead starts a read on the slave and keeps the test from ending with
// it still blocked. Closing the master hangs the slave up, which ends a read
// the test's own input did not; the read has to be over before the seams are
// restored, or its last calls would race with the restore.
func startPtyRead(t *testing.T, master, slave *os.File, echo, out *bytes.Buffer) *rawRead {
	t.Helper()
	r := startRawRead(slave, echo, out)
	t.Cleanup(func() {
		select {
		case <-r.done:
			return
		default:
		}
		master.Close()
		select {
		case <-r.done:
		case <-time.After(seamDeadline):
			t.Errorf("readRawTerminal still running after the terminal was hung up")
		}
	})
	return r
}

// A SIGTERM while the prompt holds the terminal raw restores the terminal,
// closes the prompt line on out and exits 143, all from the handler, since the
// blocked read cannot be interrupted to do it.
func TestReadRawTerminalSIGTERMDuringReadRestoresAndExits(t *testing.T) {
	stub := stubSignals(t, nil)
	master, slave := openPty(t)
	fd := int(slave.Fd())
	var echo, out bytes.Buffer

	r := startPtyRead(t, master, slave, &echo, &out)

	sigs := await(t, stub.notified, "the SIGTERM handler to be registered")
	sendTerm(t, sigs)
	await(t, stub.exited, "the exit after the SIGTERM")

	// The exit seam returns in a test, so the read is still blocked here: the
	// handler restored the terminal without waiting for it.
	if flags := lflags(t, fd); !cooked(flags) {
		t.Errorf("terminal still raw when the process exits: lflag=%#x", flags)
	}

	// The terminal is cooked again, with ICRNL back on, so a bare Enter would
	// arrive as the \n a read with nothing typed skips, and the read would never
	// return. A byte ahead of it ends the line.
	if _, err := master.WriteString("x\r"); err != nil {
		t.Fatal(err)
	}
	value, err := r.wait(t)
	if err != nil || value != "x" {
		t.Errorf("read = %q, %v; want the line typed to release it", value, err)
	}

	stub.assertExitedOnce(t)
	stub.assertRegisteredAndStopped(t)
	if out.String() != "\n" {
		t.Errorf("out = %q, want the newline that closes the prompt line", out.String())
	}
	if bytes.Contains(echo.Bytes(), []byte("\n")) {
		t.Errorf("echo = %q, want the newline on out, not on the echo writer", echo.String())
	}
}

// A SIGTERM that arrives as the read finishes, after the handler has stopped
// selecting but before the registration is removed, stays buffered on the
// channel. The check after the removal is then its only receiver, and it has to
// take the same exit.
func TestReadRawTerminalSIGTERMPendingAtStopExits(t *testing.T) {
	stub := stubSignals(t, func(c chan<- os.Signal) { sendTerm(t, c) })
	master, slave := openPty(t)
	fd := int(slave.Fd())
	var echo, out bytes.Buffer

	r := startPtyRead(t, master, slave, &echo, &out)

	// The line is typed once the terminal is raw, so it arrives the way a typed
	// one does: raw mode clears ICRNL, and the \r is the Enter that ends it.
	deadline := time.Now().Add(seamDeadline)
	for lflags(t, fd)&unix.ICANON != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("terminal not raw within %s", seamDeadline)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := master.WriteString("x\r"); err != nil {
		t.Fatal(err)
	}
	value, err := r.wait(t)
	if err != nil || value != "x" {
		t.Errorf("read = %q, %v; want x", value, err)
	}

	stub.assertExitedOnce(t)
	stub.assertRegisteredAndStopped(t)
	if out.String() != "\n" {
		t.Errorf("out = %q, want the newline that closes the prompt line", out.String())
	}
	if flags := lflags(t, fd); !cooked(flags) {
		t.Errorf("terminal left raw: lflag=%#x", flags)
	}
}

// While a handler that took a SIGTERM is still exiting, readRawTerminal must
// not return, or its caller would print a newline of its own and act on the
// value read. The real exit never returns, so the test holds the exit seam
// open and checks that the read stays put until it is released.
func TestReadRawTerminalWaitsForHandlerExit(t *testing.T) {
	stub := stubSignals(t, nil)
	record := exitProcess
	release := make(chan struct{})
	exitProcess = func(code int) {
		record(code)
		<-release
	}
	master, slave := openPty(t)
	var echo, out bytes.Buffer

	r := startPtyRead(t, master, slave, &echo, &out)
	// Registered after the read's own cleanup, so it runs first: a failing
	// run frees the held exit before the read is joined.
	var releaseOnce sync.Once
	releaseExit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseExit)

	sigs := await(t, stub.notified, "the SIGTERM handler to be registered")
	sendTerm(t, sigs)
	await(t, stub.exited, "the handler to reach the exit")

	// The handler restored the terminal before exiting, so the line typed now
	// is cooked and ends the read, which then has to wait for the handler.
	if _, err := master.WriteString("x\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
		t.Fatalf("readRawTerminal returned while the handler was still exiting: %q, %v", r.value, r.err)
	case <-time.After(300 * time.Millisecond):
	}

	releaseExit()
	value, err := r.wait(t)
	if err != nil || value != "x" {
		t.Errorf("read = %q, %v; want x", value, err)
	}
	stub.assertExitedOnce(t)
	if out.String() != "\n" {
		t.Errorf("out = %q, want the newline that closes the prompt line", out.String())
	}
}
