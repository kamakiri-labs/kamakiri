package cdn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// These tests pin the glue that composes a provider switch, not the two legs
// themselves, which have their own suites. They drive the real legs kept short:
// the exit takes its non-TTY resume path, which returns without a watch loop,
// and the go-live converges on its first poll with the certificate gate
// disabled.

// forceSwitchStreamable opens the gate that would otherwise require a real
// terminal, so a switch can be composed against a buffer.
func forceSwitchStreamable(t *testing.T) {
	t.Helper()
	prev := switchStreamable
	t.Cleanup(func() { switchStreamable = prev })
	switchStreamable = func(bool, io.Writer) bool { return true }
}

// stubSignalCtxBackground removes the real signal handler from the picture.
func stubSignalCtxBackground(t *testing.T) {
	t.Helper()
	prev := signalCtx
	t.Cleanup(func() { signalCtx = prev })
	signalCtx = func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	}
}

func mustContainInOrder(t *testing.T, s string, subs ...string) {
	t.Helper()
	idx := 0
	for _, sub := range subs {
		i := strings.Index(s[idx:], sub)
		if i < 0 {
			t.Errorf("missing or out-of-order %q in:\n%s", sub, s)
			return
		}
		idx += i + len(sub)
	}
}

func waContentDomain(name string) api.DNSRecord {
	return api.DNSRecord{Name: name, Type: "cname", Value: "ind.kamakiri-pages.site."}
}

// The cleanup line must come only after the second leg converges: naming an
// orphan while traffic is still moving would send the user to delete a live
// resource.
func TestSwitchWebAccelToCloudflareComposes(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvSkipCertCheck, "1")
	forceSwitchStreamable(t)
	stubSignalCtxBackground(t)

	var setModes []string
	client := &scriptClient{
		mockClient: mockClient{
			setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
				setModes = append(setModes, mode)
				return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
			},
			listDomainsFn: func(string) (*api.DomainList, error) {
				return &api.DomainList{Domains: []api.Domain{{
					Domain:             "shop.example.com",
					Role:               "canonical",
					DNSRecordsExpected: []api.DNSRecord{waContentDomain("shop.example.com")},
				}}}, nil
			},
		},
		frames: []*api.CDNStatusResponse{
			{CDNMode: "webaccel", Provider: "webaccel", Domains: []api.CDNDomainStatus{
				{Domain: "shop.example.com", CdnState: CdnStateActive, CDNID: "wa-123"}}},
			{CDNMode: "cloudflare", Provider: "cloudflare", Domains: []api.CDNDomainStatus{
				{Domain: "shop.example.com", CdnState: CdnStateActive, DnsVerdict: "correct"}}},
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "cloudflare", false, &out); err != nil {
		t.Fatalf("switch returned error: %v", err)
	}
	got := out.String()

	// The set milestone is matched whole, terminator included: the
	// no-content-domains variant of it opens with the same words and only
	// differs after them.
	mustContainInOrder(t, got,
		"Switching CDN from WebAccel to Cloudflare. This routes through direct serving so your site never drops.",
		"✓ CDN deactivated",          // leg 1
		"✓ CDN set to Cloudflare\n",  // leg 2 mode set
		"✓ live via Cloudflare",      // leg 2 converged
		"Run `kamakiri cdn cleanup`", // switch-aware cleanup, after leg 2
	)
	if !strings.Contains(got, "The old WebAccel resource(s) for shop.example.com are no longer used.") {
		t.Errorf("missing switch-aware cleanup line: %q", got)
	}
	// The standalone orphan copy tells the user to repoint back to origin, which
	// is the opposite of what a switch is doing.
	if strings.Contains(got, "repoint your DNS off them first") {
		t.Errorf("inSwitch must suppress the standalone orphan copy: %q", got)
	}
	if len(setModes) != 2 || setModes[0] != "none" || setModes[1] != "cloudflare" {
		t.Errorf("SetCDN modes = %v, want [none cloudflare]", setModes)
	}
	assertCleanCopy(t, got)
}

