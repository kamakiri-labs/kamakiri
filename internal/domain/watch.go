package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/dns"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"

	"golang.org/x/term"
)

// ErrWatchInterrupted reports that the user stopped the watch, and is mapped to
// the conventional interrupt exit code, never to 0 or 1.
var ErrWatchInterrupted = errors.New("watch interrupted by signal")

const (
	// The poll cadence loosens as a watch ages, purely as API-load hygiene for a
	// watch that may stay open indefinitely; the copy never mentions it. The
	// fast band is deliberately tight, since the transitions after DNS verifies
	// are expected to be quick and a sluggish poll would add visible lag to
	// "live" for something that already happened.
	pollFastUntil   = 10 * time.Minute
	pollMediumUntil = 30 * time.Minute
	pollFast        = 5 * time.Second
	pollMedium      = 60 * time.Second
	pollSlow        = 5 * time.Minute

	// Past this the cert spinner gains a soft "still working" line. 60s is
	// calibrated against real ACME timing, where a challenge plus finalize plus
	// an edge reload routinely takes 30 to 60s under good conditions: the line
	// should reassure on a borderline-slow issuance, not alarm on a typical one.
	certSlowThreshold = 60 * time.Second

	// Past this, three times the reassurance threshold above, the spinner
	// escalates from reassurance to "taking longer than expected".
	certVerySlowThreshold = 180 * time.Second

	// The post-go-live cache caveat rides the ✓ live line only when the watch
	// reached live this quickly, since it means nothing to someone who was not
	// watching the change happen.
	liveCacheCaveatWindow = 2 * time.Minute
)

// The site's CDN mode, as the `cdn_mode` field carries it.
const (
	CdnModeNone       = "none"
	CdnModeCloudflare = "cloudflare"
	CdnModeWebAccel   = "webaccel"
)

// The cert-axis error categories. The copy names only the category the server
// observed, never an unmeasured cause such as a certificate authority, a rate
// limit, or the user's registrar.
const (
	CertErrorCertUnavailable = "cert_unavailable"
	CertErrorWrongSubject    = "wrong_subject"
	CertErrorExpired         = "expired"
	CertErrorRenewalOverdue  = "renewal_overdue"
	CertErrorChainInvalid    = "chain_invalid"
)

// The cert-axis verdicts, which the CLI renders rather than re-deriving. Only
// `wrong` is branched on; the rest are named so the whole wire contract sits in
// one place.
const (
	CertVerdictCorrect     = "correct"
	CertVerdictWrong       = "wrong"
	CertVerdictAbsent      = "absent"
	CertVerdictUnreachable = "unreachable"
)

func defaultPollSchedule(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < pollFastUntil:
		return pollFast
	case elapsed < pollMediumUntil:
		return pollMedium
	default:
		return pollSlow
	}
}

// watchOpts is the watch's environment: clock, cadence, cancellation, and
// whether it may move the cursor.
type watchOpts struct {
	pollSchedule func(elapsed time.Duration) time.Duration
	now          func() time.Time
	isTTY        bool
	ctx          context.Context

	// reissue, when non-nil, asks the server for a fresh check so it keeps
	// re-probing while the watch runs. `domain verify` sets it, since the row it
	// watches has no other driver once the server's priority window decays; the
	// go-live watch leaves it nil and stays a pure read. It is best-effort: a
	// rate-limited or failed request is swallowed, the automatic checks continue
	// regardless, and the watch is only a view.
	reissue func()
}

// verifyReissueInterval is the floor on how often the `domain verify` watch asks
// for a fresh check. The server keeps a per-domain cooldown of its own, so
// asking faster is wasted, and the value is set coarse enough to keep a
// long-running watch clear of the endpoint's rate limit.
const verifyReissueInterval = 30 * time.Second

// isTerminalWriter reports whether the renderer may rewind the cursor. A pipe or
// a buffer gets append-only output instead.
func isTerminalWriter(out io.Writer) bool {
	if file, ok := out.(*os.File); ok {
		return term.IsTerminal(int(file.Fd()))
	}
	return false
}

// watchPhase is the derived go-live phase for one status frame.
type watchPhase int

