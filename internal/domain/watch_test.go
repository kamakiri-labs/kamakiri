package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// fakeClock advances by `step` on every read, so elapsed-based copy is
// exercised without real sleeps.
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

// rewind is the escape a cleared transient block writes on a TTY: n lines up,
// then erase everything below.
func rewind(n int) string { return fmt.Sprintf("\033[%dA\033[J", n) }

// transitioningClient walks a script of domain rows, one per status read and
// holding on the last, so a single watch run streams a whole climb.
type transitioningClient struct {
	mu     sync.Mutex
	rows   []api.Domain
	idx    int
	calls  int
	err    error
	siteID string
}

func (c *transitioningClient) ListDomains(siteID string) (*api.DomainList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.siteID = siteID
	if c.err != nil {
		return nil, c.err
	}
	row := c.rows[c.idx]
	if c.idx < len(c.rows)-1 {
		c.idx++
	}
	return &api.DomainList{Domains: []api.Domain{row}}, nil
}

// The watch reads status and nothing else; the rest satisfy the interface.
func (c *transitioningClient) SetCanonical(string, string) (*api.Domain, error) {
	return nil, errors.New("unexpected")
}
func (c *transitioningClient) UnsetCanonical(string) (*api.UnsetCanonicalResult, error) {
	return nil, errors.New("unexpected")
}
func (c *transitioningClient) AddDomain(string, string, string, int) (*api.Domain, error) {
	return nil, errors.New("unexpected")
}
func (c *transitioningClient) RemoveDomain(string) (*api.RemoveDomainResult, error) {
	return nil, errors.New("unexpected")
}
func (c *transitioningClient) WaitForSync(string, int64, time.Duration) (*api.Site, error) {
	return nil, errors.New("unexpected")
}
func (c *transitioningClient) RecheckDomain(string) (*api.Domain, error) {
	return nil, errors.New("the watch reads status only and never triggers a recheck")
}

func (c *transitioningClient) RegisterDomain(string) (*api.Registration, error) { return nil, nil }
func (c *transitioningClient) UnregisterDomain(string) error                    { return nil }
func (c *transitioningClient) ListRegistrations() (*api.RegistrationList, error) {
	return nil, nil
}

func cnameRow(state, observedValue string) api.Domain {
	d := api.Domain{
		Domain: "next.altstack.jp",
		Role:   "canonical",
		State:  state,
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "next.altstack.jp", Type: "cname", Value: "0gbr-1.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
		},
	}
	if observedValue != "" {
		// The verdict is the server's judgment that the observed value matches
		// the current target; the observed entry is that raw value.
		d.DnsVerdict = "correct"
		d.DNSRecordsObserved = []api.DNSRecordObserved{
			{Name: "next.altstack.jp", Type: "cname", Values: []string{observedValue}},
		}
	}
	return d
}

// cnameRowAbsent is a row with no record observed yet, which must stay waiting
// and never reach the DNS milestone.
func cnameRowAbsent() api.Domain {
	d := cnameRow(StateAwaitingDNS, "")
	d.DnsVerdict = "absent"
	return d
}

// cnameRowWrong is an awaiting-DNS row whose observed record does not match the
// target, the actionable wrong-target case the transient frame anchors on.
func cnameRowWrong(observedValue string) api.Domain {
	d := cnameRow(StateAwaitingDNS, observedValue)
	d.DnsVerdict = "present_but_wrong"
	return d
}

// cnameRowUnreachable is a row whose last probe hit a resolver error, so the
// match is unknown and the watch must wait rather than declare either way.
func cnameRowUnreachable() api.Domain {
	d := cnameRow(StateAwaitingDNS, "")
	d.DnsVerdict = "unreachable"
	return d
}

// apexRow is the apex record shape. The phase decision ignores record shape, so
// these cases confirm the shape alone never breaks it.
func apexRow(state string) api.Domain {
	return api.Domain{
		Domain: "altstack.jp",
		Role:   "canonical",
		State:  state,
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "altstack.jp", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "altstack.jp", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
	}
}

// Each case pins which signal does, and does not, advance the milestone, which
// the end-to-end tests can only assert transitively.
func TestDerivePhase(t *testing.T) {
	cases := []struct {
		name string
		row  api.Domain
		want watchPhase
	}{
		{
			name: "absent verdict → waiting",
			row:  cnameRowAbsent(),
			want: phaseWaiting,
		},
		{
			name: "no observation, awaiting_dns → waiting",
			row:  cnameRow(StateAwaitingDNS, ""),
			want: phaseWaiting,
		},
		{
			// The milestone keys on the verdict, so it advances even while the
			// row is still nominally awaiting DNS.
			name: "correct verdict, awaiting_dns → detected",
			row:  cnameRow(StateAwaitingDNS, "0gbr-1.c2.kamakiri-pages.site"),
			want: phaseDetected,
		},
		{
			name: "serving → live",
			row:  cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
			want: phaseLive,
		},
		{
			name: "present_but_wrong verdict → wrong",
			row:  cnameRowWrong("104.21.5.5"),
			want: phaseWrong,
		},
		{
			// The match is unknown, so waiting beats declaring a wrong target.
			name: "unreachable verdict → waiting",
			row:  cnameRowUnreachable(),
			want: phaseWaiting,
		},
		{
			name: "apex/ALIAS awaiting_dns → waiting",
			row:  apexRow(StateAwaitingDNS),
			want: phaseWaiting,
		},
		{
			name: "apex/ALIAS serving → live",
			row:  apexRow(StateServing),
			want: phaseLive,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := c.row
			if got := derivePhase(&row); got != c.want {
				t.Errorf("derivePhase() = %v, want %v", got, c.want)
			}
		})
	}
}

// testOpts drives a watch on a 1ms constant schedule, so nothing using it
// exercises the production backoff; TestPollScheduleBacksOff covers that.
func testOpts(clk *fakeClock, ctx context.Context) watchOpts {
	return watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return time.Millisecond },
		now:          clk.Now,
		isTTY:        false,
		ctx:          ctx,
	}
}

