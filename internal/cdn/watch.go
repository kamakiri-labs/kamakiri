package cdn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/dns"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// ErrCdnWatchInterrupted reports that the user stopped the watch, and is mapped
// to the conventional interrupt exit code, never to 0 or 1.
var ErrCdnWatchInterrupted = errors.New("cdn watch interrupted by signal")

// ErrCdnReported marks a failure whose reason is already on stdout, so the
// caller exits non-zero without echoing it again. One report, not two.
var ErrCdnReported = errors.New("cdn command failed (already reported)")

const (
	// The poll cadence loosens as a watch ages, purely as API-load hygiene for
	// a watch that may stay open indefinitely. The copy never mentions it.
	cdnPollFastUntil   = 1 * time.Minute
	cdnPollMediumUntil = 11 * time.Minute
	cdnPollFast        = 2 * time.Second
	cdnPollMedium      = 10 * time.Second
	cdnPollSlow        = 30 * time.Second

	// A persistent outage would otherwise spin the fast loop forever, printing
	// the same error every couple of seconds.
	cdnStatusFetchBudget = 10
)

// The server's verdict on the customer's delivery record. Only deliveryCorrect
// counts a Cloudflare row live, and only these two failures are definite enough
// to show the user a correction; anything else waits for the next poll.
const (
	deliveryCorrect = "correct"
	deliveryWrong   = "present_but_wrong"
	deliveryAbsent  = "absent"
)

func defaultCdnPollSchedule(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < cdnPollFastUntil:
		return cdnPollFast
	case elapsed < cdnPollMediumUntil:
		return cdnPollMedium
	default:
		return cdnPollSlow
	}
}

// cdnWatchOpts is the watch's environment: clock, cadence, cancellation, and
// whether it may move the cursor.
type cdnWatchOpts struct {
	pollSchedule func(elapsed time.Duration) time.Duration
	now          func() time.Time
	isTTY        bool
	ctx          context.Context

	// certCheck probes one public host, returning nil once a trusted certificate
	// is served and the response is not an edge error. A nil one skips the gate.
	certCheck func(ctx context.Context, host, mode string) error
}

// isTerminalWriter reports whether the renderer may rewind the cursor. A pipe
// or a buffer gets append-only output instead.
func isTerminalWriter(out io.Writer) bool {
	if file, ok := out.(*os.File); ok {
		return term.IsTerminal(int(file.Fd()))
	}
	return false
}

// cdnSnapshot collapses one status frame, the whole content-serving set, into
// the counts the milestone timeline needs.
type cdnSnapshot struct {
	total                int
	registering          int // Cloudflare is still registering the hostname, so no records yet
	awaitingDCV          int // validation records exist and are waiting on the customer
	provisioning         int // the WebAccel resource is still being created
	awaitingVerification int // the ownership TXT has not been observed
	validated            int // the provider validated it; the DNS flip is pending
	// awaitingDelivery counts Cloudflare rows the provider considers ready but
	// whose customer record is not yet pointing at the indirection host. That is
	// not a live site, so the row holds the watch open. WebAccel proves delivery
	// through its own latch and never lands here.
	awaitingDelivery int
	active           int
	degraded         int
	errored          []string // "domain: reason"
	unknown          []string // "domain: state"
	dcv              []api.DNSRecord
	// deliveryGuidance holds the Cloudflare delivery records to show the user,
	// collected on the verdict rather than the position so a wrong record
	// surfaces alongside the validation ask instead of minutes after the flip.
	deliveryGuidance []api.DNSRecord
	ownership        []api.DNSRecord
	// mismatch holds the ownership records for rows whose published TXT carries
	// the wrong value, kept apart from ownership so the watch shows a correction
	// rather than the neutral waiting line.
	mismatch []api.DNSRecord
	// The WebAccel go-live facts, counted across content rows. A milestone
	// renders only once every row carries its fact, so a multi-domain site tracks
	// its slowest domain. Each comes from the current poll alone, so an
	// interrupted run resumes on the same milestone.
	waResourceReady int
	waOwnershipDNS  int
	waEnabled       int
	waRouted        int
	// delivery holds the phase-2 guidance: which domain the customer points at
	// which WebAccel host.
	delivery []deliveryTarget
	// cliTerminal holds "domain: reason" for rows this run stops on rather than
	// waits out. The server keeps re-driving them, so they are not terminal
	// there, but the watch must name them and exit rather than loop forever.
	cliTerminal []string
}

// deliveryTarget is the phase-2 guidance for one content domain: which WebAccel
// host the customer points it at, and whether that has to be an apex record.
type deliveryTarget struct {
	domain string
	// subdomain is what WebAccel returns: the full delivery host, not a bare
	// label, and the customer's record target verbatim.
	subdomain string
	apex      bool
}

// deliveryHost returns the target the customer's record must carry. The
// trailing dot is required, not cosmetic: without it the value is relative and
// many registrars append the zone, naming a host that does not exist.
func (d deliveryTarget) deliveryHost() string {
	return strings.TrimSuffix(d.subdomain, ".") + "."
}

