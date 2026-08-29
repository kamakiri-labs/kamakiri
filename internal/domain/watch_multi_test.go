package domain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
)

// framesClient walks a script of whole-site status frames, one per status read
// and holding on the last, so the multi-domain watch runs without a real poll
// loop. Only the status read carries behaviour.
type framesClient struct {
	mu     sync.Mutex
	frames [][]api.Domain
	idx    int
}

func (c *framesClient) ListDomains(string) (*api.DomainList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.frames[c.idx]
	if c.idx < len(c.frames)-1 {
		c.idx++
	}
	return &api.DomainList{Domains: append([]api.Domain(nil), f...)}, nil
}

func (c *framesClient) SetCanonical(string, string) (*api.Domain, error) {
	return nil, nil
}
func (c *framesClient) UnsetCanonical(string) (*api.UnsetCanonicalResult, error) {
	return nil, nil
}
func (c *framesClient) AddDomain(string, string, string, int) (*api.Domain, error) {
	return nil, nil
}
func (c *framesClient) RemoveDomain(string) (*api.RemoveDomainResult, error) {
	return nil, nil
}
func (c *framesClient) WaitForSync(string, int64, time.Duration) (*api.Site, error) {
	return nil, nil
}
func (c *framesClient) RecheckDomain(string) (*api.Domain, error) {
	return nil, nil
}

func (c *framesClient) RegisterDomain(string) (*api.Registration, error)  { return nil, nil }
func (c *framesClient) UnregisterDomain(string) error                     { return nil }
func (c *framesClient) ListRegistrations() (*api.RegistrationList, error) { return nil, nil }

// contentRow builds a content row in direct mode, so the terminal milestone is
// the certificate one rather than a routed-via line.
func contentRow(domainName, state, verdict string) api.Domain {
	return api.Domain{
		Domain:     domainName,
		Role:       "canonical",
		State:      state,
		DnsVerdict: verdict,
		CdnMode:    CdnModeNone,
		DNSRecordsExpected: []api.DNSRecord{
			{Name: domainName, Type: "cname", Value: "h1lc.c2.kamakiri-pages.site.", Purpose: "primary", Required: true},
		},
	}
}

// Live commits only once every content domain is serving: a site whose
// canonical converges first must not declare live while an alias is still on
// its stale record.
func TestRunWatchMultiCommitsLiveOnlyWhenAllServing(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: 31 * time.Second}
	client := &framesClient{frames: [][]api.Domain{
		{ // canonical already serving, alias still absent
			contentRow("a.example.com", StateServing, "correct"),
			contentRow("b.example.com", StateAwaitingDNS, "absent"),
		},
		{
			contentRow("a.example.com", StateServing, "correct"),
			contentRow("b.example.com", StateServing, "correct"),
		},
	}}

	var out bytes.Buffer
	if err := runWatch(client, "site123", []string{"a.example.com", "b.example.com"}, &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("runWatch() error = %v", err)
	}
	s := out.String()

	if strings.Count(s, "✓ live: https://a.example.com") != 1 {
		t.Errorf("expected one live line for the canonical:\n%s", s)
	}
	if strings.Count(s, "✓ live: https://b.example.com") != 1 {
		t.Errorf("expected one live line for the alias:\n%s", s)
	}
	// The canonical was already serving on the first frame, so an implementation
	// that committed live per domain would print its live line before the
	// alias's waiting tick. The ordering below is what proves live was withheld
	// for the whole set.
	if !strings.Contains(s, "b.example.com:") {
		t.Errorf("the laggard alias should be named in the waiting tick:\n%s", s)
	}
	if i := strings.Index(s, "b.example.com:"); i < 0 || i > strings.Index(s, "✓ live: https://a.example.com") {
		t.Errorf("the canonical's live line must not precede the alias's waiting tick:\n%s", s)
	}
	if strings.Count(s, "✓ DNS propagated") != 1 {
		t.Errorf("DNS milestone should commit exactly once (when all records correct):\n%s", s)
	}
}

