// Package syncwait holds the wait every own-side change shares: the commands
// that queue a change and block until the product has applied it on its own
// side. It polls, it owns the one row of output the wait shows while it runs,
// and it returns when the change lands, when it fails, or when the user
// interrupts.
//
// The wait has no give-up timer. It returns success only once the server has
// said the change is applied. Otherwise it returns an error when the server
// says the change failed, when the server could not be read past a short run
// of transient failures, or when it refuses this CLI version, and
// ErrInterrupted after a Ctrl-C. A command's `--no-wait` is the only way to
// return before any of those.
package syncwait

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/streamui"
)

// ErrInterrupted is returned when the user interrupts the wait. The queued
// change keeps converging server-side, and the stop line on stdout (where the
// options carry one) has already said so. A command must not echo this error
// on top of it; the 130 exit is the whole report.
var ErrInterrupted = errors.New("own-side wait interrupted by signal")

// transientBudget caps how many consecutive transient step failures the wait
// tolerates, the count reset by any other answer. It absorbs brief 5xx during a
// deploy and momentary network blips; past the budget something is genuinely
// wrong and the error propagates.
const transientBudget = 5

// ttyTickAfter and nonTTYTickInterval set when the liveness token appears. A
// terminal is redrawn in place, so a token there costs one row however often it
// is rewritten and can show up early. Without one every token is a line kept,
// so the first waits out a minute and the rest come one a minute.
const (
	ttyTickAfter       = 20 * time.Second
	nonTTYTickInterval = 60 * time.Second
)

// client is the minimal API surface Sync needs. *api.Client satisfies it.
type client interface {
	GetSite(id string) (*api.Site, error)
}

// Options is one wait's rendering and timing. Out, Schedule and Now are
// required; the rest choose the row's mode and its copy.
//
// The row has two modes. With Prefix set the wait runs in open-line mode: the
// prefix goes on screen at once with no newline and the row is left open on
// every return but an interrupt, so the caller appends its own terminator. With
// Prefix empty and TickText set it runs in late-line mode: nothing is printed
// until the token is due. On a terminal the row is erased again before the wait
// returns; off one each token is a finished line of its own. Either way the
// caller's result line lands on a clean row.
type Options struct {
	Out io.Writer

	// Prefix is the open-line row's text. On a terminal it is printed verbatim
	// as the wait starts and restored verbatim before it returns. Off a
	// terminal the start writes it with its trailing whitespace trimmed, since
	// nothing written there can be taken back, and the return writes that
	// whitespace where no token was drawn or a fresh verbatim copy of the whole
	// prefix where one was, the token having ended the line it sat on.
	Prefix string

	// TickText is the late-line row's text, which the elapsed token follows.
	TickText string

	// StopLine is printed after an interrupt. A caller with stop copy of its own
	// to render leaves it empty; SharedStopLine is what most commands pass.
	StopLine string

	// Schedule is the delay before the next poll, given the elapsed wait time.
	Schedule func(elapsed time.Duration) time.Duration

	// Now is the clock, a seam so a test can drive elapsed time exactly.
	Now func() time.Time

	// IsTTY gates the in-place rewrite; streamui.IsTerminalWriter answers it for
	// a real writer.
	IsTTY bool
}