// isApexDomain reports whether a domain is the registrant's bare domain. It is
// a label-count heuristic; the authoritative rejection is server-side.
func isApexDomain(domain string) bool {
	return strings.Count(domain, ".") <= 1
}

// webaccelTerminalReasons names the conditions this run stops on rather than
// waits out: the TXT mismatch and the panel misconfiguration need the user to
// act, and the enable keeps failing after the cutover. A resource that has gone
// missing is deliberately absent: the reconciler recreates it, so the watch
// should keep waiting.
var webaccelTerminalReasons = map[string]struct{}{
	"webaccel_txt_mismatch":              {},
	"webaccel_enable_failed_post_verify": {},
	"webaccel_panel_misconfigured":       {},
}

func isWebAccelTerminalReason(reason string) bool {
	_, ok := webaccelTerminalReasons[reason]
	return ok
}

// pending counts the rows still in flight; the watch runs until it reaches
// zero. The rows this run stops on are deliberately absent: each ends the watch
// with its own report rather than holding it open.
func (s cdnSnapshot) pending() int {
	return s.registering + s.awaitingDCV + s.provisioning + s.awaitingVerification + s.validated + s.awaitingDelivery
}

func (s cdnSnapshot) allResourceReady() bool { return s.total > 0 && s.waResourceReady == s.total }
func (s cdnSnapshot) allOwnershipDNS() bool  { return s.total > 0 && s.waOwnershipDNS == s.total }
func (s cdnSnapshot) allEnabled() bool       { return s.total > 0 && s.waEnabled == s.total }
func (s cdnSnapshot) allRouted() bool        { return s.total > 0 && s.waRouted == s.total }

// isContentRow reports whether a row carries a CDN lifecycle position at all. A
// row carrying none, or sitting at off, is not fronted by the CDN, and every
// part of the package that reasons about content-serving domains skips it.
func isContentRow(d api.CDNDomainStatus) bool {
	return d.CdnState != "" && d.CdnState != CdnStateOff
}

// deriveCdnPhase folds one status response into the snapshot the timeline
// reasons about.
func deriveCdnPhase(status *api.CDNStatusResponse, mode string) cdnSnapshot {
	var s cdnSnapshot
	for _, d := range status.Domains {
		if !isContentRow(d) {
			continue
		}
		s.total++

		if mode == "webaccel" {
			if d.WebaccelSubdomain != "" {
				s.waResourceReady++
			}
			if d.WebaccelOwnershipStatus == "verified" {
				s.waOwnershipDNS++
			}
			if d.WebaccelEnabledObservedAt != "" {
				s.waEnabled++
			}
			if d.WebaccelRoutedObservedAt != "" {
				s.waRouted++
			}
			// Guidable only once the resource exists, which gives the block a
			// host to name, and only until the customer's record is observed.
			if d.WebaccelSubdomain != "" && d.WebaccelRoutedObservedAt == "" {
				s.delivery = append(s.delivery, deliveryTarget{
					domain:    d.Domain,
					subdomain: d.WebaccelSubdomain,
					apex:      isApexDomain(d.Domain),
				})
			}
		}

		// Keyed on the verdict rather than the position, so a wrong record is
		// raised alongside the validation ask instead of after the flip. A row
		// that failed outright is left alone: it exits with its own breakdown.
		if mode == "cloudflare" &&
			d.CdnHealth.Verdict != CdnVerdictBroken && d.CdnHealth.Verdict != CdnVerdictTimedOut &&
			(d.DnsVerdict == deliveryWrong || d.DnsVerdict == deliveryAbsent) {
			s.deliveryGuidance = append(s.deliveryGuidance, primaryRecords(d.DNSRecordsExpected)...)
		}

		// These sit at positions that otherwise read as pending, so they must be
		// peeled off before the arms below or the watch would loop on them.
		if mode == "webaccel" && isWebAccelTerminalReason(d.CdnHealth.Reason) {
			s.cliTerminal = append(s.cliTerminal,
				fmt.Sprintf("%s: %s", d.Domain, d.CdnHealth.Reason))
			// The expected record is still collected, so the failure frame can
			// show the user the value to correct.
			if d.CdnHealth.Reason == "webaccel_txt_mismatch" {
				s.mismatch = append(s.mismatch, ownershipRecords(d.DNSRecordsExpected)...)
			}
			continue
		}

		// A problem verdict decides the row wherever it sits in the lifecycle.
		switch d.CdnHealth.Verdict {
		case CdnVerdictDegraded:
			// Mid go-live a lost delivery record means not live yet, a wait the
			// user can resume. A degraded row without that reason really is serving.
			if mode == "cloudflare" && d.CdnHealth.Reason == CdnReasonCloudflareDeliveryLost {
				s.awaitingDelivery++
				continue
			}
			s.degraded++
			continue
		case CdnVerdictBroken, CdnVerdictTimedOut:
			reason := d.CdnHealth.Reason
			if reason == "" {
				reason = i18n.T("cdn.reason_unspecified")
			}
			s.errored = append(s.errored, fmt.Sprintf("%s: %s", d.Domain, reason))
			continue
		}

		switch d.CdnState {
		case CdnStateActive:
			// For Cloudflare this position covers only the provider's own side.
			// Traffic arrives only once the customer's record is observed at the
			// indirection host, so anything else waits rather than claiming a live
			// site. WebAccel proves delivery through its own latch.
			if mode == "cloudflare" && d.DnsVerdict != deliveryCorrect {
				s.awaitingDelivery++
				continue
			}
			s.active++
		case CdnStateAwaitingProvisioning:
			s.provisioning++
		case CdnStateAwaitingVerification:
			// A mismatched record was peeled off above, so this is the ordinary
			// wait for one not yet published or not yet propagated. The list can be
			// empty for a poll or two, which is what the guidance below allows for.
			s.awaitingVerification++
			s.ownership = append(s.ownership, ownershipRecords(d.DNSRecordsExpected)...)
		case CdnStateReadyToFlip:
			s.validated++
		case CdnStateAwaitingCFValidation:
			if hasValidationRecords(d.DNSRecordsExpected) {
				s.awaitingDCV++
				s.dcv = append(s.dcv, validationRecords(d.DNSRecordsExpected)...)
			} else {
				// The records exist only after registration succeeds, so the first
				// polls can land here with none. Never instruct one we cannot name.
				s.registering++
			}
		default:
			if _, known := CdnKnownStates[d.CdnState]; !known {
				s.unknown = append(s.unknown, fmt.Sprintf("%s: %s", d.Domain, d.CdnState))
				continue
			}
			// Unreachable today. It exists so a position added to the known set
			// without an arm here keeps waiting rather than terminating wrongly.
			s.registering++
		}
	}
	return s
}

