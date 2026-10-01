package cdn

import (
	"bytes"
	"io"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/term"
)

// seamDeadline bounds every wait in the SIGTERM tests, so a broken handler
// fails the test instead of hanging the package until its timeout.
const seamDeadline = 5 * time.Second

// signalStub stands in for the signal and exit calls readRawTerminal makes.
// The test hands the handler a SIGTERM by sending on the channel it was given,
// and watches the exit without ending the test binary. No real signal is ever
// raised: one sent outside the registration window would kill the package run.
type signalStub struct {
	// notified carries the channel readRawTerminal registered, so the test can
	// send on it.
	notified chan chan<- os.Signal
	// exited is closed by the first exit, without blocking, since in most cases
	// nothing waits on it.
	exited   chan struct{}
	exitOnce sync.Once

	mu         sync.Mutex
	registered []chan<- os.Signal
	signals    []os.Signal
	stopped    []chan<- os.Signal
	codes      []int
}

// stubSignals installs a signalStub for the rest of the test. A non-nil onStop
// runs inside the stand-in for signal.Stop, before it returns, to model a
// signal that arrived just before the handler was removed.
func stubSignals(t *testing.T, onStop func(chan<- os.Signal)) *signalStub {
	t.Helper()
	s := &signalStub{
		notified: make(chan chan<- os.Signal, 1),
		exited:   make(chan struct{}),
	}
	origNotify, origStop, origExit := notifySignals, stopSignals, exitProcess
	t.Cleanup(func() { notifySignals, stopSignals, exitProcess = origNotify, origStop, origExit })

	notifySignals = func(c chan<- os.Signal, sig ...os.Signal) {
		s.mu.Lock()
		s.registered = append(s.registered, c)
		s.signals = append(s.signals, sig...)
		s.mu.Unlock()
		select {
		case s.notified <- c:
		default:
		}
	}
	stopSignals = func(c chan<- os.Signal) {
		s.mu.Lock()
		s.stopped = append(s.stopped, c)
		s.mu.Unlock()
		if onStop != nil {
			onStop(c)
		}
	}
	exitProcess = func(code int) {
		s.mu.Lock()
		s.codes = append(s.codes, code)
		s.mu.Unlock()
		s.exitOnce.Do(func() { close(s.exited) })
	}
	return s
}

// exitCodes returns the codes the exit seam was called with, in order.
func (s *signalStub) exitCodes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.codes)
}

// assertExitedOnce checks that the exit seam ran exactly once, with 143.
func (s *signalStub) assertExitedOnce(t *testing.T) {
	t.Helper()
	if codes := s.exitCodes(); !slices.Equal(codes, []int{143}) {
		t.Errorf("exit codes = %v, want exactly one exit with 143", codes)
	}
}

// assertRegisteredAndStopped checks that the handler was registered for
// SIGTERM alone, on one channel, and removed from that same channel, so the
// signal is back to its default action once the read is over.
func (s *signalStub) assertRegisteredAndStopped(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.registered) != 1 {
		t.Fatalf("handler registered %d times, want once", len(s.registered))
	}
	if !slices.Equal(s.signals, []os.Signal{syscall.SIGTERM}) {
		t.Errorf("registered for %v, want SIGTERM alone", s.signals)
	}
	if len(s.stopped) != 1 || s.stopped[0] != s.registered[0] {
		t.Errorf("handler stopped on %v, want once on the registered channel", s.stopped)
	}
}

// sendTerm delivers a SIGTERM on c the way the runtime does, without blocking.
// A channel with no room is reported rather than waited on: a signal sent to a
// full or unbuffered channel nobody is receiving from is lost.
func sendTerm(t *testing.T, c chan<- os.Signal) {
	t.Helper()
	select {
	case c <- syscall.SIGTERM:
	default:
		t.Errorf("the signal channel had no room for a SIGTERM")
	}
}

// await receives one value from c, failing the test if none comes in time.
func await[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(seamDeadline):
		t.Fatalf("timed out after %s waiting for %s", seamDeadline, what)
		var zero T
		return zero
	}
}

// rawRead runs readRawTerminal on a goroutine of its own, the way a prompt
// sits blocked in it while a signal is handled on another.
type rawRead struct {
	done  chan struct{}
	value string
	err   error
}

func startRawRead(file *os.File, echo, out io.Writer) *rawRead {
	r := &rawRead{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.value, r.err = readRawTerminal(file, echo, out, false)
	}()
	return r
}

// wait blocks until readRawTerminal has returned and hands back its results.
func (r *rawRead) wait(t *testing.T) (string, error) {
	t.Helper()
	select {
	case <-r.done:
		return r.value, r.err
	case <-time.After(seamDeadline):
		t.Fatalf("readRawTerminal had not returned after %s", seamDeadline)
		return "", nil
	}
}

// Raw mode is unavailable on anything but a terminal, so the read never starts
// and there is no mode to restore. A SIGTERM that arrived before the handler
// was removed still ends the process; without one the error comes back, and
// the check for a pending signal must not wait for one.
func TestReadRawTerminalWhenRawModeFails(t *testing.T) {
	pipe := func(t *testing.T) *os.File {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		return r
	}

	t.Run("a pending SIGTERM exits", func(t *testing.T) {
		stub := stubSignals(t, func(c chan<- os.Signal) { sendTerm(t, c) })
		file := pipe(t)
		var echo, out bytes.Buffer

		_, err := startRawRead(file, &echo, &out).wait(t)

		if err == nil {
			t.Error("readRawTerminal returned no error with raw mode unavailable")
		}
		stub.assertExitedOnce(t)
		stub.assertRegisteredAndStopped(t)
		if out.String() != "\n" {
			t.Errorf("out = %q, want the newline that closes the prompt line", out.String())
		}
		if echo.Len() != 0 {
			t.Errorf("echo = %q, want nothing", echo.String())
		}
	})

	t.Run("no signal returns the error", func(t *testing.T) {
		stub := stubSignals(t, nil)
		file := pipe(t)
		_, wantErr := term.MakeRaw(int(file.Fd()))
		if wantErr == nil {
			t.Fatal("raw mode succeeded on a pipe; the test proves nothing")
		}
		var echo, out bytes.Buffer

		value, err := startRawRead(file, &echo, &out).wait(t)

		if err == nil || err.Error() != wantErr.Error() {
			t.Errorf("error = %v, want the raw-mode error %q", err, wantErr)
		}
		if value != "" {
			t.Errorf("value = %q, want empty", value)
		}
		if codes := stub.exitCodes(); len(codes) != 0 {
			t.Errorf("exit called with %v, want no exit", codes)
		}
		stub.assertRegisteredAndStopped(t)
		if out.Len() != 0 || echo.Len() != 0 {
			t.Errorf("out = %q, echo = %q, want nothing written", out.String(), echo.String())
		}
	})
}
