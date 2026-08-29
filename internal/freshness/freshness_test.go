package freshness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: the elapsed and span annotations on the
// milestone lines render from the message catalog. Load rather than Setup:
// nothing here reports which language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

// step is one scripted GetSite result; a non-nil err models a transport
// failure.
type step struct {
	site *api.Site
	err  error
}

// scriptClient returns a fixed sequence of GetSite results, the last repeating
// once exhausted so a wait that keeps polling does not run off the end. onPoll
// fires with the pre-advance index, which lets a test cancel mid-poll.
type scriptClient struct {
	steps  []step
	i      int
	onPoll func(idx int)
	gotID  string
}

func (c *scriptClient) GetSite(id string) (*api.Site, error) {
	c.gotID = id
	idx := c.i
	if c.i < len(c.steps)-1 {
		c.i++
	}
	if c.onPoll != nil {
		c.onPoll(idx)
	}
	return c.steps[idx].site, c.steps[idx].err
}

func pendingSteps(sites ...*api.Site) []step {
	out := make([]step, len(sites))
	for i, s := range sites {
		out[i] = step{site: s}
	}
	return out
}

// fakeClock advances by a fixed step per Now(). The mutex guards the signal
// path, which reads it from another goroutine.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(c.step)
	return now
}

// listClock returns a predetermined sequence of times, the last repeating, so a
// test controls elapsed exactly. Now() runs once at start and once per poll, so
// times[0] is the start and times[k] is poll k.
type listClock struct {
	mu    sync.Mutex
	times []time.Time
	i     int
}

func (c *listClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.times[c.i]
	if c.i < len(c.times)-1 {
		c.i++
	}
	return t
}

var base = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// rewind is the escape a cleared transient block writes on a TTY: n lines up,
// then erase everything below.
func rewind(n int) string { return fmt.Sprintf("\033[%dA\033[J", n) }

func testOpts(now func() time.Time, tty bool, ctx context.Context) waitOpts {
	return waitOpts{
		pollSchedule: func(time.Duration) time.Duration { return time.Millisecond },
		now:          now,
		isTTY:        tty,
		ctx:          ctx,
	}
}

func siteFresh(url string) *api.Site {
	return &api.Site{Freshness: &api.Freshness{State: "fresh", LiveURL: url}}
}

func sitePending(targets ...api.PendingTarget) *api.Site {
	return &api.Site{Freshness: &api.Freshness{State: "pending", Pending: targets}}
}

func siteBlocked(bs ...api.Blocker) *api.Site {
	return &api.Site{Freshness: &api.Freshness{State: "blocked", Blockers: bs}}
}

func cdnTarget(provider, host, owed string) api.PendingTarget {
	return api.PendingTarget{Axis: "cdn", Provider: provider, Host: host, OwedSince: owed}
}

