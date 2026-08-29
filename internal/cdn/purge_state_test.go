package cdn

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
)

func TestFormatPurgeStateLine_OkRendersEmpty(t *testing.T) {
	cases := []struct {
		name   string
		domain api.Domain
	}{
		{"explicit ok", api.Domain{CdnPurgeHealth: "ok"}},
		// An absent value is not the same fact as "ok", but it renders the same
		// way: silence beats guessing.
		{"empty (older server)", api.Domain{CdnPurgeHealth: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FormatPurgeStateLine(&c.domain); got != "" {
				t.Errorf("FormatPurgeStateLine(%q) = %q, want empty", c.domain.CdnPurgeHealth, got)
			}
		})
	}
}

func TestFormatPurgeStateLine_NilDomain(t *testing.T) {
	if got := FormatPurgeStateLine(nil); got != "" {
		t.Errorf("FormatPurgeStateLine(nil) = %q, want empty", got)
	}
}

func TestFormatPurgeStateLine_FailingHintsPerTag(t *testing.T) {
	cases := []struct {
		tag      string
		label    string
		hint     string
		suffixNo string // must not appear while merely failing
	}{
		{"http_401", "HTTP 401", "API key may be invalid", "rotate API key in dashboard"},
		{"http_403", "HTTP 403", "API key forbidden", "rotate API key in dashboard"},
		{"http_404", "HTTP 404", "site or zone not found", "re-provision"},
		// This tag has no suffix at all, so there is nothing to assert absent;
		// the empty value skips the negative check for this row.
		{"http_5xx_persistent", "HTTP 5xx", "likely transient", ""},
		{"network_timeout", "network timeout", "provider unreachable", "status page"},
		{"connection_refused", "connection refused", "rejected connection", "status page"},
		{"unknown", "unknown error", "purge failing for unknown reason", "contact support"},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			d := api.Domain{
				CdnPurgeHealth:              "failing",
				CdnPurgeErrorReason:         c.tag,
				CdnPurgeConsecutiveFailures: 3,
			}
			got := FormatPurgeStateLine(&d)
			mustContain(t, got, "  Purges: ⚠ failing")
			mustContain(t, got, "3 failures")
			mustContain(t, got, c.label)
			mustContain(t, got, c.hint)
			if c.suffixNo != "" && strings.Contains(got, c.suffixNo) {
				t.Errorf("failing(%s) = %q, leaks broken-state suffix %q", c.tag, got, c.suffixNo)
			}
		})
	}
}

// Once a row is broken the remediation replaces the hint: at that point the
// user needs the fix, not the description.
func TestFormatPurgeStateLine_BrokenSuffixReplacesHint(t *testing.T) {
	cases := []struct {
		tag      string
		label    string
		suffix   string
		hintGone string // failing-state hint that must NOT appear in broken state
	}{
		{"http_401", "HTTP 401", "rotate API key in dashboard", "API key may be invalid"},
		{"http_403", "HTTP 403", "rotate API key in dashboard", "API key forbidden"},
		{"http_404", "HTTP 404", "run `kamakiri cdn cloudflare` or `kamakiri cdn webaccel` to re-provision", "site or zone not found"},
		{"network_timeout", "network timeout", "check provider status page", "provider unreachable"},
		{"connection_refused", "connection refused", "check provider status page", "rejected connection"},
		{"unknown", "unknown error", "see server logs and contact support", "purge failing for unknown reason"},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			d := api.Domain{
				CdnPurgeHealth:      "broken",
				CdnPurgeErrorReason: c.tag,
			}
			got := FormatPurgeStateLine(&d)
			mustContain(t, got, "  Purges: ✗ broken")
			mustContain(t, got, c.label)
			mustContain(t, got, c.suffix)
			if strings.Contains(got, c.hintGone) {
				t.Errorf("broken(%s) = %q, still contains failing-state hint %q (suffix should replace)",
					c.tag, got, c.hintGone)
			}
		})
	}
}