const (
	phaseWaiting watchPhase = iota
	phaseWrong
	phaseDetected
	// phaseCertProvisioning is the wait on certificate issuance. derivePhase
	// reaches it from State alone, so dnsCorrect counts such a row as
	// DNS-correct whatever its verdict says.
	phaseCertProvisioning
	phaseLive
)

// derivePhase collapses one status row into the streamed-view phase.
//
// The cert and live phases key on State, the authoritative cert-axis signal.
// The DNS milestone keys on the verdict instead, because a `degraded` row is
// ambiguous between the two axes. The verdict is also a comparison against the
// target expected right now, so a record that was correct before the target
// changed reads as wrong immediately, and "DNS propagated" can never latch on a
// stale observation.
//
// An apex domain runs through the same arms: nothing here branches on the
// record shape.
func derivePhase(d *api.Domain) watchPhase {
	switch d.State {
	case StateServing:
		return phaseLive
	case StateAwaitingCert:
		return phaseCertProvisioning
	}

	switch d.DnsVerdict {
	case "present_but_wrong":
		return phaseWrong
	case "correct":
		return phaseDetected
	default:
		// Absent, unreachable, or missing entirely on an older server: all wait.
		// A row already past DNS with no verdict lands here too; the State arm
		// above reclaims it once it reaches awaiting_cert or serving, but one
		// resting in awaiting_edge keeps showing the DNS waiting tick.
		return phaseWaiting
	}
}

// WatchToLive runs the streamed go-live watch under a signal context of its
// own. `domain set` and `domain add` do not enter through it: they own one
// cancellable region spanning the pre-watch sync and the watch, so that a
// Ctrl-C before the watch starts still prints the detach copy, and they use
// watchToLiveCtx.
func WatchToLive(client APIClient, siteID, domainName string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return watchToLiveCtx(ctx, client, siteID, domainName, out)
}

// watchToLiveCtx runs the streamed go-live watch under a caller-owned
// cancellable context, so one signal region can span a pre-watch wait and the
// watch.
func watchToLiveCtx(ctx context.Context, client APIClient, siteID, domainName string, out io.Writer) error {
	return watchToLive(client, siteID, domainName, out, watchOpts{
		pollSchedule: defaultPollSchedule,
		now:          time.Now,
		isTTY:        isTerminalWriter(out),
		ctx:          ctx,
	})
}

// watchVerifyCtx is the go-live watch plus a periodic request for a fresh
// check, so the server keeps re-probing for as long as `domain verify` watches:
// the priority window the first request opened decays, and the automatic
// cadence alone would leave a settled row checked rarely.
func watchVerifyCtx(ctx context.Context, client APIClient, siteID, domainName string, out io.Writer) error {
	return watchToLive(client, siteID, domainName, out, watchOpts{
		pollSchedule: defaultPollSchedule,
		now:          time.Now,
		isTTY:        isTerminalWriter(out),
		ctx:          ctx,
		reissue:      func() { _, _ = client.RecheckDomain(domainName) },
	})
}

// watchAllToLiveCtx runs the multi-domain go-live watch under a caller-owned
// cancellable context, committing live only once every watched domain is
// serving. Watching the canonical alone would declare live while an alias is
// still on its stale record.
func watchAllToLiveCtx(ctx context.Context, client APIClient, siteID string, names []string, out io.Writer) error {
	return runWatch(client, siteID, names, out, watchOpts{
		pollSchedule: defaultPollSchedule,
		now:          time.Now,
		isTTY:        isTerminalWriter(out),
		ctx:          ctx,
	})
}

// watchToLive is the single-domain adapter over runWatch, which with a
// one-element name slice reduces to exactly the single-domain timeline.
func watchToLive(client APIClient, siteID, domainName string, out io.Writer, opts watchOpts) error {
	return runWatch(client, siteID, []string{domainName}, out, opts)
}

