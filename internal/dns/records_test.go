package dns

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// testLang is the catalog every test here renders against. A test that switches
// away restores this rather than a literal of its own, so the pin moves in one
// place.
const testLang = "en"

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under: every headline, status column and diagnosis this
// package renders comes from the message catalog. Load rather than Setup:
// nothing here reports which language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load(testLang)
	os.Exit(m.Run())
}

func TestFormatRecordsTable_NilDomain(t *testing.T) {
	if got := FormatRecordsTable(nil, FormatOpts{}); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestFormatRecordsTable_EmptyExpected(t *testing.T) {
	d := &api.Domain{Domain: "example.com"}
	if got := FormatRecordsTable(d, FormatOpts{}); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestFormatRecordsTable_SubdomainNoObserved(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp", Purpose: "primary", Required: true},
		},
	}
	want := "  www.example.com  CNAME  → site-abc.kamakiri.jp\n"
	if got := FormatRecordsTable(d, FormatOpts{}); got != want {
		t.Errorf("mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestFormatRecordsTable_SubdomainObservedMatches(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "www.example.com", Type: "cname", Values: []string{"site-abc.kamakiri.jp"}},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "www.example.com") || !strings.Contains(got, "✓ matches") {
		t.Errorf("expected ✓ matches row, got:\n%s", got)
	}
	if strings.Contains(got, "✗") {
		t.Errorf("unexpected ✗ in output:\n%s", got)
	}
}

// The server emits the expected CNAME as a dotted FQDN while real resolvers
// return the RDATA dot-less, so without normalization on both sides every
// correctly published domain would render as a mismatch.
func TestFormatRecordsTable_DottedExpectedDotlessObservedMatches(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "www.example.com", Type: "cname", Values: []string{"site-abc.kamakiri-pages.jp"}},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "✓ matches") {
		t.Errorf("dotted-expected + dot-less-observed must render ✓ matches, got:\n%s", got)
	}
	if strings.Contains(got, "✗") {
		t.Errorf("unexpected ✗ for correctly-published record:\n%s", got)
	}
}

// The same dotted-against-dot-less pair must yield no diff here either, or a
// propagated domain would be framed as pointing at the wrong target.
func TestWrongTargetDetail_DottedExpectedDotlessObservedIsClean(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "www.example.com", Type: "cname", Values: []string{"site-abc.kamakiri-pages.jp"}},
		},
	}
	if expected, got := WrongTargetDetail(d); expected != "" || got != nil {
		t.Errorf("dotted-expected + dot-less-observed must yield no wrong-target diff, got (%q, %v)", expected, got)
	}
}

// Normalization must not mask a real mismatch: these differ with or without the
// trailing dot, so each still yields the expected-against-got diff.
func TestWrongTargetDetail_GenuinelyWrongUnaffectedByDotNorm(t *testing.T) {
	cases := []struct {
		name     string
		observed []string
	}{
		{"cloudflare-proxy-ip", []string{"104.21.5.5"}},
		{"a-instead-of-cname", []string{"203.0.113.9"}},
		{"appended-zone", []string{"site-abc.kamakiri-pages.jp.www.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &api.Domain{
				Domain: "www.example.com",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri-pages.jp.", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "www.example.com", Type: "cname", Values: tc.observed},
				},
			}
			expected, got := WrongTargetDetail(d)
			if expected != "site-abc.kamakiri-pages.jp." {
				t.Errorf("%s: expected target = %q, want the dotted CNAME", tc.name, expected)
			}
			if len(got) != 1 || got[0] != tc.observed[0] {
				t.Errorf("%s: observed diff = %v, want %v", tc.name, got, tc.observed)
			}
		})
	}
}

func TestFormatRecordsTable_SubdomainObservedMismatch(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "www.example.com", Type: "cname", Values: []string{"wrong-target.example.org"}},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "✗ got wrong-target.example.org") {
		t.Errorf("expected ✗ got <wrong>, got:\n%s", got)
	}
}

func TestFormatRecordsTable_NoObservation(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp", Purpose: "primary", Required: true},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "⧗ not yet visible") {
		t.Errorf("expected ⧗ not yet visible, got:\n%s", got)
	}
}

