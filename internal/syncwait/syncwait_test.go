package syncwait

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the whole-row comparisons below hold
// whatever locale the suite runs under: the token and the stop line render from
// the message catalog. Load rather than Setup: nothing here reports which
// language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

// The two row shapes the callers have: a prefix ending in whitespace that the
// caller's terminator reads on from, and a standalone ⧗ line.
const (
	prefix   = "Creating site... "
	tickText = "⧗ renaming the subdomain"
)

const stopLine = "Stopped. The change continues server-side; run `kamakiri status` to confirm."

var base = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

// clock hands out a predetermined sequence of times, the last repeating. Now()
// runs once before the first poll and once after every poll that did not
// resolve, so times[0] is the start and times[k] is where poll k left elapsed.
// It needs no lock: the wait reads its clock from its own goroutine, never from
// the one a poll runs in.
type clock struct {
	times []time.Time
	i     int
}

func (c *clock) Now() time.Time {
	t := c.times[c.i]
	if c.i < len(c.times)-1 {
		c.i++
	}
	return t
}

// at builds a clock from the elapsed offsets each poll is to land on. With no
// offsets every reading is the start, so elapsed stays zero.
func at(offsets ...time.Duration) *clock {
	times := []time.Time{base}
	for _, d := range offsets {
		times = append(times, base.Add(d))
	}
	return &clock{times: times}
}

func testOpts(out io.Writer, now func() time.Time, tty bool) Options {
	return Options{
		Out:      out,
		Schedule: func(time.Duration) time.Duration { return time.Millisecond },
		Now:      now,
		IsTTY:    tty,
	}
}

type answer struct {
	done bool
	err  error
}

// stepper answers a scripted sequence of poll results, the last repeating once
// exhausted so a wait that keeps polling does not run off the end. Its state is
// mutex-guarded because a poll abandoned on an interrupt keeps running after
// the wait it belonged to has returned.
//
// A poll that cancels the context and then answers leaves the wait choosing at
// random between two ready cases, so a test that needs the interrupt to land at
// a known point holds that poll open instead: with hold set, the calls from
// holdFrom on never answer at all.
type stepper struct {
	mu       sync.Mutex
	answers  []answer
	calls    int
	onCall   func(n int) // fires with the 1-based call number, before answering
	hold     chan struct{}
	holdFrom int
}

func (s *stepper) step() (bool, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	a := s.answers[min(n-1, len(s.answers)-1)]
	onCall, hold, holdFrom := s.onCall, s.hold, s.holdFrom
	s.mu.Unlock()

	if onCall != nil {
		onCall(n)
	}
	if hold != nil && n >= holdFrom {
		<-hold
	}
	return a.done, a.err
}

func (s *stepper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// keepPolling is n answers of (false, nil), the "not resolved yet" reply.
func keepPolling(n int) []answer { return make([]answer, n) }

// siteScript answers a scripted sequence of GetSite results, the last
// repeating. Mutex-guarded for the same reason stepper is.
type siteScript struct {
	mu    sync.Mutex
	steps []siteAnswer
	calls int
	gotID string
}

type siteAnswer struct {
	site *api.Site
	err  error
}

func (s *siteScript) GetSite(id string) (*api.Site, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.gotID = id
	step := s.steps[min(s.calls-1, len(s.steps)-1)]
	return step.site, step.err
}

func (s *siteScript) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *siteScript) id() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotID
}

func synced(id int64, outcome, reason string) *api.Site {
	return &api.Site{
		ID:   "site1",
		Sync: &api.Sync{LatestAttemptID: id, Outcome: outcome, Error: reason},
	}
}

// rewrite is what an in-place row rewrite puts on the wire.
func rewrite(text string) string { return "\r\033[K" + text }

func assertOutput(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("output\n got %q\nwant %q", got, want)
	}
}

