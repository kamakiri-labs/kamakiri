package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/dns"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/timeago"
)

// The `state` values a domain row can carry.
const (
	StateAwaitingDNS  = "awaiting_dns"
	StateAwaitingEdge = "awaiting_edge"
	// StateAwaitingCert sits between `awaiting_edge` and `serving`: the route is
	// already on the edge and no valid TLS handshake has been observed yet.
	StateAwaitingCert = "awaiting_cert"
	StateServing      = "serving"
	StateDegraded     = "degraded"
	StateTearingDown  = "tearing_down"
)

var (
	// RoutedStates is the set of `state` values for which a route has been
	// emitted to the edge. `awaiting_cert` is one of them because the route has
	// to be in place before issuance can start.
	//
	// A member is not necessarily cert-truthful: `awaiting_cert` means the route
	// is up but nothing has been issued yet, and `degraded` means at least one
	// axis (DNS or cert) is failing. Code that needs "the user's URL actually
	// works in a browser" must compare the state against StateServing rather
	// than call IsRouted.
	RoutedStates = []string{StateAwaitingEdge, StateAwaitingCert, StateServing, StateDegraded}

	// KnownStates is every state this CLI renders with copy of its own.
	// FormatStatus passes anything else through raw and warns once, so a newer
	// server never collapses a row to a blank line.
	KnownStates = []string{
		StateAwaitingDNS,
		StateAwaitingEdge,
		StateAwaitingCert,
		StateServing,
		StateDegraded,
		StateTearingDown,
	}

	// The unknown-state warning fires once per state, so a status view with many
	// rows in the same unknown state writes one line to stderr, not one per row.
	unknownStateWarnedMu   sync.Mutex
	unknownStateWarned               = map[string]bool{}
	unknownStateWarnWriter io.Writer = os.Stderr
)

// IsRouted reports whether a domain in the given state has had its route pushed
// to the edge. Routed is not cert-truthful; see the caveat on RoutedStates.
func IsRouted(state string) bool {
	return slices.Contains(RoutedStates, state)
}

// warnUnknownStateOnce prints a warning the first time a given unknown state is
// seen, and stays silent for that state afterwards.
func warnUnknownStateOnce(state string) {
	unknownStateWarnedMu.Lock()
	defer unknownStateWarnedMu.Unlock()
	if unknownStateWarned[state] {
		return
	}
	unknownStateWarned[state] = true
	fmt.Fprintln(unknownStateWarnWriter, i18n.Tf("domain.warn_unknown_state", state))
}

// deployFirstLine is the shared body for a domain blocked only because its site
// has no published content. No route is emitted for a site with nothing
// published, so such a domain can reach neither a route nor a certificate until
// the user deploys. FormatStatus prints it at column 0; the watch indents it to
// match its other transient frames.
//
// The copy stays CDN-neutral and deploy-count-neutral: a CDN domain reaches the
// same branch and issues no certificate of ours, and a site that was live can
// lose its deploy, so the line must not say "SSL certificate" or "first deploy".
// It names the path argument `kamakiri deploy` requires, since a user following
// a bare `kamakiri deploy` gets a usage error instead of a deploy.
//
// A package-level value cannot hold it: package initialization runs before the
// language is resolved, so the line would freeze in whatever the environment
// suggested and the saved preference would never reach it.
func deployFirstLine() string {
	return i18n.T("domain.deploy_first")
}

// noLiveDeploy reports whether the server said explicitly that the domain's
// site has no live deploy. A nil `has_live_deploy` is deliberately not
// no-deploy: only an explicit false flips the copy, so an older server that
// omits the field keeps the edge/cert copy instead of mislabelling every
// awaiting-edge domain.
func noLiveDeploy(d api.Domain) bool {
	return d.HasLiveDeploy != nil && !*d.HasLiveDeploy
}