func hasValidationRecords(rs []api.DNSRecord) bool {
	for _, r := range rs {
		if r.Purpose == "validation" {
			return true
		}
	}
	return false
}

func validationRecords(rs []api.DNSRecord) []api.DNSRecord {
	out := make([]api.DNSRecord, 0, len(rs))
	for _, r := range rs {
		if r.Purpose == "validation" {
			out = append(out, r)
		}
	}
	return out
}

func ownershipRecords(rs []api.DNSRecord) []api.DNSRecord {
	out := make([]api.DNSRecord, 0, len(rs))
	for _, r := range rs {
		if r.Purpose == "ownership" {
			out = append(out, r)
		}
	}
	return out
}

func primaryRecords(rs []api.DNSRecord) []api.DNSRecord {
	out := make([]api.DNSRecord, 0, len(rs))
	for _, r := range rs {
		if r.Purpose == "primary" {
			out = append(out, r)
		}
	}
	return out
}

// WatchToLive runs the CDN go-live watch under a context of its own. A provider
// switch uses WatchToLiveCtx instead, so one interrupt covers both its legs.
func WatchToLive(client APIClient, siteID, mode string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return WatchToLiveCtx(ctx, client, siteID, mode, out)
}

// WatchToLiveCtx runs the go-live watch under a caller-supplied context, so a
// switch can put both of its legs under one interrupt region.
func WatchToLiveCtx(ctx context.Context, client APIClient, siteID, mode string, out io.Writer) error {
	certCheck := httpsHeadCertCheck
	if os.Getenv(EnvSkipCertCheck) != "" {
		certCheck = nil
	}

	return watchToLive(client, siteID, mode, out, cdnWatchOpts{
		pollSchedule: defaultCdnPollSchedule,
		now:          time.Now,
		isTTY:        isTerminalWriter(out),
		ctx:          ctx,
		certCheck:    certCheck,
	})
}

// EnvSkipCertCheck disables the HTTPS gate, for harnesses that drive a site
// live with no real edge behind it, where the probe could never pass and the
// gate would block forever. Production must never set it.
const EnvSkipCertCheck = "KAMAKIRI_CDN_SKIP_CERT_CHECK"