func TestWatchStreamsWaitingDetectedLive(t *testing.T) {
	// A step over the 30s floor, so the milestones carry real durations rather
	// than correctly emitting the bare line.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 31 * time.Second}
	// The observed values are dot-less and the expected one is a dotted FQDN,
	// which is what a real resolver and a real server produce.
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),                               // waiting
		cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site"), // detected
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),      // live
	}}

	var out bytes.Buffer
	err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background()))
	if err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"waiting for your DNS",
		"✓ DNS propagated (~",
		"provisioning SSL certificate",
		"✓ SSL certificate provisioned (~",
		"✓ live: https://next.altstack.jp",
		"DNS might not have propagated close to you",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	for _, banned := range []string{
		"sub-minute", "—", "that part's not on us", "Ready ~",
		"DNS detected", "caching old DNS", "✓ live —",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("output must not contain %q:\n%s", banned, s)
		}
	}
}

// The watch stays in the waiting tick while nothing matching the current target
// has been observed, and commits the milestone only on a correct verdict. A
// stale observation reads as not-correct, so it can never advance the milestone.
func TestWatchStaysWaitingUntilVerdictCorrect(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRowAbsent(), // no record observed yet
		cnameRowAbsent(), // still absent
		cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site"), // correct verdict → detected
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),      // live
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "waiting for your DNS") {
		t.Errorf("must stay in the honest waiting tick while the verdict is absent:\n%s", s)
	}
	if !strings.Contains(s, "✓ DNS propagated") {
		t.Errorf("must commit the DNS milestone once the verdict is correct:\n%s", s)
	}
	// Ordering proves the absent frames did not advance the phase early.
	if strings.Index(s, "waiting for your DNS") > strings.Index(s, "✓ DNS propagated") {
		t.Errorf("premature ✓ DNS propagated before the correct verdict:\n%s", s)
	}
}

// A correct verdict advances the milestone even on a frame where the row is
// still nominally awaiting DNS: the milestone keys on the verdict, not State.
func TestWatchDNSPropagatedOnCorrectVerdictWhileAwaitingDNS(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRowAbsent(), // no record observed yet
		cnameRow(StateAwaitingDNS, "0gbr-1.c2.kamakiri-pages.site"), // awaiting_dns but verdict correct
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),     // live
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"waiting for your DNS",
		"✓ DNS propagated",
		"provisioning SSL certificate",
		"✓ live: https://next.altstack.jp",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

// While the site has no deploy the watch shows the deploy-first line, never the
// cert spinner, and keeps blocking; once the deploy lands it converges to live
// rather than having exited early.
func TestWatchNoDeployShowsDeployFirstThenConverges(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}

	noDeploy := cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site")
	noDeploy.HasLiveDeploy = boolPtr(false)

	live := cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site")
	live.HasLiveDeploy = boolPtr(true)

	client := &transitioningClient{rows: []api.Domain{
		noDeploy, // DNS correct but no deploy
		noDeploy, // still no deploy, so the wait keeps blocking
		live,
	}}

	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "no published content yet") {
		t.Errorf("expected the deploy-first line while the site has no live deploy:\n%s", s)
	}
	// This wait blocks indefinitely, so a frozen line would look hung.
	if !strings.Contains(s, "still monitoring") {
		t.Errorf("deploy-first frame must carry a liveness tick:\n%s", s)
	}
	// The block is the missing deploy, not issuance.
	if strings.Contains(s, "provisioning SSL certificate") {
		t.Errorf("no-deploy watch must not show the cert spinner:\n%s", s)
	}
	if !strings.Contains(s, "✓ live: https://next.altstack.jp") {
		t.Errorf("watch must converge to live once the deploy lands:\n%s", s)
	}
	// Ordering proves the wait blocked and then converged, rather than exiting.
	if strings.Index(s, "no published content yet") > strings.Index(s, "✓ live") {
		t.Errorf("deploy-first line must precede live:\n%s", s)
	}
}

// A deploy-less wait is never charged to certificate issuance: with no observed
// cert phase the terminal line carries no duration at all, and with one it
// measures only that phase. The clock step clears the 30s floor, so a regression
// that anchored the cert clock early would surface a duration here.
func TestWatchNoDeployTerminalDurationIsHonest(t *testing.T) {
	t.Run("jump straight to serving: no fabricated duration", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 31 * time.Second}

		noDeploy := cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site")
		noDeploy.HasLiveDeploy = boolPtr(false)
		live := cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site")
		live.HasLiveDeploy = boolPtr(true)

		client := &transitioningClient{rows: []api.Domain{noDeploy, noDeploy, live}}
		var out bytes.Buffer
		if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
			t.Fatalf("watchToLive() error = %v", err)
		}
		s := out.String()
		if !strings.Contains(s, "✓ SSL certificate provisioned") {
			t.Fatalf("expected the terminal SSL milestone:\n%s", s)
		}
		// No cert phase was ever observed, so the milestone must be bare.
		if strings.Contains(s, "SSL certificate provisioned (~") {
			t.Errorf("terminal SSL line must not carry a duration for a no-deploy wait:\n%s", s)
		}
	})

	t.Run("through awaiting_cert: duration measures only the observed cert phase", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 31 * time.Second}

		noDeploy := cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site")
		noDeploy.HasLiveDeploy = boolPtr(false)
		certPhase := cnameRow(StateAwaitingCert, "0gbr-1.c2.kamakiri-pages.site")
		certPhase.HasLiveDeploy = boolPtr(true)
		live := cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site")
		live.HasLiveDeploy = boolPtr(true)

		// The cert clock re-anchors when the deploy lands, so the observed cert
		// phase is one step rather than the two since DNS went correct. An early
		// anchor would report ~1m here instead of ~31s.
		client := &transitioningClient{rows: []api.Domain{noDeploy, certPhase, live}}
		var out bytes.Buffer
		if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
			t.Fatalf("watchToLive() error = %v", err)
		}
		s := out.String()
		if !strings.Contains(s, "SSL certificate provisioned (~31s)") {
			t.Errorf("cert duration must measure only the observed cert phase (~31s), not the deploy-less wait:\n%s", s)
		}
	})
}

