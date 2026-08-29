package cdn

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
)

func TestCdnIsLive(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{CdnStateOff, false},
		{CdnStateAwaitingProvisioning, false},
		{CdnStateAwaitingCFValidation, false},
		{CdnStateReadyToFlip, false},
		{CdnStateActive, true},
		{"", false},
		{"some_future_state", false},
	}

	for _, c := range cases {
		got := CdnIsLive(c.state)
		if got != c.want {
			t.Errorf("CdnIsLive(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestFormatCdnStatusFromDomain(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	twoHoursAgo := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	thirtyMinAgo := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	cases := []struct {
		name        string
		domain      api.Domain
		mode        string
		wantPrefix  string
		wantContain []string
	}{
		{
			name:       "off renders empty",
			domain:     api.Domain{CdnState: CdnStateOff},
			mode:       "none",
			wantPrefix: "",
		},
		{
			name:       "empty cdn_state renders empty",
			domain:     api.Domain{CdnState: ""},
			mode:       "none",
			wantPrefix: "",
		},
		{
			name: "awaiting_provisioning under webaccel renders ⧗ provisioning",
			domain: api.Domain{
				CdnState: CdnStateAwaitingProvisioning,
			},
			mode:        "webaccel",
			wantContain: []string{"⧗ provisioning", "WebAccel"},
		},
		{
			name: "awaiting_verification under webaccel renders the NEUTRAL DNS-wait marker",
			// This shape carries no enabled latch, so it cannot tell the two
			// verification phases apart and must assert neither. Naming the wrong
			// one would send the user to republish a record they already have.
			domain: api.Domain{
				CdnState: CdnStateAwaitingVerification,
			},
			mode:       "webaccel",
			wantPrefix: "⧗ awaiting DNS (domain verification / delivery)",
		},
		{
			name: "unverified verdict (txt mismatch) surfaces the expected record",
			// Nothing times this gate out, so a typo'd record would wait forever
			// unless the line shows the value it should have carried.
			domain: api.Domain{
				CdnState:  CdnStateAwaitingVerification,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictUnverified, Reason: "webaccel_txt_mismatch"},
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "_webaccel.example.com", Type: "txt", Value: "webaccel=ou8mw1sw", Purpose: "ownership"},
				},
			},
			mode: "webaccel",
			wantContain: []string{
				"✗", "_webaccel.example.com", "doesn't match", `"webaccel=ou8mw1sw"`,
			},
		},
		{
			name: "awaiting_cf_validation renders awaiting marker",
			domain: api.Domain{
				CdnState:  CdnStateAwaitingCFValidation,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictPending, Since: thirtyMinAgo},
			},
			mode:        "cloudflare",
			wantPrefix:  "⧗ awaiting CF validation",
			wantContain: []string{"awaiting CF validation"},
		},
		{
			name: "awaiting_cf_validation > 2h prints non-defensive long-running note",
			domain: api.Domain{
				CdnState:  CdnStateAwaitingCFValidation,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictPending, Since: twoHoursAgo},
			},
			mode:        "cloudflare",
			wantContain: []string{"still validating"},
		},
		{
			name: "awaiting_cf_validation age uses cdn_health.since not cf_last_checked_at",
			// A recent probe time alongside an older entry time: the age must
			// follow the entry, or it would ratchet down on every check.
			domain: api.Domain{
				CdnState:        CdnStateAwaitingCFValidation,
				CdnHealth:       api.CdnHealth{Verdict: CdnVerdictPending, Since: thirtyMinAgo},
				CfLastCheckedAt: now,
			},
			mode:        "cloudflare",
			wantContain: []string{"30m"},
		},
		{
			name:       "ready_to_flip renders DNS-flip-pending marker",
			domain:     api.Domain{CdnState: CdnStateReadyToFlip, CdnHealth: api.CdnHealth{Verdict: CdnVerdictPending}},
			mode:       "cloudflare",
			wantPrefix: "⧗ DNS flip pending",
		},
		{
			name: "ready_to_flip under webaccel stays on the neutral provisioning line",
			// The customer owns the delivery record under BYO WebAccel, so a
			// pending-flip line would claim a DNS change we do not make.
			domain:     api.Domain{CdnState: CdnStateReadyToFlip, CdnHealth: api.CdnHealth{Verdict: CdnVerdictPending}},
			mode:       "webaccel",
			wantPrefix: "⧗ provisioning WebAccel",
		},
		{
			name: "active via cloudflare renders ✓",
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictHealthy},
			},
			mode:        "cloudflare",
			wantContain: []string{"✓ via Cloudflare"},
		},
		{
			name: "active via webaccel renders a real live ✓ (cert-confirmed under BYO)",
			// The server reaches this position only after seeing the certificate
			// issued, so the tick is earned rather than hedged.
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictHealthy},
			},
			mode:        "webaccel",
			wantContain: []string{"✓ via WebAccel"},
		},
		{
			name: "misconfigured verdict surfaces an actionable panel line",
			// Only the user can clear this, so the row must name the fix rather
			// than spin on a position it will never leave by itself.
			domain: api.Domain{
				CdnState:  CdnStateAwaitingProvisioning,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictMisconfigured, Reason: "webaccel_panel_misconfigured"},
			},
			mode:        "webaccel",
			wantContain: []string{"✗", "WebAccel panel", "domain type"},
		},
		{
			name: "degraded surfaces unhealthy timestamp from cdn_health.since",
			// The row stays at the live position and the verdict is what claims
			// it, so the age must come from the verdict's own timestamp.
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictDegraded, Since: thirtyMinAgo},
			},
			mode:        "cloudflare",
			wantContain: []string{"⚠ Cloudflare reports unhealthy", "since", "30m"},
		},
		{
			name: "webaccel_delivery_lost surfaces the actionable re-point line with the target",
			// The whole line is pinned, trimmed trailing dot included, so that a
			// rewrite blaming the customer or their registrar fails here.
			domain: api.Domain{
				CdnState: CdnStateActive,
				CdnHealth: api.CdnHealth{
					Verdict: CdnVerdictDegraded,
					Reason:  "webaccel_delivery_lost",
					Since:   thirtyMinAgo,
				},
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "abc123.user.webaccel.jp.", Purpose: "primary", Required: true},
				},
			},
			mode:       "webaccel",
			wantPrefix: "⚠ www.example.com no longer points to abc123.user.webaccel.jp; re-point the delivery record to restore the site",
		},
		{
			name: "webaccel_delivery_lost falls back to a generic re-point line without the expected record",
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictDegraded, Reason: "webaccel_delivery_lost"},
			},
			mode:        "webaccel",
			wantContain: []string{"⚠", "re-point", "delivery record", "WebAccel subdomain"},
		},
		{
			name: "cloudflare_delivery_lost surfaces the actionable re-point line with the target",
			// The same line as the WebAccel case above, with only the target
			// differing: the copy carries no provider of its own.
			domain: api.Domain{
				CdnState: CdnStateActive,
				CdnHealth: api.CdnHealth{
					Verdict: CdnVerdictDegraded,
					Reason:  "cloudflare_delivery_lost",
					Since:   thirtyMinAgo,
				},
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "www-example-com.abc.kamakiri-pages.site.", Purpose: "primary", Required: true},
				},
			},
			mode:       "cloudflare",
			wantPrefix: "⚠ www.example.com no longer points to www-example-com.abc.kamakiri-pages.site; re-point the delivery record to restore the site",
		},
		{
			name: "cloudflare_delivery_lost falls back to the Cloudflare-specific re-point line without the expected record",
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictDegraded, Reason: "cloudflare_delivery_lost"},
			},
			mode:        "cloudflare",
			wantContain: []string{"⚠", "re-point", "Cloudflare target"},
		},
		{
			name: "timed_out (CF validation give-up) renders ✗ with reason",
			domain: api.Domain{
				CdnState:  CdnStateAwaitingCFValidation,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictTimedOut, Reason: "validation_timeout"},
			},
			mode:        "cloudflare",
			wantContain: []string{"✗ validation_timeout"},
		},
		{
			name: "timed_out (WebAccel post-delivery enable give-up) renders ✗, not a spinner",
			// A timed_out verdict outranks the pending position, so the row renders
			// ✗ rather than the provisioning spinner the position alone would show.
			domain: api.Domain{
				CdnState:  CdnStateAwaitingProvisioning,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictTimedOut, Reason: "webaccel_enable_failed_post_verify"},
			},
			mode:        "webaccel",
			wantContain: []string{"✗ webaccel_enable_failed_post_verify"},
		},
		{
			name: "broken (resource gone) renders ✗ with reason",
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictBroken, Reason: "webaccel_site_deleted"},
			},
			mode:        "webaccel",
			wantContain: []string{"✗ webaccel_site_deleted"},
		},
		{
			name: "broken with no reason falls back to a generic line",
			domain: api.Domain{
				CdnState:  CdnStateActive,
				CdnHealth: api.CdnHealth{Verdict: CdnVerdictBroken},
			},
			mode:        "cloudflare",
			wantContain: []string{"✗ CDN provisioning failed"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatCdnStatusFromDomain(c.domain, c.mode)
			if c.wantPrefix != "" && !strings.HasPrefix(got, c.wantPrefix) {
				t.Errorf("FormatCdnStatusFromDomain(%v, %q) = %q, want prefix %q",
					c.domain.CdnState, c.mode, got, c.wantPrefix)
			}
			for _, want := range c.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("FormatCdnStatusFromDomain(%v, %q) = %q, want substring %q",
						c.domain.CdnState, c.mode, got, want)
				}
			}
		})
	}
}