// watchToLive polls until every content-serving row settles, a row this run
// stops on appears, the user interrupts, a version refusal aborts it, or the
// status endpoint fails past its budget. There is no give-up timeout by design:
// an interrupt is always available and a re-run resumes from wherever the site
// actually is. A degraded site returns nil, since it serves.
func watchToLive(client APIClient, siteID, mode string, out io.Writer, opts cdnWatchOpts) error {
	start := opts.now()
	provider := ProviderLabel(mode)

	// Each of the flags below guards a line printed once, so a finished run stays
	// readable in scrollback. sawAwaiting is the exception: it records that a
	// phase was really observed, which is what makes timing it honest.
	var (
		transientLines int
		dcvShown       bool
		deliveryShown  bool
		sawAwaiting    bool
		validatedDone  bool

		waDone          waMilestonesDone
		waOwnershipDone bool
		waDeliveryDone  bool
	)

	clear := func() {
		if transientLines > 0 && opts.isTTY {
			fmt.Fprintf(out, "\033[%dA\033[J", transientLines)
		}
		transientLines = 0
	}
	commit := func(lines ...string) {
		clear()
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
	}
	showTransient := func(lines ...string) {
		clear()
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
		transientLines = len(lines)
	}

	consecutiveFailures := 0
	for {
		status, err := client.CDNStatus(siteID)
		now := opts.now()
		interval := opts.pollSchedule(now.Sub(start))

		if err != nil {
			// A refused version is refused on every poll, and the give-up copy
			// points at `kamakiri status`, which the same floor refuses. The frame
			// is cleared so the upgrade message lands on a clean terminal.
			if errors.Is(err, api.ErrUpgradeRequired) {
				clear()
				return err
			}
			consecutiveFailures++
			clear()
			fmt.Fprintln(out, i18n.Tf("cdn.status_fetch_failed", api.MapError(err)))
			if consecutiveFailures >= cdnStatusFetchBudget {
				return fmt.Errorf("%s: %w",
					i18n.Tf("cdn.err_status_fetch_give_up", consecutiveFailures),
					api.MapError(err))
			}
		} else {
			consecutiveFailures = 0
			s := deriveCdnPhase(status, mode)

			// Before the wait branch: a stuck domain alongside a sibling that is
			// legitimately waiting leaves pending() above zero, and the loop would
			// wait on the sibling forever while the failure never surfaced.
			if len(s.cliTerminal) > 0 {
				return cdnOutcome(out, commit, showTransient, clear, opts, status, mode, provider,
					s, sawAwaiting, validatedDone, waDone, now.Sub(start))
			}

			if s.pending() > 0 {
				if s.registering > 0 || s.awaitingDCV > 0 || s.provisioning > 0 || s.awaitingVerification > 0 {
					sawAwaiting = true
				}
				switch mode {
				case "cloudflare":
					// Ordering is load-bearing: a commit clears the transient region,
					// so all of them must precede the single tick below.
					if s.awaitingDCV > 0 && !dcvShown {
						commit(dcvBlock(s.dcv)...)
						dcvShown = true
					}
					if len(s.deliveryGuidance) > 0 && !deliveryShown {
						commit(deliveryGuidanceBlock(s.deliveryGuidance)...)
						deliveryShown = true
					}
					if s.registering == 0 && s.awaitingDCV == 0 && !validatedDone {
						commit(providerValidatedLine(provider, sawAwaiting, now.Sub(start)))
						validatedDone = true
					}
					switch {
					case s.awaitingDCV > 0:
						showTransient(cdnWaitingTick(
							i18n.T("cdn.tick_what_cf_validate"),
							humanizeCdnElapsed(now.Sub(start))))
					case s.registering > 0 && !validatedDone:
						showTransient(i18n.T("cdn.tick_registering_cf"))
					case s.validated > 0:
						showTransient(i18n.T("cdn.tick_pointing_dns_cf"))
					case s.awaitingDelivery > 0:
						showTransient(cdnWaitingTick(
							i18n.T("cdn.tick_what_delivery_resolve"),
							humanizeCdnElapsed(now.Sub(start))))
					}
				case "webaccel":
					// Nothing in this sequence is a flip we perform. Under BYO the
					// customer owns the public DNS, so the routed milestone reports
					// their action, observed, rather than ours.
					if s.allResourceReady() && !waDone.resource {
						commit(i18n.T("cdn.milestone_wa_resource"))
						waDone.resource = true
					}
					if s.allOwnershipDNS() && !waDone.dns {
						commit(i18n.T("cdn.milestone_wa_ownership"))
						waDone.dns = true
					}
					// Enabled off the ownership record alone: no traffic moves until
					// the customer's delivery record does.
					if s.allEnabled() && !waDone.enabled {
						commit(i18n.T("cdn.milestone_wa_enabled"))
						waDone.enabled = true
					}
					if s.allRouted() && !waDone.routed {
						commit(i18n.T("cdn.milestone_wa_routed"))
						waDone.routed = true
					}

					switch {
					case !s.allResourceReady():
						showTransient(i18n.T("cdn.tick_creating_wa"))
					case !s.allOwnershipDNS():
						// Phase 1. The record is shown only once it exists.
						switch {
						case len(s.ownership) > 0:
							if !waOwnershipDone {
								commit(ownershipBlock(s.ownership)...)
								waOwnershipDone = true
							}
							showTransient(cdnWaitingTick(
								i18n.T("cdn.tick_what_wa_ownership"),
								humanizeCdnElapsed(now.Sub(start))))
						default:
							showTransient(i18n.T("cdn.tick_preparing_ownership"))
						}
					case !s.allRouted():
						// Phase 2, the cutover. This waits on a DNS change the
						// customer makes, so framing it like certificate issuance
						// would imply seconds and be wrong.
						if !waDeliveryDone {
							commit(deliveryBlock(s)...)
							waDeliveryDone = true
						}
						showTransient(cdnWaitingTick(
							i18n.T("cdn.tick_what_wa_delivery"),
							humanizeCdnElapsed(now.Sub(start))))
					default:
						// Resource, ownership and routing are all observed, so the
						// wait is on WebAccel's certificate. Issuance was measured
						// at around 43 seconds, which is what "under a minute"
						// claims.
						showTransient(i18n.T("cdn.tick_wa_cert"))
					}
				}
			} else {
				return cdnOutcome(out, commit, showTransient, clear, opts, status, mode, provider,
					s, sawAwaiting, validatedDone, waDone, now.Sub(start))
			}
		}

		select {
		case <-opts.ctx.Done():
			clear()
			printCdnDetachCopy(out)
			return ErrCdnWatchInterrupted
		case <-time.After(interval):
		}
	}
}

