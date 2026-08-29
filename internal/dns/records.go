// Package dns renders the DNS records a customer must add at their registrar,
// annotated with what the most recent server-side probe saw. Every view that
// shows the full records table goes through FormatRecordsTable, so those views
// cannot drift from one another. The narrower blocks that list a subset are
// separate renderers, WriteValidationRecords below among them.
//
// The verdict is the server's. It sends the expected records, the observed
// values and `dns_verdict`; this package only renders the diff and never
// re-implements apex or alternative-group matching, so a per-row glyph cannot
// contradict the headline verdict.
package dns

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/timeago"
)

// FormatOpts controls FormatRecordsTable rendering.
type FormatOpts struct {
	// ShowObserved adds the per-row status column from the most recent probe.
	// Callers leave it off where an observed column would add nothing, such as
	// a domain whose records have not been probed yet.
	ShowObserved bool
}

// FormatRecordsTable returns an indented multi-line block listing the DNS
// records the customer must add at their registrar for `domain`, optionally
// annotated with the most recent probe's observed status. It returns the empty
// string when the domain has no expected records.
//
// The trailing newline is included, so a caller can Fprint the result with no
// separator handling of its own.
func FormatRecordsTable(domain *api.Domain, opts FormatOpts) string {
	if domain == nil || len(domain.DNSRecordsExpected) == 0 {
		return ""
	}

	primary, validation := splitByPurpose(domain.DNSRecordsExpected)
	expectedByKey := indexExpectedValues(domain.DNSRecordsExpected)

	var out bytes.Buffer
	if len(primary) > 0 {
		renderPrimary(&out, primary, domain.DNSRecordsObserved, expectedByKey, opts)
	}
	if len(validation) > 0 {
		renderValidation(&out, validation, domain.DNSRecordsObserved, expectedByKey, opts)
	}
	return out.String()
}

func splitByPurpose(records []api.DNSRecord) (primary, validation []api.DNSRecord) {
	for _, r := range records {
		if r.Purpose == "validation" {
			validation = append(validation, r)
		} else {
			primary = append(primary, r)
		}
	}
	return primary, validation
}

// trimDot strips a single trailing dot. The server emits expected CNAMEs as
// dotted FQDNs while real resolvers return dot-less RDATA, so every comparison
// between the two normalizes through here or false-negatives on a correctly
// published record.
func trimDot(s string) string {
	return strings.TrimSuffix(s, ".")
}

// indexExpectedValues maps "name|type" to that pair's expected values,
// trimDot-normalized. Lookups must normalize the observed value the same way.
func indexExpectedValues(records []api.DNSRecord) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, r := range records {
		key := r.Name + "|" + r.Type
		if _, ok := out[key]; !ok {
			out[key] = map[string]bool{}
		}
		out[key][trimDot(r.Value)] = true
	}
	return out
}

// renderPrimary renders the alternative groups first, then the standalone
// records. An apex arrives as a group, its A records against its ALIAS/ANAME
// entry; a subdomain arrives standalone, as a single CNAME.
func renderPrimary(out *bytes.Buffer, records []api.DNSRecord, observed []api.DNSRecordObserved, expectedByKey map[string]map[string]bool, opts FormatOpts) {
	groups := map[string][]api.DNSRecord{}
	groupOrder := []string{}
	var standalone []api.DNSRecord
	for _, r := range records {
		if r.AlternativeGroup == "" {
			standalone = append(standalone, r)
			continue
		}
		if _, ok := groups[r.AlternativeGroup]; !ok {
			groupOrder = append(groupOrder, r.AlternativeGroup)
		}
		groups[r.AlternativeGroup] = append(groups[r.AlternativeGroup], r)
	}

	for _, g := range groupOrder {
		renderAlternativeGroup(out, groups[g], observed, expectedByKey, opts)
	}
	if len(standalone) > 0 {
		renderFlatRecords(out, standalone, observed, expectedByKey, opts, "  ")
	}
}

