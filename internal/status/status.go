package status

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/cdn"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/dns"
	"github.com/kamakiri-labs/kamakiri/internal/domain"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/timeago"
)

// labelGap is the blank the left column keeps between the widest label and the
// values beside it, so a value never reads as the tail of its own label.
const labelGap = 2

// rowIndent leads every row of the per-domain table, and rowGap separates two
// of its columns.
const (
	rowIndent = "  "
	rowGap    = "  "
)

// labelCDN is the one label with no catalog entry. Its Japanese is the same
// letters, which is exactly the shape the catalog's Japanese gate turns away,
// so it stays a literal here and joins the set below like any other.
const labelCDN = "CDN:"

// labelTexts is every label the left column can carry, rendered in the language
// in force. The column is as wide as the widest of them, so a label added here
// widens the column instead of colliding with the value beside it; the English
// set comes out at the fourteen columns this page has always used.
func labelTexts() []string {
	return []string{
		i18n.T("status.label_version"),
		i18n.T("status.label_credentials"),
		i18n.T("status.label_logged_in"),
		i18n.T("status.label_api_server"),
		i18n.T("status.label_site"),
		i18n.T("status.label_subdomain"),
		i18n.T("status.label_domain"),
		i18n.T("status.label_domains"),
		i18n.T("status.label_reconcile"),
		i18n.T("status.label_sites"),
		i18n.T("status.label_snapshots"),
		i18n.T("status.label_max_deploy"),
		i18n.T("status.label_status"),
		labelCDN,
	}
}

func labelColumn() int {
	widest := 0
	for _, text := range labelTexts() {
		widest = max(widest, i18n.Width(text))
	}
	return widest + labelGap
}

// label pads a rendered label out to the left column. The measure is display
// width rather than runes, which is what fmt's own %-Ns counts: a label of
// two-column glyphs counts short in runes, so rune padding over-pads it and
// starts its value well right of the column every other line uses.
func label(text string) string {
	return i18n.Pad(text, labelColumn())
}

// SiteClient is the interface used by status to fetch site info.
// May be nil if no credentials are available.
type SiteClient interface {
	GetSite(id string) (*api.Site, error)
	ListDomains(siteID string) (*api.DomainList, error)
	CDNStatus(siteID string) (*api.CDNStatusResponse, error)

	// RecheckDomain backs `status --recheck`. Plain `status` never calls it:
	// reads stay pure.
	RecheckDomain(domain string) (*api.Domain, error)
}

// Run prints version, credentials, login state, API server, and site info.
// verbose renders the per-domain DNS records table for every domain rather than
// only the rows that need user action (see shouldRenderRecordsTable).
//
// The bool is true when at least one drift was surfaced: an apex on direct A
// records left stale by a CDN mode flip. The CLI wrapper exits non-zero on true,
// so `kamakiri status` slots into polling and CI checks. The error is separate
// from it because the two mean different things: drift is a finding about the
// site, the error is status failing to report at all. It is non-nil only when
// the server refuses this CLI version; every other API failure still degrades to
// a shorter page, and the caller owns printing it.
func Run(out io.Writer, version, baseURL string, client SiteClient, verbose bool) (bool, error) {
	return run(out, version, baseURL, client, verbose, false)
}

// RunWithRecheck is `status --recheck`: it POSTs the recheck endpoint for each
// non-serving custom domain, opening the server-side verify-priority window,
// before rendering. It returns the same drift signal and error as Run.
func RunWithRecheck(out io.Writer, version, baseURL string, client SiteClient, verbose bool) (bool, error) {
	return run(out, version, baseURL, client, verbose, true)
}