// FormatStatus returns the per-domain status display shared by `kamakiri
// status` and `domain verify`'s one-shot path. It is a table over State; an
// unknown value passes through raw, with a once-per-state stderr warning so the
// user learns their CLI is behind the server.
//
// The `serving` branch carries a renewal warning when `cert_error` is
// `renewal_overdue`: the certificate is still valid, so the row stays serving,
// and the warning is the sentinel that fires ahead of expiry.
func FormatStatus(d api.Domain) string {
	switch d.State {
	case StateAwaitingDNS:
		// An "unreachable" verdict is not a missing record: we could not look, so
		// the line says that. The "add the record" nudge would misdirect a user
		// whose record is already published.
		if d.DnsVerdict == "unreachable" {
			return dnsUnreachableStatusLine(d)
		}
		// The record exists and points elsewhere, so "add the record" would
		// misdirect; name the wrong target and the fix instead.
		if d.DnsVerdict == "present_but_wrong" {
			return dnsWrongTargetStatusLine(d)
		}
		// A domain whose DNS has never been observed gets actionable copy, not
		// the alarming framing that belongs to `degraded`, and `domain verify` is
		// named because it is the fast path.
		return i18n.Tf("domain.status_awaiting_dns", d.Domain)
	case StateAwaitingEdge:
		// The real gate is the missing deploy, not an edge step. Only an explicit
		// no-deploy diverts, and awaiting_dns is left alone: the user needs the
		// record guidance there first.
		if noLiveDeploy(d) {
			return deployFirstLine()
		}
		return i18n.T("domain.status_verifying_edge")
	case StateAwaitingCert:
		// A deploy-less domain rests in awaiting_edge, so this is a defensive
		// guard: a row seen here without a deploy still cannot get a certificate,
		// and must not be shown the cert spinner.
		if noLiveDeploy(d) {
			return deployFirstLine()
		}
		// Naming the observed `cert_error` category keeps this view and the
		// watch's transient telling the user the same thing about the same fact.
		if d.CertError != "" {
			return certCategoryStatusLine(d.CertError)
		}
		return i18n.T("domain.status_provisioning_cert")
	case StateServing:
		if d.CertError == CertErrorRenewalOverdue {
			return i18n.T("domain.status_live_renewal_overdue")
		}
		return i18n.T("domain.status_live")
	case StateDegraded:
		return degradedAxisLine(d)
	case StateTearingDown:
		// Past a couple of hours the teardown is dragging, so surface its age
		// rather than the bare line.
		if since, ok := timeago.Parse(d.DeleteRequestedAt); ok && time.Since(since) > 2*time.Hour {
			return i18n.Tf("domain.status_removing_since", humanizeDuration(time.Since(since)))
		}
		return i18n.T("domain.status_removing")
	default:
		// The raw value beats a blank line, and the warning gets the user to
		// upgrade before a state this CLI misreads causes a real surprise, such
		// as IsRouted returning false for one the server means to be live.
		warnUnknownStateOnce(d.State)
		return d.State
	}
}

// degradedAxisLine returns the degraded copy for whichever axis regressed: DNS
// only, cert only, or both. The "unreachable" DNS verdict gets its own
// "couldn't reach your DNS" line rather than the definitive "no longer visible"
// one, since a resolver we could not reach tells us nothing about the record.
// Every variant states the same recovery, attributes no fault, and shows a
// timing only where one was measured.
func degradedAxisLine(d api.Domain) string {
	dnsRegressed := dnsAxisRegressed(d)
	certRegressed := certAxisRegressed(d)

	// Both axes anchor on the last probe attempt rather than a last-seen-good
	// timestamp. On the cert axis the observation timestamp would read as "last
	// valid handshake" while actually recording the last look at a wrong or
	// absent certificate.
	dnsAnchor := timeago.Coarse(d.DnsCheckedAt)
	certAnchor := timeago.Coarse(d.CertCheckedAt)

	switch {
	case dnsRegressed && certRegressed:
		if d.DnsVerdict == "unreachable" {
			// "DNS record missing" would be a fabrication: the resolver was out of
			// reach, so whether the record is there is unknown.
			return i18n.T("domain.degraded_both_unreachable")
		}
		if d.DnsVerdict == "present_but_wrong" {
			// The record is present, just pointed elsewhere, so name the wrong
			// target instead of calling it missing.
			return i18n.T("domain.degraded_both_wrong_target")
		}
		return i18n.T("domain.degraded_both_absent")

	case dnsRegressed:
		if d.DnsVerdict == "unreachable" {
			return dnsUnreachableStatusLine(d)
		}
		// Telling the user a present record vanished would send them to re-add
		// one that already exists, so name the wrong target and the fix.
		if d.DnsVerdict == "present_but_wrong" {
			return dnsWrongTargetStatusLine(d)
		}
		if dnsAnchor != "" {
			return i18n.Tf("domain.degraded_dns_aged", dnsAnchor, d.Domain)
		}
		return i18n.Tf("domain.degraded_dns", d.Domain)

	case certRegressed:
		if certAnchor != "" {
			return i18n.Tf("domain.degraded_cert_aged", certAnchor)
		}
		return i18n.T("domain.degraded_cert")

	default:
		// Degraded with neither axis reading as regressed: a failure mode this
		// CLI does not model yet, or a stale read. That includes an empty
		// verdict, where DNS is simply unknown, so blaming DNS would send the
		// user to fix something that has not been shown to be broken. The line
		// stays neutral.
		return i18n.T("domain.degraded_neutral")
	}
}

