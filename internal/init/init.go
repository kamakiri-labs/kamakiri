package initcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// defaultInitSyncTimeout is how long init waits for the reconcile to converge
// before printing the "still syncing" hint and exiting nonzero.
const defaultInitSyncTimeout = 60 * time.Second

// APIClient is the API surface the init flow needs. Site creation is accepted
// asynchronously, so WaitForSync is what turns it into a terminal outcome.
type APIClient interface {
	CreateSite(subdomain string) (*api.Site, error)
	GetSite(id string) (*api.Site, error)
	WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
}

// Run executes the init flow, reading from in and writing to out. It blocks on
// the reconcile unless noWait is set, which is for CI scripts that only want the
// site created.
//
// The local config file is written before the wait, so an interrupted or failed
// reconcile still leaves the user a handle on the site. Without it, a site would
// exist server-side that the CLI cannot address, and re-running init would only
// collide with its own subdomain.
func Run(client APIClient, noWait bool, in io.Reader, out io.Writer) error {
	creds, err := core.LoadCredentials()
	if err != nil {
		return err
	}
	if creds == nil {
		return errors.New(i18n.T("common.err_not_logged_in"))
	}

	config, err := core.LoadProject()
	if err != nil {
		return err
	}
	if config != nil {
		// The local config holds only the id, so the friendly hostname costs a
		// lookup. Offline, the id alone still tells the user which site this
		// directory is linked to.
		target := config.ID
		if site, err := client.GetSite(config.ID); err == nil && site.Subdomain != "" {
			target = site.Subdomain + "." + api.PagesDomain
		}
		fmt.Fprintln(out, i18n.Tf("init.already_initialized", target))
		return nil
	}

	reader := bufio.NewReader(in)

	for {
		fmt.Fprint(out, i18n.T("init.prompt_subdomain"))
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		subdomain := strings.TrimSpace(line)

		site, createErr := client.CreateSite(subdomain)
		if createErr == nil {
			if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: site.ID}); err != nil {
				return err
			}

			if noWait {
				fmt.Fprintln(out, i18n.Tf("init.creating_no_wait", site.Subdomain, api.PagesDomain))
				fmt.Fprintln(out, i18n.T("init.saved_config"))
				return nil
			}

			// The terminator prints only once the wait resolves: nothing may
			// report done while the edge is not yet serving the route.
			fmt.Fprint(out, i18n.T("init.creating"))
			if _, err := client.WaitForSync(site.ID, site.SyncAttemptID, defaultInitSyncTimeout); err != nil {
				if errors.Is(err, api.ErrSyncTimeout) {
					fmt.Fprintln(out, i18n.T("init.still_syncing"))
					fmt.Fprintln(out, i18n.T("init.saved_config"))
					return err
				}
				// The reason goes on stdout as well as stderr, so it is in
				// front of whoever is reading the run. The site is left in
				// place: tearing it down for the user would be destroying
				// something they never asked to destroy.
				fmt.Fprintln(out, i18n.Tf("init.failed", err))
				fmt.Fprintln(out, i18n.T("init.saved_config"))
				return err
			}

			fmt.Fprintln(out, i18n.T("init.done"))
			fmt.Fprintln(out, i18n.T("init.saved_config"))
			return nil
		}

		apiErr, ok := createErr.(*api.ErrorResponse)
		if !ok {
			return createErr
		}

		switch apiErr.Code {
		case "subdomain_taken":
			fmt.Fprintln(out, i18n.Tf("init.err_subdomain_taken", subdomain))
		case "subdomain_taken_own_account":
			fmt.Fprintln(out, i18n.Tf("init.subdomain_taken_own_account", subdomain))
			fmt.Fprintln(out, i18n.T("init.override_notice"))
			fmt.Fprint(out, i18n.T("init.prompt_use_anyway"))
			answer, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer == "y" || answer == "" {
				if apiErr.SiteID == "" {
					return errors.New(i18n.T("init.err_empty_site_id"))
				}
				if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: apiErr.SiteID}); err != nil {
					return err
				}
				// Adopting an existing site enqueues no reconcile, so there is
				// nothing to wait on; the site's last sync, if it has one, has
				// nothing to do with this command.
				fmt.Fprintln(out, i18n.T("init.saved_config"))
				return nil
			}
		case "invalid_subdomain":
			fmt.Fprintln(out, i18n.T("init.err_invalid_subdomain"))
		case "subdomain_reserved":
			fmt.Fprintln(out, i18n.T("init.err_subdomain_reserved"))
		case "unauthorized":
			return errors.New(i18n.T("api.err_unauthorized"))
		default:
			return apiErr
		}
	}
}
