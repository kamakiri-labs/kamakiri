package teardown

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// defaultTeardownSyncTimeout is how long teardown waits for the reconcile to
// converge before printing "still syncing" and exiting nonzero.
const defaultTeardownSyncTimeout = 60 * time.Second

// APIClient is the API surface the teardown flow needs.
type APIClient interface {
	GetSite(id string) (*api.Site, error)
	DeleteSite(id string) (*api.Site, error)
	WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
}

// Run drives the teardown flow: confirm, commit the deletion, wait for the
// reconcile unless noWait is set, and remove the local config. The config goes
// whatever the reconcile did, since it can only point at a site being deleted.
func Run(client APIClient, in io.Reader, out io.Writer, noWait bool) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	reader := bufio.NewReader(in)

	site, err := client.GetSite(config.ID)
	if err != nil {
		apiErr, ok := err.(*api.ErrorResponse)
		if !ok {
			return err
		}
		switch apiErr.Code {
		case "site_not_found":
			removeProjectConfig(out)
			return nil
		case "unauthorized":
			return errors.New(i18n.T("api.err_unauthorized"))
		case "forbidden":
			return errors.New(i18n.T("api.err_forbidden"))
		default:
			return err
		}
	}

	fmt.Fprintln(out, i18n.Tf("teardown.confirm_notice", site.Subdomain))
	fmt.Fprint(out, i18n.T("teardown.prompt_confirm"))

	answer, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" {
		fmt.Fprintln(out, i18n.T("teardown.aborted"))
		return nil
	}

	// The deletion is only accepted here; the edge route, DNS, CDN and stored
	// objects go with the reconcile that follows.
	resp, err := client.DeleteSite(config.ID)
	if err != nil {
		apiErr, ok := err.(*api.ErrorResponse)
		if ok {
			switch apiErr.Code {
			case "site_not_found":
				// Already gone server-side, so the local config is all
				// that is left to remove.
				removeProjectConfig(out)
				return nil
			case "forbidden":
				return errors.New(i18n.T("api.err_forbidden"))
			case "unauthorized":
				return errors.New(i18n.T("api.err_unauthorized"))
			}
		}
		return err
	}

	if noWait {
		fmt.Fprintln(out, i18n.T("teardown.queued_no_wait"))
		printRetainedWebAccelNotice(out, resp.RetainedWebAccelResources)
		removeProjectConfig(out)
		return nil
	}

	// The prefix prints before the wait, so the user sees the request land
	// rather than a silent terminal.
	fmt.Fprint(out, i18n.T("teardown.tearing_down"))
	final, syncErr := client.WaitForSync(config.ID, resp.SyncAttemptID, defaultTeardownSyncTimeout)

	switch {
	case syncErr == nil:
		fmt.Fprintln(out, i18n.T("teardown.done"))
	case errors.Is(syncErr, api.ErrUpgradeRequired):
		// Every failure label ends in "Check `kamakiri status`", a command the
		// same floor refuses, and no teardown step failed anyway. Closing the open
		// line is all this outcome needs; the sentinel carries the message.
		fmt.Fprintln(out)
	case errors.Is(syncErr, api.ErrSyncTimeout):
		fmt.Fprintln(out, i18n.T("teardown.still_syncing"))
	default:
		fmt.Fprintln(out, failureLabel(final))
	}

	// The notice belongs on every outcome, converged or not: what it reports is
	// the user's own resource, which teardown never deletes either way.
	printRetainedWebAccelNotice(out, resp.RetainedWebAccelResources)

	removeProjectConfig(out)
	return syncErr
}

// failureLabel names the teardown step a partial reconcile stopped at, inferred
// from the changes the server reported: the steps run in a fixed order, so the
// first one missing is where it stopped. With nothing to infer from (no site, no
// sync record, or no changes at all) it falls back to the unqualified failure.
func failureLabel(site *api.Site) string {
	if site == nil || site.Sync == nil || len(site.Sync.Changes) == 0 {
		return i18n.T("teardown.failed")
	}

	hasEdgeDelete := false
	hasDNS := false
	hasCDN := false
	hasStoragePurge := false
	for _, c := range site.Sync.Changes {
		switch {
		case c == "caddy_delete":
			hasEdgeDelete = true
		case strings.HasPrefix(c, "dns_delete:"):
			hasDNS = true
		case strings.HasPrefix(c, "cdn_deregister:"):
			hasCDN = true
		case c == "s3_purge":
			hasStoragePurge = true
		}
	}

	switch {
	case !hasEdgeDelete:
		return i18n.T("teardown.failed_edge_delete")
	case !hasDNS:
		// A site with no domains skips DNS legitimately, so this label is a
		// hint about where to look, not a verdict.
		return i18n.T("teardown.failed_dns_delete")
	case !hasCDN:
		return i18n.T("teardown.failed_cdn_deregister")
	case !hasStoragePurge:
		return i18n.T("teardown.failed_storage_purge")
	default:
		return i18n.T("teardown.failed")
	}
}

// printRetainedWebAccelNotice names the WebAccel resources teardown leaves
// behind. They live in the user's own Sakura account, so we never delete them,
// unlike a Cloudflare hostname, which is ours and goes automatically. The notice
// has to be printed now because afterwards the site is gone and `cdn cleanup`
// can no longer reach the resource, leaving only the Sakura panel.
func printRetainedWebAccelNotice(out io.Writer, resources []api.WebAccelResource) {
	if len(resources) == 0 {
		return
	}

	labels := make([]string, 0, len(resources))
	for _, r := range resources {
		if r.Subdomain != "" {
			labels = append(labels, r.Subdomain)
		} else {
			labels = append(labels, r.ID)
		}
	}

	if len(labels) == 1 {
		fmt.Fprintln(out, i18n.Tf("teardown.webaccel_retained_one", labels[0]))
		return
	}
	fmt.Fprintln(out, i18n.Tf("teardown.webaccel_retained_many", strings.Join(labels, ", ")))
}

func removeProjectConfig(out io.Writer) {
	if err := os.Remove(core.ProjectPath()); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintln(out, i18n.Tf("teardown.warn_remove_config", err))
		}
		return
	}
	fmt.Fprintln(out, i18n.T("teardown.removed_config"))
}