// waMilestonesDone records which WebAccel milestones the loop already printed,
// so the closing frame emits only the missing ones. That matters when the first
// poll is already live and the waiting branch never ran.
type waMilestonesDone struct {
	resource, dns, enabled, routed bool
}

// cdnOutcome renders the closing frame and decides the result: a breakdown and
// a non-zero exit for a failed or unrecognised row, a warning and a zero exit
// for a degraded one since it still serves, the live line for the rest.
func cdnOutcome(out io.Writer, commit, showTransient func(...string), clear func(),
	opts cdnWatchOpts, status *api.CDNStatusResponse, mode, provider string, s cdnSnapshot,
	sawAwaiting, validatedDone bool, waDone waMilestonesDone, elapsed time.Duration) error {

	if len(s.cliTerminal) > 0 {
		clear()
		printWebAccelTerminalBlock(out, s)
		return fmt.Errorf("%s: %w",
			i18n.Tf("cdn.err_wa_cannot_complete", len(s.cliTerminal)), ErrCdnReported)
	}

	if len(s.errored) > 0 {
		clear()
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.Tf("cdn.failed_header", len(s.errored)))
		for _, line := range s.errored {
			fmt.Fprintln(out, i18n.Tf("cdn.failed_row", line))
		}
		return fmt.Errorf("%s: %w", i18n.Tf("cdn.err_setup_failed", len(s.errored)), ErrCdnReported)
	}
	if len(s.unknown) > 0 {
		clear()
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.Tf("cdn.unknown_header", len(s.unknown)))
		for _, line := range s.unknown {
			fmt.Fprintln(out, i18n.Tf("cdn.unknown_row", line))
		}
		return fmt.Errorf("%s: %w", i18n.Tf("cdn.err_unknown_state_watch", len(s.unknown)), ErrCdnReported)
	}

	if s.total == 0 {
		commit(i18n.Tf("cdn.set_no_content_domains", provider))
		return nil
	}

	switch mode {
	case "cloudflare":
		if !validatedDone {
			commit(providerValidatedLine(provider, sawAwaiting, elapsed))
		}
		commit(i18n.T("cdn.milestone_dns_pointed_cf"))
	case "webaccel":
		// Fill in whichever milestones the loop never printed, so even a run that
		// was live on its first poll leaves the full trail. No duration: these are
		// facts observed at once, not phases waited out.
		if s.allResourceReady() && !waDone.resource {
			commit(i18n.T("cdn.milestone_wa_resource"))
		}
		if s.allOwnershipDNS() && !waDone.dns {
			commit(i18n.T("cdn.milestone_wa_ownership"))
		}
		if s.allEnabled() && !waDone.enabled {
			commit(i18n.T("cdn.milestone_wa_enabled"))
		}
		if s.allRouted() && !waDone.routed {
			commit(i18n.T("cdn.milestone_wa_routed"))
		}
	}

	// A degraded row skips the gate below: the probe would likely fail, and a
	// gate that never gives up would block forever on a condition the user has
	// already been told about.
	if s.degraded > 0 {
		commit(i18n.Tf("cdn.live_degraded", provider, s.degraded, provider))
		return nil
	}

	// Active means the site should serve, not that it does: the certificate may
	// not have reached the visitor's nearest edge yet. Nothing claims live until
	// a real HTTPS fetch from the user's own machine succeeds.
	if mode == "cloudflare" || mode == "webaccel" {
		if err := certGate(out, commit, showTransient, clear, opts, mode, contentHosts(status)); err != nil {
			return err
		}
	}

	commit(cdnLiveLine(status, mode, provider))
	return nil
}

// certGate holds back the live line until every content-serving host answers
// over HTTPS with a trusted certificate and something other than an edge error.
// Both halves matter: a host whose certificate is fine but whose routing is
// dead passes a bare handshake. It retries until they pass or the user
// interrupts, never on a timer, which would report live over a dead site.
func certGate(out io.Writer, commit, showTransient func(...string), clear func(),
	opts cdnWatchOpts, mode string, hosts []string) error {
	if opts.certCheck == nil || len(hosts) == 0 {
		return nil
	}

	start := opts.now()
	remaining := append([]string(nil), hosts...)
	waited := false

	for {
		showTransient(i18n.T("cdn.tick_checking_cert"))

		var still []string
		for _, h := range remaining {
			if err := opts.certCheck(opts.ctx, h, mode); err != nil {
				still = append(still, h)
			}
		}
		remaining = still

		if len(remaining) == 0 {
			commit(milestoneLine(i18n.T("cdn.milestone_cert_checked"), waited, opts.now().Sub(start)))
			return nil
		}
		waited = true

		select {
		case <-opts.ctx.Done():
			clear()
			printCdnDetachCopy(out)
			return ErrCdnWatchInterrupted
		case <-time.After(opts.pollSchedule(opts.now().Sub(start))):
		}
	}
}

// contentHosts returns the hosts the snapshot counts, the ones the CDN serves.
func contentHosts(status *api.CDNStatusResponse) []string {
	var hosts []string
	for _, d := range status.Domains {
		if !isContentRow(d) {
			continue
		}
		hosts = append(hosts, d.Domain)
	}
	return hosts
}