// dnsAxisRegressed reports whether the DNS axis is unhealthy, from the server's
// verdict. An empty verdict counts as healthy, so an older server falls through
// to the neutral degraded line instead of having "DNS broken" fabricated for it.
func dnsAxisRegressed(d api.Domain) bool {
	switch d.DnsVerdict {
	case "present_but_wrong", "absent", "unreachable":
		return true
	default:
		return false
	}
}

// dnsUnreachableStatusLine renders the "couldn't reach your DNS" line for the
// "unreachable" verdict, where the last probe hit a resolver error and the
// record's value is therefore unknown. It names the observed error and when,
// falling back to the last-checked timestamp when the observation carries no
// usable time, and never claims the definitive miss "no longer visible" would.
// The "couldn't reach" framing puts the failure on our side, never the
// customer's.
func dnsUnreachableStatusLine(d api.Domain) string {
	reason, at := dns.DeliveryObserveError(&d)
	ago := timeago.Coarse(at)
	if ago == "" {
		ago = timeago.Coarse(d.DnsCheckedAt)
	}
	switch {
	case reason != "" && ago != "":
		return i18n.Tf("domain.dns_unreachable_reason_aged", reason, ago)
	case reason != "":
		return i18n.Tf("domain.dns_unreachable_reason", reason)
	case ago != "":
		return i18n.Tf("domain.dns_unreachable_aged", ago)
	default:
		return i18n.T("domain.dns_unreachable")
	}
}

// dnsWrongTargetStatusLine renders the "points at the wrong target" line for
// the "present_but_wrong" verdict. It names the expected target, what was
// observed, and the likely cause when one matches; the records table below
// prints the raw ✗ without that diagnosis. The empty-detail fallback covers the
// case where no observed primary record contradicts its expected value, so
// there is no expected/got pair to show: an apex whose only expected primary is
// the ALIAS placeholder reaches it, since the diff skips that placeholder.
func dnsWrongTargetStatusLine(d api.Domain) string {
	expected, got := dns.WrongTargetDetail(&d)
	if expected == "" {
		return i18n.T("domain.wrong_target_no_detail")
	}
	// The diagnosis phrases are lowercase clauses that may carry parentheses of
	// their own, so a colon lead-in reads better than nesting them in one more.
	// Each shape is a whole message, since where the diagnosis attaches to the
	// expected/observed pair is not the same in every language.
	observed := strings.Join(got, ", ")
	var detail string
	if diag := dns.DiagnoseWrongTarget(d.Domain, expected, got); diag != "" {
		detail = i18n.Tf("domain.wrong_target_detail_diag", expected, observed, diag)
	} else {
		detail = i18n.Tf("domain.wrong_target_detail", expected, observed)
	}
	return i18n.Tf("domain.wrong_target", detail)
}

// certAxisRegressed reports whether the cert axis is the failing one. Only a
// "wrong" verdict on a cert-required row counts: an empty, absent or
// unreachable verdict falls through to the neutral degraded line rather than
// having "TLS certificate not serving" fabricated for it.
func certAxisRegressed(d api.Domain) bool {
	return d.CertRequired && d.CertVerdict == CertVerdictWrong
}

// certCategoryStatusLine renders an observed `cert_error` category for the
// status view. The four categories it names carry the same diagnosis as the
// watch's certCategoryCopy gives them, so one observed fact does not reach the
// user two different ways. `renewal_overdue` is absent because on this view it
// belongs to a row that is serving with a valid certificate, which the serving
// branch renders instead.
func certCategoryStatusLine(category string) string {
	switch category {
	case CertErrorCertUnavailable:
		return i18n.T("domain.cert_status_unavailable")
	case CertErrorWrongSubject:
		return i18n.T("domain.cert_status_wrong_subject")
	case CertErrorChainInvalid:
		return i18n.T("domain.cert_status_chain_invalid")
	case CertErrorExpired:
		return i18n.T("domain.cert_status_expired")
	default:
		return i18n.T("domain.status_provisioning_cert")
	}
}

// humanizeDuration renders a duration as "just now", "Nm", "Nh" or "Nd". It
// differs from timeago.Coarse in taking a duration rather than a timestamp, and
// in rendering a sub-minute span rather than the empty string. The wording comes
// from the message catalog, so those examples are one language's rendering, not
// the format.
func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.T("domain.age_just_now")
	case d < time.Hour:
		return i18n.Tf("domain.age_minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return i18n.Tf("domain.age_hours", int(d.Hours()))
	default:
		return i18n.Tf("domain.age_days", int(d.Hours()/24))
	}
}

