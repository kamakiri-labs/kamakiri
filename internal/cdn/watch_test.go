package cdn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// rewind is the escape a cleared transient block writes on a TTY: n lines up,
// then erase everything below.
func rewind(n int) string { return fmt.Sprintf("\033[%dA\033[J", n) }

// scriptClient drives the watch with a fixed sequence of status frames; the
// last one repeats once the script runs out.
type scriptClient struct {
	mockClient
	frames []*api.CDNStatusResponse
	n      int
}

func (s *scriptClient) CDNStatus(string) (*api.CDNStatusResponse, error) {
	f := s.frames[s.n]
	if s.n < len(s.frames)-1 {
		s.n++
	}
	return f, nil
}

// testOpts builds deterministic watch options: a negligible poll cadence, a
// fake clock that advances a second per read, and append-only output.
func testOpts(ctx context.Context) cdnWatchOpts {
	base := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	calls := 0
	return cdnWatchOpts{
		pollSchedule: func(time.Duration) time.Duration { return time.Millisecond },
		// Guarded so the watch may read the clock from any goroutine.
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			t := base.Add(time.Duration(calls) * time.Second)
			calls++
			return t
		},
		isTTY: false,
		ctx:   ctx,
	}
}

func cf(state string, recs ...api.DNSRecord) *api.CDNStatusResponse {
	verdict := ""
	// The common path is a site that was already serving directly, so its
	// delivery record was correct before the flip. Tests that exercise the gate
	// itself set a different verdict through cfVerdict.
	if state == CdnStateActive {
		verdict = "correct"
	}
	return cfVerdict(state, verdict, recs...)
}

// cfVerdict builds a Cloudflare frame carrying an explicit delivery verdict.
func cfVerdict(state, verdict string, recs ...api.DNSRecord) *api.CDNStatusResponse {
	d := api.CDNDomainStatus{Domain: "example.com", CdnState: state, DnsVerdict: verdict}
	d.DNSRecordsExpected = recs
	return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare", Domains: []api.CDNDomainStatus{d}}
}

var dcvRec = api.DNSRecord{Name: "_cf-custom-hostname.example.com", Type: "txt", Value: "tok123", Purpose: "validation"}

// cfPrimaryRec is the record the customer publishes. The gate itself reads the
// verdict, not this; the record is what the guided block names.
var cfPrimaryRec = api.DNSRecord{Name: "example.com", Type: "cname", Value: "s1ab2c3.cdn.kamakiri.app.", Purpose: "primary"}

func TestWatchCloudflareFullGoLive(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		cf(CdnStateAwaitingCFValidation),         // registering (no records yet)
		cf(CdnStateAwaitingCFValidation, dcvRec), // DCV block appears
		cf(CdnStateReadyToFlip),                  // validated, DNS flip pending
		cf(CdnStateActive),                       // live
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"⧗ registering with Cloudflare…",
		"Configure your DNS (Cloudflare validation):",
		"_cf-custom-hostname.example.com",
		"Re-run `kamakiri cdn verify`",
		"✓ Cloudflare validated",
		"✓ DNS pointed at the Cloudflare edge",
		"✓ live via Cloudflare: https://example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "✓ Cloudflare validated") > strings.Index(got, "✓ live via Cloudflare") {
		t.Errorf("milestones out of order:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// Never instruct a record that does not exist yet: the first polls can carry
// none, and the tick must say so instead.
func TestWatchCloudflareNoPrematureDCV(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		cf(CdnStateAwaitingCFValidation),
		cf(CdnStateAwaitingCFValidation),
		cf(CdnStateActive),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "⧗ registering with Cloudflare…") {
		t.Errorf("missing registering tick: %q", got)
	}
	if strings.Contains(got, "Configure your DNS") {
		t.Errorf("DCV block shown before records existed: %q", got)
	}
}

// The provider being ready is not the site being live: traffic only arrives
// once the customer's record points at the indirection host. Without this gate
// the first active frame here would declare a site nobody can reach.
func TestWatchCloudflareGatesLiveOnDeliveryVerdict(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		cf(CdnStateAwaitingCFValidation, dcvRec),                     // DCV
		cf(CdnStateReadyToFlip),                                      // validated, DNS flip pending
		cfVerdict(CdnStateActive, "absent", cfPrimaryRec),            // active, delivery not yet published
		cfVerdict(CdnStateActive, "present_but_wrong", cfPrimaryRec), // active, delivery pointed wrong
		cfVerdict(CdnStateActive, "correct", cfPrimaryRec),           // active + delivery correct → live
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"✓ Cloudflare validated",
		"waiting for your delivery record",
		"✓ live via Cloudflare",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "waiting for your delivery record") > strings.Index(got, "✓ live via Cloudflare") {
		t.Errorf("live must follow the delivery wait (no false-live on the wrong/absent frames):\n%s", got)
	}
	assertCleanCopy(t, got)
}

// Every verdict is pinned at the derivation, so the gate cannot silently narrow
// to catching one of them.
func TestDeriveCdnPhaseCloudflareDeliveryGate(t *testing.T) {
	cases := []struct {
		verdict                          string
		wantActive, wantAwaitingDelivery int
	}{
		{"correct", 1, 0},
		{"present_but_wrong", 0, 1},
		{"absent", 0, 1},
		{"unreachable", 0, 1},
		{"", 0, 1},
	}
	for _, tc := range cases {
		t.Run("verdict="+tc.verdict, func(t *testing.T) {
			s := deriveCdnPhase(cfVerdict(CdnStateActive, tc.verdict, cfPrimaryRec), "cloudflare")
			if s.active != tc.wantActive || s.awaitingDelivery != tc.wantAwaitingDelivery {
				t.Errorf("verdict %q: active=%d awaitingDelivery=%d, want active=%d awaitingDelivery=%d",
					tc.verdict, s.active, s.awaitingDelivery, tc.wantActive, tc.wantAwaitingDelivery)
			}
		})
	}
}