// A tag with no remediation of its own falls back to its failing hint, since
// waiting is the only honest advice.
func TestFormatPurgeStateLine_BrokenWithoutSuffixFallsBackToHint(t *testing.T) {
	d := api.Domain{
		CdnPurgeHealth:      "broken",
		CdnPurgeErrorReason: "http_5xx_persistent",
	}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "✗ broken")
	mustContain(t, got, "HTTP 5xx")
	mustContain(t, got, "likely transient")
	// Impossible as written, since each tag reads only its own entry. The
	// negative assertion guards against a future fallback chain that shares
	// copy between tags.
	for _, foreignSuffix := range []string{
		"rotate API key in dashboard",
		"to re-provision",
		"check provider status page",
		"see server logs and contact support",
	} {
		if strings.Contains(got, foreignSuffix) {
			t.Errorf("broken(http_5xx_persistent) = %q, leaks foreign suffix %q", got, foreignSuffix)
		}
	}
}

// An old success alongside a recent failure: the age must follow the success,
// or it would reset on every retry.
func TestFormatPurgeStateLine_BrokenSinceAgeAnchorsOnLastOkAt(t *testing.T) {
	// Padded past the six-hour boundary because the rendering truncates: with
	// an exact six hours, sub-second loss and clock drift can render as five.
	sixHoursAgo := time.Now().Add(-6*time.Hour - 30*time.Second).UTC().Format(time.RFC3339)
	tenMinutesAgo := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	d := api.Domain{
		CdnPurgeHealth:       "broken",
		CdnPurgeErrorReason:  "http_401",
		CdnPurgeLastOkAt:     sixHoursAgo,
		CdnPurgeLastFailedAt: tenMinutesAgo,
	}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "since 6h ago")
}

// With no successful purge to anchor on, the "since" clause is dropped rather
// than guessed at.
func TestFormatPurgeStateLine_BrokenWithoutLastOkAtSkipsSinceClause(t *testing.T) {
	d := api.Domain{
		CdnPurgeHealth:      "broken",
		CdnPurgeErrorReason: "http_401",
		CdnPurgeLastOkAt:    "",
	}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "✗ broken")
	if strings.Contains(got, "since") {
		t.Errorf("broken(no last_ok_at) = %q, contains 'since' clause without an anchor", got)
	}
	mustContain(t, got, "rotate API key in dashboard")
}

// The raw tag must never reach the user: it is an internal enum, and seeing
// one only means their CLI is older than the server.
func TestFormatPurgeStateLine_UnknownTagFallsBackToUnknown(t *testing.T) {
	d := api.Domain{
		CdnPurgeHealth:              "failing",
		CdnPurgeErrorReason:         "future_tag_42",
		CdnPurgeConsecutiveFailures: 1,
	}
	got := FormatPurgeStateLine(&d)
	if strings.Contains(got, "future_tag_42") {
		t.Errorf("FormatPurgeStateLine(unknown tag) = %q, leaks raw tag", got)
	}
	mustContain(t, got, "unknown error")
	mustContain(t, got, "purge failing for unknown reason")
}

func TestFormatPurgeStateLine_EmptyTagFallsBackToUnknown(t *testing.T) {
	d := api.Domain{
		CdnPurgeHealth:              "failing",
		CdnPurgeErrorReason:         "",
		CdnPurgeConsecutiveFailures: 1,
	}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "unknown error")
}

// A value from a newer server passes through rather than vanishing, which
// would hide a real condition from the user.
func TestFormatPurgeStateLine_UnknownHealthRendersVerbatim(t *testing.T) {
	resetWarnedHealthsForTest()
	t.Cleanup(resetWarnedHealthsForTest)
	prev := unknownHealthWarnWriter
	unknownHealthWarnWriter = io.Discard
	t.Cleanup(func() { unknownHealthWarnWriter = prev })

	d := api.Domain{CdnPurgeHealth: "throttled"}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "throttled")
}

func TestUnknownPurgeHealthWarnsOnce(t *testing.T) {
	resetWarnedHealthsForTest()
	t.Cleanup(resetWarnedHealthsForTest)
	prev := unknownHealthWarnWriter
	unknownHealthWarnWriter = io.Discard
	t.Cleanup(func() { unknownHealthWarnWriter = prev })

	d := api.Domain{CdnPurgeHealth: "future_health_42"}
	out := FormatPurgeStateLine(&d)
	mustContain(t, out, "future_health_42")

	_ = FormatPurgeStateLine(&d)
	w := WarnedHealths()
	if len(w) != 1 {
		t.Errorf("WarnedHealths size after two calls with the same unknown health = %d, want 1", len(w))
	}
	if _, ok := w["future_health_42"]; !ok {
		t.Errorf("WarnedHealths missing future_health_42 entry: %v", w)
	}
}

