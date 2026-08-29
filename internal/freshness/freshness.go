// Package freshness holds the streaming site-level wait behind `kamakiri
// deploy`, `kamakiri rollback` and `kamakiri cdn purge`. Each blocks until the
// server's freshness verdict reads "fresh", meaning every cache a visitor could
// hit serves the current deploy.
//
// The wait has no give-up timer. It returns success only once that verdict
// actually holds, an actionable error when the server reports a flush block the
// user must clear, or on Ctrl-C; the commands' `--no-wait` is the only way to
// return before then. It never re-derives the verdict either: deciding what is
// pending or blocked is the server's job and the CLI renders what it is told.
package freshness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/streamui"
)

// ErrInterrupted is returned when the user interrupts the wait. The work
// continues server-side and the detach copy on stdout already says how to
// resume, so a command must not echo this error on top of it; the 130 exit is
// the whole report.
var ErrInterrupted = errors.New("freshness wait interrupted by signal")

// ErrFlushBlocked is returned when the server reports a terminal flush failure
// the user must act on. The wait has already committed the actionable entry to
// stdout, so a command must not echo this error on top of it.
var ErrFlushBlocked = errors.New("flush blocked")

// errFreshnessMissing is the hard error for a server that returned no verdict.
// An absent state cannot be read as pending, which would hang every wait in the
// field, nor as fresh, which would claim live over an unknown reality.
//
// The user reads this one, so its text comes from the catalog at the moment it
// is printed: a sentinel built with errors.New would freeze the message at
// whatever the environment suggested before the saved language was resolved.
var errFreshnessMissing = i18n.NewError("freshness.err_missing_verdict")

// statusFetchBudget bounds consecutive GetSite transport failures, reset by any
// success. A mid-deploy blip must not kill a wait that has no timeout, and a
// real outage must not leave it spinning forever.
const statusFetchBudget = 10

const (
	freshnessStateFresh   = "fresh"
	freshnessStatePending = "pending"
	freshnessStateBlocked = "blocked"
)

const (
	reasonCredentialsRejected = "credentials_rejected"
	reasonResourceDeleted     = "resource_deleted"
	reasonProviderRejected    = "provider_rejected"
	reasonSiteTornDown        = "site_torn_down"
)

// Escalation and diagnostic thresholds, on elapsed wait time and observed sync
// state. A fault diagnostic replaces the tier line rather than joining it.
const (
	tier1Threshold      = 60 * time.Second
	tier2Threshold      = 5 * time.Minute
	originErrorPolls    = 3               // consecutive sync error polls before the origin diagnostic fires
	originStrandedAfter = 5 * time.Minute // continuous sync "running" before the stranded diagnostic fires
	nonTTYTickInterval  = 60 * time.Second
)

// client is the minimal API surface the wait needs. *api.Client satisfies it.
type client interface {
	GetSite(id string) (*api.Site, error)
}

// msgs is one command's copy, the strings the shared wait stitches into every
// line it renders. It holds only what the wait itself prints: a command's
// `--no-wait` path never enters the wait, so that copy stays with the command.
// Commands take a value from the constructors below rather than building one.
type msgs struct {
	gerund      string
	lead        string
	fresh       string
	detachNoun  string
	reRun       string
	showLiveURL bool
}

// DeployMsgs is the per-command copy for `kamakiri deploy`.
func DeployMsgs() msgs {
	return msgs{
		gerund:      i18n.T("freshness.gerund_deploy"),
		lead:        i18n.T("freshness.lead_deploy"),
		fresh:       i18n.T("freshness.fresh_live"),
		detachNoun:  i18n.T("freshness.detach_noun_deploy"),
		reRun:       "kamakiri deploy",
		showLiveURL: true,
	}
}

// RollbackMsgs is the per-command copy for `kamakiri rollback`.
func RollbackMsgs() msgs {
	return msgs{
		gerund:      i18n.T("freshness.gerund_rollback"),
		lead:        i18n.T("freshness.lead_rollback"),
		fresh:       i18n.T("freshness.fresh_live"),
		detachNoun:  i18n.T("freshness.detach_noun_rollback"),
		reRun:       "kamakiri rollback",
		showLiveURL: true,
	}
}

