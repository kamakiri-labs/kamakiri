package deploys

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// columnGap separates two columns of the listing. The table is laid out by hand
// rather than through text/tabwriter, which measures a cell in runes: it reads a
// three-character Japanese label as three columns wide where a terminal gives it
// six, and pads against that, so a cell already padded to its display width is
// pushed right again.
const columnGap = "  "

// APIClient is the API surface the deploy listing needs.
type APIClient interface {
	ListDeploys(siteID string) ([]api.Deploy, error)
}

// Run lists recent deploys for the current site.
func Run(client APIClient, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	deploys, err := client.ListDeploys(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	if len(deploys) == 0 {
		fmt.Fprintln(out, i18n.T("deploys.empty"))
		return nil
	}

	idHeader := i18n.T("deploys.header_id")
	statusHeader := i18n.T("deploys.header_status")

	// Each column is as wide as its widest cell, the header included. The last
	// column is never padded, so no line ends in whitespace.
	idWidth := i18n.Width(idHeader)
	statusWidth := i18n.Width(statusHeader)
	for _, d := range deploys {
		idWidth = max(idWidth, i18n.Width(d.ID))
		statusWidth = max(statusWidth, i18n.Width(displayStatus(d.Status)))
	}

	row := func(id, status, created string) {
		fmt.Fprintln(out, strings.Join([]string{
			i18n.Pad(id, idWidth),
			i18n.Pad(status, statusWidth),
			created,
		}, columnGap))
	}

	row(idHeader, statusHeader, i18n.T("deploys.header_created"))
	for _, d := range deploys {
		row(d.ID, displayStatus(d.Status), formatCreatedAt(d.CreatedAt))
	}

	return nil
}

func displayStatus(status string) string {
	if status == "live" {
		return i18n.T("deploys.status_live")
	}
	return ""
}

func formatCreatedAt(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