// renderAlternativeGroup renders one alternative group as a "pick ONE option:"
// header over one sub-block per type. A group left with only its ALIAS bucket
// collapses to a "Required record:" frame instead, since "pick ONE option"
// misleads when there is only one. A lone A bucket takes neither frame and
// renders flat, a defensive fallback: the server always emits the ALIAS member,
// so only the ALIAS-only shape occurs.
func renderAlternativeGroup(out *bytes.Buffer, records []api.DNSRecord, observed []api.DNSRecordObserved, expectedByKey map[string]map[string]bool, opts FormatOpts) {
	byType := map[string][]api.DNSRecord{}
	for _, r := range records {
		byType[r.Type] = append(byType[r.Type], r)
	}
	types := orderedTypes(byType)

	if len(types) == 1 {
		if types[0] == "alias_or_aname" {
			renderAliasOnlyApex(out, byType[types[0]], observed, opts)
			return
		}
		renderFlatRecords(out, byType[types[0]], observed, expectedByKey, opts, "  ")
		return
	}

	fmt.Fprintln(out, i18n.T("dns.header_required_records"))
	fmt.Fprintln(out)

	// tabwriter measures a cell in runes and offers no width hook, so a column
	// whose cells differ in how many wide glyphs they carry comes out ragged.
	// This table keeps it anyway: every cell it pads is a hostname, a record
	// type or an arrow before an ASCII value, one column per rune in any
	// language, and the status cell that can carry Japanese is the last in its
	// line, which tabwriter never pads. The option header carries no tab at
	// all, so it passes through untouched however wide it renders.
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for i, t := range types {
		fmt.Fprintln(w, i18n.Tf("dns.option_line", optionLetter(i), optionTypeLabel(t)))
		for _, r := range byType[t] {
			row := fmt.Sprintf("      %s\t%s\t→ %s", r.Name, displayType(r.Type), r.Value)
			if opts.ShowObserved {
				row += "\t" + observedStatus(r, observed, expectedByKey)
			}
			fmt.Fprintln(w, row)
		}
		if i < len(types)-1 {
			fmt.Fprintln(w)
		}
	}
	w.Flush()

	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("dns.apex_options_advice"))
	fmt.Fprintln(out)
}