// PurgeMsgs is the per-command copy for `kamakiri cdn purge`. It claims no URL:
// a manual purge names no address, only that the caches flushed.
func PurgeMsgs() msgs {
	return msgs{
		gerund:      i18n.T("freshness.gerund_purge"),
		lead:        i18n.T("freshness.lead_purge"),
		fresh:       i18n.T("freshness.fresh_caches_flushed"),
		detachNoun:  i18n.T("freshness.detach_noun_purge"),
		reRun:       "kamakiri cdn purge",
		showLiveURL: false,
	}
}

var signalCtx = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

type waitOpts struct {
	pollSchedule func(elapsed time.Duration) time.Duration
	now          func() time.Time
	isTTY        bool
	ctx          context.Context
}

// defaultPollSchedule is the production poll cadence, brisk while a deploy is
// converging and backing off afterwards so a wait left open for hours is not a
// busy loop.
func defaultPollSchedule(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 2*time.Minute:
		return 2 * time.Second
	case elapsed < 10*time.Minute:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

// EnvFreshnessPollMS names the environment variable that overrides the wait's
// poll cadence with a fixed interval in milliseconds. It exists so an automated
// suite can watch a deploy converge without waiting out the production backoff,
// and is not meant for production use. Only Wait consults it.
const EnvFreshnessPollMS = "KAMAKIRI_FRESHNESS_POLL_MS"

// pollScheduleFromEnv returns the cadence Wait uses. With EnvFreshnessPollMS set
// to a positive integer N it polls every N milliseconds regardless of elapsed
// time; an unset, empty, non-positive, or unparseable value falls back to
// defaultPollSchedule.
func pollScheduleFromEnv() func(elapsed time.Duration) time.Duration {
	if ms, err := strconv.Atoi(os.Getenv(EnvFreshnessPollMS)); err == nil && ms > 0 {
		interval := time.Duration(ms) * time.Millisecond
		return func(time.Duration) time.Duration { return interval }
	}
	return defaultPollSchedule
}

// Wait blocks until the site named by siteID reads fresh, the server reports a
// terminal flush block, or the user interrupts. It mints its own signal region
// and renders to out, redrawing in place on a TTY and appending throttled lines
// without one. It never degrades to --no-wait off a TTY: the freshness
// guarantee is the point of the command, in CI as much as anywhere.
func Wait(c client, siteID string, m msgs, out io.Writer) error {
	ctx, stop := signalCtx()
	defer stop()
	return wait(c, siteID, m, out, waitOpts{
		pollSchedule: pollScheduleFromEnv(),
		now:          time.Now,
		isTTY:        streamui.IsTerminalWriter(out),
		ctx:          ctx,
	})
}

// wait polls the freshness state on opts' schedule until the verdict resolves,
// the context is cancelled, a version refusal aborts it, or transport errors
// exhaust statusFetchBudget.
func wait(c client, siteID string, m msgs, out io.Writer, opts waitOpts) error {
	start := opts.now()
	r := streamui.New(out, opts.isTTY)

	consecutiveFailures := 0

	// Gates the (~Ns) span on the fresh milestone: a wait that never saw a
	// pending flush measured nothing and must claim no duration.
	pendingObserved := false

	// Diagnostic accumulators over consecutive pending polls. runningSince is
	// zero whenever the latest poll was not running.
	errorPolls := 0
	var runningSince time.Time

	// Non-TTY emission state, which throttles ticks to one per
	// nonTTYTickInterval and suppresses frames that say nothing new.
	firstFramePrinted := false
	lastSig := ""
	var lastEmit time.Time

	for {
		site, err := c.GetSite(siteID)
		now := opts.now()
		elapsed := now.Sub(start)
		interval := opts.pollSchedule(elapsed)

		if err != nil {
			// A refused version is refused on every poll, so the budget would only
			// stack retry noise in front of the upgrade message. The frame is
			// cleared so that message lands on a clean terminal.
			if errors.Is(err, api.ErrUpgradeRequired) {
				r.Clear()
				return err
			}
			consecutiveFailures++
			r.Commit(i18n.Tf("freshness.status_fetch_failed", api.MapError(err)))
			if consecutiveFailures >= statusFetchBudget {
				return fmt.Errorf("%s: %w",
					i18n.Tf("freshness.err_status_fetch_giving_up", consecutiveFailures),
					api.MapError(err))
			}
		} else {
			consecutiveFailures = 0
			f := site.Freshness
			if f == nil || f.State == "" {
				r.Clear()
				return errFreshnessMissing
			}

			switch f.State {
			case freshnessStateFresh:
				r.Commit(freshMilestone(m, f, pendingObserved, elapsed))
				return nil

			case freshnessStateBlocked:
				// A blocked verdict with nothing to render would commit no line
				// and still exit non-zero, since the command suppresses the echo
				// trusting an entry reached stdout. The failure would appear
				// nowhere, so a verdict this shape is malformed.
				if len(f.Blockers) == 0 {
					r.Clear()
					return errors.New(i18n.T("freshness.err_block_without_detail"))
				}
				var lines []string
				for _, b := range f.Blockers {
					lines = append(lines, FormatBlocker(m.lead, b)...)
				}
				r.Commit(lines...)
				return ErrFlushBlocked

			case freshnessStatePending:
				pendingObserved = true
				errorPolls, runningSince = advanceDiagnostics(site, now, errorPolls, runningSince)
				v := buildPendingView(site, elapsed, now, errorPolls, runningSince)

				if opts.isTTY {
					r.Transient(v.render(m, now)...)
				} else {
					sig := v.sig()
					switch {
					case !firstFramePrinted || sig != lastSig:
						r.Commit(v.render(m, now)...)
						firstFramePrinted = true
						lastSig = sig
						lastEmit = now
					case now.Sub(lastEmit) >= nonTTYTickInterval:
						r.Commit(v.tick(m, elapsed, now)...)
						lastEmit = now
					}
				}

			default:
				r.Clear()
				return errors.New(i18n.Tf("freshness.err_unrecognized_state", f.State))
			}
		}

		select {
		case <-opts.ctx.Done():
			r.Clear()
			printDetach(out, m)
			return ErrInterrupted
		case <-time.After(interval):
		}
	}
}

// advanceDiagnostics folds this poll's sync outcome into the origin-diagnostic
// accumulators. A poll is error, running or neither, so the two accumulators
// are mutually exclusive and their diagnostics can never both fire.
func advanceDiagnostics(site *api.Site, now time.Time, errorPolls int, runningSince time.Time) (int, time.Time) {
	outcome := ""
	if site.Sync != nil {
		outcome = site.Sync.Outcome
	}
	switch outcome {
	case "error":
		return errorPolls + 1, time.Time{}
	case "running":
		if runningSince.IsZero() {
			runningSince = now
		}
		return 0, runningSince
	default:
		return 0, time.Time{}
	}
}

// pendingView is one poll's rendering decision, held apart from the strings so
// the non-TTY path can take its signature without re-running the logic.
type pendingView struct {
	originDiag   string // "", "errored", or "stranded"
	originErr    string
	strandedFrom string
	edgeDiag     bool
	edgeReason   string

	// tier is 0, 1 or 2. It and the target fields under it are read only when
	// no diagnostic applies, and the targets describe the first pending entry.
	tier      int
	axis      string
	host      string
	provider  string
	owedSince string
}

func buildPendingView(site *api.Site, elapsed time.Duration, now time.Time, errorPolls int, runningSince time.Time) pendingView {
	v := pendingView{}

	switch {
	case errorPolls >= originErrorPolls:
		v.originDiag = "errored"
		if site.Sync != nil {
			v.originErr = site.Sync.Error
		}
	case !runningSince.IsZero() && now.Sub(runningSince) >= originStrandedAfter:
		v.originDiag = "stranded"
		if site.Sync != nil {
			v.strandedFrom = site.Sync.AttemptedAt
		}
	}

	if site.EdgePurgeHealth == "failing" || site.EdgePurgeHealth == "broken" {
		v.edgeDiag = true
		v.edgeReason = site.EdgePurgeErrorReason
	}

	switch {
	case elapsed >= tier2Threshold:
		v.tier = 2
	case elapsed >= tier1Threshold:
		v.tier = 1
	default:
		v.tier = 0
	}
	if f := site.Freshness; f != nil && len(f.Pending) > 0 {
		p := f.Pending[0]
		v.axis, v.host, v.provider, v.owedSince = p.Axis, p.Host, p.Provider, p.OwedSince
	}

	return v
}

// hasDiag reports whether a fault diagnostic applies and so replaces the tier
// line.
func (v pendingView) hasDiag() bool {
	return v.originDiag != "" || v.edgeDiag
}

// render returns the frame's lines, with ages resolved against now. When both
// diagnostics apply it shows origin first, then edge.
func (v pendingView) render(m msgs, now time.Time) []string {
	if v.hasDiag() {
		var lines []string
		switch v.originDiag {
		case "errored":
			lines = append(lines, withClause(m,
				i18n.Tf("freshness.diag_origin_failing", v.originErr)))
		case "stranded":
			// Drop the duration when the server's timestamp is missing or
			// unparseable: a fabricated "under a minute" would contradict the
			// five-minute trigger that fired this very line.
			if age, ok := ageSince(v.strandedFrom, now); ok {
				lines = append(lines, withClause(m,
					i18n.Tf("freshness.diag_origin_running_for", streamui.HumanizeElapsed(age))))
			} else {
				lines = append(lines, withClause(m,
					i18n.T("freshness.diag_origin_still_running")))
			}
		}
		if v.edgeDiag {
			lines = append(lines, withClause(m,
				i18n.Tf("freshness.diag_edge_flush_failing", v.edgeReason)))
		}
		return lines
	}

	switch v.tier {
	case 2:
		return []string{v.tier2Line(m, now)}
	case 1:
		return []string{tier1Line(m)}
	default:
		return []string{headline(m)}
	}
}

// tick returns the non-TTY periodic liveness line. An active diagnostic is
// re-emitted instead, so a log keeps saying what is outstanding rather than
// only that time passed.
func (v pendingView) tick(m msgs, elapsed time.Duration, now time.Time) []string {
	if v.hasDiag() {
		return v.render(m, now)
	}
	return []string{i18n.Tf("freshness.tick", m.gerund, streamui.HumanizeElapsed(elapsed))}
}

// sig is the frame's semantic identity. It excludes an age's value but includes
// whether one is present, so the non-TTY path emits on a real transition, the
// owed-age parenthetical appearing among them, and not on an age ticking up.
func (v pendingView) sig() string {
	if v.hasDiag() {
		return fmt.Sprintf("diag|%s|%s|%t|edge:%t:%s",
			v.originDiag, v.originErr, v.strandedFrom != "", v.edgeDiag, v.edgeReason)
	}
	if v.tier == 2 {
		return fmt.Sprintf("tier2|%s|%s|%s|%t", v.axis, v.host, v.provider, v.owedSince != "")
	}
	return fmt.Sprintf("tier%d", v.tier)
}

// tier2Line names the first pending target with how long it has been owed. That
// age is measured from the server's owed_since, never from this wait's elapsed
// time, so a re-run or an already-pending purge reports the real owed time
// rather than the seconds since the user pressed enter.
func (v pendingView) tier2Line(m msgs, now time.Time) string {
	switch v.axis {
	case "cdn":
		return owedLine(m,
			i18n.Tf("freshness.waiting_cdn_flush", providerDisplay(v.provider), v.host),
			v.owedSince, now, true)
	case "edge":
		return owedLine(m, i18n.T("freshness.waiting_edge_flush"), v.owedSince, now, true)
	case "origin":
		return owedLine(m, i18n.T("freshness.waiting_config_to_edge"), v.owedSince, now, false)
	default:
		// Nothing nameable to wait on, so fall back to the tier-1 reassurance
		// rather than dropping to a bare headline this late in a wait.
		return tier1Line(m)
	}
}

// headline is the bare progress line, the gerund and nothing else. Its frame is
// keyed even though the two languages happen to write it the same, because it is
// the frame rather than the message: its siblings that carry a clause or an age
// do differ, and a frame that is a key in one language and a literal in another
// is a frame nobody can fix without touching code.
func headline(m msgs) string {
	return i18n.Tf("freshness.progress", m.gerund)
}

func tier1Line(m msgs) string {
	return withClause(m, i18n.T("freshness.tier1_clause"))
}

// withClause frames a progress line that carries a clause. The frame is catalog
// copy of its own rather than concatenation here, because what separates the
// gerund from the clause differs by language: the English needs a space, while
// the Japanese leader writes none, since the frame has no way to know whether
// the clause it is given opens with a full-width bracket, and a full-width
// bracket takes nothing beside it.
func withClause(m msgs, clause string) string {
	return i18n.Tf("freshness.progress_clause", m.gerund, clause)
}

// owedLine appends the age parenthetical to a clause and drops it when
// owedSince is absent or unparseable. owedPrefix picks the wording that says the
// flush has been outstanding that long; without it the age is bare, which is
// what the origin axis wants, since a config push is not owed to anyone.
func owedLine(m msgs, clause, owedSince string, now time.Time, owedPrefix bool) string {
	line := withClause(m, clause)
	age, ok := ageSince(owedSince, now)
	if !ok {
		return line
	}
	if owedPrefix {
		return line + i18n.Tf("freshness.owed_suffix", streamui.HumanizeElapsed(age))
	}
	return line + i18n.Tf("freshness.age_suffix", streamui.HumanizeElapsed(age))
}

// freshMilestone renders the committed "fresh" line. The URL is whatever the
// server sent, appended only for the commands that claim one and never
// fabricated when it is empty. The (~Ns) span attaches only to a wait that
// observed a pending flush for at least one poll and ran at least 30s, so a
// duration is shown only where one was really measured.
func freshMilestone(m msgs, f *api.Freshness, pendingObserved bool, elapsed time.Duration) string {
	line := m.fresh
	if m.showLiveURL && f.LiveURL != "" {
		line += ": " + f.LiveURL
	}
	if pendingObserved && elapsed >= 30*time.Second {
		line += i18n.Tf("freshness.span_suffix", streamui.HumanizeSpan(elapsed))
	}
	return line
}

// FormatBlocker renders one blocked-flush entry as a ✗ headline plus an
// indented remediation line. lead is the caller's own outcome clause, which a
// command has ("Deployed, but") and `kamakiri status` does not: it deployed
// nothing, so it has nothing to report ahead of the condition and passes "". The
// site_torn_down entry drops the lead either way: there is no achievement to
// report ahead of it.
func FormatBlocker(lead string, b api.Blocker) []string {
	provider := providerDisplay(b.Provider)
	opening := blockerOpening(lead, provider)
	switch b.Reason {
	case reasonCredentialsRejected:
		return []string{
			i18n.Tf("freshness.blocker_credentials_rejected", opening),
			i18n.T("freshness.blocker_credentials_rejected_fix"),
		}
	case reasonResourceDeleted:
		return []string{
			i18n.Tf("freshness.blocker_resource_deleted", opening, b.Host),
			i18n.Tf("freshness.blocker_resource_deleted_fix", b.Provider),
		}
	case reasonProviderRejected:
		if b.Detail != "" {
			return []string{
				i18n.Tf("freshness.blocker_provider_rejected_detail", opening, provider, b.Host, b.Detail),
				i18n.T("freshness.blocker_provider_rejected_detail_fix"),
			}
		}
		return []string{
			i18n.Tf("freshness.blocker_provider_rejected", opening, provider, b.Host),
			i18n.T("freshness.blocker_provider_rejected_fix"),
		}
	case reasonSiteTornDown:
		return []string{
			i18n.T("freshness.blocker_site_torn_down"),
			i18n.T("freshness.blocker_site_torn_down_fix"),
		}
	default:
		// Fail closed: a reason this CLI predates still renders an actionable
		// entry rather than an empty one.
		return []string{
			i18n.Tf("freshness.blocker_provider_rejected", opening, provider, b.Host),
			i18n.T("freshness.blocker_provider_rejected_fix"),
		}
	}
}

// blockerOpening builds the clause before the colon. The two forms are separate
// copy rather than one lead glued onto a shared tail: with a lead the caller has
// already reported a past action, so the cache "was not flushed", while without
// one the line has to state a present condition and say which cache it is. A
// language that words the two differently needs both whole.
func blockerOpening(lead, provider string) string {
	if lead == "" {
		return i18n.Tf("freshness.blocker_opening_condition", provider)
	}
	return i18n.Tf("freshness.blocker_opening_after_lead", lead, provider)
}

// printDetach writes the detach block: the work is committed, keeps converging,
// and can be watched again. Only an interrupt reaches it, since the wait has no
// give-up timeout of its own.
func printDetach(out io.Writer, m msgs) {
	fmt.Fprintln(out, i18n.Tf("freshness.detached", m.detachNoun))
	fmt.Fprintln(out, i18n.Tf("freshness.detach_resume", m.reRun))
}

// providerDisplay maps a wire provider tag to its display name. The "CDN"
// fallback keeps a sentence readable if a blocker ever arrives without one. Two
// vendors' own product names and an initialism the Japanese market uses as-is,
// so none of this is catalog copy.
func providerDisplay(p string) string {
	switch p {
	case "webaccel":
		return "WebAccel"
	case "cloudflare":
		return "Cloudflare"
	default:
		return "CDN"
	}
}

// ageSince returns now minus the RFC3339 timestamp iso, clamped at zero, and
// whether it parsed. Callers render a false result as no age at all rather than
// as a zero one.
func ageSince(iso string, now time.Time) (time.Duration, bool) {
	if strings.TrimSpace(iso) == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return 0, false
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d, true
}
