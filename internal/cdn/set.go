package cdn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/domain"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

var signalCtx = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// SetMode sets the CDN mode for the linked site, either cloudflare or none.
// WebAccel has its own entry point, SetWebAccel, because it needs credentials.
//
// Unless noWait is set, the command blocks until the site is actually serving
// the way the user asked. For cloudflare that is the go-live watch, which
// guides the validation DNS step and returns only once every content-serving
// domain is live. Switching off WebAccel is the exception: that route needs a
// terminal to stream both of its legs, and without one it queues the exit to
// direct serving alone and names the command to run next. For none the wait is
// the climb back to direct serving, the domain-set flow in reverse: a WebAccel
// prior needs the customer to re-point their own record, while a Cloudflare
// prior only needs our indirection to propagate. A none prior with nothing
// fronted has nothing to bring back and returns after the single change.
func SetMode(client APIClient, mode string, noWait bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	if mode == "none" {
		return setNone(client, config.ID, noWait, out)
	}

	// The server rejects a direct provider flip, so a site already on WebAccel
	// has to route through none. Reading the prior mode here lets that route be
	// composed as one streamed flow. The read is best-effort: if it fails the
	// single-leg path runs and the server's own guard still refuses the flip
	// cleanly, rather than leaving a half-configured switch.
	var priorStatus *api.CDNStatusResponse
	if st, sErr := client.CDNStatus(config.ID); sErr == nil {
		priorStatus = st
	}
	if priorStatus != nil && priorStatus.CDNMode == "webaccel" {
		return switchToCloudflare(client, config.ID, priorStatus, noWait, out)
	}

	result, err := client.SetCDN(config.ID, mode)
	if err != nil {
		return api.MapError(err)
	}
	if result == nil || result.SyncAttemptID == 0 {
		return errNoSyncAttempt()
	}

	fmt.Fprintln(out, i18n.Tf("cdn.set_mode_done", ProviderLabel(mode)))
	WriteApexARecordsBlock(out, result.ApexARecordsToUpdate)
	WriteApexARecordsPendingBlock(out, result.ApexARecordsPending)
	if noWait {
		fmt.Fprintln(out, i18n.T("cdn.change_queued_verify"))
		return nil
	}
	return WatchToLive(client, config.ID, mode, out)
}

// switchToCloudflare composes a WebAccel to Cloudflare switch as one streamed
// flow: the exit back to live direct serving, then the Cloudflare go-live.
//
// The apex check runs before anything is written, because Cloudflare cannot
// serve an apex: without it the site would be torn off a working WebAccel for
// a switch that can never complete. The server enforces this independently and
// the check here is only for a better refusal.
//
// Without a stream to watch it degrades to the exit leg plus guidance rather
// than queueing a flip the server would reject.
func switchToCloudflare(client APIClient, siteID string, priorStatus *api.CDNStatusResponse, noWait bool, out io.Writer) error {
	if blockers := cloudflareApexBlockers(priorStatus); len(blockers) > 0 {
		return refuseCloudflareApexSwitch(out, blockers)
	}
	if !switchStreamable(noWait, out) {
		return switchNoWaitDegrade(client, siteID, "cloudflare", out)
	}

	return composeSwitch(client, siteID, priorStatus, "webaccel", "cloudflare",
		func(ctx context.Context) error {
			result, err := client.SetCDN(siteID, "cloudflare")
			if err != nil {
				return api.MapError(err)
			}
			if result == nil || result.SyncAttemptID == 0 {
				return errNoSyncAttempt()
			}
			fmt.Fprintln(out, i18n.Tf("cdn.set_mode_done", ProviderLabel("cloudflare")))
			WriteApexARecordsBlock(out, result.ApexARecordsToUpdate)
			WriteApexARecordsPendingBlock(out, result.ApexARecordsPending)
			if err := WatchToLiveCtx(ctx, client, siteID, "cloudflare", out); err != nil {
				return err
			}
			writeSwitchWebAccelCleanupLine(out, priorStatus)
			return nil
		}, out)
}

// switchStreamable reports whether a switch can stream both legs as one flow.
// It dwells at live direct serving between them, which only an interactive run
// that was not asked to return early can show.
var switchStreamable = func(noWait bool, out io.Writer) bool {
	return !noWait && isTerminalWriter(out)
}