func TestSyncResolvesAtOrAboveTheAttemptIDAndIgnoresAnOlderOkRow(t *testing.T) {
	var buf bytes.Buffer
	c := &siteScript{steps: []siteAnswer{
		{site: synced(41, "ok", "")},
		{site: synced(42, "ok", "")},
	}}

	site, err := Sync(context.Background(), c, "site-xyz", 42, testOpts(&buf, at().Now, false))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if site == nil || site.Sync.LatestAttemptID != 42 {
		t.Fatalf("site = %+v, want the attempt 42 row", site)
	}
	if n := c.count(); n != 2 {
		t.Fatalf("polls = %d, want 2: the attempt 41 row must not satisfy a wait for 42", n)
	}
	if got := c.id(); got != "site-xyz" {
		t.Fatalf("siteID = %q, want site-xyz", got)
	}
}

// A row that has not reached the attempt yet, and one with no sync block at
// all, both keep the wait open rather than resolving it.
func TestSyncKeepsPollingUntilTheAttemptResolves(t *testing.T) {
	var buf bytes.Buffer
	c := &siteScript{steps: []siteAnswer{
		{site: &api.Site{ID: "site1"}},
		{site: synced(42, "running", "")},
		{site: synced(42, "ok", "")},
	}}

	site, err := Sync(context.Background(), c, "site1", 42, testOpts(&buf, at().Now, false))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if site == nil || site.Sync.Outcome != "ok" {
		t.Fatalf("site = %+v, want the resolved ok row", site)
	}
	if n := c.count(); n != 3 {
		t.Fatalf("polls = %d, want 3", n)
	}
}

func TestSyncSurfacesTheReconcilerReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{"reason given", "edge delete failed", "reconcile failed: edge delete failed"},
		{"reason empty", "", "reconcile failed: see `kamakiri status`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &siteScript{steps: []siteAnswer{{site: synced(7, "error", tc.reason)}}}

			site, err := Sync(context.Background(), c, "site1", 7, testOpts(&buf, at().Now, false))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if site == nil || site.Sync.Outcome != "error" {
				t.Fatalf("site = %+v, want the failed row alongside the error", site)
			}
			// The type is what a caller matches on to word the reconciler's
			// verdict differently from a failure to read it.
			var failure *ReconcileFailure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v (%T), want a *ReconcileFailure", err, err)
			}
		})
	}
}

// Every unresolved return hands back a nil site, so a caller that names a field
// off it cannot name one read before the wait was over.
func TestSyncReturnsNoSiteWhenTheWaitDidNotResolve(t *testing.T) {
	transport := &api.ErrorResponse{Code: "site_not_found", Message: "not found"}

	t.Run("budget exhausted", func(t *testing.T) {
		var buf bytes.Buffer
		c := &siteScript{steps: []siteAnswer{
			{site: synced(1, "ok", "")}, // an older row, read before the outage
			{err: transport},
		}}

		site, err := Sync(context.Background(), c, "site1", 42, testOpts(&buf, at().Now, false))
		if err == nil || err.Error() != `site not found. Run "kamakiri init"` {
			t.Fatalf("err = %v, want the mapped transport error", err)
		}
		if site != nil {
			t.Fatalf("site = %+v, want nil", site)
		}
		if n := c.count(); n != 7 {
			t.Fatalf("polls = %d, want 7: one good read then six failures", n)
		}
		// Nothing judged the change here, so the caller that words a verdict
		// must not see one.
		var failure *ReconcileFailure
		if errors.As(err, &failure) {
			t.Errorf("err = %v, want the mapped transport error, not a *ReconcileFailure", err)
		}
	})

	t.Run("interrupted", func(t *testing.T) {
		var buf bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := &siteScript{steps: []siteAnswer{{site: synced(42, "ok", "")}}}

		site, err := Sync(ctx, c, "site1", 42, testOpts(&buf, at().Now, false))
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("err = %v, want ErrInterrupted", err)
		}
		if site != nil {
			t.Fatalf("site = %+v, want nil", site)
		}
	})

	t.Run("version refusal", func(t *testing.T) {
		var buf bytes.Buffer
		c := &siteScript{steps: []siteAnswer{{err: api.ErrUpgradeRequired}}}

		site, err := Sync(context.Background(), c, "site1", 42, testOpts(&buf, at().Now, false))
		if !errors.Is(err, api.ErrUpgradeRequired) {
			t.Fatalf("err = %v, want ErrUpgradeRequired", err)
		}
		if site != nil {
			t.Fatalf("site = %+v, want nil", site)
		}
	})
}