// WebAccel proves delivery through its own latch, so this verdict must not gate
// it as well.
func TestDeriveCdnPhaseWebAccelActiveIgnoresDeliveryGate(t *testing.T) {
	f := waFrame(waLatches{state: CdnStateActive, subdomain: "ou8mw1sw.user.webaccel.jp",
		ownershipStatus: "verified", enabledAt: "t", routedAt: "t"})
	f.Domains[0].DnsVerdict = "absent"
	s := deriveCdnPhase(f, "webaccel")
	if s.active != 1 || s.awaitingDelivery != 0 {
		t.Errorf("webaccel active must ignore the delivery gate: active=%d awaitingDelivery=%d, want active=1 awaitingDelivery=0",
			s.active, s.awaitingDelivery)
	}
}

// Mid go-live a lost delivery record means the site is not live yet, not that
// it is serving badly. Without the reason branch this would exit zero on its
// first frame.
func TestWatchCloudflareDeliveryLostWaitsNotDegraded(t *testing.T) {
	lost := cfVerdict(CdnStateActive, "present_but_wrong", cfPrimaryRec)
	lost.Domains[0].CdnHealth = api.CdnHealth{Verdict: CdnVerdictDegraded, Reason: CdnReasonCloudflareDeliveryLost}
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		lost,
		cfVerdict(CdnStateActive, "correct", cfPrimaryRec),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	if strings.Contains(got, "⚠ live via Cloudflare") {
		t.Errorf("delivery-lost must not render the exit-0 ⚠-live degraded line:\n%s", got)
	}
	if !strings.Contains(got, "✓ live via Cloudflare") {
		t.Errorf("must reach live once delivery reads correct:\n%s", got)
	}
	if !strings.Contains(got, "Configure your DNS (point your domain at the indirection host):") {
		t.Errorf("delivery-lost must surface the guided delivery record:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// A definitively wrong record earns the guided block, named from the record
// itself, and the block must commit once however many polls repeat the verdict.
func TestWatchCloudflareSurfacesDeliveryRecord(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		cfVerdict(CdnStateActive, "absent", cfPrimaryRec),
		cfVerdict(CdnStateActive, "absent", cfPrimaryRec),
		cfVerdict(CdnStateActive, "correct", cfPrimaryRec),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Configure your DNS (point your domain at the indirection host):",
		"example.com",
		"CNAME",
		"s1ab2c3.cdn.kamakiri.app.", // the indirection host, read from the record Value
		"Add the record above at your registrar",
		"live through Cloudflare once it resolves to the indirection host",
		"Re-run `kamakiri cdn verify`",
		"✓ live via Cloudflare",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "Configure your DNS (point your domain at the indirection host):"); n != 1 {
		t.Errorf("delivery block must commit exactly once, got %d:\n%s", n, got)
	}
	assertCleanCopy(t, got)
}

// A verdict that only means "could not tell" still holds the watch open but
// must not print a correction: the block would flap in and out.
func TestWatchCloudflareDeliveryNoSurfaceOnTransientVerdict(t *testing.T) {
	for _, verdict := range []string{"unreachable", ""} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			client := &scriptClient{frames: []*api.CDNStatusResponse{
				cfVerdict(CdnStateActive, verdict, cfPrimaryRec),
				cfVerdict(CdnStateActive, "correct", cfPrimaryRec),
			}}
			var out bytes.Buffer
			if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
				t.Fatalf("err = %v", err)
			}
			got := out.String()
			if strings.Contains(got, "Configure your DNS (point your domain at the indirection host):") {
				t.Errorf("must not surface the delivery block on a transient %q verdict:\n%s", verdict, got)
			}
			if !strings.Contains(got, "✓ live via Cloudflare") {
				t.Errorf("must still reach live once delivery reads correct:\n%s", got)
			}
		})
	}
}

// A site already serving directly needs no DNS change to go live behind the
// CDN, so nothing should be asked of the user.
func TestWatchCloudflareLiveDirectFirstNoBlock(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		cfVerdict(CdnStateActive, "correct", cfPrimaryRec),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ live via Cloudflare") {
		t.Errorf("missing live line:\n%s", got)
	}
	if strings.Contains(got, "Configure your DNS (point your domain at the indirection host):") {
		t.Errorf("no delivery block on the already-correct path:\n%s", got)
	}
	if strings.Contains(got, "waiting for your delivery record") {
		t.Errorf("no delivery wait on the already-correct path:\n%s", got)
	}
	assertCleanCopy(t, got)
}

func TestDeliveryGuidanceBlock(t *testing.T) {
	block := strings.Join(deliveryGuidanceBlock([]api.DNSRecord{cfPrimaryRec}), "\n")
	for _, want := range []string{
		"Configure your DNS (point your domain at the indirection host):",
		"example.com",
		"CNAME",
		// The trailing dot stays: without it a registrar may append the zone.
		"s1ab2c3.cdn.kamakiri.app.",
		"Add the record above at your registrar",
		"live through Cloudflare once it resolves to the indirection host",
		"kamakiri cdn verify",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q in:\n%s", want, block)
		}
	}
	assertCleanCopy(t, block)
}

func TestPrimaryRecords(t *testing.T) {
	recs := []api.DNSRecord{
		dcvRec,
		cfPrimaryRec,
		{Name: "_webaccel.example.com", Type: "txt", Value: "webaccel=x", Purpose: "ownership"},
	}
	got := primaryRecords(recs)
	if len(got) != 1 || got[0].Purpose != "primary" || got[0].Name != "example.com" {
		t.Errorf("primaryRecords = %+v, want exactly the one primary record", got)
	}
}