func run(out io.Writer, version, baseURL string, client SiteClient, verbose, recheck bool) (bool, error) {
	fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_version")), version)

	credPath, pathErr := core.CredentialsPath()
	if pathErr != nil {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_credentials")), i18n.Tf("status.value_error", pathErr))
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_logged_in")), i18n.T("status.value_no"))
	} else if _, err := os.Stat(credPath); err != nil {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_credentials")), i18n.T("status.value_not_found"))
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_logged_in")), i18n.T("status.value_no"))
	} else {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_credentials")), credPath)
		creds, err := core.LoadCredentials()
		if err != nil {
			fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_logged_in")), i18n.Tf("status.value_error", err))
		} else if creds == nil {
			fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_logged_in")), i18n.T("status.value_no"))
		} else if creds.Email != "" {
			fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_logged_in")), creds.Email)
		} else {
			fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_logged_in")), i18n.T("status.value_yes"))
		}
	}

	fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_api_server")), baseURL)

	config, _ := core.LoadProject()
	if config == nil {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_site")), i18n.T("status.value_none"))
		return false, nil
	}

	if client == nil {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_site")), config.ID)
		return false, nil
	}

	site, err := client.GetSite(config.ID)
	if err != nil {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_site")), config.ID)
		return false, versionRefusal(err)
	}

	// Branch on the teardown lifecycle before any per-projection rendering: for a
	// site mid-teardown those rows are mostly empty, and the diff-only logic
	// would frame them as "syncing", the wrong story for a teardown.
	if rendered := renderStatusBranch(out, site); rendered {
		return false, nil
	}

	fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_subdomain")), api.SubdomainLine(site))

	result, err := client.ListDomains(config.ID)
	if err != nil {
		return false, versionRefusal(err)
	}

	// The recheck endpoint does not probe: it opens the verify-priority window so
	// the next server-side tick picks the domain up. Its errors are non-fatal,
	// since the goal is still to render status: the pre-recheck row is kept.
	if recheck {
		for i := range result.Domains {
			if result.Domains[i].State == domain.StateServing {
				continue
			}
			if fresh, err := client.RecheckDomain(result.Domains[i].Domain); err == nil && fresh != nil {
				result.Domains[i] = *fresh
			}
		}
	}

	var canonical string
	var domainCount int
	for _, d := range result.Domains {
		domainCount++
		if d.Role == "canonical" {
			canonical = d.Domain
		}
	}

	if canonical != "" {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_domain")), canonical)
	}
	if domainCount > 1 {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_domains")),
			i18n.Tf("status.value_additional_domains", domainCount-1))
	} else if domainCount == 0 && canonical == "" {
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_domain")), i18n.T("status.value_none"))
	}

	cdnResult, err := client.CDNStatus(config.ID)
	if err != nil {
		return false, versionRefusal(err)
	}

	// The mode is the name of the `kamakiri cdn <mode>` subcommand that set it
	// and the provider label is the vendor's own product name, so both stay as
	// they are in every language.
	cdnLabel := cdnResult.CDNMode
	if cdnResult.Provider != "" && cdnResult.Provider != cdnResult.CDNMode {
		cdnLabel = fmt.Sprintf("%s (%s)", cdnResult.CDNMode, cdn.ProviderLabel(cdnResult.Provider))
	}
	fmt.Fprintf(out, "%s%s\n", label(labelCDN), cdnLabel)

	cdn.WriteCredentialsHealthLine(out, cdnResult, "  ")

	// A `cdn none` from webaccel, or a removed domain, leaves the resource in the
	// customer's account.
	cdn.WriteCleanupAwaitingLine(out, cdnResult, "  ")

	renderQuota(out, site)

	renderFreshnessBlock(out, site)

	// Hosts already covered by a site-level ✗ entry above, so the per-domain loop
	// can drop their Purges: line rather than report the same failure twice with
	// a different remediation.
	blockedHosts := blockedHostSet(site)

	return renderPerDomainStatus(out, result.Domains, cdnResult, verbose, recheck, blockedHosts), nil
}

// versionRefusal returns err when the server refused this CLI version, and nil
// for any other API failure. Status degrades to a shorter page when a read
// fails, which is right while the rest of it still holds; a refused version is
// not that, since every further read fails the same way and the page left would
// report an absence rather than a fact.
func versionRefusal(err error) error {
	if errors.Is(err, api.ErrUpgradeRequired) {
		return err
	}
	return nil
}

