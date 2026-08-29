package api

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// ErrSyncTimeout is returned by WaitForSync when no terminal outcome is seen
// before the deadline expires. Callers match it with errors.Is to tell a timeout
// from a genuine "error" outcome.
var ErrSyncTimeout = errors.New("sync timed out")

// transientBudget caps how many consecutive GetSite errors WaitForSync tolerates
// before giving up. It absorbs brief 5xx during deploys and momentary network
// blips; past the budget something is genuinely wrong and the error propagates.
const transientBudget = 5

// WaitForSync polls GET /v1/pages/sites/:id until a sync attempt with id >=
// sinceAttemptID reaches outcome ok or error, or until timeout. The id
// comparison keeps an older ok row from satisfying the wait. On error it returns
// the site alongside a non-nil error carrying the reconciler's own failure
// reason, or a pointer to `kamakiri status` when the row carries none. Up to
// transientBudget consecutive GetSite errors are tolerated, the count resetting
// on the first success. A version refusal is the exception, tolerated zero
// times: it is returned on the first poll that sees it.
func (c *Client) WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*Site, error) {
	deadline := time.Now().Add(timeout)
	transientErrs := 0
	var lastSite *Site

	for {
		site, err := c.GetSite(siteID)
		if err != nil {
			// A refused version is refused on every poll, so spending the budget
			// on it only delays the upgrade message the caller is about to show.
			if errors.Is(err, ErrUpgradeRequired) {
				return nil, err
			}
			transientErrs++
			if transientErrs > transientBudget {
				return nil, MapError(err)
			}
			if time.Now().After(deadline) {
				return lastSite, ErrSyncTimeout
			}
			time.Sleep(pollInterval())
			continue
		}
		transientErrs = 0
		lastSite = site

		if site.Sync != nil && site.Sync.LatestAttemptID >= sinceAttemptID {
			switch site.Sync.Outcome {
			case "ok":
				return site, nil
			case "error":
				msg := site.Sync.Error
				if msg == "" {
					msg = i18n.T("api.see_status")
				}
				// The reconciler's own reason is a server-origin string and
				// travels verbatim; only the frame around it is ours to word.
				return site, errors.New(i18n.Tf("api.err_reconcile_failed", msg))
			}
		}
		if time.Now().After(deadline) {
			return site, ErrSyncTimeout
		}
		time.Sleep(pollInterval())
	}
}

// pollInterval returns 500ms with ±20% of jitter. Without the jitter, many CLIs
// polling at once (a CI fan-out, say) would synchronize on the same tick and
// thunder the server; the spread is small enough not to move single-user
// latency.
func pollInterval() time.Duration {
	const base = 500 * time.Millisecond
	const spread = 200 * time.Millisecond // ±100ms
	return base - spread/2 + time.Duration(rand.Int64N(int64(spread)))
}