// Two domains at different phases in one poll. Both guided blocks commit, in
// order, but only one tick may render: a second would clear the first on a
// terminal, and the DCV wait outranks the delivery wait.
func TestWatchCloudflareMultiDomainBlockOrderAndSingleTick(t *testing.T) {
	mixed := &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare",
		Domains: []api.CDNDomainStatus{
			{Domain: "a.example.com", CdnState: CdnStateAwaitingCFValidation,
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "_cf-custom-hostname.a.example.com", Type: "txt", Value: "tokA", Purpose: "validation"}}},
			{Domain: "b.example.com", CdnState: CdnStateActive, DnsVerdict: "absent",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "b.example.com", Type: "cname", Value: "bdeliv.cdn.kamakiri.app.", Purpose: "primary"}}},
		}}
	bothLive := &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare",
		Domains: []api.CDNDomainStatus{
			{Domain: "a.example.com", CdnState: CdnStateActive, DnsVerdict: "correct"},
			{Domain: "b.example.com", CdnState: CdnStateActive, DnsVerdict: "correct"},
		}}
	client := &scriptClient{frames: []*api.CDNStatusResponse{mixed, bothLive}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	dcvHeader := "Configure your DNS (Cloudflare validation):"
	deliveryHeader := "Configure your DNS (point your domain at the indirection host):"
	for _, want := range []string{dcvHeader, deliveryHeader,
		"_cf-custom-hostname.a.example.com", "bdeliv.cdn.kamakiri.app.", "✓ live via Cloudflare"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, dcvHeader) > strings.Index(got, deliveryHeader) {
		t.Errorf("DCV block must be committed before the delivery block:\n%s", got)
	}
	if !strings.Contains(got, "waiting for Cloudflare to validate the record") {
		t.Errorf("missing the DCV wait tick:\n%s", got)
	}
	if strings.Contains(got, "waiting for your delivery record") {
		t.Errorf("delivery wait must not show while a DCV wait is the active transient:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// Re-attaching mid-flight, the state a user returns to after an interrupt. The
// CDN mutators have no function set, so any of them would panic here: the
// resume must be read-only.
func TestWatchResumeMidDCV(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		cf(CdnStateAwaitingCFValidation, dcvRec), // resumed here (records already published)
		cf(CdnStateReadyToFlip),
		cf(CdnStateActive),
	}}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Configure your DNS (Cloudflare validation):",
		"✓ Cloudflare validated",
		"✓ live via Cloudflare: https://example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in resumed stream:\n%s", want, got)
		}
	}
	assertCleanCopy(t, got)
}

func TestWatchCloudflareCtrlCDetach(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{cf(CdnStateAwaitingCFValidation, dcvRec)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupt before the first sleep
	var out bytes.Buffer
	err := watchToLive(client, "s1", "cloudflare", &out, testOpts(ctx))
	if err != ErrCdnWatchInterrupted {
		t.Fatalf("err = %v, want ErrCdnWatchInterrupted", err)
	}
	got := out.String()
	if !strings.Contains(got, "Stopped watching") || !strings.Contains(got, "kamakiri cdn verify") {
		t.Errorf("missing detach copy naming cdn verify: %q", got)
	}
	assertCleanCopy(t, got)
}

// waLatches is the set of go-live latches waFrame builds a WebAccel frame from.
// The milestones project from the latches, while the position decides whether
// the row still counts as pending.
type waLatches struct {
	state               string
	health              api.CdnHealth // the derived verdict the server would surface
	subdomain           string
	ownershipStatus     string // "", "absent", "verified", "mismatch"
	enabledAt, routedAt string
	recs                []api.DNSRecord
}

func waFrame(l waLatches) *api.CDNStatusResponse {
	d := api.CDNDomainStatus{
		Domain:                    "example.com",
		CdnState:                  l.state,
		CdnHealth:                 l.health,
		WebaccelSubdomain:         l.subdomain,
		WebaccelOwnershipStatus:   l.ownershipStatus,
		WebaccelEnabledObservedAt: l.enabledAt,
		WebaccelRoutedObservedAt:  l.routedAt,
		DNSRecordsExpected:        l.recs,
	}
	return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
		Domains: []api.CDNDomainStatus{d}}
}

var waOwnRec = api.DNSRecord{Name: "_webaccel.example.com", Type: "txt", Value: "webaccel=ou8mw1sw.user.webaccel.jp", Purpose: "ownership"}

// waFrameDomain is waFrame for a named domain. A subdomain keeps the apex
// guidance out of the happy path.
func waFrameDomain(domain string, l waLatches) *api.CDNStatusResponse {
	f := waFrame(l)
	f.Domains[0].Domain = domain
	return f
}

var waOwnRecSub = api.DNSRecord{Name: "_webaccel.next.example.com", Type: "txt", Value: "webaccel=ou8mw1sw.user.webaccel.jp", Purpose: "ownership"}