func TestFormatBlocker(t *testing.T) {
	const lead = "Deployed, but"
	for _, tc := range []struct {
		name string
		b    api.Blocker
		want []string
	}{
		{
			name: "credentials_rejected",
			b:    api.Blocker{Provider: "webaccel", Host: "shop.example.com", Reason: "credentials_rejected"},
			want: []string{
				"✗ Deployed, but the WebAccel cache was not flushed: the stored credentials were rejected.",
				"  Run kamakiri cdn credentials to update them; once they are accepted the flush retries and completes on its own.",
			},
		},
		{
			name: "resource_deleted names the provider command",
			b:    api.Blocker{Provider: "webaccel", Host: "shop.example.com", Reason: "resource_deleted"},
			want: []string{
				"✗ Deployed, but the WebAccel cache was not flushed: no CDN resource exists for shop.example.com.",
				"  Run kamakiri cdn webaccel to re-provision it; once it exists the flush retries and completes on its own.",
			},
		},
		{
			name: "provider_rejected with detail",
			b:    api.Blocker{Provider: "cloudflare", Host: "shop.example.com", Reason: "provider_rejected", Detail: "zone is paused"},
			want: []string{
				"✗ Deployed, but the Cloudflare cache was not flushed: Cloudflare rejected the request for shop.example.com (zone is paused).",
				"  The flush retries automatically and completes once it is accepted.",
			},
		},
		{
			name: "provider_rejected without detail drops the parenthetical",
			b:    api.Blocker{Provider: "cloudflare", Host: "shop.example.com", Reason: "provider_rejected"},
			want: []string{
				"✗ Deployed, but the Cloudflare cache was not flushed: Cloudflare rejected the request for shop.example.com.",
				"  Check kamakiri status for the recorded reason, or contact support if it persists.",
			},
		},
		{
			name: "site_torn_down drops the lead entirely",
			b:    api.Blocker{Reason: "site_torn_down"},
			want: []string{
				"✗ This site has been torn down, so the deploy cannot go live.",
				"  Run kamakiri init to create a new site.",
			},
		},
		{
			name: "unrecognized reason fails closed to the provider_rejected form",
			b:    api.Blocker{Provider: "cloudflare", Host: "shop.example.com", Reason: "some_future_tag"},
			want: []string{
				"✗ Deployed, but the Cloudflare cache was not flushed: Cloudflare rejected the request for shop.example.com.",
				"  Check kamakiri status for the recorded reason, or contact support if it persists.",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatBlocker(lead, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("line count: got %d %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d:\n got: %q\nwant: %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A caller with no lead of its own must state a present condition, never a past
// deploy, so a blocked entry does not open by reporting an achievement.
func TestFormatBlockerWithoutLead(t *testing.T) {
	got := FormatBlocker("", api.Blocker{Provider: "webaccel", Host: "h", Reason: "credentials_rejected"})
	want := "✗ The WebAccel cache is not flushed: the stored credentials were rejected."
	if got[0] != want {
		t.Fatalf("non-verb headline:\n got: %q\nwant: %q", got[0], want)
	}
	if strings.Contains(got[0], "Deployed, but") || strings.Contains(got[0], "was not flushed") {
		t.Fatalf("status lead must state a present condition, not a past action: %q", got[0])
	}
}

func TestWaitBlockedCommitsEntriesAndReturnsErrFlushBlocked(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	c := &scriptClient{steps: pendingSteps(siteBlocked(
		api.Blocker{Provider: "webaccel", Host: "a.example.com", Reason: "credentials_rejected"},
		api.Blocker{Provider: "cloudflare", Host: "b.example.com", Reason: "provider_rejected", Detail: "nope"},
	))}

	err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background()))
	if !errors.Is(err, ErrFlushBlocked) {
		t.Fatalf("err = %v, want ErrFlushBlocked", err)
	}
	out := buf.String()
	if !strings.Contains(out, "the WebAccel cache was not flushed: the stored credentials were rejected.") {
		t.Errorf("missing credentials entry:\n%s", out)
	}
	if !strings.Contains(out, "Cloudflare rejected the request for b.example.com (nope).") {
		t.Errorf("missing provider_rejected entry:\n%s", out)
	}
}

func TestWaitHardErrorOnMissingOrUnknownState(t *testing.T) {
	for _, tc := range []struct {
		name string
		site *api.Site
	}{
		{"nil freshness block", &api.Site{}},
		{"empty state", &api.Site{Freshness: &api.Freshness{State: ""}}},
		{"unrecognised state", &api.Site{Freshness: &api.Freshness{State: "syncing"}}},
		{"blocked with no blockers", &api.Site{Freshness: &api.Freshness{State: "blocked"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			clk := &fakeClock{t: base, step: time.Second}
			c := &scriptClient{steps: pendingSteps(tc.site)}
			err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background()))
			if err == nil {
				t.Fatal("expected a hard error, got nil")
			}
			if errors.Is(err, ErrFlushBlocked) || errors.Is(err, ErrInterrupted) {
				t.Fatalf("unknown state must be a hard error, not a sentinel: %v", err)
			}
		})
	}
}

func TestFreshMilestone(t *testing.T) {
	deploy := DeployMsgs()
	purge := PurgeMsgs()
	for _, tc := range []struct {
		name            string
		m               msgs
		liveURL         string
		pendingObserved bool
		elapsed         time.Duration
		want            string
	}{
		{"serving canonical", deploy, "https://shop.example.com", true, 42 * time.Second, "✓ live: https://shop.example.com (~42s)"},
		{"subdomain fallback rendered verbatim", deploy, "https://my-site.kamakiri-pages.jp", false, time.Second, "✓ live: https://my-site.kamakiri-pages.jp"},
		{"nil live_url renders bare", deploy, "", true, 42 * time.Second, "✓ live (~42s)"},
		{"sub-30s observed is bare", deploy, "https://shop.example.com", true, 20 * time.Second, "✓ live: https://shop.example.com"},
		{"first-poll fresh is bare (not observed)", deploy, "https://shop.example.com", false, 90 * time.Second, "✓ live: https://shop.example.com"},
		{"purge claims no url even if server sends one", purge, "https://shop.example.com", true, 70 * time.Second, "✓ caches flushed (~1m)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &api.Freshness{State: "fresh", LiveURL: tc.liveURL}
			if got := freshMilestone(tc.m, f, tc.pendingObserved, tc.elapsed); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The wait renders whatever live_url the server sends, and nothing at all when
// it sends none.
func TestWaitFreshLiveURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"canonical", "https://shop.example.com", "✓ live: https://shop.example.com"},
		{"bare on nil", "", "✓ live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			clk := &fakeClock{t: base, step: time.Second}
			c := &scriptClient{steps: pendingSteps(siteFresh(tc.url))}
			if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
				t.Fatalf("err = %v", err)
			}
			if got := strings.TrimSpace(buf.String()); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitEscalation(t *testing.T) {
	var buf bytes.Buffer
	owed := iso(base) // owed since the start; at tier2 (elapsed 5m) that is owed 5m
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", owed))
	c := &scriptClient{steps: pendingSteps(pend, pend, pend, siteFresh("https://shop.example.com"))}
	clk := &listClock{times: []time.Time{
		base,                        // start
		base,                        // poll1: elapsed 0   -> tier 0 headline
		base.Add(60 * time.Second),  // poll2: elapsed 60s -> tier 1
		base.Add(300 * time.Second), // poll3: elapsed 5m  -> tier 2
		base.Add(300 * time.Second), // poll4: fresh
	}}

	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()

	headline := "  ⧗ publishing…\n"
	tier1 := "  ⧗ publishing… (still working; the flush retries automatically)\n"
	tier2 := "  ⧗ publishing… waiting on the WebAccel cache flush for shop.example.com (owed 5m)\n"

	iH, i1, i2 := strings.Index(out, headline), strings.Index(out, tier1), strings.Index(out, tier2)
	if iH < 0 || i1 < 0 || i2 < 0 {
		t.Fatalf("missing a tier line\n headline@%d tier1@%d tier2@%d\n%s", iH, i1, i2, out)
	}
	if !(iH < i1 && i1 < i2) {
		t.Fatalf("tiers out of order: headline@%d tier1@%d tier2@%d\n%s", iH, i1, i2, out)
	}
}

func TestTier2AxisForms(t *testing.T) {
	now := base.Add(6 * time.Minute)
	owed := iso(base.Add(-4 * time.Minute)) // owed 10m at `now`
	for _, tc := range []struct {
		name   string
		target api.PendingTarget
		want   string
	}{
		{
			name:   "cdn names provider host and owed age",
			target: api.PendingTarget{Axis: "cdn", Provider: "webaccel", Host: "shop.example.com", OwedSince: owed},
			want:   "  ⧗ publishing… waiting on the WebAccel cache flush for shop.example.com (owed 10m)",
		},
		{
			name:   "cdn pending at zero failures is still named (no health gate)",
			target: api.PendingTarget{Axis: "cdn", Provider: "cloudflare", Host: "a.example.com", OwedSince: owed},
			want:   "  ⧗ publishing… waiting on the Cloudflare cache flush for a.example.com (owed 10m)",
		},
		{
			name:   "edge uses the owed prefix",
			target: api.PendingTarget{Axis: "edge", OwedSince: owed},
			want:   "  ⧗ publishing… waiting on our edge cache flush (owed 10m)",
		},
		{
			name:   "origin uses a bare age, no owed prefix",
			target: api.PendingTarget{Axis: "origin", OwedSince: owed},
			want:   "  ⧗ publishing… waiting on the site config to reach the edge (10m)",
		},
		{
			name:   "nil owed_since drops the parenthetical",
			target: api.PendingTarget{Axis: "cdn", Provider: "webaccel", Host: "shop.example.com"},
			want:   "  ⧗ publishing… waiting on the WebAccel cache flush for shop.example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := sitePending(tc.target)
			v := buildPendingView(site, 6*time.Minute, now, 0, time.Time{})
			got := v.render(DeployMsgs(), now)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiagnostics(t *testing.T) {
	now := base.Add(2 * time.Minute)
	m := DeployMsgs()
	pend := sitePending(api.PendingTarget{Axis: "origin", OwedSince: iso(base)})

	t.Run("origin errored fires at 3 consecutive polls, not 2", func(t *testing.T) {
		site := *pend
		site.Sync = &api.Sync{Outcome: "error", Error: "caddy push rejected (4xx)"}

		v2 := buildPendingView(&site, 2*time.Minute, now, 2, time.Time{})
		if v2.hasDiag() {
			t.Fatalf("2 error polls must not fire the origin diagnostic: %q", v2.render(m, now))
		}
		v3 := buildPendingView(&site, 2*time.Minute, now, 3, time.Time{})
		got := v3.render(m, now)
		want := "  ⧗ publishing… the site config push is failing: caddy push rejected (4xx). We keep retrying."
		if len(got) != 1 || got[0] != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("origin stranded fires past 5m running, age from attempted_at", func(t *testing.T) {
		site := *pend
		site.Sync = &api.Sync{Outcome: "running", AttemptedAt: iso(now.Add(-7 * time.Minute))}
		runningSince := now.Add(-5 * time.Minute)
		v := buildPendingView(&site, 2*time.Minute, now, 0, runningSince)
		got := v.render(m, now)
		want := "  ⧗ publishing… the site config push has been running for 7m. We keep retrying."
		if len(got) != 1 || got[0] != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("origin stranded drops the age when attempted_at is absent", func(t *testing.T) {
		site := *pend
		site.Sync = &api.Sync{Outcome: "running"} // no AttemptedAt
		runningSince := now.Add(-5 * time.Minute)
		v := buildPendingView(&site, 2*time.Minute, now, 0, runningSince)
		got := v.render(m, now)
		want := "  ⧗ publishing… the site config push is still running. We keep retrying."
		if len(got) != 1 || got[0] != want {
			t.Fatalf("a missing attempted_at must not fabricate an age: got %q, want %q", got, want)
		}
	})

	t.Run("failing edge renders on-us copy", func(t *testing.T) {
		site := *pend
		site.EdgePurgeHealth = "failing"
		site.EdgePurgeErrorReason = "http_503"
		v := buildPendingView(&site, 2*time.Minute, now, 0, time.Time{})
		got := v.render(m, now)
		want := "  ⧗ publishing… our edge cache flush is failing (http_503). We keep retrying."
		if len(got) != 1 || got[0] != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("both fire, origin then edge", func(t *testing.T) {
		site := *pend
		site.Sync = &api.Sync{Outcome: "error", Error: "boom"}
		site.EdgePurgeHealth = "broken"
		site.EdgePurgeErrorReason = "http_500"
		v := buildPendingView(&site, 2*time.Minute, now, 3, time.Time{})
		got := v.render(m, now)
		if len(got) != 2 {
			t.Fatalf("expected two diagnostic lines, got %q", got)
		}
		if !strings.Contains(got[0], "site config push is failing") {
			t.Errorf("first line should be the origin diagnostic: %q", got[0])
		}
		if !strings.Contains(got[1], "edge cache flush is failing") {
			t.Errorf("second line should be the edge diagnostic: %q", got[1])
		}
	})

	t.Run("none: falls through to the tier line", func(t *testing.T) {
		v := buildPendingView(pend, 0, now, 0, time.Time{})
		if v.hasDiag() {
			t.Fatalf("no diagnostic should apply: %q", v.render(m, now))
		}
		got := v.render(m, now)
		if len(got) != 1 || got[0] != "  ⧗ publishing…" {
			t.Fatalf("expected the bare headline, got %q", got)
		}
	})
}

func TestAdvanceDiagnostics(t *testing.T) {
	now := base
	syncSite := func(outcome string) *api.Site {
		return &api.Site{Sync: &api.Sync{Outcome: outcome}}
	}

	if ep, rs := advanceDiagnostics(syncSite("error"), now, 2, now.Add(-time.Minute)); ep != 3 || !rs.IsZero() {
		t.Errorf("error: got (%d, zero=%t), want (3, true)", ep, rs.IsZero())
	}
	if ep, rs := advanceDiagnostics(syncSite("running"), now, 4, time.Time{}); ep != 0 || rs != now {
		t.Errorf("running-first: got (%d, %v), want (0, %v)", ep, rs, now)
	}
	prev := now.Add(-3 * time.Minute)
	if ep, rs := advanceDiagnostics(syncSite("running"), now, 0, prev); ep != 0 || rs != prev {
		t.Errorf("running-continued: got (%d, %v), want (0, %v)", ep, rs, prev)
	}
	if ep, rs := advanceDiagnostics(syncSite("ok"), now, 5, now.Add(-time.Minute)); ep != 0 || !rs.IsZero() {
		t.Errorf("ok: got (%d, zero=%t), want (0, true)", ep, rs.IsZero())
	}
}

func TestWaitSelfGuardOnlyFreshIsLive(t *testing.T) {
	var buf bytes.Buffer
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	c := &scriptClient{steps: pendingSteps(pend, pend, siteFresh("https://shop.example.com"))}
	clk := &listClock{times: []time.Time{base, base.Add(5 * time.Second), base.Add(20 * time.Second), base.Add(42 * time.Second)}}

	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if strings.Count(out, "✓ live") != 1 {
		t.Fatalf("live milestone must appear exactly once, on fresh:\n%s", out)
	}
	if !strings.Contains(out, "✓ live: https://shop.example.com (~42s)") {
		t.Fatalf("fresh milestone should carry the observed span:\n%s", out)
	}
}

func TestWaitFirstPollFreshAccepted(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	c := &scriptClient{steps: pendingSteps(siteFresh("https://shop.example.com"))}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "✓ live: https://shop.example.com" {
		t.Fatalf("first-poll fresh should be bare: %q", got)
	}
}

func TestWaitTransportBudgetExhausted(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	steps := make([]step, statusFetchBudget)
	for i := range steps {
		steps[i] = step{err: errors.New("boom")}
	}
	c := &scriptClient{steps: steps}
	err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background()))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the mapped transport error, got %v", err)
	}
	if !strings.Contains(err.Error(), "10 times consecutively") {
		t.Fatalf("error should name the exhausted budget: %v", err)
	}
	// The give-up frame is written as "%s: %w", so the localized sentence sits
	// in front of the last error and that error still unwraps out of the result.
	if errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), "boom") {
		t.Errorf("wait give-up error = %v, want the keyed frame over the last error", err)
	}
}

// A refused CLI version answers every poll the same way, so the budget buys
// nothing: the wait aborts on the first refusal, returns the sentinel bare for
// main to render as the upgrade message, and prints no retry noise in front of
// it.
func TestWaitAbortsOnAVersionRefusal(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	polls := 0
	steps := make([]step, statusFetchBudget)
	for i := range steps {
		steps[i] = step{err: api.ErrUpgradeRequired}
	}
	c := &scriptClient{steps: steps, onPoll: func(int) { polls++ }}
	err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background()))
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1: the refusal must not be retried", polls)
	}
	if got := buf.String(); got != "" {
		t.Errorf("output = %q, want nothing printed before the upgrade message", got)
	}
}

// The floor is armed while the wait is already running, so the refusal normally
// arrives with a frame on screen. It is rewound before the wait returns, or the
// upgrade message would print under a spinner that stopped meaning anything.
func TestWaitClearsTheFrameBeforeAbortingOnARefusal(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	var drawn string
	c := &scriptClient{steps: []step{{site: pend}, {err: api.ErrUpgradeRequired}}}
	c.onPoll = func(idx int) {
		if idx == 1 {
			drawn = buf.String()
		}
	}
	err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, true, context.Background()))
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("err = %v, want the refusal returned bare", err)
	}
	if drawn == "" {
		t.Fatal("the first poll must leave a frame on screen, or the rewind pins nothing")
	}
	want := drawn + rewind(strings.Count(drawn, "\n"))
	if got := buf.String(); got != want {
		t.Errorf("output = %q, want the drawn frame rewound: %q", got, want)
	}
}

// A verdict the server did not send cannot be read as pending, which would hang
// the wait forever, nor as fresh, which would claim live over an unknown
// reality. Both shapes it can arrive in are the same hard stop, and the sentinel
// stays matchable through it.
func TestWaitRefusesAVerdictItDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name string
		site *api.Site
	}{
		{name: "no freshness block", site: &api.Site{}},
		{name: "an empty state", site: &api.Site{Freshness: &api.Freshness{State: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			clk := &fakeClock{t: base, step: time.Second}
			c := &scriptClient{steps: pendingSteps(tc.site)}
			err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background()))
			if !errors.Is(err, errFreshnessMissing) {
				t.Fatalf("err = %v, want the missing-verdict sentinel", err)
			}
			// The sentinel looks its message up when it is printed, so it must
			// carry the catalog's text rather than an empty or a key-shaped one.
			if got, want := err.Error(), i18n.T("freshness.err_missing_verdict"); got != want {
				t.Errorf("err.Error() = %q, want %q", got, want)
			}
			if buf.Len() != 0 {
				t.Errorf("nothing should be committed for a verdict that never arrived: %q", buf.String())
			}
		})
	}
}