// The transient-error annotation wins even though a prior good value is still
// stored, because the server could not confirm that value on this pass.
func TestFormatRecordsTable_SubdomainObservedUnreachable(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{
				Name:           "www.example.com",
				Type:           "cname",
				Values:         []string{"site-abc.kamakiri.jp"},
				ObserveError:   "servfail",
				ObserveErrorAt: time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "⚠ couldn't check (servfail, ~4m ago)") {
		t.Errorf("expected ⚠ couldn't check with reason and age, got:\n%s", got)
	}
	if strings.Contains(got, "✓ matches") {
		t.Errorf("a transient error must not render ✓ matches (the value is stale), got:\n%s", got)
	}
}

// An unusable timestamp drops the age rather than fabricating "~0m ago".
func TestFormatRecordsTable_SubdomainObservedUnreachableNoTimestamp(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp", Purpose: "primary", Required: true},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "www.example.com", Type: "cname", ObserveError: "timeout"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "⚠ couldn't check (timeout)") {
		t.Errorf("expected ⚠ couldn't check (timeout), got:\n%s", got)
	}
	if strings.Contains(got, "ago") {
		t.Errorf("a sub-minute/unset error_at must not render an age, got:\n%s", got)
	}
}

func TestFormatRecordsTable_ApexWithAlternativesAllOK(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "a", Value: "203.0.113.2", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "a", Values: []string{"203.0.113.1", "203.0.113.2"}, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "alias_or_aname", Values: nil, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	for _, want := range []string{
		"Required records (pick ONE option):",
		"Option A: ALIAS or ANAME (if your DNS provider supports it)",
		"Option B: A records",
		"203.0.113.1",
		"203.0.113.2",
		"fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.",
		"✓ matches",
		"(alternative to the A records above)",
		"Both options work.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "✗") {
		t.Errorf("unexpected ✗ in happy-path output:\n%s", got)
	}
	if strings.Contains(got, "(recommended)") || strings.Contains(got, "(fallback)") {
		t.Errorf("expected no recommended/fallback suffixes (both options equal), got:\n%s", got)
	}
}

func TestFormatRecordsTable_ApexWithAlternativesPartialMatch(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "a", Value: "203.0.113.2", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "a", Values: []string{"203.0.113.1", "192.0.2.99"}, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "alias_or_aname", Values: nil, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "✓ matches") {
		t.Errorf("expected ✓ matches for 203.0.113.1, got:\n%s", got)
	}
	if !strings.Contains(got, "✗ got 192.0.2.99") {
		t.Errorf("expected ✗ got 192.0.2.99 for 203.0.113.2, got:\n%s", got)
	}
}

func TestFormatRecordsTable_ValidationBlock(t *testing.T) {
	d := &api.Domain{
		Domain: "www.example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "www.example.com", Type: "cname", Value: "abc.cloudflare.net", Purpose: "primary", Required: true},
			{Name: "_cf-custom-hostname.www.example.com", Type: "txt", Value: "ca3-validation-token", Purpose: "validation", Required: false},
			{Name: "_acme-challenge.www.example.com", Type: "cname", Value: "www.example.com.uuid.dcv.cloudflare.com.", Purpose: "validation", Required: false},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{})
	for _, want := range []string{
		"www.example.com",
		"abc.cloudflare.net",
		"Validation records (add temporarily):",
		"_cf-custom-hostname.www.example.com",
		"ca3-validation-token",
		"_acme-challenge.www.example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// An apex with A records and no ALIAS member cannot occur in production, since
// the server always emits one. This covers the renderer's defensive fallback
// for that shape, which must collapse to a flat block like any single option.
func TestFormatRecordsTable_ApexOnlyARecords(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "a", Value: "203.0.113.2", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "a", Values: []string{"203.0.113.1", "203.0.113.2"}, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	for _, banned := range []string{"pick ONE option", "Option A"} {
		if strings.Contains(got, banned) {
			t.Errorf("expected no %q with single-option group, got:\n%s", banned, got)
		}
	}
	for _, want := range []string{"203.0.113.1", "203.0.113.2", "✓ matches"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestFormatRecordsTable_ApexAliasOnlyNoObserved(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{})
	for _, want := range []string{
		"Required record:",
		"example.com",
		"ALIAS",
		"fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	for _, banned := range []string{
		"pick ONE option",
		"Option A",
		"A records are not supported",
		"not supported in this deployment",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in ALIAS-only output:\n%s", banned, got)
		}
	}
}

func TestFormatRecordsTable_ApexAliasOnlyObservedMatch(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "alias_or_aname", Values: nil, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "a", Values: []string{"133.242.23.108"}, AliasResolved: []string{"133.242.23.108"}, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "✓ matches") {
		t.Errorf("expected ✓ matches on ALIAS row, got:\n%s", got)
	}
	if strings.Contains(got, "pick ONE option") {
		t.Errorf("unexpected 'pick ONE option' framing in ALIAS-only output:\n%s", got)
	}
	if strings.Contains(got, "✗") {
		t.Errorf("unexpected ✗ in happy-path output:\n%s", got)
	}
}

// The renderer must not assume a sibling A observation exists just because the
// expected record is alias_or_aname.
func TestFormatRecordsTable_ApexAliasOnlyObservedNotYet(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "alias_or_aname", Values: nil, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "⧗ not yet visible") {
		t.Errorf("expected ⧗ not yet visible, got:\n%s", got)
	}
	if strings.Contains(got, "✓") || strings.Contains(got, "✗") {
		t.Errorf("unexpected ✓/✗ in awaiting output:\n%s", got)
	}
}