// composeSwitch streams a provider-to-provider switch as one flow: the exit
// back to live direct serving, then goLive for the new provider, both under one
// interrupt region.
//
// A failure in the first leg short-circuits before the second, leaving the site
// on the old provider or on none, either of which serves. A failure in the
// second leaves the site on none, or on the new provider once the flip is
// accepted; the server converges on whichever was last accepted. Nothing forces
// the second leg: the user may stop at the dwell for good.
func composeSwitch(client APIClient, siteID string, priorStatus *api.CDNStatusResponse, fromProvider, toProvider string, goLive func(ctx context.Context) error, out io.Writer) error {
	fmt.Fprintln(out, i18n.Tf("cdn.switching_providers",
		ProviderLabel(fromProvider), ProviderLabel(toProvider)))

	ctx, stop := signalCtx()
	defer stop()

	if _, _, err := exitToDirect(ctx, client, siteID, priorStatus, isTerminalWriter(out), true, out); err != nil {
		return err
	}
	return goLive(ctx)
}

// switchNoWaitDegrade handles a switch that cannot be streamed. It queues only
// the exit leg and tells the user which command to run once the site is serving
// directly. Queueing the flip itself is not an option: the server rejects a
// direct one, so the request would fail later with nothing watching.
func switchNoWaitDegrade(client APIClient, siteID, targetMode string, out io.Writer) error {
	result, err := client.SetCDN(siteID, "none")
	if err != nil {
		return api.MapError(err)
	}
	if result == nil || result.SyncAttemptID == 0 {
		return errNoSyncAttempt()
	}
	fmt.Fprintln(out, i18n.T("cdn.switch_degrade_queued"))
	// targetMode is the subcommand to run, so it stays the wire token in every
	// language.
	fmt.Fprintln(out, i18n.Tf("cdn.switch_degrade_next", targetMode))
	return nil
}

// cloudflareApexBlockers returns the content-serving apex domains that make a
// switch to Cloudflare impossible: Cloudflare serves subdomains only.
func cloudflareApexBlockers(status *api.CDNStatusResponse) []string {
	var apexes []string
	for _, d := range status.Domains {
		if !isContentRow(d) {
			continue
		}
		if isApexDomain(d.Domain) {
			apexes = append(apexes, d.Domain)
		}
	}
	return apexes
}

// refuseCloudflareApexSwitch refuses the switch before any mode is written and
// returns an already-reported error, so the caller exits non-zero without
// echoing the refusal a second time.
func refuseCloudflareApexSwitch(out io.Writer, apexes []string) error {
	names := strings.Join(apexes, ", ")
	fmt.Fprintln(out, i18n.Tf("cdn.apex_refuse_headline", names))
	fmt.Fprintln(out, i18n.T("cdn.apex_refuse_body"))
	return fmt.Errorf("%s: %w", i18n.Tf("cdn.err_apex_cloudflare", names), ErrCdnReported)
}

// writeSwitchWebAccelCleanupLine surfaces the WebAccel resources a converged
// switch just orphaned in the customer's own account. Unlike
// writeWebAccelOrphanLine it carries no repoint warning: traffic already moved.
func writeSwitchWebAccelCleanupLine(out io.Writer, prior *api.CDNStatusResponse) {
	var domains []string
	for _, d := range prior.Domains {
		if d.CDNID != "" {
			domains = append(domains, d.Domain)
		}
	}
	fmt.Fprintln(out)
	if len(domains) == 0 {
		fmt.Fprintln(out, i18n.T("cdn.switch_orphan_single"))
	} else {
		fmt.Fprintln(out, i18n.Tf("cdn.switch_orphan_named", strings.Join(domains, ", ")))
	}
	fmt.Fprintln(out, i18n.T("cdn.switch_orphan_cleanup_hint"))
}

// errNoSyncAttempt reports an accepted change that came back without the
// attempt identifier the waits key off, leaving nothing to watch.
func errNoSyncAttempt() error {
	return errors.New(i18n.T("cdn.err_no_sync_attempt"))
}