func TestWaitTransportBudgetToleratesAndResets(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	steps := make([]step, statusFetchBudget-1)
	for i := range steps {
		steps[i] = step{err: errors.New("blip")}
	}
	steps = append(steps, step{site: siteFresh("https://shop.example.com")})
	c := &scriptClient{steps: steps}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("N-1 failures must be tolerated, got %v", err)
	}
	if !strings.Contains(buf.String(), "✓ live") {
		t.Fatalf("expected to reach fresh after tolerated blips:\n%s", buf.String())
	}
}

// Without the reset on success, two separate runs of failures would sum past
// the budget and the wait would give up on a connection that kept recovering.
func TestWaitTransportBudgetResetsAfterSuccess(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	var steps []step
	for i := 0; i < statusFetchBudget-1; i++ {
		steps = append(steps, step{err: errors.New("blip")})
	}
	steps = append(steps, step{site: pend})
	for i := 0; i < statusFetchBudget-1; i++ {
		steps = append(steps, step{err: errors.New("blip")})
	}
	steps = append(steps, step{site: siteFresh("https://shop.example.com")})
	c := &scriptClient{steps: steps}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("the budget must reset after an intervening success, got %v", err)
	}
	if !strings.Contains(buf.String(), "✓ live") {
		t.Fatalf("expected to reach fresh after the reset:\n%s", buf.String())
	}
}