// Leaving Cloudflare needs nothing from the customer and leaves them nothing to
// clean up: both the indirection and the orphaned hostname are ours.
func TestSwitchCloudflareToWebAccelComposes(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvSkipCertCheck, "1")
	forceSwitchStreamable(t)
	stubSignalCtxBackground(t)

	var setWAcalled bool
	client := &scriptClient{
		mockClient: mockClient{
			setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
				return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
			},
			setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
				setWAcalled = true
				return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 2}, nil
			},
			listDomainsFn: func(string) (*api.DomainList, error) {
				return &api.DomainList{Domains: []api.Domain{{
					Domain: "shop.example.com", Role: "canonical"}}}, nil
			},
		},
		frames: []*api.CDNStatusResponse{
			{CDNMode: "cloudflare", Provider: "cloudflare", Domains: []api.CDNDomainStatus{
				{Domain: "shop.example.com", CdnState: CdnStateActive, DnsVerdict: "correct"}}},
			{CDNMode: "webaccel", Provider: "webaccel", Domains: []api.CDNDomainStatus{{
				Domain: "shop.example.com", CdnState: CdnStateActive,
				WebaccelSubdomain:         "ubol.user.webaccel.jp",
				WebaccelOwnershipStatus:   "verified",
				WebaccelEnabledObservedAt: "2026-01-01T00:00:00Z",
				WebaccelRoutedObservedAt:  "2026-01-01T00:00:00Z",
			}}},
		},
	}

	var out bytes.Buffer
	if err := SetWebAccel(client, WebAccelOptions{Token: "t", Secret: "s"}, false, nil, &out); err != nil {
		t.Fatalf("switch returned error: %v", err)
	}
	got := out.String()

	mustContainInOrder(t, got,
		"Switching CDN from Cloudflare to WebAccel. This routes through direct serving so your site never drops.",
		"✓ CDN deactivated",               // leg 1
		"⧗ switching traffic to our edge", // leg 1 cloudflare exit (our-side re-point)
		"CDN set to WebAccel.",            // leg 2 mode set
		"✓ live via WebAccel",             // leg 2 converged
	)
	if !setWAcalled {
		t.Error("leg 2 must call SetCDNWebAccel")
	}
	if strings.Contains(got, "kamakiri cdn cleanup") {
		t.Errorf("cloudflare → webaccel switch must not surface cleanup: %q", got)
	}
	assertCleanCopy(t, got)
}

// The refusal has to land before anything is written, or the site would be torn
// off a working provider for a switch that can never complete.
func TestSwitchToCloudflareRefusedOnApex(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var setCalled bool
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
				Domains: []api.CDNDomainStatus{{Domain: "example.com", CdnState: CdnStateActive, CDNID: "wa-1"}}}, nil
		},
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			setCalled = true
			return nil, nil
		},
	}

	var out bytes.Buffer
	err := SetMode(client, "cloudflare", false, &out)
	if err == nil {
		t.Fatal("a switch to cloudflare on an apex must be refused (non-zero)")
	}
	if setCalled {
		t.Error("apex refusal must touch nothing (no SetCDN)")
	}
	got := out.String()
	if !strings.Contains(got, "We don't support Cloudflare as a CDN for an apex domain (example.com)") {
		t.Errorf("missing apex refusal guidance: %q", got)
	}
	// The refusal is already on stdout, so the returned error is marked as
	// reported and the caller exits non-zero without echoing it a second time.
	if !errors.Is(err, ErrCdnReported) {
		t.Errorf("apex refusal error = %v, want it to wrap ErrCdnReported", err)
	}
	assertCleanCopy(t, got)
}

// A switch that cannot be streamed queues only its first leg. Queueing the flip
// itself would fail later with nobody watching.
func TestSwitchToCloudflareDegrades(t *testing.T) {
	cases := []struct {
		name   string
		noWait bool
	}{
		{"no-wait", true},
		{"non-tty", false}, // bytes.Buffer is non-TTY, so the default gate degrades
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProjectConfig(t, "site123")

			var setModes []string
			client := &mockClient{
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
					return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
						Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateActive}}}, nil
				},
				setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
					setModes = append(setModes, mode)
					return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
				},
			}

			var out bytes.Buffer
			if err := SetMode(client, "cloudflare", tc.noWait, &out); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, "Setting CDN to none is queued.") {
				t.Errorf("degrade must queue none: %q", got)
			}
			if !strings.Contains(got, "run: kamakiri cdn cloudflare") {
				t.Errorf("degrade must name the provider command to run once direct: %q", got)
			}
			if len(setModes) != 1 || setModes[0] != "none" {
				t.Errorf("SetCDN modes = %v, want [none] (no direct provider flip queued)", setModes)
			}
			assertCleanCopy(t, got)
		})
	}
}

// The degrade must happen before credentials are resolved: this path never uses
// them, so prompting for a token here would ask for nothing.
func TestSwitchToWebAccelDegrades(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var setModes []string
	var setWAcalled bool
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare",
				Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateActive}}}, nil
		},
		setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
			setModes = append(setModes, mode)
			return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
		},
		setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
			setWAcalled = true
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := SetWebAccel(client, WebAccelOptions{}, true, nil, &out); err != nil {
		t.Fatalf("degrade must not error on missing creds: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Setting CDN to none is queued.") {
		t.Errorf("degrade must queue none: %q", got)
	}
	if !strings.Contains(got, "run: kamakiri cdn webaccel") {
		t.Errorf("degrade must name the webaccel command to run once direct: %q", got)
	}
	if len(setModes) != 1 || setModes[0] != "none" {
		t.Errorf("SetCDN modes = %v, want [none] (no direct provider flip queued)", setModes)
	}
	if setWAcalled {
		t.Error("degrade must not call SetCDNWebAccel")
	}
	assertCleanCopy(t, got)
}