func TestPollKeepsPollingUntilAStepIsDone(t *testing.T) {
	var buf bytes.Buffer
	s := &stepper{answers: append(keepPolling(3), answer{done: true})}

	if err := Poll(context.Background(), testOpts(&buf, at().Now, false), s.step); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n := s.count(); n != 4 {
		t.Fatalf("polls = %d, want 4", n)
	}
}

// The error travels unchanged and the row is handed back verbatim, since the
// caller's own failure copy is what terminates it.
func TestPollReturnsATerminalErrorUnchanged(t *testing.T) {
	var buf bytes.Buffer
	terminal := errors.New("the reconcile failed")
	opts := testOpts(&buf, at(20*time.Second).Now, true)
	opts.Prefix = prefix
	s := &stepper{answers: []answer{{}, {done: true, err: terminal}}}

	err := Poll(context.Background(), opts, s.step)
	if err != terminal {
		t.Fatalf("err = %v, want the step's own error unwrapped and unmapped", err)
	}
	assertOutput(t, buf.String(),
		prefix+rewrite("Creating site... (under a minute)")+rewrite(prefix))
}

func TestPollToleratesFiveConsecutiveTransientFailures(t *testing.T) {
	var buf bytes.Buffer
	transient := errors.New("connection reset")
	answers := []answer{}
	for range 5 {
		answers = append(answers, answer{err: transient})
	}
	// A poll that answers anything else resets the count, so the five after it
	// are tolerated in their turn.
	answers = append(answers, answer{})
	for range 5 {
		answers = append(answers, answer{err: transient})
	}
	answers = append(answers, answer{done: true})
	s := &stepper{answers: answers}

	if err := Poll(context.Background(), testOpts(&buf, at().Now, false), s.step); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n := s.count(); n != 12 {
		t.Fatalf("polls = %d, want 12", n)
	}
}

// The exhausted budget is a return the caller reports on, so it leaves the row
// as any other one does: the token that was drawn has ended the line it was
// drawn on, and a fresh copy of the prefix goes out for the caller's
// terminator to read on from.
func TestPollFailsOnTheSixthConsecutiveTransientFailure(t *testing.T) {
	var buf bytes.Buffer
	opts := testOpts(&buf, at(time.Minute).Now, false)
	opts.Prefix = prefix
	s := &stepper{answers: []answer{{err: &api.ErrorResponse{Code: "site_not_found", Message: "not found"}}}}

	err := Poll(context.Background(), opts, s.step)
	if err == nil || err.Error() != `site not found. Run "kamakiri init"` {
		t.Fatalf("err = %v, want the mapped transport error", err)
	}
	if n := s.count(); n != 6 {
		t.Fatalf("polls = %d, want 6", n)
	}
	assertOutput(t, buf.String(), "Creating site... (1m)\nCreating site... ")
}

// A refused version is refused on every later poll, so it ends the wait where
// it arrives, spends none of the budget, and reaches the caller unwrapped so
// the upgrade copy is the whole message.
func TestPollReturnsAVersionRefusalBareOnThePollThatCarriesIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		done bool
	}{
		{"not done", false},
		{"done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := &stepper{answers: []answer{{done: tc.done, err: api.ErrUpgradeRequired}}}

			err := Poll(context.Background(), testOpts(&buf, at().Now, false), s.step)
			if err != api.ErrUpgradeRequired {
				t.Fatalf("err = %v, want ErrUpgradeRequired itself", err)
			}
			if n := s.count(); n != 1 {
				t.Fatalf("polls = %d, want 1", n)
			}
		})
	}
}

