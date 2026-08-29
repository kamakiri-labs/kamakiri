package cdn

import (
	"fmt"
	"io"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// Verify re-attaches the go-live watch to a CDN change already in flight,
// after a Ctrl-C or from another machine. It mutates nothing and can be run as
// often as the user likes.
//
// It deliberately does not ask the server to re-check anything. Domain
// verification offers a recheck for a user waiting on its DNS poll; here the
// CLI assumes CDN validation is already polled on every reconciler tick, so a
// recheck for symmetry would buy nothing.
func Verify(client APIClient, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	status, err := client.CDNStatus(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	// A site with no CDN and one halfway through leaving its CDN look the same
	// from here, and this command can resume neither. The second line is what
	// keeps the mid-exit user from being stranded: their climb resumes under a
	// different command.
	if status.CDNMode == "" || status.CDNMode == "none" {
		fmt.Fprintln(out, i18n.T("cdn.verify_no_cdn"))
		fmt.Fprintln(out, i18n.T("cdn.verify_no_cdn_exit_hint"))
		return nil
	}

	return WatchToLive(client, config.ID, status.CDNMode, out)
}