// renderAliasOnlyApex emits the apex-primary group when its single member is an
// ALIAS/ANAME entry, which happens while the server has no cluster IPs to offer
// as the A-records alternative.
func renderAliasOnlyApex(out *bytes.Buffer, records []api.DNSRecord, observed []api.DNSRecordObserved, opts FormatOpts) {
	fmt.Fprintln(out, i18n.T("dns.header_required_record"))
	fmt.Fprintln(out)

	// Safe under tabwriter for the reason renderAlternativeGroup gives.
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range records {
		row := fmt.Sprintf("    %s\t%s\t→ %s", r.Name, displayType(r.Type), r.Value)
		if opts.ShowObserved {
			row += "\t" + aliasOnlyApexStatus(r, observed)
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
	fmt.Fprintln(out)
}

// aliasOnlyApexStatus renders the ALIAS row's status column in observedStatus's
// vocabulary, read off the sibling (name, "a") observation: an alias_or_aname
// entry has no wire-level values of its own, so the apex A probe carries the
// only result there is. It matches when the observed A values cover ALL of
// AliasResolved (the target's own resolved A set), not merely intersect it,
// because the server's headline verdict applies that same full-coverage rule
// and a looser one here would contradict it within a single status block.
//
// "got X" shows every observed IP rather than the unexpected subset
// observedStatus filters down to: the expected set is the target's resolved
// IPs, which the server does not emit as expected records, so there is nothing
// to filter against.
func aliasOnlyApexStatus(r api.DNSRecord, observed []api.DNSRecordObserved) string {
	obs := findObserved(observed, r.Name, "a")
	if obs == nil {
		return i18n.T("dns.status_not_yet_visible")
	}
	if obs.ObserveError != "" {
		return couldNotCheck(obs.ObserveError, obs.ObserveErrorAt)
	}
	if len(obs.Values) == 0 {
		return i18n.T("dns.status_not_yet_visible")
	}
	if coversAll(obs.Values, obs.AliasResolved) {
		return i18n.T("dns.status_matches")
	}
	return i18n.Tf("dns.status_got", strings.Join(obs.Values, ", "))
}

// coversAll reports whether every value in `required` appears in `observed`,
// compared after trimDot normalization. An empty `required` is never covered:
// nothing to match against means unsatisfied, not vacuously satisfied.
func coversAll(observed, required []string) bool {
	if len(required) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(observed))
	for _, v := range observed {
		set[trimDot(v)] = struct{}{}
	}
	for _, r := range required {
		if _, ok := set[trimDot(r)]; !ok {
			return false
		}
	}
	return true
}

// orderedTypes returns the keys of byType in a stable presentation order:
// ALIAS/ANAME, A, CNAME, TXT, then anything else alphabetically. Both apex
// options are advised equally, so the order is presentation, not preference.
func orderedTypes(byType map[string][]api.DNSRecord) []string {
	rank := map[string]int{
		"alias_or_aname": 0,
		"a":              1,
		"cname":          2,
		"txt":            3,
	}
	keys := make([]string, 0, len(byType))
	for k := range byType {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ri, oki := rank[keys[i]]
		rj, okj := rank[keys[j]]
		if !oki {
			ri = 99
		}
		if !okj {
			rj = 99
		}
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// optionLetter labels one alternative. A letter, and a number past Z, so it
// reads the same in every language.
func optionLetter(i int) string {
	if i < 26 {
		return string(rune('A' + i))
	}
	return fmt.Sprintf("%d", i+1)
}

// optionTypeLabel names one alternative in the "pick ONE option" header. The
// two arms that read as a sentence are catalog copy; a bare record type is the
// wire name of a DNS record and stays as the customer's registrar spells it.
func optionTypeLabel(t string) string {
	switch t {
	case "a":
		return i18n.T("dns.option_type_a_records")
	case "alias_or_aname":
		return i18n.T("dns.option_type_alias_or_aname")
	case "cname":
		return "CNAME"
	case "txt":
		return "TXT"
	default:
		return strings.ToUpper(t)
	}
}

// renderValidation appends the validation block. Validation records are always
// standalone, never part of an alternative group, so they render flat.
func renderValidation(out *bytes.Buffer, records []api.DNSRecord, observed []api.DNSRecordObserved, expectedByKey map[string]map[string]bool, opts FormatOpts) {
	fmt.Fprintln(out, i18n.T("dns.header_validation_records"))
	renderFlatRecords(out, records, observed, expectedByKey, opts, "    ")
}

func renderFlatRecords(out *bytes.Buffer, records []api.DNSRecord, observed []api.DNSRecordObserved, expectedByKey map[string]map[string]bool, opts FormatOpts, indent string) {
	// Safe under tabwriter for the reason renderAlternativeGroup gives.
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range records {
		row := fmt.Sprintf("%s%s\t%s\t→ %s", indent, r.Name, displayType(r.Type), r.Value)
		if opts.ShowObserved {
			row += "\t" + observedStatus(r, observed, expectedByKey)
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
}

// displayType names a record type as the customer's registrar names it, which
// is the same in every language.
func displayType(t string) string {
	if t == "alias_or_aname" {
		return "ALIAS"
	}
	return strings.ToUpper(t)
}

// observedStatus computes the per-record status column, deciding in this order:
//
//   - alias_or_aname: informational, since the sibling A entry carries the
//     wire-level result. Both apex options are advised equally, so the copy
//     stays neutral.
//   - no observation for this (name, type): ⧗ not yet visible.
//   - the latest probe was a transient error: ⚠ couldn't check. This precedes
//     the value match, because a stored value the server could not confirm on
//     that pass is stale and ✓ would vouch for it.
//   - no values returned: ⧗ not yet visible.
//   - the record's value is among the observed values: ✓ matches.
//   - otherwise ✗, naming the observed values that are not expected, or "not
//     present" when the resolver returned only other expected values.
func observedStatus(r api.DNSRecord, observed []api.DNSRecordObserved, expectedByKey map[string]map[string]bool) string {
	if r.Type == "alias_or_aname" {
		return i18n.T("dns.status_alias_alternative")
	}
	obs := findObserved(observed, r.Name, r.Type)
	if obs == nil {
		return i18n.T("dns.status_not_yet_visible")
	}
	if obs.ObserveError != "" {
		return couldNotCheck(obs.ObserveError, obs.ObserveErrorAt)
	}
	if len(obs.Values) == 0 {
		return i18n.T("dns.status_not_yet_visible")
	}
	if containsDNSValue(obs.Values, r.Value) {
		return i18n.T("dns.status_matches")
	}
	expectedSet := expectedByKey[r.Name+"|"+r.Type]
	var unexpected []string
	for _, v := range obs.Values {
		if !expectedSet[trimDot(v)] {
			unexpected = append(unexpected, v)
		}
	}
	if len(unexpected) > 0 {
		return i18n.Tf("dns.status_got", strings.Join(unexpected, ", "))
	}
	return i18n.T("dns.status_not_present")
}

// couldNotCheck renders the annotation for a record whose latest probe could
// not reach a resolver. reason is the server's short failure label ("timeout",
// "servfail"); at is dropped unless it parses to at least a minute ago, so no
// age is ever fabricated. The copy puts the failure on our side, since what we
// could not look up says nothing about the customer's record.
func couldNotCheck(reason, at string) string {
	if ago := timeago.Coarse(at); ago != "" {
		return i18n.Tf("dns.status_could_not_check_aged", reason, ago)
	}
	return i18n.Tf("dns.status_could_not_check", reason)
}

// DeliveryObserveError returns the delivery record's last transient probe error
// and when, or ("", "") when there is none. The delivery record is the first
// required primary expected record, read at the (name, "a") observation for an
// apex because the ALIAS placeholder has no wire-level result of its own.
//
// It is the same single record the server's "unreachable" verdict tracks, so
// callers gate on that verdict and use this only to fill in the reason and age.
// The records table is what shows every unreachable record.
func DeliveryObserveError(domain *api.Domain) (reason, at string) {
	if domain == nil {
		return "", ""
	}
	for _, r := range domain.DNSRecordsExpected {
		if !r.Required || r.Purpose != "primary" {
			continue
		}
		obsType := r.Type
		if r.AlternativeGroup == "apex_primary" {
			obsType = "a"
		}
		if obs := findObserved(domain.DNSRecordsObserved, r.Name, obsType); obs != nil {
			return obs.ObserveError, obs.ObserveErrorAt
		}
		return "", ""
	}
	return "", ""
}

// WrongTargetDetail returns the expected value and the contradicting observed
// values of the first primary record that does not match, for the wrong-target
// diagnosis line. It skips the alias_or_aname placeholder, whose sibling A
// entry carries the wire-level result.
//
// It computes only the diff to render, never the verdict: callers gate on the
// server's `present_but_wrong` first. It returns ("", nil) when no primary
// record contradicts, as on an apex served through ALIAS, where there is no
// CNAME-shaped diff and the caller falls back to the records table.
func WrongTargetDetail(domain *api.Domain) (string, []string) {
	if domain == nil {
		return "", nil
	}
	primary, _ := splitByPurpose(domain.DNSRecordsExpected)
	expectedByKey := indexExpectedValues(domain.DNSRecordsExpected)

	for _, r := range primary {
		if r.Type == "alias_or_aname" {
			continue
		}
		obs := findObserved(domain.DNSRecordsObserved, r.Name, r.Type)
		if obs == nil || len(obs.Values) == 0 {
			continue
		}
		if containsDNSValue(obs.Values, r.Value) {
			continue
		}
		expectedSet := expectedByKey[r.Name+"|"+r.Type]
		var unexpected []string
		for _, v := range obs.Values {
			if !expectedSet[trimDot(v)] {
				unexpected = append(unexpected, v)
			}
		}
		if len(unexpected) == 0 {
			unexpected = obs.Values
		}
		return r.Value, unexpected
	}
	return "", nil
}

// looksLikeCloudflareProxyIP reports whether `v` falls in one of Cloudflare's
// published anycast proxy ranges. A customer who leaves the orange cloud on
// resolves their A record to one of these instead of the expected target, the
// most common cause of a present-but-wrong record.
func looksLikeCloudflareProxyIP(v string) bool {
	ip := net.ParseIP(v)
	if ip == nil || ip.To4() == nil {
		return false
	}
	for _, cidr := range cloudflareProxyCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var cloudflareProxyCIDRs = func() []*net.IPNet {
	raw := []string{
		"104.16.0.0/13",
		"172.64.0.0/13",
		"162.158.0.0/15",
		"188.114.96.0/20",
		"131.0.72.0/22",
		"108.162.192.0/18",
		"173.245.48.0/20",
		"103.21.244.0/22",
	}
	out := make([]*net.IPNet, 0, len(raw))
	for _, c := range raw {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// DiagnoseWrongTarget names the most likely cause of a wrong-target
// observation, or returns "" when no known footgun matches and the caller
// should just show expected against got. `expected` is the expected primary
// value, `got` the observed ones.
//
// The patterns, in priority order:
//   - an observed value is a Cloudflare proxy IP: the orange cloud is on.
//   - expected is a hostname but an observed value is an IP literal: an A
//     record where a CNAME is required.
//   - an observed value is the expected target with the zone appended: the
//     target was entered without its trailing dot.
func DiagnoseWrongTarget(domainName, expected string, got []string) string {
	for _, g := range got {
		if looksLikeCloudflareProxyIP(g) {
			return i18n.T("dns.diagnose_cloudflare_proxy")
		}
	}
	expectedIsHost := net.ParseIP(strings.TrimSuffix(expected, ".")) == nil
	if expectedIsHost {
		for _, g := range got {
			if net.ParseIP(strings.TrimSuffix(g, ".")) != nil {
				return i18n.T("dns.diagnose_a_where_cname_required")
			}
		}
	}
	// A registrar given a host with no trailing dot appends the zone, so the
	// observed value is the expected target with the customer's own zone glued
	// onto its end.
	base := strings.TrimSuffix(expected, ".")
	for _, g := range got {
		gt := strings.TrimSuffix(g, ".")
		if gt != base && strings.HasPrefix(gt, base+".") {
			return i18n.T("dns.diagnose_zone_appended")
		}
		if domainName != "" && strings.HasSuffix(gt, "."+strings.TrimSuffix(domainName, ".")) &&
			strings.HasPrefix(gt, base) {
			return i18n.T("dns.diagnose_zone_appended")
		}
	}
	return ""
}

func findObserved(observed []api.DNSRecordObserved, name, recordType string) *api.DNSRecordObserved {
	for i := range observed {
		if observed[i].Name == name && observed[i].Type == recordType {
			return &observed[i]
		}
	}
	return nil
}

// containsDNSValue reports whether `needle` appears in `haystack`, compared
// after trimDot normalization on both sides.
func containsDNSValue(haystack []string, needle string) bool {
	n := trimDot(needle)
	for _, s := range haystack {
		if trimDot(s) == n {
			return true
		}
	}
	return false
}

// WriteValidationRecords prints only the validation entries from `expected` to
// w, one indented line each, and returns how many it wrote, which lets a caller
// tell an empty block from a rendered one and pluralize the copy it frames the
// block with.
//
// It is the narrow companion to FormatRecordsTable, for the CDN progress views
// where the primary records are already verified and only the validation step
// is outstanding. It selects on purpose alone and never looks at CDN state, so
// a caller must decide for itself, by gating on `awaiting_cf_validation`,
// whether validation is being awaited at all.
//
// The rows carry a tab, so a caller writing them into a text/tabwriter shares a
// column with whatever else it writes there. The instruction word ahead of the
// record type is CLI copy and changes width with the language, which tabwriter
// measures in runes: a caller mixing these rows with its own in one tabwriter
// has to lay that column out itself.
func WriteValidationRecords(w io.Writer, expected []api.DNSRecord) int {
	lines := 0
	for _, r := range expected {
		if r.Purpose != "validation" {
			continue
		}
		fmt.Fprintf(w, "%s\t%s → %s\n",
			i18n.Tf("dns.validation_add", strings.ToUpper(r.Type)), r.Name, r.Value)
		lines++
	}
	return lines
}
