package cdn

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/domain"
)

// Returning early means no watch, not no reads at all: one status read still
// happens up front to detect a provider switch.
func TestSetModeCloudflareNoWaitExitsAfter202(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var statusCalls int
	client := &mockClient{
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "cloudflare", SyncAttemptID: 1}, nil
		},
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			statusCalls++
			return &api.CDNStatusResponse{}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "cloudflare", true, &out); err != nil {
		t.Fatal(err)
	}
	// Exactly one read: the watch would have polled many times.
	if statusCalls != 1 {
		t.Fatalf("CDNStatus called %d times, want exactly 1 (switch detection, no watch)", statusCalls)
	}
	got := out.String()
	// The whole line, terminator included: the no-content-domains variant of
	// this milestone opens with the same words and only differs after them.
	if !strings.Contains(got, "✓ CDN set to Cloudflare\n") {
		t.Errorf("missing set milestone: %q", got)
	}
	if strings.Contains(got, "✓ live via") {
		t.Errorf("--no-wait must not reach a go-live milestone: %q", got)
	}
	if !strings.Contains(got, "kamakiri cdn verify") {
		t.Errorf("missing resume hint: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestSetModeNoneOneShot(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var waitCalled bool
	client := &mockClient{
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 42}, nil
		},
		waitForSyncFn: func(siteID string, since int64, _ time.Duration) (*api.Site, error) {
			waitCalled = true
			if since != 42 {
				t.Errorf("sinceAttemptID = %d, want 42", since)
			}
			return &api.Site{ID: siteID}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	if !waitCalled {
		t.Fatal("none must wait for the pre-watch sync")
	}
	got := out.String()
	if !strings.Contains(got, "✓ CDN deactivated") {
		t.Errorf("missing deactivated line: %q", got)
	}
	if !strings.Contains(got, "Traffic goes directly to origin.") {
		t.Errorf("missing origin line: %q", got)
	}
	assertCleanCopy(t, got)
}

// Turning the CDN off must never delete the customer's own resource, so the
// exit names it and the command that would.
func TestSetModeNoneFromWebAccelSurfacesCleanup(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
				Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CDNID: "123456", CdnState: CdnStateActive}}}, nil
		},
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 7}, nil
		},
		waitForSyncFn: func(siteID string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{ID: siteID}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"✓ CDN deactivated",
		"The WebAccel resource(s) for shop.example.com remain in your Sakura account.",
		"kamakiri cdn cleanup",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing cleanup-surfacing content %q in:\n%s", want, got)
		}
	}
	assertCleanCopy(t, got)
}

// The domain still resolves to WebAccel until the customer repoints, so this
// exit must show them the record to change and must not claim traffic has
// already returned to origin.
func TestSetModeNoneFromWebAccelShowsRevertRecord(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "webaccel", Provider: "webaccel",
				Domains: []api.CDNDomainStatus{{Domain: "next.example.com", CdnState: CdnStateActive}}}, nil
		},
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 5}, nil
		},
		waitForSyncFn: func(siteID string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{ID: siteID}, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{{
				Domain: "next.example.com",
				Role:   "canonical",
				DNSRecordsExpected: []api.DNSRecord{{
					Name:  "next.example.com",
					Type:  "cname",
					Value: "h1lc-59d2pp7ubge7v16z.c2.kamakiri-pages.site.",
				}},
			}}}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"✓ CDN deactivated",
		"Configure your DNS:",
		"h1lc-59d2pp7ubge7v16z.c2.kamakiri-pages.site.",
		"kamakiri domain verify next.example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing revert view content %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Traffic goes directly to origin") {
		t.Errorf("BYO teardown must not claim instant fallback:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// Leaving Cloudflare needs no DNS change from the customer, since the record
// they point at is ours to repoint. There is nothing to show them and nothing
// instant to claim: the climb back still has to happen.
func TestSetModeNoneFromCloudflareStreamsExit(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare",
				Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateActive}}}, nil
		},
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 8}, nil
		},
		waitForSyncFn: func(siteID string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{ID: siteID}, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "shop.example.com", Role: "canonical"},
			}}, nil
		},
	}

	// A buffer is not a terminal, so the exit takes its resume path: the lead-in
	// and a status hint, which is testable without running the watch loop.
	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ CDN deactivated") {
		t.Errorf("missing deactivated line: %q", got)
	}
	if !strings.Contains(got, "switching traffic to our edge") {
		t.Errorf("missing the our-edge lead-in: %q", got)
	}
	if strings.Contains(got, "Traffic goes directly to origin") {
		t.Errorf("cloudflare exit must not claim the one-shot fallback:\n%s", got)
	}
	if strings.Contains(got, "Configure your DNS") {
		t.Errorf("cloudflare exit must not show a records block (no customer DNS step):\n%s", got)
	}
	if !strings.Contains(got, "kamakiri status") {
		t.Errorf("missing the status resume hint: %q", got)
	}
	assertCleanCopy(t, got)
}