// APIClient is the API surface the domain commands need.
type APIClient interface {
	SetCanonical(siteID, domain string) (*api.Domain, error)
	UnsetCanonical(siteID string) (*api.UnsetCanonicalResult, error)
	AddDomain(siteID, domain, role string, redirectStatus int) (*api.Domain, error)
	RemoveDomain(domain string) (*api.RemoveDomainResult, error)
	ListDomains(siteID string) (*api.DomainList, error)
	WaitForSync(siteID string, sinceAttemptID int64, timeout time.Duration) (*api.Site, error)

	// RecheckDomain asks the server to re-probe the domain soon and returns its
	// current status. No probe runs as part of the call, so the status it
	// returns is not yet the result of that re-probe.
	RecheckDomain(domain string) (*api.Domain, error)

	// The account-scoped registration calls. The Domain suffix keeps
	// RegisterDomain from colliding with the concrete client's auth Register.
	RegisterDomain(name string) (*api.Registration, error)
	UnregisterDomain(name string) error
	ListRegistrations() (*api.RegistrationList, error)
}

// IsNeverLive reports whether the DNS verdict is "absent", meaning the server
// looked and found no record published yet. It separates a brand-new domain,
// which gets actionable waiting copy, from one that was live and broke, which
// gets the alarming copy. An "unreachable" verdict is deliberately not
// never-live: a resolver blip is not the same as the user never having set up
// DNS.
func IsNeverLive(d api.Domain) bool {
	return d.DnsVerdict == "absent"
}

// VerifyNudge returns the one-line nudge for a domain still waiting on DNS.
// `domain verify`'s one-shot path and the status view both print it, so the
// wording lives in one place.
//
// inBurst says whether the caller just asked the server for a fresh check. Only
// then is a small ETA truthful, so only then is one printed; every other caller
// gets the backoff framing rather than a number it cannot compute. A pure read
// never knows, because the status payload does not say how soon the next check
// is due.
func VerifyNudge(domainName string, inBurst bool) string {
	if inBurst {
		return i18n.Tf("domain.verify_nudge_in_burst", domainName)
	}
	return i18n.Tf("domain.verify_nudge_backoff", domainName)
}

var watchVerifyFn = watchVerifyCtx

// Verify implements `kamakiri domain verify <domain>`. It asks the server for a
// fresh check, then blocks on a streamed watch until the domain is live,
// re-issuing that request so the server keeps re-probing. Ctrl-C detaches, and
// the automatic checks carry on without it.
//
// `--no-wait`, and a non-TTY invocation, keep the one-shot nudge instead:
// report the request and the current status, and return before the fresh result
// lands.
//
// A linked project is required even though the server identifies the domain by
// name and API-key ownership: its site id is what the watch then polls.
func Verify(client APIClient, domainName string, noWait bool, out io.Writer) error {
	// A piped or captured invocation must neither emit cursor control nor block
	// on the watch, so it behaves as `--no-wait`.
	interactive := !noWait && isTerminalWriter(out)
	return verify(client, domainName, interactive, out)
}

// verify is the testable core of Verify, taking `interactive` rather than
// deriving it from a real terminal.
func verify(client APIClient, domainName string, interactive bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	d, err := client.RecheckDomain(domainName)
	if err != nil {
		// With no domain row but a pending registration at this exact name,
		// verify nudges and watches that registration instead. The match is by
		// exact name, since you nudge a registration by the name you registered:
		// verifying www.example.com while only example.com is registered is not a
		// registration recheck.
		if isErrorCode(err, "domain_not_found") && hasPendingRegistration(client, domainName) {
			return register(client, domainName, interactive, out)
		}
		return api.MapError(err)
	}

	if !interactive {
		fmt.Fprintln(out, i18n.Tf("domain.verify_requested", d.Domain))
		fmt.Fprintln(out, i18n.T("domain.verify_next_cycle"))
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.Tf("domain.verify_status_header", d.Domain))
		fmt.Fprintf(out, "  %s\n", FormatStatus(*d))

		if d.State != StateServing {
			printVerifyDnsInstructions(out, d)
			// This call just asked for a fresh check, so the small ETA in the
			// in-burst nudge is truthful here.
			if IsNeverLive(*d) {
				fmt.Fprintln(out, "\n"+VerifyNudge(d.Domain, true))
			}
		}
		return nil
	}

	fmt.Fprintln(out, i18n.Tf("domain.verify_rechecking", d.Domain))
	// The watch's waiting tick never names the target, assuming the records are
	// already on screen. Without this a verify run started fresh, on another
	// machine or before the record was published, would say "waiting for your
	// DNS" with nothing to act on.
	if d.State != StateServing {
		printVerifyDnsInstructions(out, d)
	}
	ctx, cancel := signalCtx()
	defer cancel()
	return watchVerifyFn(ctx, client, config.ID, d.Domain, out)
}

// printVerifyDnsInstructions renders the records block for `domain verify`. It
// asks for the observed values too, so re-checking an existing record is
// diagnostic: expected alongside what is actually out there.
func printVerifyDnsInstructions(out io.Writer, d *api.Domain) {
	table := dns.FormatRecordsTable(d, dns.FormatOpts{ShowObserved: true})
	if table == "" {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("domain.configure_dns"))
	fmt.Fprint(out, table)
}