// The row a refusal leaves behind is the contract every wait shares: an open
// line is back to its verbatim prefix and still open for the caller, and a late
// line is gone, so the upgrade copy lands on a clean terminal.
func TestVersionRefusalLeavesTheRowAsAnyOtherReturnDoes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tty      bool
		prefix   string
		tickText string
		want     string
	}{
		{
			name:   "open line on a terminal",
			tty:    true,
			prefix: prefix,
			want:   prefix + rewrite("Creating site... (under a minute)") + rewrite(prefix),
		},
		{
			name:   "open line without one",
			tty:    false,
			prefix: prefix,
			want:   "Creating site... (1m)\nCreating site... ",
		},
		{
			name:     "late line on a terminal",
			tty:      true,
			tickText: tickText,
			want:     rewrite(tickText+" (under a minute)") + rewrite(""),
		},
		{
			name:     "late line without one",
			tty:      false,
			tickText: tickText,
			want:     tickText + " (1m)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			// The first poll draws the token, so the refusal arrives with one on
			// screen and the row has something to restore.
			elapsed := 20 * time.Second
			if !tc.tty {
				elapsed = time.Minute
			}
			opts := testOpts(&buf, at(elapsed).Now, tc.tty)
			opts.Prefix = tc.prefix
			opts.TickText = tc.tickText
			s := &stepper{answers: []answer{{}, {err: api.ErrUpgradeRequired}}}

			if err := Poll(context.Background(), opts, s.step); err != api.ErrUpgradeRequired {
				t.Fatalf("err = %v, want ErrUpgradeRequired", err)
			}
			assertOutput(t, buf.String(), tc.want)
		})
	}
}

// On a terminal the row is rewritten whole on every poll from twenty seconds
// on, and restored to the verbatim prefix for the caller's terminator.
func TestOpenLineRowOnATerminal(t *testing.T) {
	var buf bytes.Buffer
	opts := testOpts(&buf, at(19*time.Second, 20*time.Second, time.Minute).Now, true)
	opts.Prefix = prefix
	s := &stepper{answers: append(keepPolling(3), answer{done: true})}

	if err := Poll(context.Background(), opts, s.step); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	assertOutput(t, buf.String(),
		prefix+
			rewrite("Creating site... (under a minute)")+
			rewrite("Creating site... (1m)")+
			rewrite(prefix))
}

// Without a terminal the row commits whole lines: the first token completes the
// prefix line a minute in, every later token repeats the prefix on a line of
// its own, and the close writes the prefix once more for the caller's
// terminator, so a consumer that renders line by line sees the wait is alive
// while it runs.
func TestOpenLineRowWithoutATerminal(t *testing.T) {
	var buf bytes.Buffer
	opts := testOpts(&buf, at(59*time.Second, time.Minute, 2*time.Minute).Now, false)
	opts.Prefix = prefix
	s := &stepper{answers: append(keepPolling(3), answer{done: true})}

	if err := Poll(context.Background(), opts, s.step); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	// The caller's own "done." then lands as "Creating site... done." on the
	// last line.
	assertOutput(t, buf.String(), "Creating site... (1m)\nCreating site... (2m)\nCreating site... ")
}