// A wrong-target frame names the offending domain: the records were shown up
// front, so the user needs to know which of them regressed.
func TestRunWatchMultiLabelsWrongTargetLaggard(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}

	wrong := contentRow("b.example.com", StateAwaitingDNS, "present_but_wrong")
	wrong.DNSRecordsObserved = []api.DNSRecordObserved{
		{Name: "b.example.com", Type: "cname", Values: []string{"old.webaccel.jp"}},
	}

	client := &framesClient{frames: [][]api.Domain{
		{ // canonical correct, alias still on its stale target
			contentRow("a.example.com", StateServing, "correct"),
			wrong,
		},
		{
			contentRow("a.example.com", StateServing, "correct"),
			contentRow("b.example.com", StateServing, "correct"),
		},
	}}

	var out bytes.Buffer
	if err := runWatch(client, "site123", []string{"a.example.com", "b.example.com"}, &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("runWatch() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "b.example.com:") {
		t.Errorf("wrong-target transient must name the offending domain:\n%s", s)
	}
	if !strings.Contains(s, "points at the wrong target") {
		t.Errorf("wrong-target transient must show the actionable diagnosis:\n%s", s)
	}
	if !strings.Contains(s, "old.webaccel.jp") {
		t.Errorf("wrong-target transient must show what was observed:\n%s", s)
	}
}

// Every record correct but one domain still issuing: the DNS milestone commits,
// the cert spinner runs, and live is held until the laggard's certificate lands.
func TestRunWatchMultiHoldsForCertLaggard(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &framesClient{frames: [][]api.Domain{
		{ // both records correct, alias still issuing its cert
			contentRow("a.example.com", StateServing, "correct"),
			contentRow("b.example.com", StateAwaitingCert, "correct"),
		},
		{
			contentRow("a.example.com", StateServing, "correct"),
			contentRow("b.example.com", StateServing, "correct"),
		},
	}}

	var out bytes.Buffer
	if err := runWatch(client, "site123", []string{"a.example.com", "b.example.com"}, &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("runWatch() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "✓ DNS propagated") {
		t.Errorf("DNS milestone must commit once all records are correct:\n%s", s)
	}
	if !strings.Contains(s, "provisioning SSL certificate") {
		t.Errorf("the cert spinner must show while a domain is still issuing:\n%s", s)
	}
	// The laggard is named here as in every other frame, so the user sees which
	// domain is blocking.
	if !strings.Contains(s, "b.example.com:") {
		t.Errorf("the cert-laggard domain should be named in the cert wait:\n%s", s)
	}
	if strings.Contains(s, "waiting for your DNS") {
		t.Errorf("must not show the DNS waiting tick once all records are correct:\n%s", s)
	}
	if i := strings.Index(s, "provisioning SSL certificate"); i < 0 || i > strings.Index(s, "✓ live") {
		t.Errorf("the cert spinner must precede live (live withheld for the laggard):\n%s", s)
	}
	if strings.Count(s, "✓ live: https://a.example.com") != 1 ||
		strings.Count(s, "✓ live: https://b.example.com") != 1 {
		t.Errorf("both domains must reach live exactly once:\n%s", s)
	}
}

// Interrupting a multi-domain watch points at `kamakiri status`, since naming
// one domain would under-report the rest.
func TestRunWatchMultiCtrlCDetach(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &framesClient{frames: [][]api.Domain{{
		contentRow("a.example.com", StateAwaitingDNS, "absent"),
		contentRow("b.example.com", StateAwaitingDNS, "absent"),
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the first select

	var out bytes.Buffer
	err := runWatch(client, "site123", []string{"a.example.com", "b.example.com"}, &out, testOpts(clk, ctx))
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("want ErrWatchInterrupted (→ exit 130), got %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"Stopped watching. Your domains are still being set up.",
		"kamakiri status",
		"kamakiri domain verify <domain>",
		"--no-wait",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("multi-domain detach copy missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "kamakiri domain verify a.example.com") {
		t.Errorf("multi-domain detach must not name one domain in the verify hint:\n%s", s)
	}
}

// A site already converged on the first poll prints the milestones bare and one
// live line per domain.
func TestRunWatchMultiAlreadyAllServing(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &framesClient{frames: [][]api.Domain{
		{
			contentRow("a.example.com", StateServing, "correct"),
			contentRow("b.example.com", StateServing, "correct"),
		},
	}}

	var out bytes.Buffer
	if err := runWatch(client, "site123", []string{"a.example.com", "b.example.com"}, &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("runWatch() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"✓ DNS propagated",
		"✓ SSL certificate provisioned",
		"✓ live: https://a.example.com",
		"✓ live: https://b.example.com",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	// Nothing was watched, so no duration would be honest.
	if strings.Contains(s, "(~") {
		t.Errorf("already-live run must not fabricate a duration:\n%s", s)
	}
}

// The Cloudflare exit watches every content domain and shows no records block:
// the customer's record already points at our indirection host, so there is
// nothing for them to change.
func TestWatchExitToLiveWatchesAllContentNoRecords(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "www.example.com", Role: "alias"},
				{Domain: "shop.example.com", Role: "canonical"},
				{Domain: "go.example.com", Role: "redirect"}, // not content-serving
			}}, nil
		},
	}

	var watched []string
	orig := watchAllToLiveFn
	watchAllToLiveFn = func(_ context.Context, _ APIClient, _ string, names []string, _ io.Writer) error {
		watched = names
		return nil
	}
	defer func() { watchAllToLiveFn = orig }()

	var out bytes.Buffer
	streamed, err := WatchExitToLive(context.Background(), client, "site123", true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !streamed {
		t.Error("a site with content domains must report streamed=true (so the caller does not settle on top)")
	}
	want := []string{"shop.example.com", "www.example.com"} // canonical first
	if len(watched) != len(want) || watched[0] != want[0] || watched[1] != want[1] {
		t.Errorf("watched = %v, want %v", watched, want)
	}
	got := out.String()
	if !strings.Contains(got, "switching traffic to our edge") {
		t.Errorf("missing the our-edge lead-in:\n%s", got)
	}
	if strings.Contains(got, "Configure your DNS") {
		t.Errorf("Cloudflare exit must not show a records block:\n%s", got)
	}
	if strings.Contains(got, "go.example.com") {
		t.Errorf("redirect domain must not be watched or shown:\n%s", got)
	}
}

// The non-watching exit prints the lead-in and a resume hint, then returns.
func TestWatchExitToLiveNoWaitPrintsResumeHint(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "shop.example.com", Role: "canonical"},
			}}, nil
		},
	}

	orig := watchAllToLiveFn
	watchAllToLiveFn = func(context.Context, APIClient, string, []string, io.Writer) error {
		t.Fatal("--no-wait must not enter the watch")
		return nil
	}
	defer func() { watchAllToLiveFn = orig }()

	var out bytes.Buffer
	if _, err := WatchExitToLive(context.Background(), client, "site123", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "switching traffic to our edge") {
		t.Errorf("missing the our-edge lead-in:\n%s", got)
	}
	if !strings.Contains(got, "kamakiri status") {
		t.Errorf("non-watch exit must point at `kamakiri status`:\n%s", got)
	}
}