// Each cert tier must state only what was observed. A cause is named only where
// it was measured, so the copy never asserts a rate limit or an authority we
// never saw.
func TestCertSlowCopyIsNeutralNeverFabricatesCause(t *testing.T) {
	d := &api.Domain{Domain: "next.altstack.jp", Role: "canonical", State: StateAwaitingEdge}

	steady := transientStatus(d, phaseDetected, "", false, false)
	if len(steady) != 1 || !strings.Contains(steady[0], "provisioning SSL certificate") {
		t.Fatalf("steady cert line malformed: %v", steady)
	}
	if strings.Contains(steady[0], "still working") || strings.Contains(steady[0], "taking longer") {
		t.Errorf("steady cert line must be the bare provisioning line:\n%s", steady[0])
	}

	slow := transientStatus(d, phaseDetected, "", true, false)
	if len(slow) != 1 {
		t.Fatalf("slow cert status must be a single line, got %d: %v", len(slow), slow)
	}
	if !strings.Contains(slow[0], "provisioning SSL certificate") ||
		!strings.Contains(slow[0], "still working") {
		t.Errorf("slow cert line must be the 'still working' reassurance:\n%s", slow[0])
	}
	if strings.Contains(slow[0], "taking longer") {
		t.Errorf("slow (not very-slow) must NOT escalate to 'taking longer':\n%s", slow[0])
	}

	verySlow := transientStatus(d, phaseDetected, "", true, true)
	if len(verySlow) != 1 ||
		!strings.Contains(verySlow[0], "taking longer than expected") {
		t.Errorf("very-slow cert line must escalate to 'taking longer than expected':\n%s", verySlow[0])
	}

	for _, banned := range []string{
		"rate-limiting", "rate limiting", "certificate authority",
		"automatic, no action needed", "taking longer than usual", "—",
	} {
		for _, line := range []string{slow[0], verySlow[0]} {
			if strings.Contains(line, banned) {
				t.Errorf("cert line must not contain fabricated/defensive copy %q:\n%s", banned, line)
			}
		}
	}
}

// The loop itself walks both escalations, which the tier tests assert only on
// the renderer.
func TestWatchCertSlowRendersNeutralNoticeThroughLoop(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 90 * time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site"), // cert clock anchors here
		cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site"), // +90s, slow
		cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site"), // +180s, very slow
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "provisioning SSL certificate… still working") {
		t.Errorf("slow (60s+) frame must render the 'still working' reassurance:\n%s", s)
	}
	if !strings.Contains(s, "provisioning SSL certificate… taking longer than expected") {
		t.Errorf("very-slow (180s+) frame must escalate to 'taking longer than expected':\n%s", s)
	}
	for _, banned := range []string{
		"rate-limiting", "certificate authority", "automatic, no action needed",
		"taking longer than usual", "—",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("fabricated cause %q must never appear:\n%s", banned, s)
		}
	}
	if !strings.Contains(s, "✓ live: https://next.altstack.jp") {
		t.Errorf("must still reach live:\n%s", s)
	}
}

// A re-run that finds the site already serving goes straight to the milestone
// block, with no transient line and no duration: nothing was watched, so no
// duration would be honest.
func TestWatchAlreadyLiveFastPath(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"✓ DNS propagated\n",
		"✓ SSL certificate provisioned\n",
		"✓ live: https://next.altstack.jp",
		"DNS might not have propagated close to you",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("already-ready output missing %q:\n%s", want, s)
		}
	}
	for _, banned := range []string{
		"DNS propagated (~", "SSL certificate provisioned (~",
		"waiting for your DNS", "provisioning SSL certificate",
		"—", "that part's not on us",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("already-ready output must not contain %q:\n%s", banned, s)
		}
	}
}

func TestWatchDetectedButWrongCloudflareProxy(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	// A proxy address in place of the CNAME target.
	client := &transitioningClient{rows: []api.Domain{
		cnameRowWrong("104.21.5.5"),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}

	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"found a record, but it points at the wrong target",
		"expected:",
		"0gbr-1.c2.kamakiri-pages.site.",
		"got:",
		"104.21.5.5",
		"Cloudflare proxy",
		"orange cloud",
		", please update",
		"still monitoring",
		"waiting for your DNS",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// The frame must not tell the user to run anything: the watch is still
	// going and picks up the corrected record itself.
	if strings.Contains(s, "Fix it, then") {
		t.Errorf("wrong-target frame must not tell the user to run a command:\n%s", s)
	}
}

func TestWatchDetectedButWrongARecordInsteadOfCNAME(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRowWrong("203.0.113.9"), // a plain address, not a known proxy range
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "A record") || !strings.Contains(s, "CNAME") {
		t.Errorf("expected A-instead-of-CNAME diagnosis:\n%s", s)
	}
}

func TestWatchDetectedButWrongAppendedZone(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	// The target was entered without a trailing dot and the registrar appended
	// the zone to it.
	client := &transitioningClient{rows: []api.Domain{
		cnameRowWrong("0gbr-1.c2.kamakiri-pages.site.next.altstack.jp"),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "zone name was appended") {
		t.Errorf("expected appended-zone diagnosis:\n%s", s)
	}
}

func TestWatchExitMatrixLive(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site")}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("live must return nil (exit 0), got %v", err)
	}
}

// The watch never gives up on a timer, so an interrupt is the only way the user
// can stop it.
func TestWatchExitMatrixCtrlC(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{cnameRow(StateAwaitingDNS, "")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the first select
	var out bytes.Buffer
	err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, ctx))
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("want ErrWatchInterrupted (→ exit 130), got %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"Stopped watching",
		"kamakiri domain verify next.altstack.jp",
		"kamakiri status",
		"--no-wait",
		"We keep checking automatically in the background",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Ctrl-C detach copy missing %q:\n%s", want, s)
		}
	}
	// The fast path is named first.
	if strings.Index(s, "domain verify") > strings.Index(s, "kamakiri status") {
		t.Errorf("`domain verify` must precede `status` in detach copy:\n%s", s)
	}
	// The watch never gives up on a timer, so any copy promising a cutoff would
	// be false. User-facing output also carries no em-dash.
	for _, banned := range []string{"~30 min", "not waiting further", "Still no DNS", "after 2h", "—"} {
		if strings.Contains(s, banned) {
			t.Errorf("stale/banned detach copy %q must be gone:\n%s", banned, s)
		}
	}
}

func TestWatchExitMatrixTransportError(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{
		rows: []api.Domain{cnameRow(StateAwaitingDNS, "")},
		err:  errors.New("connection refused"),
	}
	var out bytes.Buffer
	err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background()))
	if err == nil {
		t.Fatal("transport error must return non-nil (→ non-zero exit)")
	}
	if errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("transport error must not masquerade as interrupt: %v", err)
	}
}