// The two ⧗ callers hand in a prefix that ends in no whitespace and write a
// line of their own under the row, so what the row leaves them has to be the
// prefix and nothing else: a separator would only push that line along, and a
// wait that drew tokens has to leave the same last line as one that drew none.
func TestOpenLineRowWithoutATerminalForAPrefixEndingInNoWhitespace(t *testing.T) {
	const glyphPrefix = "⧗ Deactivating CDN"

	for _, tc := range []struct {
		name    string
		offsets []time.Duration
		answers []answer
		want    string
	}{
		{
			name:    "tokens drawn",
			offsets: []time.Duration{59 * time.Second, time.Minute, 2 * time.Minute},
			answers: append(keepPolling(3), answer{done: true}),
			want:    "⧗ Deactivating CDN (1m)\n⧗ Deactivating CDN (2m)\n⧗ Deactivating CDN",
		},
		{
			name:    "resolved before the first token",
			offsets: []time.Duration{5 * time.Second},
			answers: []answer{{done: true}},
			want:    glyphPrefix,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := testOpts(&buf, at(tc.offsets...).Now, false)
			opts.Prefix = glyphPrefix
			s := &stepper{answers: tc.answers}

			if err := Poll(context.Background(), opts, s.step); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			assertOutput(t, buf.String(), tc.want)
		})
	}
}

// The fast case writes what a command with no wait at all would, with or
// without a terminal, so a wait that resolves at once changes no command's
// final screen.
func TestOpenLineRowResolvedBeforeTheFirstToken(t *testing.T) {
	for _, tty := range []bool{true, false} {
		var buf bytes.Buffer
		opts := testOpts(&buf, at(5*time.Second).Now, tty)
		opts.Prefix = prefix
		s := &stepper{answers: []answer{{done: true}}}

		if err := Poll(context.Background(), opts, s.step); err != nil {
			t.Fatalf("tty=%v: err = %v, want nil", tty, err)
		}
		if got := buf.String(); got != prefix {
			t.Fatalf("tty=%v: output = %q, want the verbatim prefix alone", tty, got)
		}
	}
}

// The committed lines in Japanese. The token's own full-width bracket
// separates it from the leader it follows, so a token line carries the trimmed
// prefix, while the leader still owes a space to the Japanese prose the
// caller's terminator reads on with, which the verbatim prefix on the closing
// line carries.
func TestOpenLineRowWithoutATerminalInJapanese(t *testing.T) {
	const japanesePrefix = "サイトを作成中... "

	i18n.Load("ja")
	defer i18n.Load("en")

	for _, tc := range []struct {
		name    string
		offsets []time.Duration
		answers []answer
		want    string
	}{
		{
			name:    "tokens drawn",
			offsets: []time.Duration{59 * time.Second, time.Minute, 2 * time.Minute},
			answers: append(keepPolling(3), answer{done: true}),
			want:    "サイトを作成中...（1分）\nサイトを作成中...（2分）\nサイトを作成中... ",
		},
		{
			name:    "resolved before the first token",
			offsets: []time.Duration{5 * time.Second},
			answers: []answer{{done: true}},
			want:    japanesePrefix,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := testOpts(&buf, at(tc.offsets...).Now, false)
			opts.Prefix = japanesePrefix
			s := &stepper{answers: tc.answers}

			if err := Poll(context.Background(), opts, s.step); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			assertOutput(t, buf.String(), tc.want)
		})
	}
}

// On a terminal the late row appears at twenty seconds, is redrawn on every
// poll after that, and is off screen again before the wait returns.
func TestLateLineRowOnATerminal(t *testing.T) {
	var buf bytes.Buffer
	opts := testOpts(&buf, at(19*time.Second, 20*time.Second, 21*time.Second).Now, true)
	opts.TickText = tickText
	s := &stepper{answers: append(keepPolling(3), answer{done: true})}

	if err := Poll(context.Background(), opts, s.step); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	assertOutput(t, buf.String(),
		rewrite(tickText+" (under a minute)")+
			rewrite(tickText+" (under a minute)")+
			rewrite(""))
}

// Without a terminal nothing can be redrawn, so each token is a line of its own
// and the row needs no closing.
func TestLateLineRowWithoutATerminal(t *testing.T) {
	var buf bytes.Buffer
	opts := testOpts(&buf, at(59*time.Second, time.Minute, 2*time.Minute).Now, false)
	opts.TickText = tickText
	s := &stepper{answers: append(keepPolling(3), answer{done: true})}

	if err := Poll(context.Background(), opts, s.step); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	assertOutput(t, buf.String(), tickText+" (1m)\n"+tickText+" (2m)\n")
}