func TestFormatPurgeStateLine_FailingWithoutCounter(t *testing.T) {
	d := api.Domain{
		CdnPurgeHealth:      "failing",
		CdnPurgeErrorReason: "http_401",
	}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "⚠ failing")
	mustContain(t, got, "HTTP 401")
	mustContain(t, got, "API key may be invalid")
	if strings.Contains(got, "0 failures") {
		t.Errorf("FormatPurgeStateLine(failing, no counter) = %q, should omit zero-count instead of '0 failures'", got)
	}
}

// Every tag here must render its own label and hint. Falling through to the
// generic unknown line would strip the user of the one detail they can act on.
func TestFormatPurgeStateLine_NewHintEntries(t *testing.T) {
	cases := []struct {
		tag   string
		label string
		hint  string
	}{
		{"provider_rejected", "provider rejected", "the CDN provider rejected the purge"},
		{"missing_credentials", "missing credentials", "no stored CDN credentials"},
		{"network_error", "network error", "provider unreachable"},
		{"lock_timeout", "lock timeout", "purge briefly contended"},
		{"http_408", "HTTP 408", "provider request timed out"},
		{"http_429", "HTTP 429", "rate-limited"},
		{"invalid_cdn_mode", "invalid CDN mode", "does not support purging"},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			d := api.Domain{CdnPurgeHealth: "failing", CdnPurgeErrorReason: c.tag, CdnPurgeConsecutiveFailures: 2}
			got := FormatPurgeStateLine(&d)
			mustContain(t, got, c.label)
			mustContain(t, got, c.hint)
			if strings.Contains(got, "unknown error") {
				t.Errorf("%s must not fall through to the unknown hint: %q", c.tag, got)
			}
		})
	}
}

// Each remediation must name a command that exists; the negative assertion
// pins one that does not.
func TestFormatPurgeStateLine_NewBrokenSuffixes(t *testing.T) {
	cases := []struct {
		tag    string
		suffix string
	}{
		{"missing_credentials", "kamakiri cdn credentials"},
		{"provider_rejected", "kamakiri status"},
		{"invalid_cdn_mode", "kamakiri cdn status"},
		{"http_404", "kamakiri cdn cloudflare"},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			d := api.Domain{CdnPurgeHealth: "broken", CdnPurgeErrorReason: c.tag}
			got := FormatPurgeStateLine(&d)
			mustContain(t, got, "✗ broken")
			mustContain(t, got, c.suffix)
			if strings.Contains(got, "cdn enable") {
				t.Errorf("%s must not name the nonexistent `cdn enable`: %q", c.tag, got)
			}
		})
	}
}

// A status with no entry of its own still shows its number, which is more use
// than "unknown error"; a non-numeric suffix has nothing to show and does not.
func TestFormatPurgeStateLine_UnlistedHttpStatusFallback(t *testing.T) {
	d := api.Domain{CdnPurgeHealth: "failing", CdnPurgeErrorReason: "http_422", CdnPurgeConsecutiveFailures: 1}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "HTTP 422")
	if strings.Contains(got, "unknown error") {
		t.Errorf("http_422 must render its status, not the unknown hint: %q", got)
	}

	d2 := api.Domain{CdnPurgeHealth: "failing", CdnPurgeErrorReason: "http_weird", CdnPurgeConsecutiveFailures: 1}
	got2 := FormatPurgeStateLine(&d2)
	mustContain(t, got2, "unknown error")
}

func TestFormatPurgeStateLine_SingularFailureGrammar(t *testing.T) {
	d := api.Domain{CdnPurgeHealth: "failing", CdnPurgeErrorReason: "http_401", CdnPurgeConsecutiveFailures: 1}
	got := FormatPurgeStateLine(&d)
	mustContain(t, got, "1 failure, last error")
	if strings.Contains(got, "1 failures") {
		t.Errorf("singular count must read '1 failure', got: %q", got)
	}
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("output = %q, missing substring %q", got, want)
	}
}