func TestFormatCdnStatusFromCDN(t *testing.T) {
	d := api.CDNDomainStatus{
		Domain:   "example.com",
		CdnState: CdnStateActive,
	}

	got := FormatCdnStatusFromCDN(d, "cloudflare")
	if !strings.Contains(got, "✓ via Cloudflare") {
		t.Errorf("FormatCdnStatusFromCDN(active) = %q, want substring ✓ via Cloudflare", got)
	}

	lost := api.CDNDomainStatus{
		Domain:   "www.example.com",
		CdnState: CdnStateActive,
		CdnHealth: api.CdnHealth{
			Verdict: CdnVerdictDegraded,
			Reason:  "webaccel_delivery_lost",
		},
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "abc123.user.webaccel.jp.", Purpose: "primary", Required: true},
		},
	}
	if got := FormatCdnStatusFromCDN(lost, "webaccel"); !strings.Contains(got, "www.example.com no longer points to abc123.user.webaccel.jp") {
		t.Errorf("FormatCdnStatusFromCDN(delivery_lost) = %q, want the re-point line with the target", got)
	}
}

// One position covers two different customer-DNS waits, and the enabled latch
// is the only fact that separates them: unset means phase 1's ownership record
// is outstanding, set means phase 2's delivery record is.
func TestFormatCdnStatusAwaitingVerificationDisambiguation(t *testing.T) {
	notEnabled := api.CDNDomainStatus{
		Domain:   "next.example.com",
		CdnState: CdnStateAwaitingVerification,
	}
	if got := FormatCdnStatusFromCDN(notEnabled, "webaccel"); !strings.Contains(got, "publish the TXT") {
		t.Errorf("enabled-latch null must render the publish-the-TXT copy; got %q", got)
	}

	enabled := api.CDNDomainStatus{
		Domain:                    "next.example.com",
		CdnState:                  CdnStateAwaitingVerification,
		WebaccelEnabledObservedAt: "2026-05-24T00:00:00Z",
	}
	got := FormatCdnStatusFromCDN(enabled, "webaccel")
	if !strings.Contains(got, "delivery CNAME") {
		t.Errorf("enabled-latch set must render the delivery-CNAME copy; got %q", got)
	}
	if strings.Contains(got, "publish the TXT") {
		t.Errorf("enabled-latch set must NOT mention the TXT; got %q", got)
	}
}

func TestUnknownStateWarnsOnce(t *testing.T) {
	resetWarnedStatesForTest()
	t.Cleanup(resetWarnedStatesForTest)
	prev := unknownStateWarnWriter
	unknownStateWarnWriter = io.Discard
	t.Cleanup(func() { unknownStateWarnWriter = prev })

	d := api.Domain{CdnState: "future_state_42"}
	out := FormatCdnStatusFromDomain(d, "cloudflare")
	if out != "future_state_42" {
		t.Errorf("FormatCdnStatusFromDomain(unknown) = %q, want pass-through %q", out,
			"future_state_42")
	}

	_ = FormatCdnStatusFromDomain(d, "cloudflare")
	w := WarnedStates()
	if len(w) != 1 {
		t.Errorf("WarnedStates size after two calls with the same unknown state = %d, want 1", len(w))
	}
	if _, ok := w["future_state_42"]; !ok {
		t.Errorf("WarnedStates missing future_state_42 entry: %v", w)
	}
}