// runWatch polls status until every watched domain is live, the context is
// cancelled, a version refusal aborts it, or a transport error exhausts the
// budget. There is no give-up timeout: a domain moving off an old host waits
// out that record's TTL, which is routinely an hour and sometimes far longer,
// and an arbitrary cutoff would abandon a watch while nothing is wrong. Ctrl-C
// is always available, and an open watch reaches nothing outside our own API:
// it never probes DNS or the edge itself.
//
// The milestone timeline aggregates across the watched set: DNS commits only
// once every record is correct, the cert spinner runs while any domain is still
// issuing, and live commits only once every domain is serving.
//
// It returns nil when every domain went live, ErrWatchInterrupted on Ctrl-C,
// and the underlying error on a transport or API failure.
func runWatch(client APIClient, siteID string, names []string, out io.Writer, opts watchOpts) error {
	start := opts.now()
	lastReissue := start
	var dnsSeenAt time.Time
	// A milestone carries a `(~dur)` only when its phase was watched for at
	// least one poll. A re-run that finds everything already serving therefore
	// prints all three lines bare: nothing was observed, so no duration would be
	// honest.
	var dnsPendingObserved bool
	var certPendingObserved bool
	var dnsMilestoneDone bool
	var sawNoDeploy bool // waiting on a deploy, not on a certificate
	var transientLines int

	multi := len(names) > 1

	const statusFetchBudget = 10
	consecutiveFailures := 0

	// commit prints lines scrollback keeps, so a finished run stays auditable;
	// showTransient redraws the one status block in place.
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

	commitDNSMilestone := func() {
		if !dnsMilestoneDone {
			commit(milestoneLine(i18n.T("domain.milestone_dns_propagated"), dnsPendingObserved, dnsSeenAt.Sub(start)))
			dnsMilestoneDone = true
		}
	}

	for {
		rows, err := fetchDomains(client, siteID, names)
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
			fmt.Fprintln(out, i18n.Tf("domain.status_fetch_failed", api.MapError(err)))
			if consecutiveFailures >= statusFetchBudget {
				return fmt.Errorf("%s: %w",
					i18n.Tf("domain.err_status_fetch_give_up", consecutiveFailures),
					api.MapError(err))
			}
		} else {
			consecutiveFailures = 0
			phases := make([]watchPhase, len(rows))
			allLive, allDNSOK := true, true
			for i, d := range rows {
				phases[i] = derivePhase(d)
				if phases[i] != phaseLive {
					allLive = false
				}
				if !dnsCorrect(phases[i]) {
					allDNSOK = false
				}
			}
			// The cert wait starts once every watched record is correct, so the
			// cert clock anchors there.
			if allDNSOK && dnsSeenAt.IsZero() {
				dnsSeenAt = now
			}

			switch {
			case allLive:
				commitDNSMilestone()
				// Any row's `cdn_mode` is the site's: the watched set always comes
				// from one site, and the mode is a property of the site.
				commit(terminalCertLine(rows[0], certPendingObserved, now.Sub(dnsSeenAt)))
				for _, d := range rows {
					commit(i18n.Tf("domain.live_url", d.Domain))
				}
				if now.Sub(start) <= liveCacheCaveatWindow {
					fmt.Fprintln(out)
					fmt.Fprintln(out, i18n.T("domain.live_cache_caveat"))
				}
				return nil

			case allDNSOK:
				// Every record is correct, so the site is either issuing its
				// certificate or blocked on a missing deploy. Either way the
				// transient anchors on the first domain that is not live yet.
				commitDNSMilestone()
				rep, _ := firstByPhase(rows, phases, func(p watchPhase) bool { return p != phaseLive })
				if rep != nil && noLiveDeploy(*rep) {
					// With no deploy the cert phase has not started, so it must not
					// be marked observed. Remembering the deploy-less wait lets the
					// cert clock re-anchor once the deploy lands; without that, a
					// wait of minutes or hours on the user would be charged to
					// certificate issuance by the terminal duration and the
					// slow-issuance escalation alike.
					sawNoDeploy = true
					lines := transientStatus(rep, phaseCertProvisioning, humanizeElapsed(now.Sub(start)), false, false)
					if multi {
						lines = labelTransient(lines, rep.Domain)
					}
					showTransient(lines...)
				} else {
					// At least one domain is still issuing. The spinner anchors on
					// it because it carries the `cert_error` the escalating copy
					// names, and it is named when watching more than one so the
					// user sees which certificate is pending.
					if sawNoDeploy {
						// The deploy just landed, so the cert clock starts here and
						// the reported duration measures the observed cert phase
						// rather than the wait that preceded it.
						dnsSeenAt = now
						sawNoDeploy = false
					}
					certPendingObserved = true
					slowCert := !dnsSeenAt.IsZero() && now.Sub(dnsSeenAt) >= certSlowThreshold
					verySlowCert := !dnsSeenAt.IsZero() && now.Sub(dnsSeenAt) >= certVerySlowThreshold
					lines := transientStatus(rep, phaseCertProvisioning, "", slowCert, verySlowCert)
					if multi {
						lines = labelTransient(lines, rep.Domain)
					}
					showTransient(lines...)
				}

			default:
				// At least one domain is still waiting or pointed at the wrong
				// target. A wrong-target row wins the frame, being the actionable
				// one, and is named when watching more than one domain so the user
				// knows which record needs attention.
				dnsPendingObserved = true
				rep, repPhase := firstByPhase(rows, phases, func(p watchPhase) bool { return p == phaseWrong })
				if rep == nil {
					rep, repPhase = firstByPhase(rows, phases, func(p watchPhase) bool { return !dnsCorrect(p) })
				}
				lines := transientStatus(rep, repPhase, humanizeElapsed(now.Sub(start)), false, false)
				if multi {
					lines = labelTransient(lines, rep.Domain)
				}
				showTransient(lines...)
			}
		}

		// verifyReissueInterval is a floor, not a schedule: it is checked once
		// per poll, so the effective spacing is the larger of that floor and the
		// poll cadence, and in the slow bands the cadence dominates.
		if opts.reissue != nil && now.Sub(lastReissue) >= verifyReissueInterval {
			opts.reissue()
			lastReissue = now
		}

		select {
		case <-opts.ctx.Done():
			clear()
			printDetachCopy(out, names)
			return ErrWatchInterrupted
		case <-time.After(interval):
		}
	}
}