// httpsHeadCertCheck probes one host over HTTPS with ordinary verification,
// deliberately not relaxed, and succeeds on a trusted certificate plus a
// response that is not an edge error.
//
// Under WebAccel it demands more. Mid-cutover, stale DNS can still land on our
// own origin, which serves a valid certificate and a 200 for the same host, so
// the response must also carry a WebAccel edge signature.
func httpsHeadCertCheck(ctx context.Context, host, mode string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		// A 3xx already proves what the probe is after, having come back through
		// the edge over a verified handshake. Following it could chase to another
		// host and prove something else.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		// Expected: the certificate may not have reached this edge yet, and a
		// WebAccel site still disabled fails verification outright.
		return err
	}
	defer resp.Body.Close()
	return evalEdgeResponse(mode, resp.StatusCode, resp.Header)
}

// evalEdgeResponse decides whether an already-verified response means the host
// is live, returning a reason to keep waiting when it does not.
func evalEdgeResponse(mode string, statusCode int, header http.Header) error {
	if isEdgeError(mode, statusCode) {
		return errors.New(i18n.Tf("cdn.err_edge_not_serving", statusCode))
	}
	if mode == "webaccel" && !servedByWebAccel(header) {
		// A trusted certificate and a good status with no WebAccel signature is
		// almost certainly our own origin answering on stale DNS mid-cutover.
		return errors.New(i18n.T("cdn.err_not_served_by_webaccel"))
	}
	return nil
}

// webAccelEdgeHeaders are the response headers that prove a response came back
// through the WebAccel edge. An empty substring matches on the header being
// present at all; otherwise the value must contain it, compared lowercased, so
// entries here are written in lowercase.
//
// The signal is one-way. Our own origin emits no x-webaccel-* header at all,
// which is what makes the header discriminating. WebAccel stamps it only when
// the edge fetches from the origin, though, so a warm cache hit omits it and its
// absence proves nothing. That suits go-live, where the probe is normally the
// first request to a new resource, but this check must not be reused against a
// possibly-warm edge.
//
// It also proves only that WebAccel answered, not that our resource did: a
// customer pointing delivery at another tenant would pass it. The server's own
// certificate observation is what closes that gap.
//
// Do not reach for the Via header instead: WebAccel's Via names its cache
// software and a datacentre, with no token of its own, so a match would
// never fire.
var webAccelEdgeHeaders = []struct{ header, valueSubstr string }{
	// Only unambiguously WebAccel signatures belong here. Generic cache headers
	// name no provider, so anything on the path can stamp one, and matching them
	// would reopen the stale-origin false positive this check exists to kill.
	{"X-Webaccel-Origin-Status", ""},
}

// servedByWebAccel reports whether a response carries a WebAccel edge
// signature. Read webAccelEdgeHeaders before trusting a false result.
func servedByWebAccel(h http.Header) bool {
	for _, sig := range webAccelEdgeHeaders {
		v := h.Get(sig.header)
		if v == "" {
			continue
		}
		if sig.valueSubstr == "" {
			return true
		}
		if strings.Contains(strings.ToLower(v), sig.valueSubstr) {
			return true
		}
	}
	return false
}

func isEdgeError(mode string, code int) bool {
	switch mode {
	case "webaccel":
		return isWebAccelEdgeError(code)
	default:
		return isCloudflareEdgeError(code)
	}
}

// isCloudflareEdgeError reports whether a status code is Cloudflare saying it
// cannot serve the origin. The 52x and 53x family is origin-unreachable; the
// 409 is the zone-association failure an apex produces.
func isCloudflareEdgeError(code int) bool {
	return code == http.StatusConflict || (code >= 520 && code <= 530)
}