// PointBackToOriginAndWatch renders the reverse cutover for a site leaving a
// CDN whose delivery record the customer owns: their domain still points at the
// provider's host, so they have to repoint it themselves. It lists the site's
// content-serving domains, prints the records to publish, and with watch true
// blocks until every one of them is serving on origin again.
//
// The view is the `domain set` one without the confirmation line, because after
// the CDN goes away the domain is an ordinary one waiting on DNS. The caller has
// already printed its own header.
//
// ctx is the caller's, so that one interrupt region can span a deactivation wait
// and this watch; a cancel here returns ErrWatchInterrupted.
//
// The bool reports whether there was a fronted content domain to bring back. It
// is false, with nothing printed, when the site has none, leaving the caller to
// print its own settled line.
func PointBackToOriginAndWatch(ctx context.Context, client APIClient, siteID string, watch bool, out io.Writer) (bool, error) {
	list, err := client.ListDomains(siteID)
	if err != nil {
		return false, api.MapError(err)
	}

	content := contentDomains(list)
	if len(content) == 0 {
		return false, nil
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("domain.configure_dns"))
	for i := range content {
		fmt.Fprint(out, dns.FormatRecordsTable(&content[i], dns.FormatOpts{ShowObserved: false}))
	}

	names := domainNames(content)

	fmt.Fprintln(out)
	if len(names) == 1 {
		fmt.Fprintln(out, names[0])
		fmt.Fprintln(out, i18n.Tf("domain.repoint_single", names[0]))
	} else {
		fmt.Fprintln(out, i18n.T("domain.repoint_multi"))
	}
	fmt.Fprintln(out)

	if !watch {
		return true, nil
	}

	// Every content domain, not just the canonical: committing live when one
	// converges would under-report an alias still on its stale record.
	return true, watchAllToLiveFn(ctx, client, siteID, names, out)
}

// contentDomains returns the site's content-serving domains, canonical first so
// the streamed live lines come out in the natural order. Only these were
// fronted by the CDN; a redirect domain never was, so it needs no bringing
// back.
func contentDomains(list *api.DomainList) []api.Domain {
	var canonical, aliases []api.Domain
	for i := range list.Domains {
		switch list.Domains[i].Role {
		case "canonical":
			canonical = append(canonical, list.Domains[i])
		case "alias":
			aliases = append(aliases, list.Domains[i])
		}
	}
	return append(canonical, aliases...)
}

func domainNames(domains []api.Domain) []string {
	names := make([]string, len(domains))
	for i := range domains {
		names[i] = domains[i].Domain
	}
	return names
}

// WatchExitToLive renders the CDN exit for a provider whose delivery record we
// own: the customer's domain already points at our indirection host, so there
// is no customer DNS step and no records block. Repointing the indirection off
// the provider is ours to do, so the copy asks the user for nothing.
//
// With watch true it streams every content-serving domain and commits live only
// once all converge; otherwise it prints the resume hint and returns. The caller
// has already printed its own header.
//
// Liveness is the watch's `serving` state, never a public DNS or HTTPS check of
// our own: during the flip window a visitor can still be served the provider's
// cached certificate, so only what our own edge serves is worth trusting.
//
// ctx is the caller's, so one interrupt region can span a deactivation wait and
// this watch. The bool reports whether there was a fronted content domain to
// bring back; false leaves the caller to print its own settled line, which
// happens when the site has only a redirect domain, or no custom domain at all.
func WatchExitToLive(ctx context.Context, client APIClient, siteID string, watch bool, out io.Writer) (bool, error) {
	list, err := client.ListDomains(siteID)
	if err != nil {
		return false, api.MapError(err)
	}

	content := contentDomains(list)
	if len(content) == 0 {
		return false, nil
	}
	names := domainNames(content)

	// The repoint is ours and propagates on a TTL we control, so the lead-in
	// names it without asking the user for anything.
	fmt.Fprintln(out, i18n.T("domain.switching_to_edge"))

	if !watch {
		fmt.Fprintln(out, i18n.T("domain.watch_status_hint"))
		return true, nil
	}

	return true, watchAllToLiveFn(ctx, client, siteID, names, out)
}

// ourSideWaitTimeout covers the server's first reconcile pass, which pushes the
// edge route and upserts DNS where we own it. That takes a few seconds, so a
// minute leaves generous headroom for retries against a flaky edge or DNS API.
const ourSideWaitTimeout = 60 * time.Second