// renderFreshnessBlock prints the site-level freshness lines: whether the
// current deploy is actually live at every cache a visitor could hit, plus any
// terminal flush blocker and a hard reconcile-push error. It renders the
// server's axes verdict and never re-derives one from timestamps.
//
// The whole block is skipped for a torn-down site. The teardown line above
// already owns that framing, and a torn-down site's edge facts are frozen, so
// its edge axis reads "flushing" forever: a "still flushing (since <age>)" line
// would report a frozen artifact as live news with an ever-growing age.
func renderFreshnessBlock(out io.Writer, site *api.Site) {
	if site.Status == "deleted" {
		return
	}

	// A hard reconcile push failure stays visible after a Ctrl-C, freshness block
	// or not: the verdict may still read pending, but the attempt errored and the
	// user needs the reason.
	if site.Sync != nil && site.Sync.Outcome == "error" && site.Sync.Error != "" {
		fmt.Fprintf(out, "%s✗ %s\n", label(i18n.T("status.label_reconcile")), site.Sync.Error)
	}

	f := site.Freshness
	if f == nil || f.State == "fresh" {
		return
	}

	// The still-converging line renders only while the verdict is pending. A
	// blocked site is not going live on its own, so such a line above the
	// terminal ✗ entries would contradict them; there, they are the message.
	if f.State == "pending" {
		if f.Axes.Edge == "flushing" {
			line := i18n.T("status.edge_still_flushing")
			if age := timeago.Coarse(site.EdgePurgeLastOkAt); age != "" {
				line += i18n.Tf("status.edge_since", age)
			}
			// On-us copy, never a user action: that cache is ours end to end,
			// with no credential to rotate and no setting a user could correct.
			if site.EdgePurgeHealth == "failing" || site.EdgePurgeHealth == "broken" {
				line += i18n.T("status.edge_flush_failing")
				if site.EdgePurgeErrorReason != "" {
					// The server's own short failure label. Its parentheses
					// follow the ASCII label they wrap rather than the sentence
					// around them, so they read the same in every language and
					// are not catalog copy.
					line += fmt.Sprintf(" (%s)", site.EdgePurgeErrorReason)
				}
				line += i18n.T("status.edge_keep_retrying")
			}
			fmt.Fprintln(out, line)
		} else {
			fmt.Fprintln(out, i18n.T("status.still_going_live"))
		}
	}

	// No lead: status deployed nothing, so a verb lead ("Deployed, but") would
	// be untrue here and the entry states the condition instead.
	for _, b := range f.Blockers {
		for _, l := range freshness.FormatBlocker("", b) {
			fmt.Fprintln(out, l)
		}
	}
}

// blockedHostSet is the set of hosts the site's freshness verdict reports as
// blocked. It comes from the same site payload as the blocker entries, so the
// two views cannot disagree within one frame. The site-level site_torn_down
// blocker carries no host and is skipped, so a non-CDN blocker never suppresses
// a domain row.
func blockedHostSet(site *api.Site) map[string]struct{} {
	if site.Status == "deleted" || site.Freshness == nil {
		return nil
	}
	set := make(map[string]struct{}, len(site.Freshness.Blockers))
	for _, b := range site.Freshness.Blockers {
		if b.Host != "" {
			set[b.Host] = struct{}{}
		}
	}
	return set
}

// renderQuota prints the storage usage lines from the site read's quota block:
// sites used against the account cap, snapshots for this site against the
// per-site cap, and the per-deploy size ceiling. An absent block (an older
// server) prints nothing, so status degrades cleanly.
func renderQuota(out io.Writer, site *api.Site) {
	q := site.Quota
	if q == nil {
		return
	}

	fmt.Fprintf(out, "%s%d / %d\n", label(i18n.T("status.label_sites")), q.SitesUsed, q.SitesMax)
	fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_snapshots")),
		i18n.Tf("status.value_snapshots", q.SnapshotsUsed, q.SnapshotsMax))
	fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_max_deploy")), formatMaxDeploy(q.MaxSnapshotBytes))
}

// formatMaxDeploy renders the per-deploy byte ceiling in MB, trimming a whole
// number to "100 MB" rather than "100.0 MB". A ceiling that is not MB-aligned
// keeps one decimal. A number and its unit symbol read the same in every
// language, so neither is catalog copy.
func formatMaxDeploy(bytes int64) string {
	const mb = 1024 * 1024
	if bytes%mb == 0 {
		return fmt.Sprintf("%d MB", bytes/mb)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/mb)
}