// setNone runs `kamakiri cdn none` on its own, as opposed to as the first leg
// of a provider switch. It settles with the direct-serving line when the exit
// leg completes without streaming a climb.
func setNone(client APIClient, siteID string, noWait bool, out io.Writer) error {
	// The prior mode has to be read before the flip erases it, since it decides
	// which reverse-DNS guidance is honest: a WebAccel prior needs the customer
	// to re-point their own record, a Cloudflare prior does not. Best-effort, a
	// failed read only costs the extra guidance.
	var priorStatus *api.CDNStatusResponse
	if st, sErr := client.CDNStatus(siteID); sErr == nil {
		priorStatus = st
	}

	if noWait {
		// The streaming path issues its own flip inside exitToDirect, which this
		// path never enters, so it has to issue one here.
		result, err := client.SetCDN(siteID, "none")
		if err != nil {
			return api.MapError(err)
		}
		if result == nil || result.SyncAttemptID == 0 {
			return errNoSyncAttempt()
		}
		fmt.Fprintln(out, i18n.T("cdn.change_queued_status"))
		return nil
	}

	// One interruptible region spans the deactivation wait and the exit watch, so
	// a Ctrl-C anywhere in it prints detach copy instead of dying silently. The
	// change keeps converging server-side either way.
	ctx, stop := signalCtx()
	defer stop()

	interactive := isTerminalWriter(out)
	streamed, result, err := exitToDirect(ctx, client, siteID, priorStatus, interactive, false, out)
	if err != nil {
		return err
	}
	if streamed {
		return nil
	}

	// No climb streamed, so the command settles here.
	fmt.Fprintln(out, i18n.T("cdn.direct_serving"))
	WriteApexARecordsBlock(out, result.ApexARecordsToUpdate)
	WriteApexARecordsPendingBlock(out, result.ApexARecordsPending)
	return nil
}

// exitToDirect runs the exit leg: the flip to none, the deactivation wait, and
// the streamed climb back to live direct serving. ctx belongs to the caller so
// that a switch can put both of its legs under one interrupt region.
//
// inSwitch suppresses the standalone orphan copy, which tells the user to
// repoint back to origin. Mid-switch they are repointing toward the new
// provider instead, and the switch prints its own cleanup line at the end.
//
// The first return reports whether the climb streamed. When the error is nil,
// false means nothing was streamed (the prior had no fronted content domain,
// its mode could not be read, or the client cannot drive the climb), so the
// caller settles from the returned response instead.
func exitToDirect(ctx context.Context, client APIClient, siteID string, priorStatus *api.CDNStatusResponse, interactive, inSwitch bool, out io.Writer) (bool, *api.CDNSetResponse, error) {
	result, err := client.SetCDN(siteID, "none")
	if err != nil {
		return false, nil, api.MapError(err)
	}
	if result == nil || result.SyncAttemptID == 0 {
		return false, nil, errNoSyncAttempt()
	}

	// On a TTY this line is redrawn in place into the settled glyph below; a
	// non-TTY run keeps both lines, since it carries no ANSI.
	fmt.Fprintln(out, i18n.T("cdn.deactivating"))

	// WaitForSync blocks on its own poll loop and cannot be cancelled, so it runs
	// detached and is raced against ctx. The buffer is what makes abandoning it
	// safe: on an interrupt nobody reads the result, and an unbuffered send would
	// leak the goroutine.
	type syncResult struct{ err error }
	done := make(chan syncResult, 1)
	go func() {
		_, err := client.WaitForSync(siteID, result.SyncAttemptID, ourSideWaitTimeout)
		done <- syncResult{err: err}
	}()

	select {
	case <-ctx.Done():
		// No records have been shown and the content domains are still unknown, so
		// the stop copy stays generic. Once the exit watch is running it prints its
		// own detach copy for the domains it is watching.
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.T("cdn.stopped_deactivating"))
		return false, result, domain.ErrWatchInterrupted
	case r := <-done:
		if r.err != nil {
			// The fix copy below names commands the same floor refuses, so the
			// status line is rewound and the sentinel travels bare, unmarked, for
			// the command layer to print as the whole message.
			if errors.Is(r.err, api.ErrUpgradeRequired) {
				if interactive {
					fmt.Fprint(out, "\033[1A\033[J") // rewind the ⧗ line
				}
				return false, result, r.err
			}
			if errors.Is(r.err, api.ErrSyncTimeout) {
				fmt.Fprintln(out, i18n.T("cdn.change_queued_status"))
				return false, result, r.err
			}
			// The copy below is the report, so the error is marked as already
			// reported: the caller exits non-zero without echoing the raw sync
			// error to stderr as well.
			fmt.Fprintln(out, i18n.T("cdn.change_failed"))
			fmt.Fprintln(out, i18n.T("cdn.change_failed_fix"))
			return false, result, fmt.Errorf("%w: %w", ErrCdnReported, r.err)
		}
	}

	if interactive {
		fmt.Fprint(out, "\033[1A\033[J") // rewind the ⧗ line
	}
	fmt.Fprintln(out, i18n.T("cdn.deactivated"))

	// The climb back reuses the domain package's cutover view, which needs a
	// richer client than this package's own interface. A client that does not
	// satisfy it returns without streaming and the caller settles.
	if priorStatus != nil && priorStatus.CDNMode == "webaccel" {
		// The customer's record still points at the WebAccel host, so they have to
		// repoint it themselves. The flip never deletes the resource, so the orphan
		// is named before the repoint guidance.
		if !inSwitch {
			writeWebAccelOrphanLine(out, priorStatus)
		}
		if dc, ok := client.(domain.APIClient); ok {
			streamed, err := domain.PointBackToOriginAndWatch(ctx, dc, siteID, interactive, out)
			if err != nil {
				return false, result, err
			}
			return streamed, result, nil
		}
		return false, result, nil
	} else if priorStatus != nil && priorStatus.CDNMode == "cloudflare" {
		// Under Cloudflare the customer points at an indirection we own, so this
		// exit needs no DNS step from them and no records block: we repoint the
		// indirection back at origin ourselves. The orphaned hostname is ours too
		// and is cleaned up automatically.
		if dc, ok := client.(domain.APIClient); ok {
			streamed, err := domain.WatchExitToLive(ctx, dc, siteID, interactive, out)
			if err != nil {
				return false, result, err
			}
			return streamed, result, nil
		}
		return false, result, nil
	}

	return false, result, nil
}