// The ALIAS row applies the same transient-error precedence observedStatus
// does, so the two renderers never disagree on one unreachable A observation.
func TestFormatRecordsTable_ApexAliasOnlyObservedUnreachable(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "a", ObserveError: "timeout", ObserveErrorAt: time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "⚠ couldn't check (timeout, ~3m ago)") {
		t.Errorf("expected ⚠ couldn't check on the ALIAS row, got:\n%s", got)
	}
	if strings.Contains(got, "✓") || strings.Contains(got, "✗") {
		t.Errorf("a transient error must not render ✓/✗, got:\n%s", got)
	}
}

func TestFormatRecordsTable_ApexAliasOnlyObservedMismatch(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "alias_or_aname", Values: nil, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "a", Values: []string{"1.2.3.4"}, AliasResolved: []string{"133.242.23.108"}, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if !strings.Contains(got, "✗ got 1.2.3.4") {
		t.Errorf("expected ✗ got 1.2.3.4, got:\n%s", got)
	}
	if strings.Contains(got, "✓ matches") {
		t.Errorf("unexpected ✓ matches in mismatch output:\n%s", got)
	}
}

// Covering only part of the target's resolved set must read ✗, not a premature
// ✓: an any-overlap rule here would contradict the server's headline verdict
// within the same status block.
func TestFormatRecordsTable_ApexAliasOnlyPartialCoverageIsMismatch(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
		DNSRecordsObserved: []api.DNSRecordObserved{
			{Name: "example.com", Type: "alias_or_aname", Values: nil, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "a", Values: []string{"133.242.23.108"}, AliasResolved: []string{"133.242.23.108", "133.242.23.109"}, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{ShowObserved: true})
	if strings.Contains(got, "✓ matches") {
		t.Errorf("partial coverage must not read ✓ matches, got:\n%s", got)
	}
	if !strings.Contains(got, "✗ got 133.242.23.108") {
		t.Errorf("expected ✗ surfacing the observed IP, got:\n%s", got)
	}
}

// The three probe outcomes this test pins are one catalog lookup each, sitting
// in the same status column, so a substring check on the marker alone cannot
// tell them apart. These cases pin whole blocks: the ALIAS row whose A
// observation came back empty, the CNAME row whose observation came back
// empty, and the record whose observed value belongs to a sibling of the same
// name and type rather than to this row.
func TestObservedStatusBlocksRenderWhole(t *testing.T) {
	empty := []string{}

	tests := []struct {
		name   string
		domain *api.Domain
		want   string
	}{
		{
			name: "an ALIAS apex whose A observation came back empty",
			domain: &api.Domain{
				Domain: "example.com",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "example.com", Type: "alias_or_aname", Value: "site.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
				},
				// The observation must be the sibling (name, "a") row: an
				// alias_or_aname one is not what the ALIAS status column reads,
				// and would leave this case on the no-observation branch instead.
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "example.com", Type: "a", Values: nil, AliasResolved: []string{"203.0.113.10"}},
				},
			},
			want: "  Required record:\n\n" +
				"    example.com  ALIAS  → site.kamakiri-pages.site.  ⧗ not yet visible\n\n",
		},
		{
			name: "a CNAME whose observation came back empty",
			domain: &api.Domain{
				Domain: "www.example.com",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "www.example.com", Type: "cname", Value: "site.kamakiri-pages.site", Purpose: "primary", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "www.example.com", Type: "cname", Values: empty},
				},
			},
			want: "  www.example.com  CNAME  → site.kamakiri-pages.site  ⧗ not yet visible\n",
		},
		{
			name: "a record whose only observed value belongs to its sibling",
			domain: &api.Domain{
				Domain: "example.com",
				DNSRecordsExpected: []api.DNSRecord{
					{Name: "_acme-challenge.example.com", Type: "txt", Value: "one", Purpose: "validation", Required: true},
					{Name: "_acme-challenge.example.com", Type: "txt", Value: "two", Purpose: "validation", Required: true},
				},
				DNSRecordsObserved: []api.DNSRecordObserved{
					{Name: "_acme-challenge.example.com", Type: "txt", Values: []string{"two"}},
				},
			},
			want: "  Validation records (add temporarily):\n" +
				"    _acme-challenge.example.com  TXT  → one  ✗ not present\n" +
				"    _acme-challenge.example.com  TXT  → two  ✓ matches\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatRecordsTable(tt.domain, FormatOpts{ShowObserved: true}); got != tt.want {
				t.Errorf("FormatRecordsTable() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

// Both shapes a registrar's zone-append produces reach the same diagnosis, and
// the second one, where the typed target and the zone overlap rather than abut,
// is otherwise indistinguishable from a plain wrong target.
func TestDiagnoseWrongTargetNamesTheZoneAppend(t *testing.T) {
	const want = "looks like the zone name was appended; enter the target as the fully-qualified name shown above (trailing dot included)"

	for _, got := range []string{"site.kamakiri-pages.site.example.com.", "site.kamakiri-pages.site-x.example.com."} {
		if diagnosis := DiagnoseWrongTarget("example.com", "site.kamakiri-pages.site.", []string{got}); diagnosis != want {
			t.Errorf("DiagnoseWrongTarget(%q) = %q, want %q", got, diagnosis, want)
		}
	}
}

// The ordering is presentation-only: neither option is marked recommended.
func TestFormatRecordsTable_OptionOrderingStable(t *testing.T) {
	d := &api.Domain{
		Domain: "example.com",
		DNSRecordsExpected: []api.DNSRecord{
			{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			{Name: "example.com", Type: "alias_or_aname", Value: "fzks-qpb6r87d79wfblfj.c1.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
		},
	}
	got := FormatRecordsTable(d, FormatOpts{})
	idxA := strings.Index(got, "Option A: ALIAS or ANAME (if your DNS provider supports it)")
	idxB := strings.Index(got, "Option B: A records")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("missing Option headers in:\n%s", got)
	}
	if idxA >= idxB {
		t.Errorf("Option A (ALIAS/ANAME) should appear before Option B (A records), got A at %d, B at %d:\n%s", idxA, idxB, got)
	}
	if !strings.Contains(got, "Both options work.") {
		t.Errorf("expected closing 'Both options work.' paragraph, got:\n%s", got)
	}
}

func TestWriteValidationRecords_FiltersAndCounts(t *testing.T) {
	records := []api.DNSRecord{
		{Name: "www.example.com", Type: "cname", Value: "abc.cloudflare.net", Purpose: "primary", Required: true},
		{Name: "_cf-custom-hostname.www.example.com", Type: "txt", Value: "ca3-token", Purpose: "validation", Required: false},
		{Name: "_acme-challenge.www.example.com", Type: "cname", Value: "www.example.com.uuid.dcv.cloudflare.com.", Purpose: "validation", Required: false},
	}

	var buf bytes.Buffer
	n := WriteValidationRecords(&buf, records)

	if n != 2 {
		t.Errorf("line count = %d, want 2", n)
	}
	out := buf.String()
	if strings.Contains(out, "abc.cloudflare.net") {
		t.Errorf("primary record leaked into validation rendering: %q", out)
	}
	for _, want := range []string{
		"add TXT", // uppercase preserved (server sends lowercase)
		"_cf-custom-hostname.www.example.com",
		"ca3-token",
		"add CNAME",
		"_acme-challenge.www.example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
}

func TestWriteValidationRecords_NoValidation(t *testing.T) {
	records := []api.DNSRecord{
		{Name: "www.example.com", Type: "cname", Value: "abc", Purpose: "primary", Required: true},
	}
	var buf bytes.Buffer
	if n := WriteValidationRecords(&buf, records); n != 0 {
		t.Errorf("line count = %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output, got %q", buf.String())
	}
}

func TestDeliveryObserveError(t *testing.T) {
	at := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)

	t.Run("nil domain", func(t *testing.T) {
		if r, a := DeliveryObserveError(nil); r != "" || a != "" {
			t.Errorf("nil domain = (%q, %q), want empty", r, a)
		}
	})

	t.Run("subdomain CNAME error", func(t *testing.T) {
		d := &api.Domain{
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp.", Purpose: "primary", Required: true},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "www.example.com", Type: "cname", ObserveError: "servfail", ObserveErrorAt: at},
			},
		}
		if r, a := DeliveryObserveError(d); r != "servfail" || a != at {
			t.Errorf("subdomain = (%q, %q), want (servfail, %q)", r, a, at)
		}
	})

	t.Run("apex reads the A observation, not the ALIAS placeholder", func(t *testing.T) {
		d := &api.Domain{
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "example.com", Type: "alias_or_aname", Value: "tgt.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
				{Name: "example.com", Type: "a", Value: "203.0.113.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "example.com", Type: "a", ObserveError: "timeout", ObserveErrorAt: at},
			},
		}
		if r, a := DeliveryObserveError(d); r != "timeout" || a != at {
			t.Errorf("apex = (%q, %q), want (timeout, %q)", r, a, at)
		}
	})

	t.Run("healthy delivery record yields no error", func(t *testing.T) {
		d := &api.Domain{
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp.", Purpose: "primary", Required: true},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "www.example.com", Type: "cname", Values: []string{"site-abc.kamakiri.jp"}},
			},
		}
		if r, a := DeliveryObserveError(d); r != "" || a != "" {
			t.Errorf("healthy = (%q, %q), want empty", r, a)
		}
	})

	t.Run("error on a non-delivery validation record is ignored", func(t *testing.T) {
		d := &api.Domain{
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "www.example.com", Type: "cname", Value: "site-abc.kamakiri.jp.", Purpose: "primary", Required: true},
				{Name: "_cf-custom-hostname.www.example.com", Type: "txt", Value: "tok", Purpose: "validation", Required: false},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "www.example.com", Type: "cname", Values: []string{"site-abc.kamakiri.jp"}},
				{Name: "_cf-custom-hostname.www.example.com", Type: "txt", ObserveError: "servfail", ObserveErrorAt: at},
			},
		}
		if r, a := DeliveryObserveError(d); r != "" || a != "" {
			t.Errorf("non-delivery error leaked = (%q, %q), want empty", r, a)
		}
	})
}

