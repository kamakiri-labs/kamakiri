package cdn

import (
	"fmt"
	"io"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// Purge issues a manual CDN cache purge for the linked site.
//
// Unless noWait is set it returns only once every cache a visitor could hit,
// ours and the provider's alike, is serving the current deploy. That wait is
// deliberately unbounded: it ends when the flush lands, when something blocks
// it that only the user can clear, when the user interrupts it, or when the
// status endpoint fails past its budget, never on a timer that would report
// success over a stale edge.
func Purge(client APIClient, noWait bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	if _, err := client.PurgeCDN(config.ID); err != nil {
		return api.MapError(err)
	}

	if noWait {
		fmt.Fprintln(out, i18n.T("cdn.purge_queued"))
		return nil
	}

	return freshness.Wait(client, config.ID, freshness.PurgeMsgs(), out)
}