// The whole two-phase go-live in order. The phase-2 wait must be framed as
// waiting on the customer's record, not on certificate issuance, which would
// imply seconds for something that can take hours.
func TestWatchWebAccelFullGoLive(t *testing.T) {
	const dom = "next.example.com" // a subdomain (not apex) for the happy path
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrameDomain(dom, waLatches{state: CdnStateAwaitingProvisioning}),                                                                             // creating resource
		waFrameDomain(dom, waLatches{state: CdnStateAwaitingVerification, subdomain: "ou8mw1sw.user.webaccel.jp", recs: []api.DNSRecord{waOwnRecSub}}), // resource ready, ownership TXT shown
		waFrameDomain(dom, waLatches{state: CdnStateAwaitingVerification, subdomain: "ou8mw1sw.user.webaccel.jp",
			ownershipStatus: "verified", enabledAt: "2026-05-24T00:00:00Z"}),
		waFrameDomain(dom, waLatches{state: CdnStateAwaitingProvisioning, subdomain: "ou8mw1sw.user.webaccel.jp",
			ownershipStatus: "verified", enabledAt: "2026-05-24T00:00:00Z", routedAt: "2026-05-24T00:00:30Z"}),
		waFrameDomain(dom, waLatches{state: CdnStateActive, subdomain: "ou8mw1sw.user.webaccel.jp", ownershipStatus: "verified",
			enabledAt: "2026-05-24T00:00:00Z", routedAt: "2026-05-24T00:00:30Z"}),
	}}
	var out bytes.Buffer
	opts := testOpts(context.Background())
	opts.certCheck = func(context.Context, string, string) error { return nil }
	if err := watchToLive(client, "s1", "webaccel", &out, opts); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"⧗ creating WebAccel resource…",
		"✓ WebAccel resource created/updated",
		"prove you own the domain (phase 1 of 2)",
		`_webaccel.next.example.com`,
		`webaccel=ou8mw1sw.user.webaccel.jp`,
		"waiting for your DNS to propagate",
		"✓ domain ownership verified",
		"✓ WebAccel enabled",
		"send traffic to WebAccel (phase 2 of 2, the cutover)",
		"next.example.com",
		"CNAME",
		"ou8mw1sw.user.webaccel.jp",
		"waiting for your delivery CNAME",
		"✓ your delivery CNAME is live",
		"WebAccel is issuing the HTTPS certificate",
		"✓ SSL certificate checked",
		"✓ live via WebAccel: https://next.example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// WebAccel returns a full host, not a label, so appending its own suffix to
	// it would double the suffix and name a host that does not exist.
	if strings.Contains(got, "user.webaccel.jp.user.webaccel.jp") {
		t.Errorf("delivery host has a doubled .user.webaccel.jp suffix:\n%s", got)
	}
	// Without the trailing dot registrars append the zone to the target.
	if !strings.Contains(got, "ou8mw1sw.user.webaccel.jp.") {
		t.Errorf("delivery CNAME target must be fully qualified with a trailing dot:\n%s", got)
	}
	// Shown unquoted: registrars add their own quoting, and a pasted pair of
	// literal quotes is a common way to get the record rejected.
	if strings.Contains(got, `"webaccel=`) {
		t.Errorf("ownership TXT value must not be wrapped in quotes:\n%s", got)
	}
	if strings.Index(got, "✓ WebAccel resource created/updated") > strings.Index(got, "✓ domain ownership verified") {
		t.Errorf("milestones out of order:\n%s", got)
	}
	if strings.Index(got, "✓ WebAccel enabled") > strings.Index(got, "✓ your delivery CNAME is live") {
		t.Errorf("milestones out of order:\n%s", got)
	}
	if strings.Index(got, "✓ your delivery CNAME is live") > strings.Index(got, "✓ live via WebAccel") {
		t.Errorf("milestones out of order:\n%s", got)
	}
	if strings.Index(got, "phase 1 of 2") > strings.Index(got, "phase 2 of 2") {
		t.Errorf("phase 1 block must precede phase 2 block:\n%s", got)
	}
	if strings.Index(got, "✓ SSL certificate checked") > strings.Index(got, "✓ live via WebAccel") {
		t.Errorf("SSL milestone must precede live:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// A run that finds everything already done still leaves the full trail: the
// milestones are filled in from the latches at the end rather than skipped,
// so scrollback reads the same whether the run watched the steps happen or
// arrived after them.
func TestWatchWebAccelFillsInMilestonesWhenLiveOnFirstPoll(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrame(waLatches{state: CdnStateActive, subdomain: "ou8mw1sw.user.webaccel.jp",
			ownershipStatus: "verified", enabledAt: "2026-05-24T00:00:00Z", routedAt: "2026-05-24T00:00:30Z"}),
	}}
	var out bytes.Buffer
	opts := testOpts(context.Background())
	opts.certCheck = func(context.Context, string, string) error { return nil }
	if err := watchToLive(client, "s1", "webaccel", &out, opts); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// Each milestone is pinned whole: several of them open with the same words
	// and only the tail tells them apart.
	for _, want := range []string{
		"✓ WebAccel resource created/updated\n",
		"✓ domain ownership verified\n",
		"✓ WebAccel enabled\n",
		"✓ your delivery CNAME is live (traffic now flows to WebAccel)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing filled-in milestone %q in:\n%s", want, got)
		}
	}
	assertCleanCopy(t, got)
}

// The record can be missing for a poll or two, and the tick must stay neutral
// rather than instruct a record it cannot name.
func TestWatchWebAccelNoPrematureOwnershipRecord(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrame(waLatches{state: CdnStateAwaitingVerification, subdomain: "ou8mw1sw.user.webaccel.jp"}), // resource ready, no record yet
		waFrame(waLatches{state: CdnStateActive, subdomain: "ou8mw1sw.user.webaccel.jp", ownershipStatus: "verified",
			enabledAt: "t", routedAt: "t"}),
	}}
	var out bytes.Buffer
	opts := testOpts(context.Background())
	opts.certCheck = func(context.Context, string, string) error { return nil }
	if err := watchToLive(client, "s1", "webaccel", &out, opts); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "⧗ preparing the domain-ownership record…") {
		t.Errorf("missing preparing tick: %q", got)
	}
	if strings.Contains(got, "Add this DNS record") {
		t.Errorf("ownership block shown before record existed: %q", got)
	}
	assertCleanCopy(t, got)
}