// One interrupt region spans both legs, so a Ctrl-C in the second still prints
// detach copy. The new provider is already the desired state by then.
func TestSwitchCtrlCInLeg2(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	forceSwitchStreamable(t)

	// The context is cancelled from inside the second leg's poll, so the first
	// completes normally and only the second sees the interrupt.
	ctx, cancel := context.WithCancel(context.Background())
	prevSig := signalCtx
	t.Cleanup(func() { signalCtx = prevSig })
	signalCtx = func() (context.Context, context.CancelFunc) { return ctx, func() {} }

	var setModes []string
	var statusCalls int
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			statusCalls++
			if statusCalls == 1 {
				return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
					Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateActive}}}, nil
			}
			// Cancel, then report a still-pending frame, so the watch has to take
			// the cancellation arm rather than finishing.
			cancel()
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare",
				Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateAwaitingCFValidation}}}, nil
		},
		setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
			setModes = append(setModes, mode)
			return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{{Domain: "shop.example.com", Role: "canonical"}}}, nil
		},
	}

	var out bytes.Buffer
	err := SetMode(client, "cloudflare", false, &out)
	if !errors.Is(err, ErrCdnWatchInterrupted) {
		t.Fatalf("want ErrCdnWatchInterrupted (exit 130), got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Stopped watching. Your CDN change is still being applied.") {
		t.Errorf("missing leg-2 detach copy: %q", got)
	}
	if len(setModes) != 2 || setModes[0] != "none" || setModes[1] != "cloudflare" {
		t.Errorf("SetCDN modes = %v, want [none cloudflare] (desired state on new provider)", setModes)
	}
}

func TestExitToDirectInSwitchSuppressesOrphan(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	mk := func() *mockClient {
		return &mockClient{
			setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
				return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 1}, nil
			},
			listDomainsFn: func(string) (*api.DomainList, error) {
				return &api.DomainList{Domains: []api.Domain{{Domain: "shop.example.com", Role: "canonical"}}}, nil
			},
		}
	}
	prior := &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
		Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateActive, CDNID: "wa-1"}}}

	const orphanCopy = "repoint your DNS off them first"

	var inSwitchOut bytes.Buffer
	if _, _, err := exitToDirect(context.Background(), mk(), "site123", prior, false, true, &inSwitchOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(inSwitchOut.String(), orphanCopy) {
		t.Errorf("inSwitch=true must suppress the orphan copy:\n%s", inSwitchOut.String())
	}

	var standaloneOut bytes.Buffer
	if _, _, err := exitToDirect(context.Background(), mk(), "site123", prior, false, false, &standaloneOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(standaloneOut.String(), orphanCopy) {
		t.Errorf("inSwitch=false must print the orphan copy:\n%s", standaloneOut.String())
	}
}

// A refusal is not the CDN change failing, and the fix copy names commands the
// same floor refuses, so the exit prints neither and the sentinel travels
// unmarked for the command layer to render as the upgrade message. The status
// line is all that is left, rewound in place where the run is interactive so
// the upgrade message lands on a clean terminal.
func TestExitToDirectAbortsOnAVersionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
	}{
		{name: "non-interactive", interactive: false},
		{name: "interactive", interactive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockClient{
				setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
					return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 1}, nil
				},
				waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
					return nil, api.ErrUpgradeRequired
				},
			}

			var out bytes.Buffer
			_, _, err := exitToDirect(context.Background(), client, "site123", nil, tc.interactive, false, &out)
			if !errors.Is(err, api.ErrUpgradeRequired) {
				t.Fatalf("err = %v, want the refusal returned bare", err)
			}
			// main echoes this error verbatim, so anything wrapped around the
			// sentinel prints in front of the upgrade copy.
			if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
				t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
			}
			// ErrCdnReported suppresses the stderr echo, which is the only place
			// the upgrade message would appear.
			if errors.Is(err, ErrCdnReported) {
				t.Errorf("the refusal must not be marked as already reported: %v", err)
			}
			got := out.String()
			for _, banned := range []string{
				i18n.T("cdn.change_failed"),
				i18n.T("cdn.change_failed_fix"),
				i18n.T("cdn.change_queued_status"),
				i18n.T("cdn.deactivated"),
			} {
				if strings.Contains(got, banned) {
					t.Errorf("output must not carry %q:\n%s", banned, got)
				}
			}
			// The ⧗ line is the whole of the output: interactively it is rewound
			// where the settled glyph would have gone, and off a TTY it stays,
			// terminated, with nothing under it.
			want := i18n.T("cdn.deactivating") + "\n"
			if tc.interactive {
				want += rewind(1)
			}
			if got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
			assertCleanCopy(t, got)
		})
	}
}