// splitRecordRow cuts a rendered record row at the padding that separates the
// value cell from the status cell, returning everything tabwriter aligned and
// the status it left alone. A row with no status comes back whole.
func splitRecordRow(row string) (aligned, status string) {
	arrow := strings.Index(row, "→")
	if arrow < 0 {
		return row, ""
	}
	gap := strings.Index(row[arrow:], "  ")
	if gap < 0 {
		return row, ""
	}
	end := arrow + gap
	for end < len(row) && row[end] == ' ' {
		end++
	}
	return row[:end], row[end:]
}

// These tables keep text/tabwriter where the `kamakiri deploys` listing could
// not, and this is what says they may: the only cell that changes width with the
// language is the status, which is the last cell in its row and so is never
// padded. Everything tabwriter does align is a hostname, a record type and an
// ASCII value behind an arrow, one column per rune in either language.
func TestRecordTablesKeepTheirColumnsInJapanese(t *testing.T) {
	shapes := map[string]*api.Domain{
		"alternative group": {
			Domain: "example.com",
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "example.com", Type: "alias_or_aname", Value: "edge.kamakiri.jp.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
				{Name: "example.com", Type: "a", Value: "192.0.2.1", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
				{Name: "example.com", Type: "a", Value: "203.0.113.77", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "example.com", Type: "a", Values: []string{"192.0.2.1"}, AliasResolved: []string{"192.0.2.1"}},
			},
		},
		"alias-only apex": {
			Domain: "example.com",
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "example.com", Type: "alias_or_aname", Value: "edge.kamakiri.jp.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "example.com", Type: "a", ObserveError: "servfail", ObserveErrorAt: time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339)},
			},
		},
		"flat with validation": {
			Domain: "shop.example.com",
			DNSRecordsExpected: []api.DNSRecord{
				{Name: "shop.example.com", Type: "cname", Value: "edge.kamakiri.jp.", Purpose: "primary", Required: true},
				{Name: "_acme-challenge.shop.example.com", Type: "txt", Value: "abc123", Purpose: "validation", Required: true},
				{Name: "_cf.shop.example.com", Type: "txt", Value: "cf-token", Purpose: "validation", Required: true},
			},
			DNSRecordsObserved: []api.DNSRecordObserved{
				{Name: "shop.example.com", Type: "cname", Values: []string{"someone-else.example.net."}},
				{Name: "_cf.shop.example.com", Type: "txt", Values: []string{"cf-token"}},
			},
		},
	}

	for name, d := range shapes {
		t.Run(name, func(t *testing.T) {
			english := strings.Split(FormatRecordsTable(d, FormatOpts{ShowObserved: true}), "\n")
			i18n.Load("ja")
			japanese := strings.Split(FormatRecordsTable(d, FormatOpts{ShowObserved: true}), "\n")
			i18n.Load(testLang)

			if len(english) != len(japanese) {
				t.Fatalf("line count: en %d, ja %d", len(english), len(japanese))
			}

			translated := 0
			for i := range english {
				enAligned, enStatus := splitRecordRow(english[i])
				jaAligned, jaStatus := splitRecordRow(japanese[i])
				if enStatus == "" {
					continue
				}
				if enAligned != jaAligned {
					t.Errorf("line %d: the aligned cells moved with the language:\n en %q\n ja %q", i, enAligned, jaAligned)
				}
				if jaStatus == enStatus {
					t.Errorf("line %d: the status column is still English: %q", i, jaStatus)
					continue
				}
				translated++
				if i18n.Width(jaStatus) == len([]rune(jaStatus)) {
					t.Errorf("line %d: the Japanese status %q is one column per rune, so this table proves nothing about wide cells", i, jaStatus)
				}
			}
			if translated == 0 {
				t.Fatal("no row carried a status, so nothing about the status column was checked")
			}

			// Within one rendered table the status cells of a block start at
			// one column: that is what tabwriter is still being trusted for.
			for _, lines := range [][]string{english, japanese} {
				offsets := map[int]int{}
				for i, line := range lines {
					aligned, status := splitRecordRow(line)
					if status == "" {
						continue
					}
					offsets[i] = i18n.Width(aligned)
				}
				var previous, previousIndex = -1, -2
				for i := range lines {
					offset, ok := offsets[i]
					if !ok {
						continue
					}
					if i == previousIndex+1 && offset != previous {
						t.Errorf("line %d starts its status at column %d, the row above at %d:\n%q\n%q",
							i, offset, previous, lines[i-1], lines[i])
					}
					previous, previousIndex = offset, i
				}
			}
		})
	}
}