// Nothing but the user can clear a wrong ownership record, so this run must
// end and show them the value it should have carried.
func TestWatchWebAccelTxtMismatch(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrame(waLatches{state: CdnStateAwaitingVerification,
			health:    api.CdnHealth{Verdict: CdnVerdictUnverified, Reason: "webaccel_txt_mismatch"},
			subdomain: "ou8mw1sw.user.webaccel.jp", ownershipStatus: "mismatch", recs: []api.DNSRecord{waOwnRec}}),
	}}
	var out bytes.Buffer
	err := watchToLive(client, "s1", "webaccel", &out, testOpts(context.Background()))
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("txt mismatch must be CLI-terminal (wrap ErrCdnReported); got %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"webaccel_txt_mismatch",
		"doesn't match",
		"_webaccel.example.com",
		`webaccel=ou8mw1sw.user.webaccel.jp`,
		"Re-run `kamakiri cdn webaccel`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "✓ live via WebAccel") {
		t.Errorf("must not declare live on a terminal mismatch:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// Pins the reason to its fail-fast treatment: dropping it would silently turn
// this row into a wait that never ends.
func TestWatchWebAccelPanelMisconfigured(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrame(waLatches{state: CdnStateAwaitingProvisioning,
			health:    api.CdnHealth{Verdict: CdnVerdictMisconfigured, Reason: "webaccel_panel_misconfigured"},
			subdomain: "ou8mw1sw.user.webaccel.jp"}),
	}}
	var out bytes.Buffer
	err := watchToLive(client, "s1", "webaccel", &out, testOpts(context.Background()))
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("panel-misconfigured must be CLI-terminal (wrap ErrCdnReported); got %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"webaccel_panel_misconfigured",
		"set the site's domain type to your own domain",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	assertCleanCopy(t, got)
}

// A domain that cannot proceed alongside one that is legitimately waiting: the
// run must end naming the first rather than wait forever on the second.
func TestWatchWebAccelTerminalBeatsWaitingSibling(t *testing.T) {
	// The first domain sits at a position that reads as still pending; only its
	// reason marks it as going nowhere.
	a := api.CDNDomainStatus{Domain: "a.example.com", CdnState: CdnStateAwaitingProvisioning,
		CdnHealth:         api.CdnHealth{Verdict: CdnVerdictTimedOut, Reason: "webaccel_enable_failed_post_verify"},
		WebaccelSubdomain: "aaaa", WebaccelOwnershipStatus: "verified", WebaccelRoutedObservedAt: "t"}
	b := api.CDNDomainStatus{Domain: "b.example.com", CdnState: CdnStateAwaitingVerification,
		WebaccelSubdomain: "bbbb", DNSRecordsExpected: []api.DNSRecord{
			{Name: "_webaccel.b.example.com", Type: "txt", Value: "webaccel=bbbb", Purpose: "ownership"}}}
	frame := &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
		Domains: []api.CDNDomainStatus{a, b}}
	// The frame repeats: without the fail-fast this test would hang.
	client := &scriptClient{frames: []*api.CDNStatusResponse{frame}}
	var out bytes.Buffer
	err := watchToLive(client, "s1", "webaccel", &out, testOpts(context.Background()))
	if !errors.Is(err, ErrCdnReported) {
		t.Fatalf("a terminal sibling must fail the run fast (wrap ErrCdnReported); got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "a.example.com: webaccel_enable_failed_post_verify") {
		t.Errorf("missing terminal-A breakdown: %q", got)
	}
	if !strings.Contains(got, "kamakiri cdn none") {
		t.Errorf("enable-failed-post-verify must point to `cdn none` to revert: %q", got)
	}
	assertCleanCopy(t, got)
}

// A missing resource is recreated by the reconciler, so the watch must wait it
// out rather than treat it as going nowhere. The frames carry no reason string
// because none reaches the client: the position climbs back through
// provisioning as a plain pending row.
func TestWatchWebAccelResourceGoneKeepsWaiting(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrame(waLatches{state: CdnStateAwaitingProvisioning}), // transient recreate
		waFrame(waLatches{state: CdnStateActive, subdomain: "ou8mw1sw.user.webaccel.jp", ownershipStatus: "verified",
			enabledAt: "t", routedAt: "t"}),
	}}
	var out bytes.Buffer
	opts := testOpts(context.Background())
	opts.certCheck = func(context.Context, string, string) error { return nil }
	if err := watchToLive(client, "s1", "webaccel", &out, opts); err != nil {
		t.Fatalf("resource_gone must not be terminal; got %v", err)
	}
	if !strings.Contains(out.String(), "✓ live via WebAccel") {
		t.Errorf("expected eventual live after recreate: %q", out.String())
	}
}

// The gate covers WebAccel too, and holds back the live line until a real
// fetch succeeds.
func TestWatchWebAccelCertGateWaitsThenLive(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{
		waFrame(waLatches{state: CdnStateActive, subdomain: "ou8mw1sw.user.webaccel.jp", ownershipStatus: "verified",
			enabledAt: "t", routedAt: "t"}),
	}}
	calls := 0
	var sawMode string
	opts := testOpts(context.Background())
	opts.certCheck = func(_ context.Context, _ string, mode string) error {
		calls++
		sawMode = mode
		if calls < 3 {
			return errors.New("edge not serving yet (status 404)") // disabled-site page
		}
		return nil
	}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "webaccel", &out, opts); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{"checking SSL certificate", "✓ SSL certificate checked", "✓ live via WebAccel"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "✓ SSL certificate checked") > strings.Index(got, "✓ live via WebAccel") {
		t.Errorf("cert milestone must precede live:\n%s", got)
	}
	if sawMode != "webaccel" {
		t.Errorf("certGate must pass mode=webaccel to the probe; saw %q", sawMode)
	}
	if calls < 3 {
		t.Errorf("cert check ran %d times, want ≥3 (keep waiting until ready)", calls)
	}
	assertCleanCopy(t, got)
}

