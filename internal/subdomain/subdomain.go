package subdomain

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/streamui"
	"github.com/kamakiri-labs/kamakiri/internal/syncwait"
)

// APIClient is the API surface the subdomain commands need.
type APIClient interface {
	GetSite(id string) (*api.Site, error)
	UpdateSubdomain(siteID, subdomain string) (*api.Site, error)
	DisableSubdomain(siteID string) (*api.Site, error)
	EnableSubdomain(siteID string) (*api.Site, error)
}

var signalCtx = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// pollSchedule is the cadence a subdomain change's wait polls on, a seam so a
// test can drive several polls without waiting out the production bands.
var pollSchedule = syncwait.DefaultSchedule

// awaitChange blocks until the change the server accepted is reconciled.
// Nothing is on screen while it runs until the liveness token is due, at which
// point tickText says which of the three changes is being waited on. On a
// terminal the token is erased again before the wait returns, and off one each
// token is a finished line, so either way the caller's result line lands on a
// clean row.
func awaitChange(client APIClient, siteID string, attemptID int64, tickText string, out io.Writer) (*api.Site, error) {
	ctx, stop := signalCtx()
	defer stop()

	return syncwait.Sync(ctx, client, siteID, attemptID, syncwait.Options{
		Out:      out,
		TickText: tickText,
		StopLine: syncwait.SharedStopLine(),
		Schedule: pollSchedule,
		Now:      time.Now,
		IsTTY:    streamui.IsTerminalWriter(out),
	})
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

	final, err := awaitChange(client, config.ID, site.SyncAttemptID, i18n.T("subdomain.tick_renaming"), out)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, i18n.Tf("subdomain.renamed", final.Subdomain, api.PagesDomain))
	return nil
}

// Disable stops the subdomain route serving, waiting for the reconcile unless
// noWait is set. The name itself stays reserved.
func Disable(client APIClient, noWait bool, out io.Writer) error {
	return toggle(client, noWait, out, client.DisableSubdomain,
		i18n.T("subdomain.verbing_disable"), i18n.T("subdomain.tick_disabling"),
		func(out io.Writer, host string) {
			fmt.Fprintln(out, i18n.Tf("subdomain.disabled", host))
			fmt.Fprintln(out, i18n.T("subdomain.disabled_still_reserved"))
		})
}

// Enable puts the subdomain route back, waiting for the reconcile unless noWait
// is set.
func Enable(client APIClient, noWait bool, out io.Writer) error {
	return toggle(client, noWait, out, client.EnableSubdomain,
		i18n.T("subdomain.verbing_enable"), i18n.T("subdomain.tick_enabling"),
		func(out io.Writer, host string) {
			fmt.Fprintln(out, i18n.Tf("subdomain.enabled", host))
		})
}

// toggle is the shared body of Disable and Enable. verbing is the word the
// queued hint reads with; tickText is the whole sentence the wait's token
// follows, a string of its own rather than a frame around verbing, since
// Japanese builds neither from the other. printSuccess is the caller's, since
// only disable has anything to reassure the user about.
func toggle(
	client APIClient,
	noWait bool,
	out io.Writer,
	mutate func(string) (*api.Site, error),
	verbing string,
	tickText string,
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

	final, err := awaitChange(client, config.ID, site.SyncAttemptID, tickText, out)
	if err != nil {
		return err
	}

	printSuccess(out, final.Subdomain+"."+api.PagesDomain)
	return nil
}