// waitAndRender blocks on the sync attempt and renders its outcome. A timeout
// still returns the error, so a scripted run can tell "timed out, status
// unknown" apart from "definitely succeeded".
//
// Every caller opens an unterminated in-progress line and leaves it to this
// function to close, so every path through it must write exactly one line,
// outcome or not.
func waitAndRender(client APIClient, siteID string, attemptID int64, out io.Writer) error {
	if _, err := client.WaitForSync(siteID, attemptID, ourSideWaitTimeout); err != nil {
		// The refusal is the whole message and the command layer prints it, so a
		// "failed." line in front of it would only dull it. The caller's line
		// still has to be closed, so the outcome word is dropped, not the newline.
		if errors.Is(err, api.ErrUpgradeRequired) {
			fmt.Fprintln(out)
			return err
		}
		switch {
		case errors.Is(err, api.ErrSyncTimeout):
			fmt.Fprintln(out, i18n.T("domain.wait_still_syncing"))
		default:
			fmt.Fprintln(out, i18n.T("domain.wait_failed"))
		}
		return err
	}
	fmt.Fprintln(out, i18n.T("domain.wait_done"))
	return nil
}

// syncVerb says which of the two things the our-side line is reporting. It is a
// marker, not a word: each of the three frames below carries a whole message per
// verb, so a language that does not inflect one English participle into three
// forms still writes three ordinary sentences.
//
// Two values is the whole set. Each frame tests for removal and treats anything
// else as the link wording, so a third verb added here without its own key at
// all three frames would silently render as a link.
type syncVerb int

const (
	syncVerbLink syncVerb = iota
	syncVerbRemove
)

// syncOpts parametrizes interactiveSync for the two shapes the domain verbs
// take: setting a domain up, where the user has a record to publish and a
// go-live to wait for, and tearing one down, where they have neither and
// completing the teardown is the terminal state.
type syncOpts struct {
	verb             syncVerb
	showInstructions bool
	watch            bool
}

// ourSideStartLine is the in-progress line the our-side wait opens on.
func ourSideStartLine(verb syncVerb) string {
	if verb == syncVerbRemove {
		return i18n.T("domain.our_side_removing")
	}
	return i18n.T("domain.our_side_linking")
}

// ourSideFailedLine is the ✗ line, naming the mapped reason and a way to resume.
func ourSideFailedLine(verb syncVerb, reason, resume string) string {
	if verb == syncVerbRemove {
		return i18n.Tf("domain.our_side_remove_failed", reason, resume)
	}
	return i18n.Tf("domain.our_side_link_failed", reason, resume)
}

// ourSideDoneLine is the settled ✓ line.
func ourSideDoneLine(verb syncVerb) string {
	if verb == syncVerbRemove {
		return i18n.T("domain.our_side_removed")
	}
	return i18n.T("domain.our_side_linked")
}

// interactiveSync renders one top-to-bottom narrative for the domain verbs, in
// this order:
//
//  1. the DNS record and the resume contract, before any blocking wait, because
//     the user needs the record at their registrar and our own sync is
//     irrelevant to that;
//  2. the three-state "<verb> your domain on our side" line, which carries its
//     own resume hint on failure or timeout so the user is never left without a
//     way forward, while the error still propagates;
//  3. the streamed go-live watch.
//
// One cancellable context spans the whole region, so a Ctrl-C anywhere in it
// prints stop copy and returns ErrWatchInterrupted, never a bare exit code with
// nothing said.
func interactiveSync(client APIClient, siteID, domainName string, attemptID int64, d *api.Domain, opts syncOpts, out io.Writer) error {
	ctx, stop := signalCtx()
	defer stop()

	if opts.showInstructions {
		printDnsInstructions(out, d)
		fmt.Fprintln(out)
		fmt.Fprintln(out, domainName)
		fmt.Fprintln(out, i18n.Tf("domain.add_record_resume", domainName))
		fmt.Fprintln(out)
	}

	if err := renderOurSideCtx(ctx, client, siteID, domainName, attemptID, opts, out); err != nil {
		return err
	}

	if opts.watch {
		return watchToLiveFn(ctx, client, siteID, domainName, out)
	}
	return nil
}