// The records block is printed once up front, so an apex domain gets the same
// compact waiting tick as any other and no frame re-renders the table.
func TestWatchApexAliasShowsCompactWaitingLineNoTableReRender(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	apex := api.Domain{
		Domain: "altstack.jp",
		Role:   "canonical",
		State:  StateAwaitingDNS,
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "altstack.jp", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "altstack.jp", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
	}
	live := apex
	live.State = StateServing
	client := &transitioningClient{rows: []api.Domain{apex, live}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "waiting for your DNS") {
		t.Errorf("apex/ALIAS should show the compact waiting heartbeat:\n%s", s)
	}
	if !strings.Contains(s, "✓ live: https://altstack.jp") {
		t.Errorf("apex/ALIAS must still terminate at ✓ live:\n%s", s)
	}
	for _, banned := range []string{"Configure your DNS:", "ALIAS", "A records not supported"} {
		if strings.Contains(s, banned) {
			t.Errorf("watch must not re-render the DNS record block (found %q):\n%s", banned, s)
		}
	}
	if strings.Contains(s, "found a record, but it points at the wrong target") {
		t.Errorf("apex/ALIAS must not show the CNAME wrong-target branch:\n%s", s)
	}
}

func TestSetNonTTYBehavesAsNoWait(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{
				Domain:        "next.altstack.jp",
				Role:          "canonical",
				SyncAttemptID: 1,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "0gbr-1.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			t.Fatal("non-TTY must behave as --no-wait: no WaitForSync, no watch")
			return nil, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			t.Fatal("non-TTY must not enter the watch (no ListDomains poll)")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := Set(client, "next.altstack.jp", false, &out); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "queued") {
		t.Errorf("non-TTY output should be the queued/resume line:\n%s", s)
	}
	if !strings.Contains(s, "kamakiri domain verify next.altstack.jp") {
		t.Errorf("non-TTY output should carry the resume hint (verify first):\n%s", s)
	}
	if !strings.Contains(s, "Configure your DNS:") {
		t.Errorf("non-TTY output should still print the DNS record:\n%s", s)
	}
	if strings.Contains(s, "\033[") {
		t.Errorf("non-TTY output must not contain ANSI escapes:\n%q", s)
	}
}

// The interactive default is one top-to-bottom narrative. The record precedes
// the our-side line, because the user needs it at their registrar before any
// blocking wait of ours.
func TestSetInteractiveStreamsToLive(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var syncWaited bool
	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{
				Domain: "next.altstack.jp", Role: "canonical", SyncAttemptID: 1,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "0gbr-1.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			syncWaited = true
			return &api.Site{Sync: &api.Sync{LatestAttemptID: 1, Outcome: "ok"}}, nil
		},
	}

	prev := watchToLiveFn
	t.Cleanup(func() { watchToLiveFn = prev })
	tc := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	watchToLiveFn = func(ctx context.Context, _ APIClient, siteID, domainName string, out io.Writer) error {
		return watchToLive(tc, siteID, domainName, out, testOpts(clk, ctx))
	}

	var out bytes.Buffer
	if err := set(client, "next.altstack.jp", true, &out); err != nil {
		t.Fatalf("set(interactive) error = %v", err)
	}
	if !syncWaited {
		t.Error("interactive set must still wait for the pre-watch sync")
	}
	s := out.String()
	idxConfirm := strings.Index(s, "✓ Canonical domain set: next.altstack.jp")
	idxRecord := strings.Index(s, "Configure your DNS:")
	idxResume := strings.Index(s, "Safe to close any time")
	idxOurSide := strings.Index(s, "linking your domain on our side")
	idxLive := strings.Index(s, "✓ live: https://next.altstack.jp")
	if idxConfirm < 0 || idxRecord < 0 || idxResume < 0 || idxOurSide < 0 || idxLive < 0 {
		t.Fatalf("missing a required section:\n%s", s)
	}
	if !(idxConfirm < idxRecord && idxRecord < idxResume && idxResume < idxOurSide && idxOurSide < idxLive) {
		t.Errorf("ordering must be confirmation → record → resume → our-side → live:\n%s", s)
	}
	// The record block must appear exactly once: the watch's first frame must
	// not re-render it.
	if strings.Count(s, "Configure your DNS:") != 1 {
		t.Errorf("DNS record block must appear exactly once, got %d:\n%s",
			strings.Count(s, "Configure your DNS:"), s)
	}
}

// An interrupt during the pre-watch wait must print the same detach copy as the
// watch and return ErrWatchInterrupted, never a bare exit code with no copy.
func TestSetSIGINTDuringPreWatchSyncPrintsDetachCopyAndInterrupts(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the wait starts

	prevSig := signalCtx
	t.Cleanup(func() { signalCtx = prevSig })
	signalCtx = func() (context.Context, context.CancelFunc) {
		return ctx, func() {}
	}

	syncReleased := make(chan struct{})
	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{
				Domain: "next.altstack.jp", Role: "canonical", SyncAttemptID: 1,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "0gbr-1.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			// Blocking until teardown forces the select onto the cancelled arm,
			// modelling a long wait the user interrupts.
			<-syncReleased
			return &api.Site{}, nil
		},
	}
	t.Cleanup(func() { close(syncReleased) })

	prev := watchToLiveFn
	t.Cleanup(func() { watchToLiveFn = prev })
	watchToLiveFn = func(context.Context, APIClient, string, string, io.Writer) error {
		t.Fatal("watch must not start: SIGINT fired during the pre-watch sync")
		return nil
	}

	var out bytes.Buffer
	err := set(client, "next.altstack.jp", true, &out)
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("want ErrWatchInterrupted (→ exit 130), got %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"Configure your DNS:",
		"Stopped watching",
		"kamakiri domain verify next.altstack.jp",
		"kamakiri status",
		"--no-wait",
		"We keep checking automatically in the background",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SIGINT-during-pre-watch-sync output missing %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "Configure your DNS:") > strings.Index(s, "Stopped watching") {
		t.Errorf("record must be printed before the detach copy:\n%s", s)
	}
}

