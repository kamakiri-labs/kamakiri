package cdn

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// cleanupPollInterval paces the wait for the orphan count to reach zero. A few
// seconds keeps the wait responsive without hammering the status endpoint,
// since the deletion normally lands within a pass or two.
var cleanupPollInterval = 3 * time.Second

// cleanupWaitTimeout caps the wait for the orphans to be deleted. A transient
// delete failure is retried on the reconciler's own cron cadence, so the
// ceiling is wide enough to absorb a couple of those retries.
var cleanupWaitTimeout = 5 * time.Minute

// Cleanup deletes the linked site's orphaned WebAccel resources.
//
// This is the one command that deletes a resource from the customer's own
// account, and it does so only when asked directly: no mode flip and no domain
// removal ever deletes one as a side effect. The deletion is unconditional,
// with no check that traffic has moved off first, which is why the copy that
// leads users here tells them to repoint their DNS beforehand.
//
// Unless noWait is set, it returns only once the resources are actually gone,
// or once every remaining one is blocked on credentials the provider no longer
// accepts, which no amount of waiting will fix. The wait is bounded: at
// cleanupWaitTimeout it reports the cleanup still working and returns an error.
// A version refusal ends it on the poll that sees it, since every later poll
// would be refused the same way.
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

	fmt.Fprint(out, i18n.Tf("cdn.cleanup_working", result.OrphanCount))

	deadline := time.Now().Add(cleanupWaitTimeout)
	for {
		st, sErr := client.CDNStatus(config.ID)
		// The deletion is the server's to finish and the timeout copy points at
		// `kamakiri cdn`, which the same floor refuses. The open working line is
		// closed and the sentinel travels bare, unmarked, so the command layer
		// prints the upgrade message.
		if errors.Is(sErr, api.ErrUpgradeRequired) {
			fmt.Fprintln(out)
			return sErr
		}
		if sErr == nil {
			switch {
			case st.CleanupOrphanCount == 0:
				// The orphan count includes the blocked ones, so zero means
				// nothing is left at all, not just nothing retryable. That is what
				// keeps the blocked case below from claiming a finished cleanup.
				fmt.Fprintln(out, i18n.T("cdn.cleanup_done"))
				return nil

			case st.CleanupBlockedCount >= st.CleanupOrphanCount:
				// Nothing retryable is left, so waiting longer cannot help.
				fmt.Fprintln(out, i18n.T("cdn.cleanup_blocked"))
				fmt.Fprintln(out, i18n.T("cdn.cleanup_blocked_reason"))
				fmt.Fprintln(out, i18n.T("cdn.cleanup_blocked_fix"))
				return fmt.Errorf("%w: %s", ErrCdnReported, i18n.T("cdn.err_cleanup_blocked"))
			}
		}

		if time.Now().After(deadline) {
			fmt.Fprintln(out, i18n.T("cdn.cleanup_still_working"))
			fmt.Fprintln(out, i18n.T("cdn.cleanup_check_hint"))
			return fmt.Errorf("%w: %s", ErrCdnReported, i18n.T("cdn.err_cleanup_timed_out"))
		}

		time.Sleep(cleanupPollInterval)
	}
}