// With nothing fronted there is no climb to stream, and the exit must still
// say something rather than stopping after the deactivation line.
func TestSetModeNoneFromCloudflareNoContentSettles(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}, nil
		},
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 9}, nil
		},
		waitForSyncFn: func(siteID string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{ID: siteID}, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			return &api.DomainList{Domains: []api.Domain{
				{Domain: "go.example.com", Role: "redirect"},
			}}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ CDN deactivated") {
		t.Errorf("missing deactivated line: %q", got)
	}
	if !strings.Contains(got, "Traffic goes directly to origin.") {
		t.Errorf("a no-content cloudflare exit must settle with the direct-serving line:\n%s", got)
	}
	if strings.Contains(got, "switching traffic to our edge") {
		t.Errorf("no content: must not print the streamed exit lead-in:\n%s", got)
	}
	assertCleanCopy(t, got)
}

// Re-running the command on a site already serving directly has nothing to
// repoint on either side, so it settles at once.
func TestSetModeNoneIdempotentReRunIsOneShot(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none", Provider: ""}, nil
		},
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 11}, nil
		},
		waitForSyncFn: func(siteID string, _ int64, _ time.Duration) (*api.Site, error) {
			return &api.Site{ID: siteID}, nil
		},
		listDomainsFn: func(string) (*api.DomainList, error) {
			t.Fatal("a none-prior re-run must not fetch the domain list (no exit to stream)")
			return nil, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ CDN deactivated") {
		t.Errorf("missing deactivated line: %q", got)
	}
	if !strings.Contains(got, "Traffic goes directly to origin.") {
		t.Errorf("a none-prior re-run must take the one-shot fallback: %q", got)
	}
	if strings.Contains(got, "switching traffic to our edge") || strings.Contains(got, "Configure your DNS") {
		t.Errorf("a none-prior re-run must not enter either exit flow:\n%s", got)
	}
	assertCleanCopy(t, got)
}

func TestSetModeNoneNoWaitQueued(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var setCDNCalls int
	client := &mockClient{
		setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
			setCDNCalls++
			if mode != "none" {
				t.Errorf("mode = %q, want none", mode)
			}
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 1}, nil
		},
	}
	var out bytes.Buffer
	if err := SetMode(client, "none", true, &out); err != nil {
		t.Fatal(err)
	}
	// Exactly one flip: the streaming exit leg would have issued its own.
	if setCDNCalls != 1 {
		t.Errorf("SetCDN called %d times, want exactly 1 on the --no-wait queue-only path", setCDNCalls)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("missing queued hint: %q", out.String())
	}
	assertCleanCopy(t, out.String())
}

func TestSetModeNonePreWatchSyncTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 99}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			return nil, api.ErrSyncTimeout
		},
	}
	var out bytes.Buffer
	err := SetMode(client, "none", false, &out)
	if !errors.Is(err, api.ErrSyncTimeout) {
		t.Fatalf("err = %v, want ErrSyncTimeout", err)
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("missing queued hint: %q", out.String())
	}
	assertCleanCopy(t, out.String())
}

// A failure that is not a timeout prints its own actionable copy, so the error
// must be marked reported: the user should see one report, not two.
func TestSetModeNoneReconcileError(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 99}, nil
		},
		waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
			return nil, errors.New("reconcile failed: internal error")
		},
	}
	var out bytes.Buffer
	err := SetMode(client, "none", false, &out)
	if err == nil {
		t.Fatal("expected error for a failed deactivation")
	}
	if errors.Is(err, api.ErrSyncTimeout) {
		t.Errorf("deactivation failure must not be ErrSyncTimeout: %v", err)
	}
	if !errors.Is(err, ErrCdnReported) {
		t.Errorf("deactivation failure must wrap ErrCdnReported (so cdnExit suppresses the stderr echo); got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "CDN change failed.") || !strings.Contains(got, "Re-run `kamakiri cdn none`") {
		t.Errorf("missing actionable failure copy: %q", got)
	}
	assertCleanCopy(t, got)
}