func TestLateLineRowThatNeverAppearedWritesNothing(t *testing.T) {
	for _, tty := range []bool{true, false} {
		var buf bytes.Buffer
		opts := testOpts(&buf, at(5*time.Second).Now, tty)
		opts.TickText = tickText
		s := &stepper{answers: []answer{{done: true}}}

		if err := Poll(context.Background(), opts, s.step); err != nil {
			t.Fatalf("tty=%v: err = %v, want nil", tty, err)
		}
		if got := buf.String(); got != "" {
			t.Fatalf("tty=%v: output = %q, want nothing", tty, got)
		}
	}
}

// A wait given neither a prefix nor a tick text has no row of its own, so no
// token ever lands in the middle of whatever the caller printed, however long
// the wait runs.
func TestAWaitWithNeitherPrefixNorTickTextDrawsNoRow(t *testing.T) {
	for _, tty := range []bool{true, false} {
		var buf bytes.Buffer
		opts := testOpts(&buf, at(time.Minute, 2*time.Minute, time.Hour).Now, tty)
		s := &stepper{answers: append(keepPolling(3), answer{done: true})}

		if err := Poll(context.Background(), opts, s.step); err != nil {
			t.Fatalf("tty=%v: err = %v, want nil", tty, err)
		}
		if got := buf.String(); got != "" {
			t.Fatalf("tty=%v: output = %q, want nothing", tty, got)
		}
	}
}

// An interrupt returns while the request it was waiting on is still in flight.
// The held-back poll would block the wait forever if its answer were awaited.
func TestInterruptReturnsWithoutWaitingOutTheInFlightPoll(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })

	opts := testOpts(&buf, at().Now, true)
	opts.Prefix = prefix
	opts.StopLine = SharedStopLine()
	s := &stepper{
		answers:  []answer{{done: true}},
		hold:     hold,
		holdFrom: 1,
		onCall:   func(int) { cancel() },
	}

	err := Poll(ctx, opts, s.step)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	assertOutput(t, buf.String(), prefix+"\n"+stopLine+"\n")
}

// A caller that renders stop copy of its own passes none here, and then nothing
// but the row's own closing goes out.
func TestInterruptWithNoStopLineWritesNone(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := testOpts(&buf, at().Now, true)
	opts.Prefix = prefix
	s := &stepper{answers: []answer{{done: true}}}

	if err := Poll(ctx, opts, s.step); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	assertOutput(t, buf.String(), prefix+"\n")
}

// The row an interrupt leaves behind, per mode and per terminal. An open line
// is always terminated on a terminal, and off one only where no committed token
// has already ended it; its held-back whitespace is dropped with nothing left
// to separate. A late line is cleared on a terminal and was already
// line-terminated without one.
func TestInterruptClosesTheRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tty      bool
		prefix   string
		tickText string
		want     string
	}{
		{
			name:   "open line on a terminal",
			tty:    true,
			prefix: prefix,
			want: prefix + rewrite("Creating site... (under a minute)") +
				rewrite(prefix) + "\n" + stopLine + "\n",
		},
		{
			name:   "open line without one",
			tty:    false,
			prefix: prefix,
			want:   "Creating site..." + " (1m)" + "\n" + stopLine + "\n",
		},
		{
			name:     "late line on a terminal",
			tty:      true,
			tickText: tickText,
			// No newline: the cleared row is where the stop line goes.
			want: rewrite(tickText+" (under a minute)") + rewrite("") + stopLine + "\n",
		},
		{
			name:     "late line without one",
			tty:      false,
			tickText: tickText,
			want:     tickText + " (1m)\n" + stopLine + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			hold := make(chan struct{})
			t.Cleanup(func() { close(hold) })

			// The first poll does not resolve, which draws the token; the
			// second cancels and is held open, so the interrupt lands with the
			// token on screen.
			elapsed := 20 * time.Second
			if !tc.tty {
				elapsed = time.Minute
			}
			opts := testOpts(&buf, at(elapsed).Now, tc.tty)
			opts.Prefix = tc.prefix
			opts.TickText = tc.tickText
			opts.StopLine = SharedStopLine()
			s := &stepper{
				answers:  []answer{{}},
				hold:     hold,
				holdFrom: 2,
				onCall: func(n int) {
					if n == 2 {
						cancel()
					}
				},
			}

			if err := Poll(ctx, opts, s.step); !errors.Is(err, ErrInterrupted) {
				t.Fatalf("err = %v, want ErrInterrupted", err)
			}
			assertOutput(t, buf.String(), tc.want)
		})
	}
}