// A site whose only domain is a redirect has no fronted content to bring back,
// so the exit prints nothing at all and leaves the caller's own header as the
// whole story.
func TestWatchExitToLiveNoContentNoOp(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "go.example.com", Role: "redirect"},
			}}, nil
		},
	}

	orig := watchAllToLiveFn
	watchAllToLiveFn = func(context.Context, APIClient, string, []string, io.Writer) error {
		t.Fatal("no content domains: must not enter the watch")
		return nil
	}
	defer func() { watchAllToLiveFn = orig }()

	var out bytes.Buffer
	streamed, err := WatchExitToLive(context.Background(), client, "site123", true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if streamed {
		t.Error("no content domains: WatchExitToLive must report nothing streamed (the caller settles)")
	}
	if got := out.String(); got != "" {
		t.Errorf("no-content exit must print nothing, got:\n%s", got)
	}
}

// The reverse cutover is the same no-op on a site with no fronted content
// domain.
func TestPointBackToOriginNoContentNoOp(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "go.example.com", Role: "redirect"},
			}}, nil
		},
	}

	orig := watchAllToLiveFn
	watchAllToLiveFn = func(context.Context, APIClient, string, []string, io.Writer) error {
		t.Fatal("no content domains: must not enter the watch")
		return nil
	}
	defer func() { watchAllToLiveFn = orig }()

	var out bytes.Buffer
	streamed, err := PointBackToOriginAndWatch(context.Background(), client, "site123", true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if streamed {
		t.Error("no content domains: PointBackToOriginAndWatch must report nothing streamed (the caller settles)")
	}
	if got := out.String(); got != "" {
		t.Errorf("no-content reverse cutover must print nothing, got:\n%s", got)
	}
}