// dnsCorrect reports whether a phase means the domain's record matches the
// target. It is the aggregation predicate for the DNS milestone, which commits
// only once every watched domain satisfies it.
func dnsCorrect(p watchPhase) bool {
	return p == phaseDetected || p == phaseCertProvisioning || p == phaseLive
}

// firstByPhase returns the first watched row whose phase satisfies pred, along
// with that phase so the caller renders it without re-deriving, or (nil,
// phaseWaiting) when none do.
func firstByPhase(rows []*api.Domain, phases []watchPhase, pred func(watchPhase) bool) (*api.Domain, watchPhase) {
	for i, p := range phases {
		if pred(p) {
			return rows[i], p
		}
	}
	return nil, phaseWaiting
}

// labelTransient prefixes a transient block with the domain the watch is
// blocked on, so a site with several content domains says which one it is. The
// block's own lines are already indented continuations.
func labelTransient(lines []string, domainName string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, fmt.Sprintf("  %s:", domainName))
	return append(out, lines...)
}

// milestoneLine renders a committed ✓ milestone. The `(~dur)` is attached only
// when the phase was genuinely observed and ran at least 30s, so a single fast
// poll cannot fabricate a `~30s` for a five-second phase. A shorter observed
// phase emits the bare line, the same as one that was never observed: better
// silent than wrong.
func milestoneLine(label string, observed bool, d time.Duration) string {
	if observed && d >= 30*time.Second {
		return i18n.Tf("domain.milestone_timed", label, humanizeWatchSpan(d))
	}
	return i18n.Tf("domain.milestone", label)
}

// terminalCertLine is the terminal milestone. Under a CDN it says the site is
// routed via that vendor rather than claiming a certificate we did not issue,
// and it carries no duration: the wall-clock we watched is not ours to report
// as the vendor's provisioning time. An empty mode, which is what an older
// server sends, falls back to the direct line.
func terminalCertLine(d *api.Domain, observed bool, dur time.Duration) string {
	switch d.CdnMode {
	case CdnModeCloudflare:
		return i18n.T("domain.terminal_via_cloudflare")
	case CdnModeWebAccel:
		return i18n.T("domain.terminal_via_webaccel")
	default:
		return milestoneLine(i18n.T("domain.milestone_cert_provisioned"), observed, dur)
	}
}