// Off a terminal the open line is terminated by the first token, so an
// interrupt that lands before one has a line left open and terminates it
// itself, which is what keeps the stop copy on a line of its own.
func TestInterruptBeforeTheFirstTokenTerminatesTheOpenLineWithoutATerminal(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := testOpts(&buf, at().Now, false)
	opts.Prefix = prefix
	opts.StopLine = SharedStopLine()
	s := &stepper{answers: []answer{{done: true}}}

	if err := Poll(ctx, opts, s.step); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	assertOutput(t, buf.String(), "Creating site...\n"+stopLine+"\n")
}

// A late row that never appeared is not closed on an interrupt either, so the
// stop line is the whole output.
func TestInterruptBeforeTheFirstTokenClosesNoLateRow(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := testOpts(&buf, at().Now, true)
	opts.TickText = tickText
	opts.StopLine = SharedStopLine()
	s := &stepper{answers: []answer{{done: true}}}

	if err := Poll(ctx, opts, s.step); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	assertOutput(t, buf.String(), stopLine+"\n")
}

// A select over a cancelled context and a step that answers at once picks at
// random, so the check before each poll is what makes the interrupt win.
//
// The step reports its own entry rather than being counted after the fact: a
// poll is launched before it runs, so a counter read on return would usually be
// read before the goroutine was scheduled and would pass whether the check is
// there or not. The wait below is the time a launched poll needs to reach its
// first line, orders of magnitude more than scheduling one takes.
func TestACancelledContextBeatsAStepThatAnswersAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	entered := make(chan struct{})
	step := func() (bool, error) {
		close(entered)
		return true, nil
	}

	if err := Poll(ctx, testOpts(&buf, at().Now, false), step); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	select {
	case <-entered:
		t.Fatal("the step ran: a context cancelled before the wait starts must return before any poll is launched")
	case <-time.After(200 * time.Millisecond):
	}
}

// The production cadence: its four bands, and the jitter in the first one that
// keeps a fan-out of clients off a single tick.
func TestDefaultSchedule(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		want    time.Duration
	}{
		{2 * time.Minute, 10 * time.Second},
		{9 * time.Minute, 10 * time.Second},
		{10 * time.Minute, 30 * time.Second},
		{time.Hour, 30 * time.Second},
		{30 * time.Second, 2 * time.Second},
		{119 * time.Second, 2 * time.Second},
	} {
		if got := DefaultSchedule(tc.elapsed); got != tc.want {
			t.Errorf("DefaultSchedule(%s) = %s, want %s", tc.elapsed, got, tc.want)
		}
	}

	spread := map[time.Duration]bool{}
	for range 200 {
		got := DefaultSchedule(29 * time.Second)
		if got < 400*time.Millisecond || got >= 600*time.Millisecond {
			t.Fatalf("first band = %s, want 500ms ±20%%", got)
		}
		spread[got] = true
	}
	if len(spread) < 2 {
		t.Fatalf("the first band returned one fixed value, so it carries no jitter")
	}
}