// Every other wait test drives the non-TTY branch, so this one covers the
// in-place redraw.
func TestWaitTTYRedrawsInPlace(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	c := &scriptClient{steps: pendingSteps(pend, pend, pend, siteFresh("https://shop.example.com"))}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, true, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\033[") {
		t.Fatalf("TTY path must rewind the transient frame in place:\n%q", out)
	}
	if !strings.Contains(out, "✓ live: https://shop.example.com") {
		t.Fatalf("expected fresh milestone on the TTY path:\n%q", out)
	}
}

// The server sends owed_since with fractional seconds, which the iso() helper
// above never produces. ageSince must parse that form or every tier-2 age would
// silently drop in production while every test still passed.
func TestAgeSinceParsesFractionalSeconds(t *testing.T) {
	now := base.Add(90 * time.Second)
	d, ok := ageSince("2026-07-20T12:00:00.481Z", now)
	if !ok {
		t.Fatal("fractional-second RFC3339 (usec) must parse; production sends it")
	}
	if d < 89*time.Second || d > 90*time.Second {
		t.Fatalf("age = %v, want ~89.5s", d)
	}
}

func TestNonTTYHeadlinePrintedOnce(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second} // stays in tier 0
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	c := &scriptClient{steps: pendingSteps(pend, pend, pend, pend, pend, siteFresh("https://shop.example.com"))}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	if n := strings.Count(buf.String(), "  ⧗ publishing…\n"); n != 1 {
		t.Fatalf("bare headline should print exactly once in non-TTY, got %d:\n%s", n, buf.String())
	}
}