// Adding a domain streams the same narrative in the same order as setting one.
func TestAddInteractiveStreamsToLive(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var syncWaited bool
	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return &api.Domain{
				Domain: "next.altstack.jp", Role: "redirect", RedirectStatus: 301, SyncAttemptID: 1,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "next.altstack.jp", Type: "cname", Value: "0gbr-1.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			syncWaited = true
			return &api.Site{Sync: &api.Sync{LatestAttemptID: 1, Outcome: "ok"}}, nil
		},
	}

	prev := watchToLiveFn
	t.Cleanup(func() { watchToLiveFn = prev })
	tc := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	watchToLiveFn = func(ctx context.Context, _ APIClient, siteID, domainName string, out io.Writer) error {
		return watchToLive(tc, siteID, domainName, out, testOpts(clk, ctx))
	}

	var out bytes.Buffer
	if err := add(client, "next.altstack.jp", "redirect", 301, true, &out); err != nil {
		t.Fatalf("add(interactive) error = %v", err)
	}
	if !syncWaited {
		t.Error("interactive add must wait for the pre-watch sync")
	}
	s := out.String()
	idxConfirm := strings.Index(s, "✓ Domain added: next.altstack.jp (redirect 301)")
	idxRecord := strings.Index(s, "Configure your DNS:")
	idxResume := strings.Index(s, "Safe to close any time")
	idxOurSide := strings.Index(s, "linking your domain on our side")
	idxLive := strings.Index(s, "✓ live: https://next.altstack.jp")
	if idxConfirm < 0 || idxRecord < 0 || idxResume < 0 || idxOurSide < 0 || idxLive < 0 {
		t.Fatalf("missing a required section:\n%s", s)
	}
	if !(idxConfirm < idxRecord && idxRecord < idxResume && idxResume < idxOurSide && idxOurSide < idxLive) {
		t.Errorf("ordering must be confirmation → record → resume → our-side → live:\n%s", s)
	}
	if strings.Count(s, "Configure your DNS:") != 1 {
		t.Errorf("DNS record block must appear exactly once:\n%s", s)
	}
}

// Adding a domain shares the interactive region, so an interrupt during the
// pre-watch wait must behave the same, record printed first.
func TestAddSIGINTDuringPreWatchSyncPrintsDetachCopyAndInterrupts(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prevSig := signalCtx
	t.Cleanup(func() { signalCtx = prevSig })
	signalCtx = func() (context.Context, context.CancelFunc) { return ctx, func() {} }

	syncReleased := make(chan struct{})
	t.Cleanup(func() { close(syncReleased) })
	client := &mockClient{
		addDomainFn: func(_, _, _ string, _ int) (*api.Domain, error) {
			return &api.Domain{
				Domain: "www.example.com", Role: "redirect", RedirectStatus: 301, SyncAttemptID: 1,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "site123.c1.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			<-syncReleased
			return &api.Site{}, nil
		},
	}

	prev := watchToLiveFn
	t.Cleanup(func() { watchToLiveFn = prev })
	watchToLiveFn = func(context.Context, APIClient, string, string, io.Writer) error {
		t.Fatal("watch must not start: SIGINT fired during the pre-watch sync")
		return nil
	}

	var out bytes.Buffer
	err := add(client, "www.example.com", "redirect", 301, true, &out)
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("want ErrWatchInterrupted (→ exit 130), got %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"Configure your DNS:",
		"Stopped watching",
		"kamakiri domain verify www.example.com",
		"kamakiri status",
		"--no-wait",
		"We keep checking automatically in the background",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SIGINT-during-pre-watch-sync output missing %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "Configure your DNS:") > strings.Index(s, "Stopped watching") {
		t.Errorf("record must be printed before the detach copy:\n%s", s)
	}
}

// A teardown renders the our-side line alone: nothing for the user to configure
// and nothing to watch, since finishing the teardown is the terminal state.
func TestUnsetRemoveInteractiveTeardownOurSideOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	noWatch := func(string) (*api.DomainList, error) {
		t.Fatal("teardown must NOT enter the go-live watch (no ListDomains)")
		return nil, nil
	}

	t.Run("unset", func(t *testing.T) {
		client := &mockClient{
			unsetCanonicalFn: func(string) (*api.UnsetCanonicalResult, error) {
				return &api.UnsetCanonicalResult{SyncAttemptID: 7}, nil
			},
			listDomainsFn: noWatch,
		}
		var out bytes.Buffer
		if err := unset(client, false, true, &out); err != nil {
			t.Fatalf("unset(interactive) error = %v", err)
		}
		s := out.String()
		if !strings.Contains(s, "⧗ removing your domain on our side") ||
			!strings.Contains(s, "✓ domain removed on our side") {
			t.Errorf("want the removing→removed our-side lines:\n%s", s)
		}
		if strings.Contains(s, "Configure your DNS:") {
			t.Errorf("teardown must not print a DNS block:\n%s", s)
		}
	})

	t.Run("remove", func(t *testing.T) {
		client := &mockClient{
			removeDomainFn: func(string) (*api.RemoveDomainResult, error) {
				return &api.RemoveDomainResult{SiteID: "site123", SyncAttemptID: 9}, nil
			},
			listDomainsFn: noWatch,
		}
		var out bytes.Buffer
		if err := remove(client, "www.example.com", false, true, &out); err != nil {
			t.Fatalf("remove(interactive) error = %v", err)
		}
		if !strings.Contains(out.String(), "✓ domain removed on our side") {
			t.Errorf("want the removed our-side line:\n%s", out.String())
		}
	})
}

// An interrupt during a teardown gets the teardown stop copy, not the go-live
// detach copy: there is no record to add and no watch to resume.
func TestUnsetInteractiveSIGINTStopsWithTeardownCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prevSig := signalCtx
	t.Cleanup(func() { signalCtx = prevSig })
	signalCtx = func() (context.Context, context.CancelFunc) { return ctx, func() {} }

	syncReleased := make(chan struct{})
	t.Cleanup(func() { close(syncReleased) })
	client := &mockClient{
		unsetCanonicalFn: func(string) (*api.UnsetCanonicalResult, error) {
			return &api.UnsetCanonicalResult{SyncAttemptID: 7}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			<-syncReleased
			return &api.Site{}, nil
		},
	}

	var out bytes.Buffer
	err := unset(client, false, true, &out)
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("want ErrWatchInterrupted (→ exit 130), got %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "teardown continues server-side") ||
		!strings.Contains(s, "kamakiri status") {
		t.Errorf("want the teardown stop copy:\n%s", s)
	}
	if strings.Contains(s, "Stopped watching") {
		t.Errorf("teardown must NOT use the set/add go-live detach copy:\n%s", s)
	}
}

func TestSetExplicitNoWaitUnchanged(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCanonicalFn: func(_, _ string) (*api.Domain, error) {
			return &api.Domain{Domain: "x.example.com", Role: "canonical", SyncAttemptID: 1}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			t.Fatal("--no-wait must not WaitForSync")
			return nil, nil
		},
	}
	var out bytes.Buffer
	if err := Set(client, "x.example.com", true, &out); err != nil {
		t.Fatalf("Set(noWait=true) error = %v", err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("--no-wait should still print the queued line:\n%s", out.String())
	}
}