// With several content domains there is nothing that says which is canonical,
// so the line must name none of them.
func TestCdnLiveLine(t *testing.T) {
	single := func(mode string) *api.CDNStatusResponse {
		return &api.CDNStatusResponse{CDNMode: mode, Provider: mode,
			Domains: []api.CDNDomainStatus{{Domain: "next.altstack.jp", CdnState: CdnStateActive}}}
	}
	multi := &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
		Domains: []api.CDNDomainStatus{
			{Domain: "a.example.com", CdnState: CdnStateActive},
			{Domain: "b.example.com", CdnState: CdnStateActive},
		}}

	cases := []struct {
		name           string
		status         *api.CDNStatusResponse
		mode, provider string
		want           string
	}{
		{"webaccel single domain shows URL", single("webaccel"), "webaccel", "WebAccel",
			"✓ live via WebAccel: https://next.altstack.jp"},
		{"cloudflare single domain shows URL", single("cloudflare"), "cloudflare", "Cloudflare",
			"✓ live via Cloudflare: https://next.altstack.jp"},
		{"webaccel multi domain is bare (canonical ambiguous)", multi, "webaccel", "WebAccel",
			"✓ live via WebAccel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cdnLiveLine(tc.status, tc.mode, tc.provider); got != tc.want {
				t.Errorf("cdnLiveLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWatchTerminalTaxonomy(t *testing.T) {
	t.Run("all active", func(t *testing.T) {
		c := &scriptClient{frames: []*api.CDNStatusResponse{cf(CdnStateActive)}}
		var out bytes.Buffer
		if err := watchToLive(c, "s", "cloudflare", &out, testOpts(context.Background())); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "✓ live via Cloudflare") {
			t.Errorf("got %q", out.String())
		}
	})
	t.Run("degraded exits 0 with warning", func(t *testing.T) {
		// A degraded row still serves, so it stays at the live position and the
		// verdict is what claims it.
		d := api.CDNDomainStatus{Domain: "example.com", CdnState: CdnStateActive,
			CdnHealth: api.CdnHealth{Verdict: CdnVerdictDegraded}}
		c := &scriptClient{frames: []*api.CDNStatusResponse{
			{CDNMode: "cloudflare", Provider: "cloudflare", Domains: []api.CDNDomainStatus{d}}}}
		var out bytes.Buffer
		if err := watchToLive(c, "s", "cloudflare", &out, testOpts(context.Background())); err != nil {
			t.Fatalf("degraded must exit 0 (it is serving): %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "⚠ live via Cloudflare") || !strings.Contains(got, "reported unhealthy by Cloudflare") {
			t.Errorf("missing degraded line: %q", got)
		}
		if strings.Contains(got, "✓ live via Cloudflare") {
			t.Errorf("degraded must NOT print a bare ✓ live line: %q", got)
		}
	})
	t.Run("timed_out exits nonzero with breakdown", func(t *testing.T) {
		d := api.CDNDomainStatus{Domain: "example.com", CdnState: CdnStateAwaitingCFValidation,
			CdnHealth: api.CdnHealth{Verdict: CdnVerdictTimedOut, Reason: "validation_timeout"}}
		c := &scriptClient{frames: []*api.CDNStatusResponse{{CDNMode: "cloudflare", Domains: []api.CDNDomainStatus{d}}}}
		var out bytes.Buffer
		err := watchToLive(c, "s", "cloudflare", &out, testOpts(context.Background()))
		if !errors.Is(err, ErrCdnReported) {
			t.Fatalf("timed_out row must wrap ErrCdnReported (so cdnExit "+
				"exits nonzero WITHOUT a redundant stderr echo); got %v", err)
		}
		if !strings.Contains(out.String(), "✗ example.com: validation_timeout") {
			t.Errorf("missing error breakdown: %q", out.String())
		}
	})
	t.Run("unknown state terminal + upgrade hint", func(t *testing.T) {
		d := api.CDNDomainStatus{Domain: "example.com", CdnState: "future_42"}
		c := &scriptClient{frames: []*api.CDNStatusResponse{{CDNMode: "cloudflare", Domains: []api.CDNDomainStatus{d}}}}
		var out bytes.Buffer
		err := watchToLive(c, "s", "cloudflare", &out, testOpts(context.Background()))
		if !errors.Is(err, ErrCdnReported) {
			t.Fatalf("unknown state must terminate wrapping ErrCdnReported, not loop forever; got %v", err)
		}
		if !strings.Contains(out.String(), "upgrade kamakiri") {
			t.Errorf("missing upgrade hint: %q", out.String())
		}
	})
}

// A watch that never gives up on the site must still give up on a server that
// is down, or it would spin against it forever.
func TestWatchBudgetExhaustion(t *testing.T) {
	calls := 0
	c := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
		calls++
		return nil, context.DeadlineExceeded
	}}
	var out bytes.Buffer
	err := watchToLive(c, "s", "cloudflare", &out, testOpts(context.Background()))
	if err == nil {
		t.Fatal("a persistent CDNStatus outage must terminate the watch")
	}
	if calls != cdnStatusFetchBudget {
		t.Errorf("polled %d times, want exactly cdnStatusFetchBudget=%d", calls, cdnStatusFetchBudget)
	}
	if !strings.Contains(err.Error(), "giving up") {
		t.Errorf("err = %v, want a give-up message", err)
	}
}

func TestWatchToleratesTransientErrors(t *testing.T) {
	calls := 0
	c := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return cf(CdnStateActive), nil
	}}
	var out bytes.Buffer
	if err := watchToLive(c, "s", "cloudflare", &out, testOpts(context.Background())); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Errorf("polled %d times, want ≥2 after transient", calls)
	}
	if !strings.Contains(out.String(), "status fetch failed") {
		t.Errorf("missing transient hint: %q", out.String())
	}
}

// Nothing may claim live until a real fetch succeeds, whether the certificate
// is merely late or the edge is answering with an error of its own.
func TestWatchCloudflareCertGateWaitsThenLive(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{cf(CdnStateActive)}}
	calls := 0
	opts := testOpts(context.Background())
	opts.certCheck = func(_ context.Context, _ string, _ string) error {
		calls++
		if calls < 3 {
			return errors.New("edge not serving yet (status 409)") // CF 1001 / cert lag
		}
		return nil
	}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, opts); err != nil {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"checking SSL certificate",
		"✓ SSL certificate checked",
		"✓ live via Cloudflare",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "✓ SSL certificate checked") > strings.Index(got, "✓ live via Cloudflare") {
		t.Errorf("cert milestone must precede live:\n%s", got)
	}
	if strings.Index(got, "✓ DNS pointed at the Cloudflare edge") > strings.Index(got, "✓ SSL certificate checked") {
		t.Errorf("cert milestone must follow the DNS flip:\n%s", got)
	}
	if calls < 3 {
		t.Errorf("cert check ran %d times, want ≥3 (keep waiting until ready)", calls)
	}
	assertCleanCopy(t, got)
}