// certCategoryCopy maps an observed `cert_error` category into a neutral
// transient line, naming only what was observed. Categories a first go-live
// cannot reach are covered too, so every category the server can send has a
// line of its own rather than the fallback.
func certCategoryCopy(category string) string {
	switch category {
	case CertErrorCertUnavailable:
		return i18n.T("domain.cert_copy_unavailable")
	case CertErrorWrongSubject:
		return i18n.T("domain.cert_copy_wrong_subject")
	case CertErrorChainInvalid:
		return i18n.T("domain.cert_copy_chain_invalid")
	case CertErrorExpired:
		return i18n.T("domain.cert_copy_expired")
	case CertErrorRenewalOverdue:
		// The status view is where this category actually reaches a user, on a
		// row that is serving with a valid certificate. The arm is here so the
		// closed set stays covered in one place.
		return i18n.T("domain.cert_copy_renewal_overdue")
	default:
		// An unknown or empty category falls back to the reassurance line, so an
		// older server or a category newer than this CLI never blanks it.
		return i18n.T("domain.cert_copy_still_working")
	}
}

// waitingTick is the "still monitoring" line the wrong-target and plain-waiting
// frames both close on. The watch never gives up on a timer, so the growing
// elapsed is the only signal that it is still alive; the line states no poll
// cadence, promises nothing, and tells the user to run nothing.
func waitingTick(elapsed string) string {
	return i18n.Tf("domain.tick_waiting_dns", elapsed)
}

// wrongTargetLabelGap is the blank the expected/got column keeps between the
// wider of its two labels and the value beside it.
const wrongTargetLabelGap = 2

// wrongTargetIndent nests the expected/got pair under the line that names the
// problem. It is deeper than the two spaces the block's other detail lines take,
// so the pair reads as the evidence for that line rather than as two further
// findings of its own.
const wrongTargetIndent = "      "

// transientStatus is the in-place status block for a non-terminal phase,
// redrawn each poll and never kept in scrollback. slowCert and verySlowCert
// say which certificate threshold the wait has passed.
func transientStatus(d *api.Domain, phase watchPhase, elapsed string, slowCert, verySlowCert bool) []string {
	switch phase {
	case phaseWrong:
		expected, got := dns.WrongTargetDetail(d)
		gotLine := joinObservedValues(got)
		if diag := dns.DiagnoseWrongTarget(d.Domain, expected, got); diag != "" {
			gotLine = i18n.Tf("domain.transient_got_diag", gotLine, diag)
		}
		// The two labels sit above one another, so their values start at one
		// column: as wide as the wider label by display width, since a rune count
		// reads a two-column glyph as one and over-pads whichever label carries
		// them. Sizing it from the labels rather than from a constant means a
		// longer label widens the column instead of colliding with its value.
		expectedLabel := i18n.T("domain.label_expected")
		gotLabel := i18n.T("domain.label_got")
		column := max(i18n.Width(expectedLabel), i18n.Width(gotLabel)) + wrongTargetLabelGap
		// The frame names the problem and then closes on the same waiting tick as
		// a plain wait. It must not tell the user to run anything: the watch is
		// still going and picks up the corrected record on its own, and the
		// resume contract already covers detaching.
		return []string{
			i18n.T("domain.transient_wrong_target"),
			wrongTargetIndent + i18n.Pad(expectedLabel, column) + expected,
			wrongTargetIndent + i18n.Pad(gotLabel, column) + gotLine,
			waitingTick(elapsed),
		}

	case phaseDetected, phaseCertProvisioning:
		// A domain whose site has no deploy can never issue a certificate, so the
		// cert spinner would spin forever. It gets the deploy-first line instead,
		// with a liveness tick of its own, since this wait blocks indefinitely
		// and a frozen line would look hung. Naming the deploy is the one place a
		// transient frame tells the user to run something, and it earns the
		// exception: unlike waiting on DNS, they have an action outstanding.
		if d != nil && noLiveDeploy(*d) {
			lines := []string{"  " + deployFirstLine()}
			if elapsed != "" {
				lines = append(lines, i18n.Tf("domain.transient_still_monitoring", elapsed))
			}
			return lines
		}
		// The escalating cert tiers. Each states only what was observed and what
		// happens next, and attributes fault to nobody.
		if verySlowCert && d != nil && d.CertError != "" {
			return []string{certCategoryCopy(d.CertError)}
		}
		if verySlowCert {
			return []string{i18n.T("domain.cert_taking_longer")}
		}
		if slowCert {
			return []string{i18n.T("domain.cert_copy_still_working")}
		}
		return []string{i18n.T("domain.cert_provisioning_transient")}

	default: // phaseWaiting
		// The records block is printed once up front, so no frame re-renders it,
		// apex domains included.
		return []string{waitingTick(elapsed)}
	}
}