// WatchExitToLive must forward the caller's context into the watch rather than
// mint one of its own, since the caller owns one cancellable region spanning
// its deactivation wait and this watch. This drives the real watch, so an exit
// that dropped the context would block past the cancellation instead of
// detaching.
func TestWatchExitToLivePropagatesCallerCtxCancel(t *testing.T) {
	// A domain still climbing, so the watch does not settle live on the first
	// poll and actually reaches its cancellation check.
	client := &framesClient{frames: [][]api.Domain{{
		contentRow("shop.example.com", StateAwaitingCert, "correct"),
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the watch's first select

	var out bytes.Buffer
	streamed, err := WatchExitToLive(ctx, client, "site123", true, &out)
	if !streamed {
		t.Error("a site with a content domain must report streamed=true")
	}
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("caller ctx cancel must reach the watch: want ErrWatchInterrupted, got %v", err)
	}
	if !strings.Contains(out.String(), "Stopped watching") {
		t.Errorf("exit-watch Ctrl-C must print the detach copy:\n%s", out.String())
	}
}

// PointBackToOriginAndWatch must forward the caller's context into the watch
// too, so a cancel in the caller's region reaches the real watch and detaches
// rather than blocking past it.
func TestPointBackToOriginPropagatesCallerCtxCancel(t *testing.T) {
	client := &framesClient{frames: [][]api.Domain{{
		contentRow("next.example.com", StateAwaitingDNS, "absent"),
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the watch's first select

	var out bytes.Buffer
	streamed, err := PointBackToOriginAndWatch(ctx, client, "site123", true, &out)
	if !streamed {
		t.Error("a site with a content domain must report streamed=true")
	}
	if !errors.Is(err, ErrWatchInterrupted) {
		t.Fatalf("caller ctx cancel must reach the watch: want ErrWatchInterrupted, got %v", err)
	}
	if !strings.Contains(out.String(), "Stopped watching") {
		t.Errorf("reverse-cutover Ctrl-C must print the detach copy:\n%s", out.String())
	}
}

// The Cloudflare exit reaches the watch with DNS already correct and the
// certificate absent, so the climb is issuance to live with no spurious DNS
// wait. It is why that exit can reuse this watch with no phase mode of its own.
func TestRunWatchCloudflareExitShapeNoDNSWait(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &framesClient{frames: [][]api.Domain{
		{contentRow("shop.example.com", StateAwaitingCert, "correct")},
		{contentRow("shop.example.com", StateServing, "correct")},
	}}

	var out bytes.Buffer
	if err := runWatch(client, "site123", []string{"shop.example.com"}, &out, testOpts(clk, context.Background())); err != nil {
		t.Fatalf("runWatch() error = %v", err)
	}
	s := out.String()
	if strings.Contains(s, "waiting for your DNS") {
		t.Errorf("CF exit (DNS already correct) must not show the DNS waiting tick:\n%s", s)
	}
	for _, want := range []string{
		"provisioning SSL certificate",
		"✓ SSL certificate provisioned",
		"✓ live: https://shop.example.com",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
}

// An apex domain's guidance rides the per-domain records table, not a block of
// its own, so the reverse cutover must not lose it.
func TestPointBackToOriginRendersApexRecords(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{{
				Domain: "altstack.jp",
				Role:   "canonical",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "altstack.jp", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					{Name: "altstack.jp", Type: "a", Value: "203.0.113.2", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					{Name: "altstack.jp", Type: "alias_or_aname", Value: "fzks.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
				},
			}}}, nil
		},
	}

	var out bytes.Buffer
	if _, err := PointBackToOriginAndWatch(context.Background(), client, "site123", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Configure your DNS:",
		"203.0.113.1",
		"203.0.113.2",
		"ALIAS",
		"fzks.c1.kamakiri-pages.site.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("apex records block missing %q:\n%s", want, got)
		}
	}
}