func TestNonTTYTickThrottledToOncePer60s(t *testing.T) {
	var buf bytes.Buffer
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	c := &scriptClient{steps: pendingSteps(pend, pend, pend, pend, pend, siteFresh("https://shop.example.com"))}
	clk := &listClock{times: []time.Time{
		base,                        // start
		base.Add(301 * time.Second), // poll1: tier2, first frame emitted
		base.Add(320 * time.Second), // poll2: +19s, no tick
		base.Add(362 * time.Second), // poll3: +61s since emit, tick
		base.Add(380 * time.Second), // poll4: +18s, no tick
		base.Add(425 * time.Second), // poll5: +63s since emit, tick
		base.Add(425 * time.Second), // poll6: fresh
	}}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, "waiting on the WebAccel cache flush"); n != 1 {
		t.Fatalf("tier-2 target line should emit once on transition, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "still publishing…"); n != 2 {
		t.Fatalf("expected exactly two throttled ticks, got %d:\n%s", n, out)
	}
}

func TestNonTTYDiagnosticReemitsOnlyOnContentChange(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second} // stays in tier 0, well under a tick
	edge := func(reason string) *api.Site {
		s := sitePending(api.PendingTarget{Axis: "origin", OwedSince: iso(base)})
		s.EdgePurgeHealth = "failing"
		s.EdgePurgeErrorReason = reason
		return s
	}
	c := &scriptClient{steps: pendingSteps(edge("http_503"), edge("http_503"), edge("http_500"), siteFresh(""))}
	if err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, "(http_503)"); n != 1 {
		t.Fatalf("unchanged diagnostic must not re-emit each poll, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "(http_500)"); n != 1 {
		t.Fatalf("changed diagnostic must re-emit once, got %d:\n%s", n, out)
	}
}

