package cdn

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/syncwait"
)

// errCleanupBlocked ends the wait when nothing retryable is left. It never
// reaches a user: it is how the poll tells the caller which terminal outcome
// arrived, and the caller prints the blocked copy and returns the reported
// error in its place.
var errCleanupBlocked = errors.New("cdn cleanup blocked on invalid credentials")

// awaitCleanup blocks until the orphaned resources are gone or the deletion
// has nothing left it can retry, leaving the working line open for the caller
// to terminate.
func awaitCleanup(client APIClient, siteID string, orphans int, out io.Writer) error {
	ctx, stop := signalCtx()
	defer stop()

	return syncwait.Poll(ctx, syncwait.Options{
		Out:      out,
		Prefix:   i18n.Tf("cdn.cleanup_working", orphans),
		StopLine: syncwait.SharedStopLine(),
		Schedule: pollSchedule,
		Now:      time.Now,
		IsTTY:    isTerminalWriter(out),
	}, func() (bool, error) {
		st, err := client.CDNStatus(siteID)
		if err != nil {
			return false, err
		}
		switch {
		case st.CleanupOrphanCount == 0:
			// The orphan count includes the blocked ones, so zero means nothing
			// is left at all, not just nothing retryable. That is what keeps the
			// blocked case below from claiming a finished cleanup.
			return true, nil
		case st.CleanupBlockedCount >= st.CleanupOrphanCount:
			// Nothing retryable is left, so waiting longer cannot help.
			return true, errCleanupBlocked
		}
		return false, nil
	})
}

// Cleanup deletes the linked site's orphaned WebAccel resources.
//
// This is the one command that deletes a resource from the customer's own
// account, and it does so only when asked directly: no mode flip and no domain
// removal ever deletes one as a side effect. The deletion is unconditional,
// with no check that traffic has moved off first, which is why the copy that
// leads users here tells them to repoint their DNS beforehand.
//
// Unless noWait is set, it returns once the resources are actually gone, once
// every remaining one is blocked on credentials the provider no longer accepts,
// which no amount of waiting will fix, once the status endpoint has failed past
// what the wait tolerates, or once the user interrupts it. A version refusal
// ends it on the poll that sees it, since every later poll would be refused the
// same way.
func Cleanup(client APIClient, noWait bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	result, err := client.CleanupCDN(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	if result.OrphanCount == 0 {
		fmt.Fprintln(out, i18n.T("cdn.cleanup_nothing"))
		return nil
	}

	if noWait {
		fmt.Fprintln(out, i18n.Tf("cdn.cleanup_queued", result.OrphanCount))
		return nil
	}

	if waitErr := awaitCleanup(client, config.ID, result.OrphanCount, out); waitErr != nil {
		// The deletion is the server's to finish, and a refusal says nothing
		// about how far it got, so any copy this command has would misdirect.
		// The open working line is closed and the sentinel travels bare,
		// unmarked, so the command layer prints the upgrade message.
		if errors.Is(waitErr, api.ErrUpgradeRequired) {
			fmt.Fprintln(out)
			return waitErr
		}
		// The wait has closed the line and said the deletion carries on, so
		// anything printed here would report on resources still being deleted.
		if errors.Is(waitErr, syncwait.ErrInterrupted) {
			return waitErr
		}
		if errors.Is(waitErr, errCleanupBlocked) {
			fmt.Fprintln(out, i18n.T("cdn.cleanup_blocked"))
			fmt.Fprintln(out, i18n.T("cdn.cleanup_blocked_reason"))
			fmt.Fprintln(out, i18n.T("cdn.cleanup_blocked_fix"))
			return fmt.Errorf("%w: %s", ErrCdnReported, i18n.T("cdn.err_cleanup_blocked"))
		}
		// The status endpoint failed past what the wait tolerates, so how far
		// the deletion got is unknown. The terminator carries the reason, which
		// is the whole report, so the error is marked as already reported.
		fmt.Fprintln(out, i18n.Tf("cdn.cleanup_failed", waitErr))
		return fmt.Errorf("%w: %w", ErrCdnReported, waitErr)
	}
	fmt.Fprintln(out, i18n.T("cdn.cleanup_done"))
	return nil
}