// renderPerDomainStatus prints a per-domain detail block under the top-level
// summary lines, but only when at least one row carries a state worth
// surfacing. The DNS state and the orthogonal cdn_state render in the same
// table so the user sees both in one glance instead of alternating between
// `kamakiri status` and `kamakiri cdn`. Under each row, the full records table
// follows when shouldRenderRecordsTable says the row is actionable.
//
// An empty domain list prints nothing here (the "(none)" line is handled
// upstream), as does a site whose state columns are all blank.
func renderPerDomainStatus(out io.Writer, domains []api.Domain, cdnResult *api.CDNStatusResponse, verbose, inBurst bool, blockedHosts map[string]struct{}) bool {
	if len(domains) == 0 {
		return false
	}

	cdnMode := cdnResult.CDNMode

	rows := make([]domainRow, 0, len(domains))
	for _, d := range domains {
		// Calling domain.FormatStatus with an empty State would trip its
		// warn-once on an unknown state and pollute stderr, and a row with
		// neither state has nothing to show anyway.
		if d.State == "" && d.CdnState == "" {
			continue
		}
		dnsStatus := ""
		if d.State != "" {
			dnsStatus = domain.FormatStatus(d)
		}
		cdnStatus := cdn.FormatCdnStatusFromDomain(d, cdnMode)
		if dnsStatus == "" && cdnStatus == "" {
			continue
		}
		rows = append(rows, domainRow{
			domain:    d.Domain,
			role:      formatRole(d),
			dnsStatus: dnsStatus,
			cdnStatus: cdnStatus,
			full:      d,
		})
	}
	if len(rows) == 0 {
		return false
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("status.per_domain_header"))

	lines := perDomainLines(rows)
	drift := false
	for i, r := range rows {
		fmt.Fprintln(out, lines[i])
		if shouldRenderRecordsTable(r.full, verbose) {
			fmt.Fprint(out, dns.FormatRecordsTable(&r.full, dns.FormatOpts{ShowObserved: true}))
		}
		// Nudge for an awaiting-DNS domain that has never been live; a degraded
		// row carries its own "DNS broken" context instead.
		if r.full.State == domain.StateAwaitingDNS && domain.IsNeverLive(r.full) {
			// inBurst is true only on the --recheck path, where the CLI just
			// opened the verify-priority window and a small ETA is therefore
			// truthful. A plain read cannot know the window's state, so it takes
			// the deliberate-backoff framing rather than guess an ETA.
			fmt.Fprintf(out, "    %s\n", domain.VerifyNudge(r.full.Domain, inBurst))
		}
		// Drift is drift whether the user just rechecked or not, so inBurst does
		// not gate this.
		if line := apexARecordsDriftLine(r.full, cdnMode); line != "" {
			fmt.Fprint(out, line)
			drift = true
		}
		// The purge line comes last so the DNS-axis remediation above stays
		// closest to the row that owns it. It is suppressed for a blocked host:
		// the site-level ✗ entry already reports that failure with the right
		// remediation.
		if _, blocked := blockedHosts[r.full.Domain]; !blocked {
			if line := cdn.FormatPurgeStateLine(&r.full); line != "" {
				fmt.Fprintln(out, line)
			}
		}
	}
	return drift
}

// domainRow is one line of the per-domain table: the four cells, plus the
// domain the blocks under the row are rendered from.
type domainRow struct {
	domain    string
	role      string
	dnsStatus string
	cdnStatus string
	full      api.Domain
}

// perDomainLines lays the table out by hand rather than through text/tabwriter,
// which measures a cell in runes and so reads a Japanese DNS state as half the
// columns a terminal gives it. That state is not the last cell in its row, so
// tabwriter would pad against the miscount and skew the column after it. Every
// column but the last is padded to its widest cell across the whole block and
// the columns are joined with a fixed gap; the last cell is never padded, so a
// line ends in whitespace only where an empty CDN state leaves the padded DNS
// cell at the end of it.
func perDomainLines(rows []domainRow) []string {
	domainWidth, roleWidth, dnsWidth := 0, 0, 0
	for _, r := range rows {
		domainWidth = max(domainWidth, i18n.Width(rowIndent+r.domain))
		roleWidth = max(roleWidth, i18n.Width(r.role))
		dnsWidth = max(dnsWidth, i18n.Width(r.dnsStatus))
	}

	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, strings.Join([]string{
			i18n.Pad(rowIndent+r.domain, domainWidth),
			i18n.Pad(r.role, roleWidth),
			i18n.Pad(r.dnsStatus, dnsWidth),
			r.cdnStatus,
		}, rowGap))
	}
	return lines
}

// apexARecordsDriftLine returns the drift block when an apex domain publishes
// its own A records and they no longer match the per-mode expected set, and ""
// in every other case (ALIAS customers, subdomains). The marker is ✗ rather than
// ⚠ because the apex is not working as intended: the customer is still hitting
// the previous edge.
func apexARecordsDriftLine(d api.Domain, cdnMode string) string {
	expected, observed, found := apexAExpectedAndObserved(d)
	if !found {
		return ""
	}
	if equalAsSets(expected, observed) {
		return ""
	}

	// The two labels sit above one another, so the values beside them start at
	// one column: padded to the wider of the two by display width, since
	// counting runes reads a two-column glyph as one column and over-pads
	// whichever label carries them.
	expectedLabel := i18n.T("status.drift_label_expected")
	observedLabel := i18n.T("status.drift_label_observed")
	column := max(i18n.Width(expectedLabel), i18n.Width(observedLabel))

	var b strings.Builder
	fmt.Fprintf(&b, "  ✗ %s\n", apexARecordsDriftHeadline(cdnMode))
	fmt.Fprintf(&b, "%s %s\n", i18n.Pad(expectedLabel, column), strings.Join(expected, ", "))
	fmt.Fprintf(&b, "%s %s\n", i18n.Pad(observedLabel, column), strings.Join(observed, ", "))
	fmt.Fprintln(&b, i18n.T("status.drift_fix"))
	return b.String()
}