// DefaultSchedule is the production poll cadence every own-side wait runs on.
// The first half-minute is brisk, so the typical change, which lands within a
// second or two, is reported that quickly; the bands after it back off, so a
// wait left open for hours is not a busy loop.
func DefaultSchedule(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 30*time.Second:
		// ±20% of jitter, which lives here rather than around the schedule so a
		// test's fixed cadence stays exact. Without it, many clients polling at
		// once (a CI fan-out, say) would synchronize on the same tick and
		// thunder the server; the spread is too small to move single-user
		// latency.
		const base = 500 * time.Millisecond
		const spread = 200 * time.Millisecond
		return base - spread/2 + time.Duration(rand.Int64N(int64(spread)))
	case elapsed < 2*time.Minute:
		return 2 * time.Second
	case elapsed < 10*time.Minute:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

// SharedStopLine returns the stop copy for a wait whose work carries on
// server-side and shows up under `kamakiri status`. A command whose stop copy has to say
// something else, because the state `kamakiri status` would report on is gone
// by the time it stops, passes its own instead.
func SharedStopLine() string {
	return i18n.T("syncwait.stopped")
}

// Poll runs step on opts' schedule until it resolves, until the transient
// budget runs out, until a version refusal arrives, or until ctx is cancelled.
// It owns the row of output described on Options and closes it on every return.
//
// The step's four answers: (false, nil) keeps polling; (true, nil) is done;
// (true, err) is a terminal failure, whose error is returned unchanged;
// (false, err) is a transient failure, counted against transientBudget and
// reset by any other answer. api.ErrUpgradeRequired is the exception, tolerated
// zero times and returned bare whatever the done flag says, since a version
// floor armed mid-wait refuses every later poll the same way and retrying would
// only push the upgrade copy behind a wall of noise.
//
// Each poll runs in a goroutine, so an interrupt returns without waiting out an
// in-flight request. What a caller may rely on, and what it may not: a context
// already cancelled when the next poll would be launched always wins, and the
// step is not called at all; a context cancelled while a poll is in flight
// almost always wins too, but an answer arriving in the same instant can win
// instead, in which case Poll returns that answer rather than ErrInterrupted,
// and where the answer keeps the wait polling the row may draw one more token
// before the next check interrupts. A test that needs the interrupt to land at
// a known point holds its step open rather than answering from it.
//
// The abandoned poll finishes on its own and its answer is dropped, so a step
// has to be safe to leave running past the return: anything it writes to may
// still be written after Poll has handed back, and it must not write to
// Options.Out at all, since the row there belongs to the caller from the return
// on.
//
// ctx is expected to be a signal region and nothing else, because every
// cancellation is rendered as a user interrupt: the stop line the options
// carry, and ErrInterrupted for the command layer to turn into a 130. A
// deadline handed in here would reach the user as a Ctrl-C they did not press.
func Poll(ctx context.Context, opts Options, step func() (done bool, err error)) error {
	r := newRow(opts)
	r.start()
	start := opts.Now()
	transient := 0

	type reply struct {
		done bool
		err  error
	}

	for {
		// Checked here as well as around the poll, because a select over two
		// ready cases picks at random: without this an already-cancelled
		// context would lose to a step that answers immediately about half the
		// time, and Ctrl-C would look ignored.
		if ctx.Err() != nil {
			return interrupted(r, opts)
		}

		// Buffered, so the send never blocks and an abandoned poll's goroutine
		// still exits once its request comes back.
		answers := make(chan reply, 1)
		go func() {
			done, err := step()
			answers <- reply{done: done, err: err}
		}()

		var a reply
		select {
		case <-ctx.Done():
			return interrupted(r, opts)
		case a = <-answers:
		}

		if errors.Is(a.err, api.ErrUpgradeRequired) {
			r.close()
			return a.err
		}
		switch {
		case a.done:
			r.close()
			return a.err
		case a.err != nil:
			transient++
			if transient > transientBudget {
				r.close()
				return api.MapError(a.err)
			}
		default:
			transient = 0
		}

		elapsed := opts.Now().Sub(start)
		r.tick(elapsed)

		select {
		case <-ctx.Done():
			return interrupted(r, opts)
		case <-time.After(opts.Schedule(elapsed)):
		}
	}
}

// Sync blocks until the site named by siteID has a sync attempt at or above
// sinceAttemptID that reached ok or error. The id comparison is what keeps an
// older ok row from satisfying a wait for newer work.
//
// It hands back the site on those two outcomes only, alongside the error on the
// second, and nil on every other return, an exhausted budget, a version refusal
// and an interrupt included: a caller renders a field off the site it gets, and
// a site read before the wait resolved would have it naming the wrong attempt.
// On an error outcome the reconciler's own reason is a server-origin string
// that travels verbatim, and only the frame around it is ours to word; a row
// carrying no reason points at `kamakiri status` instead. That outcome comes
// back as a *ReconcileFailure, which is how a caller tells the reconciler's
// verdict from a transport failure; the latter is transient, spends the budget,
// and comes back mapped, never as that type.
func Sync(ctx context.Context, c client, siteID string, sinceAttemptID int64, opts Options) (*api.Site, error) {
	var resolved *api.Site

	err := Poll(ctx, opts, func() (bool, error) {
		site, err := c.GetSite(siteID)
		if err != nil {
			return false, err
		}
		if site.Sync == nil || site.Sync.LatestAttemptID < sinceAttemptID {
			return false, nil
		}
		switch site.Sync.Outcome {
		case "ok":
			resolved = site
			return true, nil
		case "error":
			resolved = site
			return true, &reconcileError{reason: site.Sync.Error}
		}
		return false, nil
	})

	if errors.Is(err, ErrInterrupted) {
		// An interrupt leaves a poll running that may still be writing to
		// resolved, so it is not read on this path at all. It resolves nothing
		// in any case, which is the answer the caller needs.
		return nil, err
	}
	var failed *reconcileError
	if errors.As(err, &failed) {
		reason := failed.Error()
		if reason == "" {
			reason = i18n.T("api.see_status")
		}
		err = &ReconcileFailure{msg: i18n.Tf("api.err_reconcile_failed", reason)}
	}
	return resolved, err
}

// ReconcileFailure is the reconciler's verdict on the change a wait watched:
// the pass that applies it ran and failed. Its message is the framed text a
// caller renders, carrying the reconciler's own public string verbatim, or the
// `kamakiri status` pointer where the failing row named no reason. A failure to
// reach the server never arrives as this type, so a caller matching it is
// looking at the verdict and nothing else.
type ReconcileFailure struct {
	msg string
}

func (e *ReconcileFailure) Error() string { return e.msg }

// reconcileError carries the reconciler's own reason out of a poll for Sync to
// word once Poll has handed back. The wording is not done inside the poll: a
// poll abandoned by an interrupt keeps running after the wait returns, and
// reading the message catalog from there would race a caller loading another
// language, which faults the process rather than merely reporting a race. It
// never reaches a caller: Sync replaces it with a ReconcileFailure on the one
// path that produces it.
type reconcileError struct {
	reason string
}

func (e *reconcileError) Error() string { return e.reason }

// interrupted closes the row for a return no caller reports on and prints the
// stop copy the options carry. An empty StopLine writes nothing rather than
// falling back to the shared line, because a caller whose work does not carry
// on the way the shared line says renders its own copy after the return.
func interrupted(r *row, opts Options) error {
	r.closeInterrupted()
	if opts.StopLine != "" {
		fmt.Fprintln(opts.Out, opts.StopLine)
	}
	return ErrInterrupted
}

// row is the single line of output a wait owns while it runs. It is one
// unterminated terminal row rather than a streamui.Renderer block, because on
// the open-line path the row starts with copy the caller supplied and stays
// open for the caller to terminate after the wait returns, which no rewind
// counting whole lines can account for.
//
// The two modes differ in who ends the row. Open line: the prefix is on screen
// from the start and stays, restored verbatim, for the caller to terminate.
// Late line: nothing is on screen until the token is due. On a terminal nothing
// is left when the wait returns; off one the token lines stay, each already
// terminated.
//
// Off a terminal nothing already written can be taken back, so an open-line row
// commits whole lines instead of rewriting one: the first token completes the
// line the prefix opened, every later token is a line of its own repeating the
// prefix, and the close writes a fresh verbatim copy of the prefix for the
// caller's terminator. That is what a consumer rendering line by line needs, a
// log collector or a CI job, which would otherwise see nothing at all until the
// wait ended. Until a token is drawn the prefix's trailing whitespace is held
// back and written at the close instead, so a wait that resolves before the
// first token leaves the bytes it would have with no wait at all. Either way
// the caller's terminator lands where it belongs, on "Creating site... done."
type row struct {
	out      io.Writer
	tty      bool
	prefix   string // open-line mode when non-empty
	trimmed  string // the prefix without its trailing whitespace
	held     string // that whitespace, restored when the row closes off a terminal with no token drawn
	tickText string // late-line mode's text, read when prefix is empty
	opened   bool
	ticked   bool
	lastTick time.Duration
}

// newRow prepares the row for the mode opts chose. The prefix is split once,
// here: the trimmed text is what goes on screen where nothing can be taken back
// and what every committed token line repeats, and the whitespace it ended with
// is what the row writes when it closes with no token in between, so the
// caller's terminator reads on from the prefix. Once a token has ended the
// line, the close writes the verbatim prefix instead and the terminator reads
// on from that fresh copy.
func newRow(opts Options) *row {
	trimmed := strings.TrimRight(opts.Prefix, " \t")
	return &row{
		out:      opts.Out,
		tty:      opts.IsTTY,
		prefix:   opts.Prefix,
		trimmed:  trimmed,
		held:     opts.Prefix[len(trimmed):],
		tickText: opts.TickText,
	}
}

// start puts an open-line row on screen. A late-line row shows nothing yet.
func (r *row) start() {
	if r.prefix == "" {
		return
	}
	if r.tty {
		fmt.Fprint(r.out, r.prefix)
	} else {
		fmt.Fprint(r.out, r.trimmed)
	}
	r.opened = true
}

// tick draws the liveness token where one is due: on a terminal the whole row
// is rewritten on every poll once the first token is due, and without one each
// token is a terminated line once a minute, the first completing the line the
// prefix opened and the rest repeating the prefix ahead of the token.
//
// A terminal row is rewritten from column zero, so a prefix and its token have
// to fit one row: a row that wrapped would leave everything above the last line
// standing. That is why the token is the elapsed time and nothing else, and it
// binds the callers that pass a prefix on a terminal. A prefix long enough to
// wrap on its own, a domain name inside it for instance, reaches this only
// without a terminal, where lines are committed rather than rewritten and a
// wrap costs nothing.
func (r *row) tick(elapsed time.Duration) {
	if !r.due(elapsed) {
		return
	}
	suffix := i18n.Tf("syncwait.tick_suffix", streamui.HumanizeElapsed(elapsed))
	switch {
	case r.prefix != "" && r.tty:
		streamui.RewriteRow(r.out, r.trimmed+suffix)
	case r.prefix != "":
		// r.ticked still reports the token before this one, being set below the
		// switch, so the first token lands on the line the prefix already
		// opened and only the ones after it carry a prefix of their own.
		line := suffix
		if r.ticked {
			line = r.trimmed + suffix
		}
		fmt.Fprintln(r.out, line)
	case r.tty:
		streamui.RewriteRow(r.out, r.tickText+suffix)
	default:
		fmt.Fprintln(r.out, r.tickText+suffix)
	}
	r.opened = true
	r.ticked = true
	r.lastTick = elapsed
}

// due reports whether a token belongs on screen at elapsed. With neither a
// prefix nor a tick text there is no row to draw on, so nothing is ever due and
// no stray token lands in the middle of whatever the caller printed.
func (r *row) due(elapsed time.Duration) bool {
	if r.prefix == "" && r.tickText == "" {
		return false
	}
	if r.tty {
		return elapsed >= ttyTickAfter
	}
	if !r.ticked {
		return elapsed >= nonTTYTickInterval
	}
	return elapsed-r.lastTick >= nonTTYTickInterval
}

// close ends the row on a return the caller reports itself. An open line is
// left carrying the verbatim prefix and open for that report, rewritten in
// place on a terminal and written again off one where a token has since ended
// the line; a late line is taken off screen so the report lands on a clean one.
// A row nothing was ever written to is left alone, so a wait that resolved
// before the first token leaves the same bytes behind as no wait at all.
func (r *row) close() {
	if !r.opened {
		return
	}
	switch {
	case r.prefix == "":
		if r.tty {
			streamui.RewriteRow(r.out, "")
		}
	case r.tty:
		if r.ticked {
			streamui.RewriteRow(r.out, r.prefix)
		}
	case r.ticked:
		// The last token ended its line, so there is no prefix left for the
		// terminator to read on from and a fresh verbatim copy goes out,
		// trailing whitespace and all. A caller that replaces the row with a
		// line of its own rather than reading on from it hands in a prefix
		// ending in no whitespace, so that copy adds nothing to push its line
		// along.
		fmt.Fprint(r.out, r.prefix)
	default:
		fmt.Fprint(r.out, r.held)
	}
}

// closeInterrupted ends the row on an interrupt, where no caller's report
// follows, so the stop copy starts on a line of its own. On a terminal an open
// line is always terminated here, the token having been rewritten in place. Off
// one it is terminated only where no token was drawn, a committed token having
// already ended the line. The held-back whitespace is dropped, having nothing
// left to separate the prefix from.
func (r *row) closeInterrupted() {
	if !r.opened {
		return
	}
	switch {
	case r.prefix == "":
		if r.tty {
			streamui.RewriteRow(r.out, "")
		}
	case r.tty:
		if r.ticked {
			streamui.RewriteRow(r.out, r.prefix)
		}
		fmt.Fprintln(r.out)
	default:
		if !r.ticked {
			fmt.Fprintln(r.out)
		}
	}
}