// A tier-2 target whose owed_since arrives late must re-emit with the age
// rather than stick on the age-less line, so the age's presence belongs in the
// re-emit key even though its value must not.
func TestSigTracksOwedAgePresence(t *testing.T) {
	now := base.Add(6 * time.Minute)
	withAge := buildPendingView(sitePending(cdnTarget("webaccel", "h", iso(base))), 6*time.Minute, now, 0, time.Time{})
	noAge := buildPendingView(sitePending(cdnTarget("webaccel", "h", "")), 6*time.Minute, now, 0, time.Time{})
	if withAge.sig() == noAge.sig() {
		t.Fatalf("owed-age presence must be part of the re-emit key: both %q", withAge.sig())
	}
}

func TestWaitCtrlCViaOptsCtx(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	clk := &fakeClock{t: base, step: time.Second}
	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	c := &scriptClient{steps: pendingSteps(pend), onPoll: func(int) { cancel() }}

	err := wait(c, "site1", DeployMsgs(), &buf, testOpts(clk.Now, false, ctx))
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	assertDetach(t, buf.String(), "The deploy", "kamakiri deploy")
}

func TestWaitCtrlCViaSignalCtxSeam(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	prev := signalCtx
	t.Cleanup(func() { signalCtx = prev })
	signalCtx = func() (context.Context, context.CancelFunc) { return ctx, func() {} }

	pend := sitePending(cdnTarget("webaccel", "shop.example.com", iso(base)))
	c := &scriptClient{steps: pendingSteps(pend), onPoll: func(int) { cancel() }}

	// The cancelled context makes the select fire at once, so driving the public
	// Wait costs no real sleep despite the production schedule.
	err := Wait(c, "site1", RollbackMsgs(), &buf)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	assertDetach(t, buf.String(), "The rollback", "kamakiri rollback")
}