// An interrupt before the exit watch starts must still print copy rather than
// dying silently. The stop line is generic at this point because no records
// have been shown and the domains are not yet known, which is why the prior
// provider makes no difference to it.
func TestSetModeNoneSIGINTDuringDeactivationInterrupts(t *testing.T) {
	for _, prior := range []string{"webaccel", "cloudflare"} {
		t.Run(prior, func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupCredentials(t)
			setupProjectConfig(t, "site123")

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // SIGINT already delivered before the wait starts.

			prevSig := signalCtx
			t.Cleanup(func() { signalCtx = prevSig })
			signalCtx = func() (context.Context, context.CancelFunc) {
				return ctx, func() {}
			}

			syncReleased := make(chan struct{})
			t.Cleanup(func() { close(syncReleased) })
			client := &mockClient{
				cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
					return &api.CDNStatusResponse{CDNMode: prior, Provider: prior,
						Domains: []api.CDNDomainStatus{{Domain: "shop.example.com", CdnState: CdnStateActive}}}, nil
				},
				setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
					return &api.CDNSetResponse{CDNMode: "none", SyncAttemptID: 3}, nil
				},
				waitForSyncFn: func(string, int64, time.Duration) (*api.Site, error) {
					// Blocks until teardown, modelling a long wait the user gives up
					// on, so the already-cancelled context is the only way out.
					<-syncReleased
					return &api.Site{}, nil
				},
				listDomainsFn: func(string) (*api.DomainList, error) {
					t.Fatal("interrupt during deactivation must not enter the exit watch")
					return nil, nil
				},
			}

			var out bytes.Buffer
			err := SetMode(client, "none", false, &out)
			if !errors.Is(err, domain.ErrWatchInterrupted) {
				t.Fatalf("want domain.ErrWatchInterrupted (exit 130), got %v", err)
			}
			got := out.String()
			if !strings.Contains(got, "⧗ Deactivating CDN") {
				t.Errorf("missing the deactivation progress line: %q", got)
			}
			if !strings.Contains(got, "Stopped. The CDN change continues server-side; run `kamakiri status` to confirm.") {
				t.Errorf("missing the generic detach line: %q", got)
			}
			if strings.Contains(got, "✓ CDN deactivated") {
				t.Errorf("must not settle the deactivated line after an interrupt: %q", got)
			}
			if strings.Contains(got, "switching traffic to our edge") || strings.Contains(got, "Configure your DNS") {
				t.Errorf("interrupt during deactivation must not enter either exit flow: %q", got)
			}
			assertCleanCopy(t, got)
		})
	}
}

func TestSetModeNoProject(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{}
	var out bytes.Buffer
	if err := SetMode(client, "none", true, &out); err == nil {
		t.Fatal("expected error")
	}
}

// assertCleanCopy holds CDN output to the copy standard: no em-dash, which
// reads as machine-generated, and no blaming the user or their providers.
func assertCleanCopy(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "—") {
		t.Errorf("user-facing copy contains an em-dash: %q", out)
	}
	// Only fault-attributing phrasings are banned. Naming the registrar as the
	// place to add a record is an instruction, not blame.
	for _, bad := range []string{
		"some DNS providers take hours",
		"that part's not on us",
		"not on us",
		"not our fault",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("user-facing copy is defensive / attributes fault (%q): %q", bad, out)
		}
	}
}

// The server owns this refusal and builds the copy, so the CLI surfaces its
// message verbatim. Nothing may claim success alongside it.
func TestSetModeCloudflareApexRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	// The server's copy, verbatim; it has to stay in step with the server's.
	msg := "We don't support Cloudflare as a CDN for an apex domain (kamakiri-labs.jp) at this time. " +
		"Use a subdomain instead, e.g. www.kamakiri-labs.jp, and point the apex at it with a redirect."

	client := &mockClient{
		setCDNFn: func(_, _ string) (*api.CDNSetResponse, error) {
			return nil, &api.ErrorResponse{Code: "apex_unsupported_for_cloudflare", Message: msg}
		},
	}

	var out bytes.Buffer
	err := SetMode(client, "cloudflare", false, &out)
	if err == nil {
		t.Fatal("apex rejection must surface a non-nil error (non-zero exit)")
	}
	if !strings.Contains(err.Error(), "www.kamakiri-labs.jp") {
		t.Errorf("surfaced error missing the www. suggestion: %v", err)
	}
	if got := out.String(); strings.Contains(got, "✓") {
		t.Errorf("no ✓ milestone may print on rejection: %q", got)
	}
	assertCleanCopy(t, err.Error())
}

func TestWriteApexARecordsBlockEmpty(t *testing.T) {
	var out bytes.Buffer
	WriteApexARecordsBlock(&out, nil)
	if got := out.String(); got != "" {
		t.Fatalf("WriteApexARecordsBlock with empty input: want empty, got %q", got)
	}
}