// isWebAccelEdgeError reports whether a status code means a WebAccel site is
// not serving yet. A disabled site usually fails the handshake outright, so
// this set is the backstop for one that answers with the disabled-site page.
// The width trades off declaring a dead site live against hanging on a live one.
func isWebAccelEdgeError(code int) bool {
	return code == http.StatusForbidden ||
		code == http.StatusNotFound ||
		code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// cdnLiveLine renders the closing line for a live site. With one content domain
// it names the URL, which is what the user wants confirmed. With several it
// does not: nothing in the payload says which is canonical.
func cdnLiveLine(status *api.CDNStatusResponse, mode, provider string) string {
	if mode == "cloudflare" || mode == "webaccel" {
		if d := soleContentDomain(status); d != "" {
			return i18n.Tf("cdn.live_url", provider, d)
		}
	}
	return i18n.Tf("cdn.live", provider)
}

func soleContentDomain(status *api.CDNStatusResponse) string {
	name := ""
	for _, d := range status.Domains {
		if !isContentRow(d) {
			continue
		}
		if name != "" {
			return ""
		}
		name = d.Domain
	}
	return name
}

// dcvBlock renders the guided block asking for Cloudflare's validation records.
// It prints once and stays in scrollback; only the tick under it is redrawn.
func dcvBlock(records []api.DNSRecord) []string {
	// Every element returned is one printed line, so a value carrying a newline
	// has to be split before it joins the slice. Two conventions for multi-line
	// guidance copy are in use in this file: a sequence of separate sentences
	// takes one key per line, as cdn.ownership_add_many and its _tail do, while a
	// note that only reads as one paragraph is a single value split on "\n" where
	// it is appended, as cdn.delivery_apex_note is. This block takes the first, so
	// the aligned record sub-block is the only thing it splits.
	lines := []string{"", i18n.T("cdn.dcv_header")}

	// Every row here comes from the same renderer, so the instruction word ahead
	// of the record type is the same phrase on each of them and only the ASCII
	// record type varies. That keeps every cell's rune count and its display
	// width the same distance apart, which is the one shape tabwriter still
	// aligns correctly once a cell carries wide characters.
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 2, 0, 2, ' ', 0)
	n := dns.WriteValidationRecords(w, records)
	w.Flush()
	if n > 0 {
		lines = append(lines, strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")...)
	}

	// A whole message per count rather than a noun picked by number: Japanese has
	// no plural to choose, and a sentence that does not need the noun must not be
	// made to carry one.
	add := i18n.T("cdn.dcv_add_many")
	if n == 1 {
		add = i18n.T("cdn.dcv_add_one")
	}
	return append(lines, "", add, i18n.T("cdn.safe_to_close"))
}

// deliveryGuidanceBlock renders the guided block asking the customer to point
// their domain at the indirection host. There is no Cloudflare-specific record
// and no apex case: they point at the same host direct serving uses, which we
// swing onto the Cloudflare edge ourselves.
func deliveryGuidanceBlock(records []api.DNSRecord) []string {
	lines := []string{"", i18n.T("cdn.delivery_guidance_header")}

	// Every cell is a DNS name, a record type or a target, so no cell in this
	// table changes width with the language and tabwriter still aligns it.
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 2, 0, 2, ' ', 0)
	n := 0
	for _, r := range records {
		fmt.Fprintf(w, "    %s\t%s\t%s\n", r.Name, strings.ToUpper(r.Type), r.Value)
		n++
	}
	w.Flush()
	if n > 0 {
		lines = append(lines, "")
		lines = append(lines, strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")...)
	}

	add := i18n.T("cdn.delivery_guidance_add_many")
	if n == 1 {
		add = i18n.T("cdn.delivery_guidance_add_one")
	}
	return append(lines,
		"",
		add,
		i18n.T("cdn.delivery_guidance_tail"),
		i18n.T("cdn.safe_to_close"),
	)
}

// ownershipBlock renders the guided block for the WebAccel ownership records
// carried by the rows still awaiting verification. The value is shown unquoted
// on purpose: registrars add their own quoting, and a pasted pair of literal
// quotes is a common way to get the record rejected.
func ownershipBlock(records []api.DNSRecord) []string {
	header := i18n.T("cdn.ownership_header_many")
	add := i18n.T("cdn.ownership_add_many")
	if len(records) == 1 {
		header = i18n.T("cdn.ownership_header_one")
		add = i18n.T("cdn.ownership_add_one")
	}
	lines := []string{"", header, ""}

	// Every cell is a DNS name, a record type or a value, so no cell in this
	// table changes width with the language and tabwriter still aligns it.
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 2, 0, 2, ' ', 0)
	n := 0
	for _, r := range records {
		fmt.Fprintf(w, "    %s\t%s\t%s\n", r.Name, strings.ToUpper(r.Type), r.Value)
		n++
	}
	w.Flush()
	if n > 0 {
		lines = append(lines, strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")...)
	}

	lines = append(lines, "")
	return append(lines,
		// True whether the domain is already serving from our origin or is brand
		// new, so one line covers both entry paths without overclaiming.
		add,
		i18n.T("cdn.ownership_add_tail"),
		i18n.T("cdn.safe_to_close"),
	)
}

// deliveryBlock renders the guided block for phase 2, the cutover. At an apex
// the record has to be an ALIAS or ANAME, which many registrars cannot do, so
// the block says so rather than letting the user find out at their registrar.
func deliveryBlock(s cdnSnapshot) []string {
	header := i18n.T("cdn.delivery_header_many")
	if len(s.delivery) == 1 {
		header = i18n.T("cdn.delivery_header_one")
	}
	lines := []string{"", header, ""}

	// Every cell is a DNS name, a record type or a delivery host, so no cell in
	// this table changes width with the language and tabwriter still aligns it.
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 2, 0, 2, ' ', 0)
	anyApex := false
	for _, t := range s.delivery {
		rtype := "CNAME"
		if t.apex {
			rtype = "ALIAS"
			anyApex = true
		}
		fmt.Fprintf(w, "    %s\t%s\t%s\n", t.domain, rtype, t.deliveryHost())
	}
	w.Flush()
	lines = append(lines, strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")...)

	lines = append(lines, "")
	if anyApex {
		lines = append(lines, strings.Split(i18n.T("cdn.delivery_apex_note"), "\n")...)
	}
	lines = append(lines, strings.Split(i18n.T("cdn.delivery_cutover_note"), "\n")...)
	return append(lines, i18n.T("cdn.safe_to_close"))
}

// printWebAccelTerminalBlock renders the frame for the rows this run stops on.
// Each reason names the condition and the user's next step, a fix at the
// registrar or in the WebAccel panel for two of them and a retreat to origin
// for the enable failure we keep retrying, and each promises a re-run resumes,
// since the server keeps re-driving the go-live.
func printWebAccelTerminalBlock(out io.Writer, s cdnSnapshot) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.Tf("cdn.wa_terminal_header", len(s.cliTerminal)))
	for _, line := range s.cliTerminal {
		fmt.Fprintln(out, i18n.Tf("cdn.failed_row", line))
	}

	reasons := map[string]struct{}{}
	for _, line := range s.cliTerminal {
		if i := strings.LastIndex(line, ": "); i >= 0 {
			reasons[line[i+2:]] = struct{}{}
		}
	}

	if _, ok := reasons["webaccel_txt_mismatch"]; ok {
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.T("cdn.wa_txt_mismatch_headline"))
		if len(s.mismatch) > 0 {
			fmt.Fprintln(out, i18n.T("cdn.wa_txt_mismatch_fix"))
			fmt.Fprintln(out)
			// Every cell is a DNS name, a record type or a value, so no cell in
			// this table changes width with the language.
			var buf bytes.Buffer
			w := tabwriter.NewWriter(&buf, 2, 0, 2, ' ', 0)
			for _, r := range s.mismatch {
				fmt.Fprintf(w, "    %s\t%s\t%s\n", r.Name, strings.ToUpper(r.Type), r.Value)
			}
			w.Flush()
			fmt.Fprint(out, buf.String())
		}
		fmt.Fprintln(out, i18n.T("cdn.wa_txt_mismatch_rerun"))
	}

	if _, ok := reasons["webaccel_panel_misconfigured"]; ok {
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.T("cdn.wa_panel_headline"))
		fmt.Fprintln(out, i18n.T("cdn.wa_panel_fix"))
		fmt.Fprintln(out, i18n.T("cdn.wa_panel_rerun"))
	}

	if _, ok := reasons["webaccel_enable_failed_post_verify"]; ok {
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.T("cdn.wa_enable_failed_headline"))
		fmt.Fprintln(out, i18n.T("cdn.wa_enable_failed_fix"))
		fmt.Fprintln(out, i18n.T("cdn.wa_enable_failed_retry"))
	}
}