// A host can pass at the edge before its sibling, so each is probed separately
// and one that has passed is not probed again while the gate waits for the
// rest.
func TestWatchCloudflareCertGatePerDomain(t *testing.T) {
	twoActive := &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare",
		Domains: []api.CDNDomainStatus{
			// Delivery is already correct on both rows, leaving the certificate
			// gate as the only thing under test.
			{Domain: "a.example.com", CdnState: CdnStateActive, DnsVerdict: "correct"},
			{Domain: "b.example.com", CdnState: CdnStateActive, DnsVerdict: "correct"},
		}}
	client := &scriptClient{frames: []*api.CDNStatusResponse{twoActive}}
	seen := map[string]int{}
	opts := testOpts(context.Background())
	opts.certCheck = func(_ context.Context, host string, _ string) error {
		seen[host]++
		if host == "b.example.com" && seen[host] < 2 {
			return errors.New("not ready") // b lags one round
		}
		return nil
	}
	var out bytes.Buffer
	if err := watchToLive(client, "s1", "cloudflare", &out, opts); err != nil {
		t.Fatal(err)
	}
	if seen["a.example.com"] == 0 || seen["b.example.com"] == 0 {
		t.Errorf("cert check must run per content domain; saw %v", seen)
	}
	// A host that passed is not probed again while another lags.
	if seen["a.example.com"] != 1 {
		t.Errorf("a passed round 1 and must not be re-probed; saw %d checks", seen["a.example.com"])
	}
	if seen["b.example.com"] < 2 {
		t.Errorf("b lagged one round and must be re-probed; saw %d checks", seen["b.example.com"])
	}
	if !strings.Contains(out.String(), "✓ SSL certificate checked") {
		t.Errorf("missing cert milestone: %q", out.String())
	}
}

// The gate never gives up on its own, so an interrupt must remain the way out
// of it, with the same detach copy as the rest of the watch.
func TestWatchCloudflareCertGateCtrlC(t *testing.T) {
	client := &scriptClient{frames: []*api.CDNStatusResponse{cf(CdnStateActive)}}
	ctx, cancel := context.WithCancel(context.Background())
	opts := testOpts(ctx)
	opts.certCheck = func(context.Context, string, string) error {
		cancel() // cert never ready; user hits Ctrl-C
		return errors.New("not ready")
	}
	var out bytes.Buffer
	err := watchToLive(client, "s1", "cloudflare", &out, opts)
	if err != ErrCdnWatchInterrupted {
		t.Fatalf("err = %v, want ErrCdnWatchInterrupted", err)
	}
	got := out.String()
	if !strings.Contains(got, "Stopped watching") || strings.Contains(got, "✓ live via Cloudflare") {
		t.Errorf("Ctrl-C during cert gate must detach without declaring live: %q", got)
	}
}

func TestIsCloudflareEdgeError(t *testing.T) {
	notReady := []int{409, 520, 521, 522, 523, 524, 525, 526, 527, 530}
	for _, c := range notReady {
		if !isCloudflareEdgeError(c) {
			t.Errorf("status %d should count as a CF edge error (not-ready)", c)
		}
	}
	ready := []int{200, 204, 301, 302, 304, 403, 404, 500, 502}
	for _, c := range ready {
		if isCloudflareEdgeError(c) {
			t.Errorf("status %d should NOT count as a CF edge error", c)
		}
	}
}

func TestIsWebAccelEdgeError(t *testing.T) {
	notReady := []int{403, 404, 502, 503, 504}
	for _, c := range notReady {
		if !isWebAccelEdgeError(c) {
			t.Errorf("status %d should count as a WebAccel edge error (not-ready)", c)
		}
	}
	ready := []int{200, 204, 301, 302, 304}
	for _, c := range ready {
		if isWebAccelEdgeError(c) {
			t.Errorf("status %d should NOT count as a WebAccel edge error", c)
		}
	}
	if !isEdgeError("webaccel", 404) {
		t.Error("isEdgeError(webaccel, 404) should be true")
	}
	if isEdgeError("cloudflare", 404) {
		t.Error("isEdgeError(cloudflare, 404) should be false (CF treats 404 as ready)")
	}
}

// A trusted certificate and a good status prove nothing here: mid-cutover our
// own origin answers with both for the same host. Only a WebAccel edge
// signature separates them.
func TestServedByWebAccel(t *testing.T) {
	cases := []struct {
		name string
		h    map[string]string
		want bool
	}{
		{"a Caddy Server header is no signature", map[string]string{"Server": "Caddy"}, false},
		{"empty headers", map[string]string{}, false},
		{"x-webaccel-origin-status present (the edge)", map[string]string{"X-Webaccel-Origin-Status": "200"}, true},
		{"x-webaccel-origin-status present, non-200 value still a signal", map[string]string{"X-Webaccel-Origin-Status": "403"}, true},
		// The real edge's Via header names its cache software, never WebAccel,
		// and our origin can carry one too.
		{"Via with ApacheTrafficServer is NOT a signal", map[string]string{"Via": "1.1 ApacheTrafficServer (sv01-osk03-jp)"}, false},
		{"Via with varnish is not a signal", map[string]string{"Via": "1.1 varnish"}, false},
		// Generic cache headers name no provider, so anything on the path can
		// stamp one and they prove nothing about which edge answered.
		{"generic X-Cache rejected (it names no provider)", map[string]string{"X-Cache": "HIT"}, false},
		{"generic X-Cache-Hits rejected", map[string]string{"X-Cache-Hits": "3"}, false},
		{"legacy X-Webaccel-Cache alone is not the pinned signal", map[string]string{"X-Webaccel-Cache": "MISS"}, false},
		{"generic cache headers rejected even alongside a Caddy Server", map[string]string{"Server": "Caddy", "X-Cache": "HIT", "X-Cache-Hits": "2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.h {
				h.Set(k, v)
			}
			if got := servedByWebAccel(h); got != tc.want {
				t.Errorf("servedByWebAccel(%v) = %v, want %v", tc.h, got, tc.want)
			}
		})
	}
}

// The target must stay fully qualified, or a registrar will append the zone to
// it, and its suffix must never be doubled.
func TestDeliveryHost(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		want string
	}{
		{"full host, no trailing dot", "2vm70gzi.user.webaccel.jp", "2vm70gzi.user.webaccel.jp."},
		{"full host already dotted, not doubled", "2vm70gzi.user.webaccel.jp.", "2vm70gzi.user.webaccel.jp."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deliveryTarget{subdomain: tc.sub}.deliveryHost()
			if got != tc.want {
				t.Errorf("deliveryHost(%q) = %q, want %q", tc.sub, got, tc.want)
			}
			if !strings.HasSuffix(got, ".") {
				t.Errorf("deliveryHost(%q) = %q, must end with a trailing dot", tc.sub, got)
			}
			if strings.Contains(got, "user.webaccel.jp.user.webaccel.jp") {
				t.Errorf("deliveryHost(%q) = %q, doubled .user.webaccel.jp suffix", tc.sub, got)
			}
		})
	}
}