func TestWriteApexARecordsBlockRendersOneApex(t *testing.T) {
	var out bytes.Buffer
	WriteApexARecordsBlock(&out, []api.ApexARecordsToUpdate{
		{Domain: "example.com", ExpectedARecords: []string{"173.245.48.0", "103.21.244.0"}},
	})
	got := out.String()
	if !strings.Contains(got, "example.com  A  → 173.245.48.0") {
		t.Errorf("missing first IP line: %q", got)
	}
	if !strings.Contains(got, "example.com  A  → 103.21.244.0") {
		t.Errorf("missing second IP line: %q", got)
	}
	if !strings.Contains(got, "Apex domains using ALIAS or ANAME need no action") {
		t.Errorf("missing ALIAS-no-action framing: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestWriteApexARecordsBlockMultiApex(t *testing.T) {
	var out bytes.Buffer
	WriteApexARecordsBlock(&out, []api.ApexARecordsToUpdate{
		{Domain: "example.com", ExpectedARecords: []string{"1.2.3.4"}},
		{Domain: "other.org", ExpectedARecords: []string{"5.6.7.8"}},
	})
	got := out.String()
	if !strings.Contains(got, "example.com  A  → 1.2.3.4") {
		t.Errorf("missing example.com IP line: %q", got)
	}
	if !strings.Contains(got, "other.org  A  → 5.6.7.8") {
		t.Errorf("missing other.org IP line: %q", got)
	}
}

func TestWriteApexARecordsPendingBlockEmpty(t *testing.T) {
	var out bytes.Buffer
	WriteApexARecordsPendingBlock(&out, nil)
	if got := out.String(); got != "" {
		t.Fatalf("WriteApexARecordsPendingBlock with empty input: want empty, got %q", got)
	}
}

func TestWriteApexARecordsPendingBlockRendersDomains(t *testing.T) {
	var out bytes.Buffer
	WriteApexARecordsPendingBlock(&out, []api.ApexARecordsPending{
		{Domain: "example.com", Reason: "webaccel_pending"},
	})
	got := out.String()
	if !strings.Contains(got, "Apex domains using A records: IPs will be available once provisioning completes:") {
		t.Errorf("missing pending header: %q", got)
	}
	if !strings.Contains(got, "Re-run `kamakiri status` shortly") {
		t.Errorf("missing re-run footer: %q", got)
	}
	if !strings.Contains(got, "    example.com") {
		t.Errorf("missing domain entry: %q", got)
	}
	// The footer has to follow the entries, not precede them.
	headerIdx := strings.Index(got, "provisioning completes:")
	itemIdx := strings.Index(got, "    example.com")
	footerIdx := strings.Index(got, "Re-run `kamakiri status`")
	if !(headerIdx < itemIdx && itemIdx < footerIdx) {
		t.Errorf("layout order wrong (want header < item < footer): %q", got)
	}
	assertCleanCopy(t, got)
}

// End-to-end over both providers, from the server's response through to the
// rendered apex blocks.
func TestSetModeNoneWithApexIPs(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
			if mode != "none" {
				t.Errorf("mode = %q, want none", mode)
			}
			return &api.CDNSetResponse{
				CDNMode:       "none",
				SyncAttemptID: 1,
				ApexARecordsToUpdate: []api.ApexARecordsToUpdate{
					{Domain: "example.com", ExpectedARecords: []string{"203.0.113.1", "203.0.113.2"}},
				},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := SetMode(client, "none", false, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ CDN deactivated") {
		t.Errorf("missing deactivated line: %q", got)
	}
	if !strings.Contains(got, "example.com  A  → 203.0.113.1") {
		t.Errorf("missing cluster IP line: %q", got)
	}
	if !strings.Contains(got, "example.com  A  → 203.0.113.2") {
		t.Errorf("missing cluster IP line: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestSetModeCloudflareWithApexIPs(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		setCDNFn: func(_, mode string) (*api.CDNSetResponse, error) {
			if mode != "cloudflare" {
				t.Errorf("mode = %q, want cloudflare", mode)
			}
			return &api.CDNSetResponse{
				CDNMode:       "cloudflare",
				SyncAttemptID: 1,
				ApexARecordsToUpdate: []api.ApexARecordsToUpdate{
					{Domain: "example.com", ExpectedARecords: []string{"173.245.48.0", "103.21.244.0"}},
				},
			}, nil
		},
	}

	var out bytes.Buffer
	// Returning early keeps the watch out of it: this pins the rendering.
	if err := SetMode(client, "cloudflare", true, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ CDN set to Cloudflare") {
		t.Errorf("missing set milestone: %q", got)
	}
	if !strings.Contains(got, "example.com  A  → 173.245.48.0") {
		t.Errorf("missing CF anycast IP line: %q", got)
	}
	if !strings.Contains(got, "example.com  A  → 103.21.244.0") {
		t.Errorf("missing CF anycast IP line: %q", got)
	}
	assertCleanCopy(t, got)
}
