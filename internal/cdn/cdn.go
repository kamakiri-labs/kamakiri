package cdn

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/dns"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// APIClient is the API surface the CDN commands need.
//
// SetCDN, SetCDNWebAccel and RotateCDNCredentials are asynchronous: they return
// an accepted sync attempt and leave the caller to watch the outcome converge. A
// credentials rotation changes no per-domain state, so it waits for the
// reconciler pass and has nothing further to watch.
type APIClient interface {
	SetCDN(siteID, mode string) (*api.CDNSetResponse, error)
	SetCDNWebAccel(siteID, token, secret string) (*api.CDNSetResponse, error)
	RotateCDNCredentials(siteID, token, secret string) (*api.CDNSetResponse, error)
	CleanupCDN(siteID string) (*api.CDNCleanupResponse, error)
	CDNStatus(siteID string) (*api.CDNStatusResponse, error)
	PurgeCDN(siteID string) (*api.CDNPurgeResponse, error)
	GetSite(id string) (*api.Site, error)
	WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)
}

// ourSideWaitTimeout caps how long the exit leg's flip to none blocks on the
// reconciler's first pass, whether it runs standalone as `cdn none` or as the
// first leg of a provider switch. The ceiling is sized for a site with many
// content domains, on the assumption that the first pass works through them one
// at a time, so a large site does not time out while still converging.
const ourSideWaitTimeout = 5 * time.Minute

// Status prints the CDN status for the linked site: the site-level mode and
// health lines, then one row per domain.
func Status(client APIClient, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	result, err := client.CDNStatus(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	if result.CDNMode == "none" {
		// Both halves are ASCII in every language: CDN is the initialism the
		// Japanese industry uses as-is, and `none` is the subcommand this line
		// names. So the line is a literal rather than a message.
		fmt.Fprintln(out, "CDN:  none")
		// Turning the CDN off leaves the provider-side resources behind, so a
		// none-mode site can still have cleanup pending.
		WriteCleanupAwaitingLine(out, result, "  ")
		return nil
	}

	// The mode and the provider are both wire tokens naming the subcommand and
	// the vendor, so this line is ASCII in every language too.
	label := result.CDNMode
	if result.Provider != "" && result.Provider != result.CDNMode {
		label = fmt.Sprintf("%s (%s)", result.CDNMode, ProviderLabel(result.Provider))
	}
	fmt.Fprintf(out, "CDN:  %s\n", label)

	WriteCredentialsHealthLine(out, result, "  ")
	WriteCleanupAwaitingLine(out, result, "  ")

	if len(result.Domains) == 0 {
		return nil
	}

	var rows [][]string
	for _, d := range result.Domains {
		status := FormatCdnStatusFromCDN(d, result.CDNMode)
		switch {
		case d.CDNID != "" && status != "":
			rows = append(rows, []string{"  " + d.Domain, d.CDNID, status})
		case d.CDNID != "":
			rows = append(rows, []string{"  " + d.Domain, d.CDNID})
		case status != "":
			rows = append(rows, []string{"  " + d.Domain, status})
		default:
			rows = append(rows, []string{"  " + d.Domain})
		}
		// The awaiting-validation row is unactionable on its own: the user
		// cannot clear it without the DCV records to add at their registrar.
		if d.CdnState == CdnStateAwaitingCFValidation {
			rows = append(rows, validationRows(d.DNSRecordsExpected)...)
		}
	}
	// A write failure on the table reaches the exit code: the rows are the
	// answer the command was asked for, so a truncated table must not report
	// success. The header lines above it are written unchecked, since a writer
	// that has failed stays failed and the table's own writes report it here.
	for _, line := range alignRows(rows) {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

// validationRows renders the DNS package's validation lines as cells for the
// table above. That renderer writes tab-separated rows into a writer of the
// caller's choosing, and its first cell is CLI copy whose width changes with the
// language, so the column it shares with the domain names has to be laid out
// here rather than left to the rows themselves.
func validationRows(expected []api.DNSRecord) [][]string {
	var buf bytes.Buffer
	if dns.WriteValidationRecords(&buf, expected) == 0 {
		return nil
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows
}

// WriteCredentialsHealthLine prints the site-level WebAccel credential-health
// note when, and only when, the axis reports failing. indent is the leading
// pad for the block. Both `kamakiri cdn` and `kamakiri status` call it, and
// they must print the same block.
//
// The observed age comes from the most recent of the two outcome timestamps;
// there is no separate last-checked timestamp to read.
func WriteCredentialsHealthLine(out io.Writer, result *api.CDNStatusResponse, indent string) {
	if result.CdnCredentialsHealth != "failing" {
		return
	}
	// Two lines and no more: the failing notice and the one command that fixes
	// it. A failing token gates purge, probe and rotate but not the edge, so
	// explaining that here would be noise on every status call, and the
	// per-domain rows below already show the edge serving.
	when := mostRecentAge(result.CdnCredentialsLastFailedAt, result.CdnCredentialsLastOkAt)
	if when != "" {
		fmt.Fprintf(out, "%s%s\n", indent, i18n.Tf("cdn.credentials_failing_aged", when))
	} else {
		fmt.Fprintf(out, "%s%s\n", indent, i18n.T("cdn.credentials_failing"))
	}
	fmt.Fprintf(out, "%s%s\n", indent, i18n.T("cdn.credentials_update_hint"))
}

// WriteCleanupAwaitingLine prints a one-line note when the site has WebAccel
// resources awaiting `kamakiri cdn cleanup`: turning the CDN off, or removing
// a domain, leaves the resource behind in the customer's Sakura account.
// indent is the leading pad. When some orphans are terminally blocked on
// invalid credentials it appends the blocked count and points at the Sakura
// panel, the only place those can be removed. Both `kamakiri cdn` and
// `kamakiri status` call it, and they must print the same block.
func WriteCleanupAwaitingLine(out io.Writer, result *api.CDNStatusResponse, indent string) {
	if result.CleanupOrphanCount == 0 {
		return
	}
	fmt.Fprintf(out, "%s%s\n", indent, i18n.Tf("cdn.cleanup_awaiting", result.CleanupOrphanCount))
	if result.CleanupBlockedCount > 0 {
		fmt.Fprintf(out, "%s  %s\n", indent, i18n.Tf("cdn.cleanup_blocked_note", result.CleanupBlockedCount))
	}
}

// mostRecentAge returns a humanized age for the most recent of the given
// RFC3339 timestamps, skipping empty and unparseable ones, or "" when there
// are none. Granularity is minutes, never seconds: a "last checked" age
// carrying seconds precision reads machine-generated.
func mostRecentAge(tss ...string) string {
	var newest time.Time
	for _, ts := range tss {
		if ts == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return ""
	}
	d := time.Since(newest)
	// The floor is load-bearing: it shadows humanizeCdnElapsed's "under a
	// minute" branch, which would render here as "last checked ~under a minute
	// ago".
	if d < time.Minute {
		return i18n.Tf("cdn.elapsed_minutes", 1)
	}
	return humanizeCdnElapsed(d)
}

// ProviderLabel returns the display name for a CDN provider, falling back to
// the raw value for one it does not know.
func ProviderLabel(provider string) string {
	switch provider {
	case "cloudflare":
		return "Cloudflare"
	case "webaccel":
		return "WebAccel"
	default:
		return provider
	}
}