// renderOurSideCtx prints the three-state "<verb> your domain on our side"
// line, racing the sync against ctx. WaitForSync blocks in a poll loop of its
// own and takes no context, so it runs in a goroutine raced against ctx.Done().
// That goroutine writes to a cap-1 channel, so it never blocks and leaks
// nothing even though the interrupt path stops reading.
//
// An interrupt prints the watch's detach copy where a go-live wait was still to
// come, and a teardown-appropriate stop line otherwise. Both return
// ErrWatchInterrupted, never a bare exit code with no copy.
func renderOurSideCtx(ctx context.Context, client APIClient, siteID, domainName string, attemptID int64, opts syncOpts, out io.Writer) error {
	tty := isTerminalWriter(out)
	fmt.Fprintln(out, ourSideStartLine(opts.verb))
	// This copy is fixed and never wraps at a sane width, so rewinding exactly
	// one line is correct. The watch counts rendered rows instead, because its
	// frames vary. Lengthening this line means revisiting the fixed rewind.
	redraw := func() {
		if tty {
			fmt.Fprint(out, "\033[1A\033[J")
		}
	}

	type syncResult struct{ err error }
	done := make(chan syncResult, 1)
	go func() {
		_, err := client.WaitForSync(siteID, attemptID, ourSideWaitTimeout)
		done <- syncResult{err: err}
	}()

	select {
	case <-ctx.Done():
		if opts.watch {
			printDetachCopy(out, []string{domainName})
		} else {
			fmt.Fprintln(out)
			fmt.Fprintln(out, i18n.T("domain.stopped_teardown"))
		}
		return ErrWatchInterrupted
	case r := <-done:
		if r.err != nil {
			redraw()
			// Nothing on our side failed, and both resume pointers name commands
			// the same floor refuses. The ✗ line and the already-reported wrap are
			// skipped so the command layer prints the upgrade message.
			if errors.Is(r.err, api.ErrUpgradeRequired) {
				return r.err
			}
			resume := i18n.Tf("domain.resume_verify", domainName)
			if !opts.watch {
				resume = i18n.T("domain.resume_status")
			}
			// A timeout is not a failure, our side is still reconciling, so the
			// line keeps the in-progress glyph and offers a resume path. The ✗
			// form is reserved for a real reported error. The error still
			// propagates, so the exit code says "status unknown".
			if errors.Is(r.err, api.ErrSyncTimeout) {
				fmt.Fprintln(out, i18n.Tf("domain.our_side_still_syncing", resume))
				return r.err
			}
			mapped := api.MapError(r.err)
			fmt.Fprintln(out, ourSideFailedLine(opts.verb, mapped.Error(), resume))
			// Wrapping the mapped error keeps the returned error and the ✗ line
			// saying the same thing, and marks the reason as already on stdout so
			// the command layer does not echo it again.
			return &ourSideError{mapped}
		}
		redraw()
		fmt.Fprintln(out, ourSideDoneLine(opts.verb))
		return nil
	}
}

// ErrOurSideShown marks a failure whose reason is already on the ✗ our-side
// line, so the command layer exits non-zero without echoing it again. One
// report, not two. Error and Unwrap still yield the original error.
var ErrOurSideShown = errors.New("our-side failure already shown to the user")

type ourSideError struct{ err error }

func (e *ourSideError) Error() string        { return e.err.Error() }
func (e *ourSideError) Unwrap() error        { return e.err }
func (e *ourSideError) Is(target error) bool { return target == ErrOurSideShown }

// Set sets or replaces the canonical domain for the linked site. By default it
// blocks until the domain is live, with a streamed go-live view: the record the
// user needs at their registrar and the resume contract first, since our own
// sync is irrelevant to the registrar step, then the wait on that sync, then the
// watch to ✓ live. There is no give-up timeout; the watch runs until live or
// Ctrl-C.
//
// `--no-wait`, and a non-TTY invocation, return as soon as the change is queued,
// with the record and the resume hint and no cursor control.
//
// Exit codes: 0 live, the conventional interrupt code on Ctrl-C, non-zero on a
// transport or API error.
func Set(client APIClient, domainName string, noWait bool, out io.Writer) error {
	interactive := !noWait && isTerminalWriter(out)
	return set(client, domainName, interactive, out)
}

// set is the testable core of Set.
func set(client APIClient, domainName string, interactive bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	domain, err := client.SetCanonical(config.ID, domainName)
	if err != nil {
		return api.MapError(err)
	}

	if !interactive {
		// The resume hint names `domain verify` first, being the fast path, and
		// the record is printed so a scripted run still has it.
		fmt.Fprintln(out, i18n.Tf("domain.set_queued", domain.Domain, domain.Domain))
		printDnsInstructions(out, domain)
		return nil
	}

	fmt.Fprintln(out, i18n.Tf("domain.set_done", domain.Domain))
	return interactiveSync(client, config.ID, domain.Domain, domain.SyncAttemptID, domain,
		syncOpts{verb: syncVerbLink, showInstructions: true, watch: true}, out)
}

var signalCtx = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

var watchToLiveFn = watchToLiveCtx

var watchAllToLiveFn = watchAllToLiveCtx

// Unset removes the canonical domain from the linked site. The removal is
// asynchronous: the domain enters teardown and the reconciler does the edge
// refresh and the delete.
func Unset(client APIClient, noWait bool, out io.Writer) error {
	interactive := !noWait && isTerminalWriter(out)
	return unset(client, noWait, interactive, out)
}