// An exit with nothing to bring back settles instead of streaming, and the
// switch must still proceed: having nothing to climb is not a failure.
func TestSwitchSourceNeverLiveStillProceedsToLeg2(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvSkipCertCheck, "1")
	forceSwitchStreamable(t)
	stubSignalCtxBackground(t)

	var setModes []string
	client := &scriptClient{
		mockClient: mockClient{
			setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
				setModes = append(setModes, mode)
				return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
			},
			listDomainsFn: func(string) (*api.DomainList, error) {
				return &api.DomainList{Domains: []api.Domain{{Domain: "go.example.com", Role: "redirect"}}}, nil
			},
		},
		frames: []*api.CDNStatusResponse{
			{CDNMode: "webaccel", Provider: "webaccel"},
			{CDNMode: "cloudflare", Provider: "cloudflare"},
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "cloudflare", false, &out); err != nil {
		t.Fatalf("switch returned error: %v", err)
	}
	got := out.String()
	mustContainInOrder(t, got,
		"Switching CDN from WebAccel to Cloudflare.",
		"✓ CDN deactivated",                       // leg 1 settled one-shot
		"no content-serving domains to route yet", // leg 2 converged with no content
	)
	if strings.Contains(got, "Configure your DNS") {
		t.Errorf("a source-never-live leg 1 must settle one-shot (no records block): %q", got)
	}
	if len(setModes) != 2 || setModes[0] != "none" || setModes[1] != "cloudflare" {
		t.Errorf("SetCDN modes = %v, want [none cloudflare]", setModes)
	}
	assertCleanCopy(t, got)
}

// WebAccel can serve an apex, so a switch toward it must never inherit the
// apex refusal that guards the switch toward Cloudflare.
func TestSwitchToWebAccelNotRefusedOnApex(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvSkipCertCheck, "1")
	forceSwitchStreamable(t)
	stubSignalCtxBackground(t)

	var setWAcalled bool
	client := &scriptClient{
		mockClient: mockClient{
			setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
				return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
			},
			setCDNWebAccelFn: func(_, _, _ string) (*api.CDNSetResponse, error) {
				setWAcalled = true
				return &api.CDNSetResponse{CDNMode: "webaccel", SyncAttemptID: 2}, nil
			},
			listDomainsFn: func(string) (*api.DomainList, error) {
				return &api.DomainList{Domains: []api.Domain{{Domain: "example.com", Role: "canonical"}}}, nil
			},
		},
		frames: []*api.CDNStatusResponse{
			{CDNMode: "cloudflare", Provider: "cloudflare", Domains: []api.CDNDomainStatus{
				{Domain: "example.com", CdnState: CdnStateActive, DnsVerdict: "correct"}}},
			{CDNMode: "webaccel", Provider: "webaccel", Domains: []api.CDNDomainStatus{{
				Domain: "example.com", CdnState: CdnStateActive,
				WebaccelSubdomain:         "ubol.user.webaccel.jp",
				WebaccelOwnershipStatus:   "verified",
				WebaccelEnabledObservedAt: "2026-01-01T00:00:00Z",
				WebaccelRoutedObservedAt:  "2026-01-01T00:00:00Z",
			}}},
		},
	}

	var out bytes.Buffer
	if err := SetWebAccel(client, WebAccelOptions{Token: "t", Secret: "s"}, false, nil, &out); err != nil {
		t.Fatalf("switch returned error: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "We don't support Cloudflare as a CDN for an apex") {
		t.Errorf("a switch to webaccel must not apex-refuse: %q", got)
	}
	if !strings.Contains(got, "Switching CDN from Cloudflare to WebAccel") {
		t.Errorf("an apex webaccel switch must still compose: %q", got)
	}
	if !setWAcalled {
		t.Error("leg 2 must call SetCDNWebAccel (no apex refusal on the webaccel target)")
	}
}

func TestSwitchNotTriggeredForNonProviderPriors(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var setModes []string
	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
		setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
			setModes = append(setModes, mode)
			return &api.CDNSetResponse{CDNMode: mode, SyncAttemptID: 1}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "cloudflare", true, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "Switching CDN from") {
		t.Errorf("a none → cloudflare set must not print a switch lead-in: %q", got)
	}
	if len(setModes) != 1 || setModes[0] != "cloudflare" {
		t.Errorf("SetCDN modes = %v, want [cloudflare] (single leg, no none flip)", setModes)
	}
}
