package cdn

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
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

	restoreCleanupTiming(t, time.Millisecond, time.Second)

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

	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CleanupOrphanCount: 2, CleanupBlockedCount: 2}, nil
		},
	}

	var out bytes.Buffer
	err := Cleanup(client, false, &out)
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("err = %v, want ErrCdnReported", err)
	}
	got := out.String()
	for _, want := range []string{"blocked.", "no longer valid", "Remove them in your Sakura panel."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing blocked copy %q in: %q", want, got)
		}
	}
	assertCleanCopy(t, got)
}

func TestCleanupTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	restoreCleanupTiming(t, time.Millisecond, 5*time.Millisecond)

	client := &mockClient{
		cleanupCDNFn: func(string) (*api.CDNCleanupResponse, error) {
			return &api.CDNCleanupResponse{OrphanCount: 2}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CleanupOrphanCount: 2}, nil
		},
	}

	var out bytes.Buffer
	err := Cleanup(client, false, &out)
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("err = %v, want ErrCdnReported", err)
	}
	if !strings.Contains(out.String(), "still working.") {
		t.Errorf("missing timeout copy: %q", out.String())
	}
	assertCleanCopy(t, out.String())
}

// A refusal says nothing about the cleanup, and the timeout copy points at
// `kamakiri cdn`, which the same floor refuses. The wait aborts on the first
// refused poll, closes the working line it left open, and lets the sentinel
// carry the message.
func TestCleanupAbortsOnAVersionRefusal(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	// A retried refusal would otherwise sit out the production ceiling.
	restoreCleanupTiming(t, time.Millisecond, 5*time.Millisecond)

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
		i18n.T("cdn.cleanup_still_working"),
		i18n.T("cdn.cleanup_check_hint"),
		i18n.T("cdn.cleanup_done"),
		i18n.T("cdn.cleanup_blocked"),
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

// restoreCleanupTiming shortens the poll cadence and ceiling for one test,
// restoring both when it ends.
func restoreCleanupTiming(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	oldInterval, oldTimeout := cleanupPollInterval, cleanupWaitTimeout
	cleanupPollInterval, cleanupWaitTimeout = interval, timeout
	t.Cleanup(func() { cleanupPollInterval, cleanupWaitTimeout = oldInterval, oldTimeout })
}