// The cadence backs off in three tiers and never escalates back or stops.
func TestPollScheduleBacksOff(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    time.Duration
	}{
		{0, pollFast},
		{5 * time.Minute, pollFast},
		{pollFastUntil - time.Second, pollFast},
		{pollFastUntil, pollMedium},
		{20 * time.Minute, pollMedium},
		{pollMediumUntil - time.Second, pollMedium},
		{pollMediumUntil, pollSlow},
		{3 * time.Hour, pollSlow},
		{72 * time.Hour, pollSlow},
	}
	for _, c := range cases {
		if got := defaultPollSchedule(c.elapsed); got != c.want {
			t.Errorf("defaultPollSchedule(%s) = %s, want %s", c.elapsed, got, c.want)
		}
	}
}

// The waiting line states the elapsed and nothing else: no poll cadence, no
// repeat of the resume contract, no blaming an old host's TTL, no em-dash.
func TestWatchWaitingCopyHonest(t *testing.T) {
	lines := transientStatus(
		&api.Domain{Domain: "next.altstack.jp", Role: "canonical", State: StateAwaitingDNS},
		phaseWaiting, "under a minute", false, false,
	)
	if len(lines) != 1 {
		t.Fatalf("waiting status must be a single line, got %d: %v", len(lines), lines)
	}
	s := lines[0]
	for _, want := range []string{
		"waiting for your DNS",
		"still monitoring",
		"under a minute elapsed",
		"DNS changes may take time to propagate",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("liveness copy must contain %q:\n%s", want, s)
		}
	}
	for _, banned := range []string{
		"usually quick", "next ~", "checked ", " ago)",
		"Safe to leave running", "your old host's TTL", "—",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("waiting copy must not contain %q:\n%s", banned, s)
		}
	}
}

// recordingSchedule captures the elapsed the loop feeds the schedule each
// iteration, proving it threads real elapsed time rather than a constant, while
// returning a cadence short enough never to block the test.
func recordingSchedule(rec *[]time.Duration) func(time.Duration) time.Duration {
	return func(elapsed time.Duration) time.Duration {
		*rec = append(*rec, elapsed)
		return time.Millisecond
	}
}

// Driving elapsed hours past any plausible cutoff with DNS still missing, then
// letting it go live, proves the loop never detaches on its own.
func TestWatchNeverGivesUp(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 30 * time.Minute}
	var elapsedSeen []time.Duration
	opts := testOpts(clk, context.Background())
	opts.pollSchedule = recordingSchedule(&elapsedSeen)

	rows := make([]api.Domain, 0, 7)
	for i := 0; i < 6; i++ {
		rows = append(rows, cnameRow(StateAwaitingDNS, ""))
	}
	rows = append(rows, cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"))
	var out bytes.Buffer
	if err := watchToLive(newClient(rows), "s", "next.altstack.jp", &out, opts); err != nil {
		t.Fatalf("must reach live, never give up: %v", err)
	}

	var maxElapsed time.Duration
	for _, e := range elapsedSeen {
		if e > maxElapsed {
			maxElapsed = e
		}
	}
	// Without this the test could pass vacuously, having never polled far
	// enough for a cutoff to have fired.
	if maxElapsed <= 2*time.Hour {
		t.Fatalf("test vacuous: never polled past the old 2h cutoff (max elapsed %s)", maxElapsed)
	}

	s := out.String()
	if strings.Contains(s, "Stopped watching") || strings.Contains(s, "Still no DNS") {
		t.Errorf("the watch must NEVER self-detach (no timeout):\n%s", s)
	}
	if !strings.Contains(s, "✓ live: https://next.altstack.jp") {
		t.Errorf("must still terminate at ✓ live:\n%s", s)
	}
}

// The elapsed fed to the schedule must climb monotonically across all three
// tiers within one watch, which the pure-function test cannot show.
func TestWatchHeartbeatBacksOffAcrossBoundary(t *testing.T) {
	// The slow tier is reached only because the schedule runs at the top of each
	// iteration, before the phase switch, so the final serving iteration is
	// recorded too. Moving that call below the switch makes the slow tier
	// silently unreachable here.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 4 * time.Minute}
	var elapsedSeen []time.Duration
	opts := testOpts(clk, context.Background())
	opts.pollSchedule = recordingSchedule(&elapsedSeen)

	rows := make([]api.Domain, 0, 8)
	for i := 0; i < 7; i++ {
		rows = append(rows, cnameRow(StateAwaitingDNS, ""))
	}
	rows = append(rows, cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"))
	var out bytes.Buffer
	if err := watchToLive(newClient(rows), "s", "next.altstack.jp", &out, opts); err != nil {
		t.Fatalf("watchToLive() = %v", err)
	}

	if len(elapsedSeen) < 2 {
		t.Fatalf("expected multiple poll iterations, got %d", len(elapsedSeen))
	}
	for i := 1; i < len(elapsedSeen); i++ {
		if elapsedSeen[i] < elapsedSeen[i-1] {
			t.Fatalf("elapsed fed to schedule must be monotonic: %v", elapsedSeen)
		}
	}
	var sawFast, sawMedium, sawSlow bool
	for _, e := range elapsedSeen {
		switch defaultPollSchedule(e) {
		case pollFast:
			sawFast = true
		case pollMedium:
			sawMedium = true
		case pollSlow:
			sawSlow = true
		}
	}
	if !sawFast || !sawMedium || !sawSlow {
		t.Fatalf("loop did not sample elapsed across all 3 cadence tiers: %v", elapsedSeen)
	}
}

// newClient is named to stay clear of the `client` local this file uses
// everywhere.
func newClient(rows []api.Domain) *transitioningClient {
	return &transitioningClient{rows: rows}
}

// The watch copy is rendered verbatim, in wording that blames nobody and
// carries no em-dash.
func TestWatchCopyIsLiteralAndIntact(t *testing.T) {
	// Wrong-target then live exercises that branch and the milestone block in
	// one run.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRowWrong("104.21.5.5"),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"⚠ found a record, but it points at the wrong target, please update",
		"expected:",
		"got:",
		"looks like a Cloudflare proxy; turn off the orange cloud",
		"⧗ waiting for your DNS… still monitoring (",
		"✓ DNS propagated",
		"✓ SSL certificate provisioned",
		"✓ live: https://next.altstack.jp",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("literal copy missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "—") || strings.Contains(s, "after your DNS appeared") ||
		strings.Contains(s, "that part's not on us") || strings.Contains(s, "Fix it, then") {
		t.Errorf("removed defensive/em-dash/injunction copy must be gone:\n%s", s)
	}
}

// cnameRowCertProvisioning is a row whose route is on the edge but whose
// certificate has not been observed serving yet.
func cnameRowCertProvisioning(observedValue string) api.Domain {
	d := cnameRow(StateAwaitingCert, observedValue)
	d.CertRequired = true
	return d
}

// State decides the cert phase ahead of the DNS verdict: this row's record
// already reads correct, and it must still derive as waiting on its
// certificate.
func TestDerivePhaseRoutesAwaitingCert(t *testing.T) {
	d := cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site")
	if got := derivePhase(&d); got != phaseCertProvisioning {
		t.Fatalf("derivePhase(awaiting_cert) = %v, want phaseCertProvisioning", got)
	}
}

// The happy path where the poll cadence catches the cert phase. The step clears
// the 30s floor, so the milestone carries a real duration.
func TestWatchStreamsDnsCertLiveFromAwaitingCert(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 31 * time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),
		cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site"),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"waiting for your DNS",
		"✓ DNS propagated",
		"provisioning SSL certificate",
		"✓ SSL certificate provisioned (~",
		"✓ live: https://next.altstack.jp",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "✓ DNS propagated") > strings.Index(s, "✓ SSL certificate provisioned") {
		t.Errorf("milestone ordering broken:\n%s", s)
	}
}

