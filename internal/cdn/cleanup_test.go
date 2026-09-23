package cdn

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/syncwait"
)

func TestCleanupNothingToClean(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{SiteID: "site123", SyncAttemptID: 1, OrphanCount: 0}, nil
		},
	}

	var out bytes.Buffer
	if err := Cleanup(client, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing to clean up") {
		t.Errorf("missing nothing-to-clean line: %q", out.String())
	}
	assertCleanCopy(t, out.String())
}

func TestCleanupNoWaitQueued(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var statusCalled bool
	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			statusCalled = true
			return &api.CDNStatusResponse{}, nil
		},
	}

	var out bytes.Buffer
	if err := Cleanup(client, true, &out); err != nil {
		t.Fatal(err)
	}
	if statusCalled {
		t.Error("--no-wait must not poll CDNStatus")
	}
	if !strings.Contains(out.String(), "Cleanup queued for 2 orphaned CDN resource(s)") {
		t.Errorf("missing queued line: %q", out.String())
	}
	assertCleanCopy(t, out.String())
}

func TestCleanupWaitsUntilGone(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	fastPolls(t)

	var polls int
	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			polls++
			if polls < 2 {
				return &api.CDNStatusResponse{CleanupOrphanCount: 2}, nil
			}
			return &api.CDNStatusResponse{CleanupOrphanCount: 0}, nil
		},
	}

	var out bytes.Buffer
	if err := Cleanup(client, false, &out); err != nil {
		t.Fatal(err)
	}
	// Read once the wait has returned, which the mock's counter rule allows.
	// A count of one would mean the still-orphaned answer ended the wait.
	if polls != 2 {
		t.Errorf("polls = %d, want 2: the wait must poll on past an orphan count that is still standing", polls)
	}
	got := out.String()
	if !strings.Contains(got, "Cleaning up 2 orphaned CDN resource(s)") || !strings.Contains(got, "done.") {
		t.Errorf("missing progress/done copy: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestCleanupBlockedTerminal(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	polls := 0
	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			polls++
			return &api.CDNStatusResponse{CleanupOrphanCount: 2, CleanupBlockedCount: 2}, nil
		},
	}

	var out bytes.Buffer
	err := Cleanup(client, false, &out)
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("err = %v, want ErrCdnReported", err)
	}
	// The blocked answer must end the wait on the poll that sees it, not
	// travel the transient-budget route to the same user-visible arm.
	if polls != 1 {
		t.Errorf("polls = %d, want 1: the blocked answer must end the wait immediately", polls)
	}
	got := out.String()
	for _, want := range []string{"blocked.", "no longer valid", "Remove them in your Sakura panel."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing blocked copy %q in: %q", want, got)
		}
	}
	assertCleanCopy(t, got)
}

// On Ctrl-C the sentinel must propagate, since the 130 exit depends on
// recognizing it, and the stop line the wait prints is the whole report: the
// deletion carries on server-side, so no outcome line may claim otherwise.
func TestCleanupInterruptStopsAndPropagatesSentinel(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	interruptBeforeWait(t)

	var polls int
	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			polls++
			return &api.CDNStatusResponse{CleanupOrphanCount: 2}, nil
		},
	}

	var out bytes.Buffer
	err := Cleanup(client, false, &out)
	if !errors.Is(err, syncwait.ErrInterrupted) {
		t.Fatalf("err = %v, want syncwait.ErrInterrupted", err)
	}
	if errors.Is(err, ErrCdnReported) {
		t.Errorf("an interrupt is not a reported failure: %v", err)
	}
	// A signal already delivered wins over a status that would answer at once,
	// so the wait never reaches its first read.
	if polls != 0 {
		t.Errorf("CDNStatus called %d times, want none after an interrupt", polls)
	}
	got := out.String()
	if n := strings.Count(got, syncwait.SharedStopLine()); n != 1 {
		t.Errorf("stop line appears %d times, want once: %q", n, got)
	}
	// The wait closed the row before the stop line, so the working copy stands
	// as a line of its own with no terminator on it.
	if want := strings.TrimRight(i18n.Tf("cdn.cleanup_working", 2), " ") + "\n"; !strings.HasPrefix(got, want) {
		t.Errorf("output = %q, want it to open with the closed working row %q", got, want)
	}
	for _, banned := range []string{
		i18n.T("cdn.cleanup_done"),
		i18n.T("cdn.cleanup_blocked"),
		// The failure terminator's frame, with no reason in it, is what every
		// rendering of that outcome carries.
		i18n.Tf("cdn.cleanup_failed", ""),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q: %q", banned, got)
		}
	}
	assertCleanCopy(t, got)
}

// A status endpoint that keeps failing ends the wait once it has failed past
// what the wait tolerates, and that outcome is the command's to report: the row
// carries the reason and the error says it has already been printed.
func TestCleanupFailsOnAPersistentStatusError(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	fastPolls(t)

	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return nil, errors.New("connection refused")
		},
	}

	var out bytes.Buffer
	err := Cleanup(client, false, &out)
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("err = %v, want ErrCdnReported", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want the status failure's reason carried out", err)
	}
	// Off a terminal the whole wait is the working row, its held whitespace
	// restored, and the terminator reads on from it.
	got := out.String()
	want := i18n.Tf("cdn.cleanup_working", 2) + i18n.Tf("cdn.cleanup_failed", "connection refused") + "\n"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	assertCleanCopy(t, got)
}

// A refusal says nothing about the cleanup, so any copy this command has to
// offer would misdirect. The wait aborts on the first refused poll, closes the
// working line it left open, and lets the sentinel carry the message.
func TestCleanupAbortsOnAVersionRefusal(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	// A retried refusal would otherwise sit out the production cadence.
	fastPolls(t)

	polls := 0
	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			polls++
			return nil, api.ErrUpgradeRequired
		},
	}

	var out bytes.Buffer
	err := Cleanup(client, false, &out)
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	// ErrCdnReported suppresses the stderr echo, which is the only place the
	// upgrade message would appear.
	if errors.Is(err, ErrCdnReported) {
		t.Errorf("the refusal must not be marked as already reported: %v", err)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1: the refusal must not be retried", polls)
	}
	got := out.String()
	for _, banned := range []string{
		i18n.T("cdn.cleanup_done"),
		i18n.T("cdn.cleanup_blocked"),
		i18n.Tf("cdn.cleanup_failed", ""),
	} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not carry %q: %q", banned, got)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("the working line must be closed before the upgrade message: %q", got)
	}
	assertCleanCopy(t, got)
}