// unset is the testable core of Unset, with three paths: `--no-wait` returns
// once queued, non-TTY runs the plain blocking render, and a terminal gets the
// shared teardown render. None of them watches, because the teardown finishing
// is the terminal state and there is no user DNS or certificate to wait on.
func unset(client APIClient, noWait, interactive bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	result, err := client.UnsetCanonical(config.ID)
	if err != nil {
		return api.MapError(err)
	}

	if noWait {
		fmt.Fprintln(out, i18n.T("domain.unset_queued"))
		return nil
	}

	if !interactive {
		fmt.Fprint(out, i18n.T("domain.unset_removing"))
		return waitAndRender(client, config.ID, result.SyncAttemptID, out)
	}

	// Teardown never renders the domain name: instructions and the watch are
	// off, and the resume copy points at `kamakiri status`. Passing "" rather
	// than a human phrase keeps a future code path from printing prose where an
	// argument belongs.
	return interactiveSync(client, config.ID, "", result.SyncAttemptID, nil,
		syncOpts{verb: syncVerbRemove, showInstructions: false, watch: false}, out)
}

// addedRoleLabel renders the role for the confirmation line. The status code is
// appended only for a redirect, being meaningless for any other role.
func addedRoleLabel(d *api.Domain) string {
	if d.RedirectStatus > 0 {
		return fmt.Sprintf("%s %d", d.Role, d.RedirectStatus)
	}
	return d.Role
}

// Add adds a redirect or alias domain to the linked site, blocking until it is
// live with the same streamed view and the same exit codes as Set.
func Add(client APIClient, domainName, role string, redirectStatus int, noWait bool, out io.Writer) error {
	interactive := !noWait && isTerminalWriter(out)
	return add(client, domainName, role, redirectStatus, interactive, out)
}

// add is the testable core of Add.
func add(client APIClient, domainName, role string, redirectStatus int, interactive bool, out io.Writer) error {
	config, err := core.RequireProject()
	if err != nil {
		return err
	}

	domain, err := client.AddDomain(config.ID, domainName, role, redirectStatus)
	if err != nil {
		return api.MapError(err)
	}

	roleSuffix := ""
	if label := addedRoleLabel(domain); label != "" {
		roleSuffix = " (" + label + ")"
	}

	if !interactive {
		fmt.Fprintln(out, i18n.Tf("domain.add_queued", domain.Domain, roleSuffix, domain.Domain))
		printDnsInstructions(out, domain)
		return nil
	}

	fmt.Fprintln(out, i18n.Tf("domain.add_done", domain.Domain, roleSuffix))
	return interactiveSync(client, config.ID, domain.Domain, domain.SyncAttemptID, domain,
		syncOpts{verb: syncVerbLink, showInstructions: true, watch: true}, out)
}

// Remove removes a redirect or alias domain from the linked site. RequireProject
// is a guardrail for the user, not a lookup key: the server identifies the
// domain by name and API-key ownership, and its response carries the site id, so
// the CLI can wait on the sync without knowing that id locally.
func Remove(client APIClient, domainName string, noWait bool, out io.Writer) error {
	interactive := !noWait && isTerminalWriter(out)
	return remove(client, domainName, noWait, interactive, out)
}

// remove is the testable core of Remove, with the same three paths as unset.
func remove(client APIClient, domainName string, noWait, interactive bool, out io.Writer) error {
	_, err := core.RequireProject()
	if err != nil {
		return err
	}

	result, err := client.RemoveDomain(domainName)
	if err != nil {
		return api.MapError(err)
	}

	if noWait {
		fmt.Fprintln(out, i18n.Tf("domain.remove_queued", domainName))
		return nil
	}

	// An empty site id would make the wait block on the wrong key and never see
	// the reconcile finish, so say so rather than let the user sit out the full
	// timeout for no reason.
	if result.SiteID == "" {
		return errors.New(i18n.T("domain.err_no_site_id"))
	}

	if !interactive {
		fmt.Fprint(out, i18n.Tf("domain.remove_removing", domainName))
		return waitAndRender(client, result.SiteID, result.SyncAttemptID, out)
	}

	return interactiveSync(client, result.SiteID, domainName, result.SyncAttemptID, nil,
		syncOpts{verb: syncVerbRemove, showInstructions: false, watch: false}, out)
}

// printDnsInstructions renders the DNS-records block for a domain that was just
// created. It renders the records the server sent and nothing else, so the CLI
// holds no apex heuristic and no hardcoded address of its own. Observed values
// are omitted because nothing has been probed yet; the status view is where the
// diff appears.
//
// A domain with no expected records renders nothing at all, header included.
func printDnsInstructions(out io.Writer, d *api.Domain) {
	table := dns.FormatRecordsTable(d, dns.FormatOpts{ShowObserved: false})
	if table == "" {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("domain.configure_dns"))
	fmt.Fprint(out, table)
}