// writeWebAccelOrphanLine names the WebAccel resources the exit just orphaned
// in the customer's Sakura account, and the command that deletes them. The copy
// insists on repointing DNS first because the delete is unconditional: run
// against a domain still pointing at the resource, it takes the domain down.
func writeWebAccelOrphanLine(out io.Writer, prior *api.CDNStatusResponse) {
	var domains []string
	for _, d := range prior.Domains {
		if d.CDNID != "" {
			domains = append(domains, d.Domain)
		}
	}

	if len(domains) == 0 {
		fmt.Fprintln(out, i18n.T("cdn.orphan_single"))
	} else {
		fmt.Fprintln(out, i18n.Tf("cdn.orphan_named", strings.Join(domains, ", ")))
	}
	fmt.Fprintln(out, i18n.T("cdn.orphan_cleanup_hint"))
}

// WriteApexARecordsBlock renders the new apex IPs a mode flip requires, and
// nothing at all for an empty list. Only apexes pinned with A records need this:
// a flip changes which IPs are correct, and an ALIAS or ANAME apex follows the
// change on its own while an A record has to be edited at the registrar.
func WriteApexARecordsBlock(out io.Writer, items []api.ApexARecordsToUpdate) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("cdn.apex_a_header"))
	fmt.Fprintln(out)
	for _, it := range items {
		for _, ip := range it.ExpectedARecords {
			fmt.Fprintf(out, "    %s  A  → %s\n", it.Domain, ip)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("cdn.apex_a_alias_note"))
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("cdn.apex_a_propagation"))
}

// WriteApexARecordsPendingBlock names the apex domains whose new IPs are not
// known yet, and nothing at all for an empty list. It deliberately prints no
// IPs: the ones available at this point are the origin's, and following them
// would pin the apex past the CDN.
func WriteApexARecordsPendingBlock(out io.Writer, items []api.ApexARecordsPending) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("cdn.apex_a_pending_header"))
	fmt.Fprintln(out)
	for _, it := range items {
		fmt.Fprintf(out, "    %s\n", it.Domain)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("cdn.apex_a_pending_hint"))
}