func assertDetach(t *testing.T, out, noun, reRun string) {
	t.Helper()
	if !strings.Contains(out, "Detached. "+noun+" is committed and keeps converging on its own.") {
		t.Errorf("missing detach line 1 for %q:\n%s", noun, out)
	}
	if !strings.Contains(out, "Check kamakiri status, or re-run "+reRun+" to watch again.") {
		t.Errorf("missing detach line 2 for %q:\n%s", reRun, out)
	}
}

func TestPerCommandCopy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		m           msgs
		gerund      string
		fresh       string
		lead        string
		reRun       string
		showLiveURL bool
	}{
		{"deploy", DeployMsgs(), "publishing", "✓ live", "Deployed, but", "kamakiri deploy", true},
		{"rollback", RollbackMsgs(), "rolling back", "✓ live", "Rolled back, but", "kamakiri rollback", true},
		{"purge", PurgeMsgs(), "flushing caches", "✓ caches flushed", "Caches purged, but", "kamakiri cdn purge", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.m
			if m.gerund != tc.gerund || m.fresh != tc.fresh || m.lead != tc.lead ||
				m.reRun != tc.reRun || m.showLiveURL != tc.showLiveURL {
				t.Fatalf("copy mismatch for %s: %+v", tc.name, m)
			}

			if got := headline(m); got != "  ⧗ "+tc.gerund+"…" {
				t.Errorf("headline = %q", got)
			}
			b := FormatBlocker(m.lead, api.Blocker{Provider: "webaccel", Host: "h", Reason: "credentials_rejected"})
			if !strings.HasPrefix(b[0], "✗ "+tc.lead+" the WebAccel cache was not flushed:") {
				t.Errorf("blocked lead = %q", b[0])
			}
			f := &api.Freshness{State: "fresh", LiveURL: "https://shop.example.com"}
			want := tc.fresh
			if tc.showLiveURL {
				want += ": https://shop.example.com"
			}
			if got := freshMilestone(m, f, false, time.Second); got != want {
				t.Errorf("fresh = %q, want %q", got, want)
			}
		})
	}
}