// fetchDomains re-reads status, a pure read that triggers no probe, and returns
// the rows for `names` in the requested order. A name missing from the list is
// an error for the whole poll, which the caller's failure budget absorbs as the
// brief blip it usually is, rather than a partial set the watch could declare
// live.
func fetchDomains(client APIClient, siteID string, names []string) ([]*api.Domain, error) {
	list, err := client.ListDomains(siteID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*api.Domain, len(list.Domains))
	for i := range list.Domains {
		d := list.Domains[i]
		byName[d.Domain] = &d
	}
	rows := make([]*api.Domain, 0, len(names))
	for _, n := range names {
		d, ok := byName[n]
		if !ok {
			return nil, errors.New(i18n.Tf("domain.err_not_in_status", n))
		}
		rows = append(rows, d)
	}
	return rows, nil
}

// humanizeWatchSpan renders a duration as `Ns`, `Nm`, or `NhMm`, truncating to
// whole units rather than rounding. The unit words come from the message
// catalog, so those examples are one language's rendering, not the format. The
// no-fabricated-duration floor lives in its one caller, milestoneLine, not here.
func humanizeWatchSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.Tf("domain.span_seconds", int(d.Seconds()))
	case d < time.Hour:
		return i18n.Tf("domain.span_minutes", int(d.Minutes()))
	default:
		return i18n.Tf("domain.span_hours", int(d.Hours()), int(d.Minutes())%60)
	}
}

// humanizeElapsed renders watch-elapsed time for the liveness tick. A
// sub-minute span reads as a whole phrase, "under a minute" in English, so the
// tick never invents a precise small number at the start of a wait that will be
// measured in minutes. The phrase itself comes from the message catalog.
func humanizeElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.T("domain.elapsed_under_a_minute")
	case d < time.Hour:
		return i18n.Tf("domain.elapsed_minutes", int(d.Minutes()))
	default:
		return i18n.Tf("domain.elapsed_hours", int(d.Hours()), int(d.Minutes())%60)
	}
}

// printDetachCopy prints the interrupt block, which the watch reaches only on a
// user interrupt since it never gives up on its own. Several commands block on
// this watch, so the copy names none of them.
//
// One domain is named in the `domain verify` hint, the fast path. Several point
// at `kamakiri status`, which covers the whole set, since naming one of them
// would under-report the rest.
func printDetachCopy(out io.Writer, names []string) {
	fmt.Fprintln(out)
	if len(names) == 1 {
		fmt.Fprintln(out, i18n.T("domain.detach_single_headline"))
		fmt.Fprintln(out, i18n.Tf("domain.detach_single_body", names[0]))
		return
	}
	fmt.Fprintln(out, i18n.T("domain.detach_multi_headline"))
	fmt.Fprintln(out, i18n.T("domain.detach_multi_body"))
}

// joinObservedValues renders observed DNS values as a comma-joined string,
// falling back to a placeholder so the wrong-target frame never prints its
// observed-values label with nothing after it.
func joinObservedValues(vs []string) string {
	if len(vs) == 0 {
		return i18n.T("domain.observed_nothing")
	}
	joined := strings.Join(vs, ", ")
	if joined == "" {
		return i18n.T("domain.observed_nothing")
	}
	return joined
}
