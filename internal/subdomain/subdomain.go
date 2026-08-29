package subdomain

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// defaultSyncTimeout is how long a subdomain change waits for the reconcile to
// converge before printing the "still syncing" hint and exiting nonzero.
const defaultSyncTimeout = 60 * time.Second

// APIClient is the API surface the subdomain commands need.
type APIClient interface {
	GetSite(id string) (*api.Site, error)
	UpdateSubdomain(siteID, subdomain string) (*api.Site, error)
	DisableSubdomain(siteID string) (*api.Site, error)
	EnableSubdomain(siteID string) (*api.Site, error)
	WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
}

// Get prints the current subdomain for the linked site.
func Get(client APIClient, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	site, err := client.GetSite(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	fmt.Fprintln(out, api.SubdomainLine(site))
	return nil
}

// Set renames the site's subdomain, waiting for the reconcile unless noWait is
// set.
func Set(client APIClient, subdomain string, noWait bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	site, err := client.UpdateSubdomain(config.ID, subdomain)
	if err != nil {
		return api.MapError(err)
	}

	if noWait {
		fmt.Fprintln(out, i18n.Tf("subdomain.renaming_no_wait", site.Subdomain, api.PagesDomain))
		return nil
	}

	final, err := client.WaitForSync(config.ID, site.SyncAttemptID, defaultSyncTimeout)
	if err != nil {
		if errors.Is(err, api.ErrSyncTimeout) {
			// The hint is for the person; the sentinel still has to reach the
			// caller, since exiting 0 would tell a CI script the rename
			// definitely landed when its status is unknown.
			fmt.Fprintln(out, i18n.T("subdomain.set_still_syncing"))
			return err
		}
		return err
	}
	fmt.Fprintln(out, i18n.Tf("subdomain.renamed", final.Subdomain, api.PagesDomain))
	return nil
}

// Disable stops the subdomain route serving, waiting for the reconcile unless
// noWait is set. The name itself stays reserved.
func Disable(client APIClient, noWait bool, out io.Writer) error {
	return toggle(client, noWait, out, client.DisableSubdomain, i18n.T("subdomain.verbing_disable"),
		func(out io.Writer, host string) {
			fmt.Fprintln(out, i18n.Tf("subdomain.disabled", host))
			fmt.Fprintln(out, i18n.T("subdomain.disabled_still_reserved"))
		})
}

// Enable puts the subdomain route back, waiting for the reconcile unless noWait
// is set.
func Enable(client APIClient, noWait bool, out io.Writer) error {
	return toggle(client, noWait, out, client.EnableSubdomain, i18n.T("subdomain.verbing_enable"),
		func(out io.Writer, host string) {
			fmt.Fprintln(out, i18n.Tf("subdomain.enabled", host))
		})
}

// toggle is the shared body of Disable and Enable. verbing is the rendered word
// the queued and still-syncing hints read with, and printSuccess is the
// caller's, since only disable has anything to reassure the user about.
func toggle(
	client APIClient,
	noWait bool,
	out io.Writer,
	mutate func(string) (*api.Site, error),
	verbing string,
	printSuccess func(out io.Writer, host string),
) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	site, err := mutate(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	if noWait {
		fmt.Fprintln(out, i18n.Tf("subdomain.toggle_no_wait", verbing, site.Subdomain, api.PagesDomain))
		return nil
	}

	final, err := client.WaitForSync(config.ID, site.SyncAttemptID, defaultSyncTimeout)
	if err != nil {
		if errors.Is(err, api.ErrSyncTimeout) {
			fmt.Fprintln(out, i18n.Tf("subdomain.toggle_still_syncing", verbing))
			return err
		}
		return err
	}

	printSuccess(out, final.Subdomain+"."+api.PagesDomain)
	return nil
}