func TestWaitPassesSiteID(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeClock{t: base, step: time.Second}
	c := &scriptClient{steps: pendingSteps(siteFresh(""))}
	_ = wait(c, "site-xyz", DeployMsgs(), &buf, testOpts(clk.Now, false, context.Background()))
	if c.gotID != "site-xyz" {
		t.Fatalf("siteID = %q, want site-xyz", c.gotID)
	}
}

// The fallback arms matter as much as the override: a stray or malformed value
// in a real user's environment must not silently slow, or busy-loop, a wait.
func TestPollScheduleFromEnv(t *testing.T) {
	// These straddle defaultPollSchedule's boundaries, so a fallback is shown to
	// reproduce that schedule rather than any one constant.
	probes := []struct {
		elapsed time.Duration
		def     time.Duration
	}{
		{time.Second, 2 * time.Second},
		{5 * time.Minute, 10 * time.Second},
		{30 * time.Minute, 30 * time.Second},
	}

	t.Run("positive value polls at that fixed interval", func(t *testing.T) {
		t.Setenv(EnvFreshnessPollMS, "200")
		sched := pollScheduleFromEnv()
		for _, p := range probes {
			if got := sched(p.elapsed); got != 200*time.Millisecond {
				t.Errorf("at elapsed=%s: got %s, want 200ms", p.elapsed, got)
			}
		}
	})

	for _, tc := range []struct {
		name, val string
		set       bool
	}{
		{name: "unset", set: false},
		{name: "empty", val: "", set: true},
		{name: "garbage", val: "soon", set: true},
		{name: "zero", val: "0", set: true},
		{name: "negative", val: "-5", set: true},
	} {
		t.Run(tc.name+" falls back to the production schedule", func(t *testing.T) {
			if tc.set {
				t.Setenv(EnvFreshnessPollMS, tc.val)
			} else {
				// t.Setenv restores the prior state at cleanup, so unsetting after
				// it is safe and blocks a real value leaking in from the shell.
				t.Setenv(EnvFreshnessPollMS, "")
				os.Unsetenv(EnvFreshnessPollMS)
			}
			sched := pollScheduleFromEnv()
			for _, p := range probes {
				if got := sched(p.elapsed); got != p.def {
					t.Errorf("at elapsed=%s: got %s, want default %s", p.elapsed, got, p.def)
				}
			}
		})
	}
}