// The same signature-less 200 must be accepted for Cloudflare and rejected for
// WebAccel. Dropping the signature requirement would turn the rejected case
// green here.
func TestEvalEdgeResponse(t *testing.T) {
	withEdge := http.Header{"X-Webaccel-Origin-Status": {"200"}}
	noEdge := http.Header{"Server": {"Caddy"}}

	cases := []struct {
		name    string
		mode    string
		status  int
		header  http.Header
		wantErr bool
	}{
		{"webaccel 200 with edge header → live", "webaccel", 200, withEdge, false},
		{"webaccel 200 without edge header → keep waiting (stale origin)", "webaccel", 200, noEdge, true},
		{"webaccel 403 (disabled-site) → keep waiting", "webaccel", 403, withEdge, true},
		{"webaccel 502 → keep waiting", "webaccel", 502, withEdge, true},
		{"cloudflare 200 without edge header → live (status-only, no signal required)", "cloudflare", 200, noEdge, false},
		{"cloudflare 521 origin-down → keep waiting", "cloudflare", 521, noEdge, true},
		{"cloudflare 409 apex zone error → keep waiting", "cloudflare", 409, noEdge, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := evalEdgeResponse(tc.mode, tc.status, tc.header)
			if tc.wantErr && err == nil {
				t.Errorf("evalEdgeResponse(%q, %d, %v) = nil, want error", tc.mode, tc.status, tc.header)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("evalEdgeResponse(%q, %d, %v) = %v, want nil", tc.mode, tc.status, tc.header, err)
			}
		})
	}
}

// With the gate disabled the production entry point still reaches live, just
// without the certificate milestone.
func TestWatchToLiveSkipCertCheckEnv(t *testing.T) {
	t.Setenv(EnvSkipCertCheck, "1")
	client := &scriptClient{frames: []*api.CDNStatusResponse{cf(CdnStateActive)}}
	var out bytes.Buffer
	if err := WatchToLive(client, "s1", "cloudflare", &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ live via Cloudflare") {
		t.Errorf("missing live line: %q", got)
	}
	if strings.Contains(got, "checking SSL certificate") {
		t.Errorf("cert gate must be skipped when %s is set: %q", EnvSkipCertCheck, got)
	}
}

func TestDefaultCdnPollScheduleTiers(t *testing.T) {
	cases := []struct {
		elapsed, want time.Duration
	}{
		{0, cdnPollFast},
		{59 * time.Second, cdnPollFast},
		{time.Minute, cdnPollMedium},
		{10 * time.Minute, cdnPollMedium},
		{11 * time.Minute, cdnPollSlow},
		{time.Hour, cdnPollSlow},
	}
	for _, tc := range cases {
		if got := defaultCdnPollSchedule(tc.elapsed); got != tc.want {
			t.Errorf("defaultCdnPollSchedule(%v) = %v, want %v", tc.elapsed, got, tc.want)
		}
	}
}

// The milestone floor rounds a measurement up rather than reporting a number the
// poll cadence cannot support, and renders it through the ordinary span copy.
func TestMilestoneDurationFloorRendersThroughItsOwnCopy(t *testing.T) {
	if got, want := milestoneLine("x", true, 2*time.Second), i18n.Tf("cdn.milestone_timed", "x", i18n.Tf("cdn.span_seconds", 30)); got != want {
		t.Errorf("milestoneLine under 30s = %q, want %q", got, want)
	}
}

// The CDN watch gives up after a run of failed status reads, framing the last
// one rather than swallowing it. Both halves matter: the sentence is the CLI's
// and localizes, the wrapped error is the server's and still unwraps.
func TestCdnWatchGiveUpFrameStillUnwraps(t *testing.T) {
	client := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
		return nil, errors.New("network is down")
	}}
	var out bytes.Buffer
	err := watchToLive(client, "site1", "cloudflare", &out, cdnWatchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	if err == nil || !strings.Contains(err.Error(), i18n.Tf("cdn.err_status_fetch_give_up", cdnStatusFetchBudget)) ||
		errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), "network is down") {
		t.Errorf("watchToLive give-up error = %v, want the keyed frame over the last error", err)
	}
	if want := i18n.Tf("cdn.status_fetch_failed", "network is down"); !strings.Contains(out.String(), want) {
		t.Errorf("watchToLive output = %q, want it to carry %q", out.String(), want)
	}
}

// A refused CLI version answers every poll the same way, so the watch aborts on
// the first one: no retry, nothing printed (the give-up copy would point at
// `kamakiri status`, which the same floor refuses), and the sentinel bare.
func TestCdnWatchAbortsOnAVersionRefusal(t *testing.T) {
	polls := 0
	client := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
		polls++
		return nil, api.ErrUpgradeRequired
	}}
	var out bytes.Buffer
	err := watchToLive(client, "site1", "cloudflare", &out, cdnWatchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("watchToLive error = %v, want the refusal returned bare", err)
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
func TestCdnWatchClearsTheFrameBeforeAbortingOnARefusal(t *testing.T) {
	var out bytes.Buffer
	var drawn string
	polls := 0
	client := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
		polls++
		if polls == 1 {
			return cf(CdnStateAwaitingCFValidation), nil
		}
		drawn = out.String()
		return nil, api.ErrUpgradeRequired
	}}
	err := watchToLive(client, "site1", "cloudflare", &out, cdnWatchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		isTTY:        true,
		ctx:          context.Background(),
	})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("watchToLive error = %v, want the refusal returned bare", err)
	}
	if drawn == "" {
		t.Fatal("the first poll must leave a frame on screen, or the rewind pins nothing")
	}
	want := drawn + rewind(strings.Count(drawn, "\n"))
	if got := out.String(); got != want {
		t.Errorf("output = %q, want the drawn frame rewound: %q", got, want)
	}
}