// The full climb, where the watch sees every state on the way. The DNS
// milestone must commit exactly once across the two phases that follow it.
func TestWatchStreamsFullFourStatePath(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 31 * time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),
		cnameRow(StateAwaitingEdge, "0gbr-1.c2.kamakiri-pages.site"),
		cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site"),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "✓ DNS propagated") {
		t.Fatalf("DNS milestone missing:\n%s", s)
	}
	if !strings.Contains(s, "✓ SSL certificate provisioned") {
		t.Fatalf("SSL milestone missing:\n%s", s)
	}
	if !strings.Contains(s, "✓ live: https://next.altstack.jp") {
		t.Fatalf("live line missing:\n%s", s)
	}
	if n := strings.Count(s, "✓ DNS propagated"); n != 1 {
		t.Errorf("DNS milestone must commit exactly once, got %d:\n%s", n, s)
	}
}

// A cert phase observed for under 30s must emit the bare milestone rather than
// round up to a fabricated duration.
func TestWatchCertMilestoneBareLineOnSubThirtySecondObservation(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 5 * time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),
		cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site"),
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "✓ SSL certificate provisioned") {
		t.Fatalf("expected SSL milestone:\n%s", s)
	}
	if strings.Contains(s, "✓ SSL certificate provisioned (~") {
		t.Errorf("sub-30s cert observation must emit bare line, not a fabricated duration:\n%s", s)
	}
}

// A cert phase that drags walks both escalations. The cert clock anchors on the
// first correct-DNS frame, so the tiers ramp from there rather than from the
// start of the watch.
func TestWatchCertSlowFiresOnExtendedCertCell(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 100 * time.Second}
	client := &transitioningClient{rows: []api.Domain{
		cnameRow(StateAwaitingDNS, ""),
		cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site"), // cert +0s, bare
		cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site"), // cert +100s, slow
		cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site"), // cert +200s, very slow
		cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site"),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("watchToLive() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "still working") {
		t.Errorf("expected 'still working' copy at 60s+ on awaiting_cert:\n%s", s)
	}
	if !strings.Contains(s, "taking longer than expected") {
		t.Errorf("expected 'taking longer than expected' escalation at 180s+:\n%s", s)
	}
}

// A go-live under a CDN must not claim a certificate we did not issue.
func TestWatchTerminalCopyBranchesOnCdnMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, want string
	}{
		{"cloudflare", CdnModeCloudflare, "✓ routed via Cloudflare"},
		{"webaccel", CdnModeWebAccel, "✓ routed via WebAccel"},
		{"none (direct)", CdnModeNone, "✓ SSL certificate provisioned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
			row := cnameRow(StateServing, "0gbr-1.c2.kamakiri-pages.site")
			row.CdnMode = tc.mode
			client := &transitioningClient{rows: []api.Domain{row}}
			var out bytes.Buffer
			if err := watchToLive(client, "site123", "next.altstack.jp", &out, testOpts(clk, context.Background())); err != nil {
				t.Fatalf("watchToLive() error = %v", err)
			}
			s := out.String()
			if !strings.Contains(s, tc.want) {
				t.Errorf("expected terminal line %q for mode %q:\n%s", tc.want, tc.mode, s)
			}
		})
	}
}

// Only past the very-slow threshold does an observed category reach the line;
// below it the reassurance copy stands.
func TestTransientStatusNamesCertCategoryPastVerySlow(t *testing.T) {
	d := cnameRowCertProvisioning("0gbr-1.c2.kamakiri-pages.site")
	d.CertError = CertErrorCertUnavailable

	lines := transientStatus(&d, phaseCertProvisioning, "", true, true)
	if len(lines) != 1 || !strings.Contains(lines[0], "our edge reports no certificate yet") {
		t.Errorf("verySlow+cert_unavailable must name the observed category:\n%v", lines)
	}
	for _, banned := range []string{
		"Let's Encrypt", "rate-limit", "rate limited",
		"your registrar", "your old host", "—",
	} {
		if strings.Contains(lines[0], banned) {
			t.Errorf("category copy must not contain fault-attribution %q:\n%s", banned, lines[0])
		}
	}

	// The user should not be shown a named certificate failure until the wait is
	// genuinely past the typical ceiling.
	slowOnly := transientStatus(&d, phaseCertProvisioning, "", true, false)
	if !strings.Contains(slowOnly[0], "still working") {
		t.Errorf("slow-but-not-verySlow must use the 'still working' line:\n%s", slowOnly[0])
	}
	if strings.Contains(slowOnly[0], "our edge reports no certificate") {
		t.Errorf("slow-but-not-verySlow must NOT promote to category copy:\n%s", slowOnly[0])
	}
	if strings.Contains(slowOnly[0], "taking longer") {
		t.Errorf("slow-but-not-verySlow must NOT escalate to 'taking longer':\n%s", slowOnly[0])
	}
}