// WriteValidationRecords writes tab-bearing rows into a caller's writer, and
// the instruction word ahead of the record type is catalog copy: it is wider in
// Japanese than tabwriter's rune count says, so a caller mixing these rows with
// its own in one tabwriter has to lay that column out itself.
func TestValidationRecordsCarryALocalizedFirstCell(t *testing.T) {
	records := []api.DNSRecord{
		{Name: "_acme-challenge.shop.example.com", Type: "txt", Value: "abc123", Purpose: "validation", Required: true},
		{Name: "shop.example.com", Type: "cname", Value: "edge.kamakiri.jp.", Purpose: "primary", Required: true},
	}

	var english bytes.Buffer
	if n := WriteValidationRecords(&english, records); n != 1 {
		t.Fatalf("wrote %d rows, want 1: only the validation record belongs here", n)
	}
	if got, want := english.String(), "    add TXT\t_acme-challenge.shop.example.com → abc123\n"; got != want {
		t.Errorf("english row = %q, want %q", got, want)
	}

	i18n.Load("ja")
	var japanese bytes.Buffer
	WriteValidationRecords(&japanese, records)
	i18n.Load(testLang)

	cell, _, found := strings.Cut(japanese.String(), "\t")
	if !found {
		t.Fatalf("the japanese row carries no tab: %q", japanese.String())
	}
	if i18n.Width(cell) == len([]rune(cell)) {
		t.Errorf("the japanese first cell %q is one column per rune, so nothing here is at risk", cell)
	}
}