// apexARecordsDriftHeadline renders the per-mode framing of the drift ✗. none
// mode has no CDN edge, the customer's apex pointing straight at the cluster, so
// the copy names the cluster rather than "the current edge for none mode".
func apexARecordsDriftHeadline(cdnMode string) string {
	switch cdnMode {
	case "none":
		return i18n.T("status.drift_headline_cluster")
	default:
		return i18n.Tf("status.drift_headline_mode", cdnMode)
	}
}

// apexAExpectedAndObserved finds the apex-primary (name, a) group for a domain.
// It reports true only when the server-derived dns_matched_alternative says the
// customer published direct apex A records; which alternative an apex is using
// is decided server-side and never re-derived here. Expected comes from the
// server's records blob, which already reflects the current mode.
func apexAExpectedAndObserved(d api.Domain) ([]string, []string, bool) {
	if d.DnsMatchedAlternative != "a" {
		return nil, nil, false
	}

	var observed []string
	for _, r := range d.DNSRecordsObserved {
		if r.AlternativeGroup == "apex_primary" && r.Type == "a" {
			observed = append([]string(nil), r.Values...)
			break
		}
	}

	var expected []string
	for _, r := range d.DNSRecordsExpected {
		if r.AlternativeGroup == "apex_primary" && r.Type == "a" {
			expected = append(expected, r.Value)
		}
	}

	return expected, observed, true
}

// equalAsSets returns true when `a` and `b` have the same elements regardless
// of order or duplicates.
func equalAsSets(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	seen := make(map[string]struct{}, len(a))
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; !ok {
			return false
		}
	}
	seenB := make(map[string]struct{}, len(b))
	for _, v := range b {
		seenB[v] = struct{}{}
	}
	for _, v := range a {
		if _, ok := seenB[v]; !ok {
			return false
		}
	}
	return true
}

// shouldRenderRecordsTable decides whether the full DNS records table renders
// under a per-domain row. Three triggers:
//
//   - state != serving: DNS is broken or pending, so the user needs the records
//     to fix it.
//   - verbose: shows the records even when serving, so DNS can be audited
//     without breaking it first.
//   - cdn_state == awaiting_cf_validation: mid-DCV setup, where the validation
//     records must be visible whatever the DNS axis says.
func shouldRenderRecordsTable(d api.Domain, verbose bool) bool {
	if verbose {
		return true
	}
	if d.State != "" && d.State != domain.StateServing {
		return true
	}
	if d.CdnState == cdn.CdnStateAwaitingCFValidation {
		return true
	}
	return false
}

// formatRole renders a domain's role. The roles are wire values the CLI echoes
// back, and a redirect carries the status code the `--status` flag set, so the
// column reads as the flags do and stays in ASCII in every language.
func formatRole(d api.Domain) string {
	if d.Role == "redirect" && d.RedirectStatus > 0 {
		return fmt.Sprintf("redirect %d", d.RedirectStatus)
	}
	return d.Role
}

// renderStatusBranch prints a top-level "Status:" line for the (status,
// status_observed) lifecycle. It returns true when the branch fully handled
// rendering (skip per-projection rows), false to fall through.
func renderStatusBranch(out io.Writer, site *api.Site) bool {
	// Treat an omitted field as "active": an older server may not send the
	// columns at all, and steady state is the right fallback. The branch then
	// fires only for explicitly deleted sites.
	desired := site.Status
	observed := site.StatusObserved
	if desired == "" {
		desired = "active"
	}
	if observed == "" {
		observed = "active"
	}

	switch {
	case desired == "active" && observed == "active":
		return false

	case desired == "deleted" && observed == "active":
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_status")), i18n.T("status.value_pending_teardown"))
		// The continuation reads on from the line above, so it is indented to
		// the same column the values start in rather than to a fixed number.
		fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", labelColumn()), i18n.T("status.teardown_edge_note"))
		return true

	case desired == "deleted" && observed == "soft_deleted":
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_status")), i18n.T("status.value_tearing_down"))
		// Fall through so the per-projection rows render ⧗ for each external
		// still in flight: that is where the work is and the user needs to see it.
		return false

	case desired == "deleted" && observed == "deleted":
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_status")), i18n.T("status.value_deleted"))
		return true

	default:
		// Impossible by contract: render the raw values so an operator can see
		// what is wrong.
		fmt.Fprintf(out, "%s%s\n", label(i18n.T("status.label_status")),
			i18n.Tf("status.value_stuck", site.Status, site.StatusObserved))
		return true
	}
}