// A row whose renewal window has passed is still serving a valid certificate,
// so the warning rides the live line rather than replacing it.
func TestFormatStatusServingRenewalOverdueWarning(t *testing.T) {
	d := api.Domain{State: StateServing, CertError: CertErrorRenewalOverdue}
	got := FormatStatus(d)
	if !strings.Contains(got, "live") || !strings.Contains(got, "renewal overdue") {
		t.Errorf("renewal_overdue must surface inline with live:\n%s", got)
	}
}

// The three degraded variants, each named from the server's verdicts.
func TestFormatStatusDegradedAxisNamed(t *testing.T) {
	seen := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	dnsOnly := api.Domain{
		State:        StateDegraded,
		Domain:       "next.altstack.jp",
		CertRequired: true,
		DnsVerdict:   "absent",
		CertVerdict:  CertVerdictCorrect,
	}
	if got := FormatStatus(dnsOnly); !strings.Contains(got, "DNS record no longer visible") {
		t.Errorf("dns-only degraded:\n%s", got)
	}

	certOnly := api.Domain{
		State:         StateDegraded,
		Domain:        "next.altstack.jp",
		CertRequired:  true,
		DnsVerdict:    "correct",
		CertVerdict:   CertVerdictWrong,
		CertCheckedAt: seen,
	}
	got := FormatStatus(certOnly)
	if !strings.Contains(got, "TLS certificate not serving") {
		t.Errorf("cert-only degraded:\n%s", got)
	}
	// The anchor is the probe attempt: a first-issuance failure never had a
	// valid handshake to report.
	if !strings.Contains(got, "Last checked") || strings.Contains(got, "valid handshake") {
		t.Errorf("cert degraded copy must anchor on last checked, not a fabricated handshake:\n%s", got)
	}

	both := api.Domain{
		State:         StateDegraded,
		Domain:        "next.altstack.jp",
		CertRequired:  true,
		DnsVerdict:    "absent",
		CertVerdict:   CertVerdictWrong,
		CertCheckedAt: seen,
	}
	got = FormatStatus(both)
	if !strings.Contains(got, "DNS record missing") || !strings.Contains(got, "TLS certificate not serving") {
		t.Errorf("both-degraded copy must name both axes:\n%s", got)
	}
}

// The give-up frame is written as "%s: %w", so the localized sentence sits in
// front of the last error and that error still unwraps out of the result.
func TestWatchGiveUpFrameStillUnwraps(t *testing.T) {
	client := &mockClient{listDomainsFn: func(string) (*api.DomainList, error) {
		return nil, errors.New("network is down")
	}}
	var out bytes.Buffer
	err := runWatch(client, "site1", []string{"a.example.com"}, &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	if err == nil || !strings.Contains(err.Error(), i18n.Tf("domain.err_status_fetch_give_up", 10)) ||
		errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), "network is down") {
		t.Errorf("runWatch give-up error = %v, want the keyed frame over the last error", err)
	}
}

// A refused CLI version answers every poll the same way, so the watch aborts on
// the first one: no retry, nothing printed (the give-up copy would point at
// `kamakiri status`, which the same floor refuses), and the sentinel bare.
func TestWatchAbortsOnAVersionRefusal(t *testing.T) {
	polls := 0
	client := &mockClient{listDomainsFn: func(string) (*api.DomainList, error) {
		polls++
		return nil, api.ErrUpgradeRequired
	}}
	var out bytes.Buffer
	err := runWatch(client, "site1", []string{"a.example.com"}, &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("runWatch error = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1: the refusal must not be retried", polls)
	}
	if got := out.String(); got != "" {
		t.Errorf("output = %q, want nothing printed before the upgrade message", got)
	}
}

// The floor is armed while the watch is already running, so the refusal normally
// arrives with a status frame on screen. It is rewound before the watch returns,
// or the upgrade message would print under a frame that stopped meaning
// anything.
func TestWatchClearsTheFrameBeforeAbortingOnARefusal(t *testing.T) {
	var out bytes.Buffer
	var drawn string
	polls := 0
	client := &mockClient{listDomainsFn: func(string) (*api.DomainList, error) {
		polls++
		if polls == 1 {
			return &api.DomainList{Domains: []api.Domain{cnameRow(StateAwaitingDNS, "")}}, nil
		}
		drawn = out.String()
		return nil, api.ErrUpgradeRequired
	}}
	err := runWatch(client, "site1", []string{"next.altstack.jp"}, &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		isTTY:        true,
		ctx:          context.Background(),
	})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("runWatch error = %v, want the refusal returned bare", err)
	}
	if drawn == "" {
		t.Fatal("the first poll must leave a frame on screen, or the rewind pins nothing")
	}
	want := drawn + rewind(strings.Count(drawn, "\n"))
	if got := out.String(); got != want {
		t.Errorf("output = %q, want the drawn frame rewound: %q", got, want)
	}
}

// A wrong-target frame with nothing observed says so rather than leaving the
// observed-values label with nothing after it.
func TestTransientWrongTargetWithNothingObservedSaysSo(t *testing.T) {
	d := &api.Domain{Domain: "a.example.com", DnsVerdict: "present_but_wrong",
		DNSRecordsExpected: []api.DNSRecord{{Name: "a.example.com", Type: "cname", Purpose: "primary", Value: "h1.kamakiri-pages.site."}},
	}
	got := transientStatus(d, phaseWrong, "", false, false)[2]
	if !strings.HasSuffix(got, i18n.T("domain.observed_nothing")) {
		t.Errorf("got line = %q, want it to end in %q", got, i18n.T("domain.observed_nothing"))
	}
}

// A cert category this CLI does not know falls back to the reassurance line
// rather than blanking the frame.
func TestCertCategoryCopyUnknownCategoryFallsBack(t *testing.T) {
	if line := certCategoryCopy("a_category_from_a_newer_server"); line != i18n.T("domain.cert_copy_still_working") {
		t.Errorf("certCategoryCopy fallback = %q, want %q", line, i18n.T("domain.cert_copy_still_working"))
	}
}

// A resolver that answered with a blank value is nothing observed, the same as
// answering with no value at all.
func TestJoinObservedValuesTreatsABlankValueAsNothing(t *testing.T) {
	if line := joinObservedValues([]string{""}); line != i18n.T("domain.observed_nothing") {
		t.Errorf("joinObservedValues([\"\"]) = %q, want %q", line, i18n.T("domain.observed_nothing"))
	}
}