// cdnWaitingTick is the line the waits on the customer's own DNS close on. It
// states the observed fact and nothing else: no cause, no blame on the user's
// registrar, no cadence. The growing elapsed time is the only sign of life it
// needs.
func cdnWaitingTick(what, elapsed string) string {
	return i18n.Tf("cdn.tick", what, elapsed)
}

// milestoneLine renders one committed milestone. The duration is attached only
// when the phase was watched for at least one poll: a timing is shown when it
// was measured, never invented.
func milestoneLine(label string, observed bool, d time.Duration) string {
	if observed {
		return i18n.Tf("cdn.milestone_timed", label, humanizeCdnSpan(d))
	}
	return i18n.Tf("cdn.milestone", label)
}

// providerValidatedLine is the milestone for a provider finishing its own
// validation. It is a whole message per shape rather than a provider name glued
// onto a participle, since where the name sits in the sentence is the
// language's call.
func providerValidatedLine(provider string, observed bool, d time.Duration) string {
	if observed {
		return i18n.Tf("cdn.milestone_provider_validated_timed", provider, humanizeCdnSpan(d))
	}
	return i18n.Tf("cdn.milestone_provider_validated", provider)
}

// printCdnDetachCopy prints the block shown when a signal stops the watch.
// Stopping the watch does not stop the change, so the copy points at how to
// resume.
func printCdnDetachCopy(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("cdn.detach_headline"))
	fmt.Fprintln(out, i18n.T("cdn.detach_body"))
}

// humanizeCdnSpan renders how long a watched phase took. Anything quicker than
// half a minute is still rendered as thirty seconds (`30s` in English), since
// the underlying checks run on a poll cycle and a finer number would claim a
// precision we do not have. The unit words come from the message catalog.
func humanizeCdnSpan(d time.Duration) string {
	switch {
	case d < 30*time.Second:
		return i18n.Tf("cdn.span_seconds", 30)
	case d < time.Minute:
		return i18n.Tf("cdn.span_seconds", int(d.Seconds()))
	case d < time.Hour:
		return i18n.Tf("cdn.span_minutes", int(d.Minutes()))
	default:
		return i18n.Tf("cdn.span_hours", int(d.Hours()), int(d.Minutes())%60)
	}
}

// humanizeCdnElapsed renders elapsed watch time for the waiting tick. The first
// minute reads as a whole phrase, "under a minute" in English: these waits are
// gated on the user's registrar, and a small precise number would suggest it is
// nearly over. The phrase itself comes from the message catalog.
func humanizeCdnElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.T("cdn.elapsed_under_a_minute")
	case d < time.Hour:
		return i18n.Tf("cdn.elapsed_minutes", int(d.Minutes()))
	default:
		return i18n.Tf("cdn.elapsed_hours", int(d.Hours()), int(d.Minutes())%60)
	}
}
