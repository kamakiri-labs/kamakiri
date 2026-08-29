package rollback

import (
	"errors"
	"fmt"
	"io"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// APIClient is the API surface the rollback flow needs. Rollback re-points the
// live deploy and returns once the server has queued the work; GetSite backs the
// freshness wait that follows.
type APIClient interface {
	Rollback(siteID, deployID string) (*api.DeployResult, error)
	GetSite(id string) (*api.Site, error)
}

// Run rolls back to the named deploy. By default it blocks on the streaming
// freshness wait, so success means every cache a visitor could hit serves the
// rolled-back deploy; with noWait it returns as soon as the work is queued.
//
// A deploy that is already live changes nothing server-side, so that path
// enqueues no work and must not enter the wait: with nothing to converge on, the
// wait would settle on an earlier reconcile and report success for work that
// never ran.
func Run(client APIClient, deployID string, noWait bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	result, err := client.Rollback(config.ID, deployID)
	if err != nil {
		if isAlreadyLive(err) {
			fmt.Fprintln(out, i18n.Tf("rollback.already_live", deployID))
			return nil
		}
		return handleError(err, deployID)
	}

	if noWait {
		// Nothing here may claim the site is live: the rollback is only queued,
		// and no cache has been flushed.
		fmt.Fprintln(out, i18n.Tf("rollback.queued_no_wait", result.DeployID))
		fmt.Fprintf(out, "→ %s\n", result.URL)
		fmt.Fprintln(out, i18n.T("rollback.background_notice"))
		return nil
	}

	// The deploy id prints before the wait so that every exit path, converged or
	// blocked or interrupted, leaves it in scrollback for the user to quote.
	fmt.Fprintln(out, i18n.Tf("rollback.rolled_back", result.DeployID))
	return freshness.Wait(client, config.ID, freshness.RollbackMsgs(), out)
}

func isAlreadyLive(err error) bool {
	apiErr, ok := err.(*api.ErrorResponse)
	return ok && apiErr.Code == "already_live"
}

func handleError(err error, deployID string) error {
	apiErr, ok := err.(*api.ErrorResponse)
	if !ok {
		return err
	}

	switch apiErr.Code {
	case "deploy_not_found":
		return errors.New(i18n.Tf("rollback.err_deploy_not_found", deployID))
	default:
		return api.MapError(err)
	}
}
