package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// devVersion is the token a build that was never stamped with a version states.
// The server accepts it: local builds are run against production deliberately,
// and since any client can claim any version, refusing it would hold nothing.
const devVersion = "dev"

// version is the CLI version every client built from here on states. main sets
// it once at startup; until then it is devVersion, since a request that states
// no version at all is refused by a server enforcing a minimum.
var version = devVersion

// SetVersion records the CLI version to state on every subsequent request. It
// is called once at startup with the build's version string and normalizes it:
// one leading "v" is dropped, and an unstamped build's "(dev)" marker becomes
// the plain dev token. An empty string leaves the current value alone, so a
// build with no version stamped in still states dev.
func SetVersion(v string) {
	if v == "" {
		return
	}
	if v == "(dev)" {
		version = devVersion
		return
	}
	version = strings.TrimPrefix(v, "v")
}

// keyFromEnvironment records whether the key the run authenticates with came
// from KAMAKIRI_API_KEY rather than from the credentials file, which is what
// decides the wording of the unauthorized error. It is package state because no
// site that renders that copy can read the answer off what it holds: MapError
// takes no receiver, and the two commands which render it themselves take
// their client as an interface that carries no provenance. Like the version
// value, it is race-free by being written once before the command runs and read
// only afterwards.
//
// The invariant every reader depends on: the value was stamped by the path that
// loads the credentials before the command runs, so a command that skips that
// path must never render the unauthorized copy. `login` is the one such command,
// and it has no unauthorized arm. `status` takes the loading path without the
// exit, so it is stamped like the rest, and a failed load stamps false: the load
// answers from the environment before it touches any file, so a load error
// means no environment key.
//
// A test that flips this value stays serial and restores it, since nothing
// synchronizes it and a parallel test would race it with nothing to notice.
var keyFromEnvironment bool

// SetKeyFromEnvironment records where the run's API key came from. It is called
// once per run, before the command runs, with true when the key was resolved
// from the environment.
func SetKeyFromEnvironment(fromEnvironment bool) {
	keyFromEnvironment = fromEnvironment
}

// UnauthorizedError is the error a rejected API key renders as. It names the
// environment variable when that is where the key came from, since re-running
// `kamakiri login` fixes nothing on a runner that reads its key from there.
func UnauthorizedError() error {
	if keyFromEnvironment {
		return errors.New(i18n.T("api.err_unauthorized_env"))
	}
	return errors.New(i18n.T("api.err_unauthorized"))
}

// latestVersionHeader is the response header a server names its newest
// published CLI release in. It is advisory: it refuses nothing, and a server
// that sets none is the normal case.
const latestVersionHeader = "X-Kamakiri-Cli-Latest-Version"

// maxLatestVersionBytes is the longest header value that is recorded at all.
// The value is a version string, so this is generous by an order of magnitude,
// and a bound is needed because the value is chosen by whatever answered the
// request: the transport allows megabytes of headers, and a reader further on
// allocates in proportion to what it is handed. A longer value is dropped where
// it arrives, so nothing holds it and nothing downstream can reach it.
const maxLatestVersionBytes = 64

// latestAdvertised is the newest release the server named, held across the
// whole command. Unlike version, which is set once at startup, it is written
// from every response, so a command issuing concurrent requests would race on
// it without the mutex.
var (
	latestMu         sync.Mutex
	latestAdvertised string
)

// LatestAdvertised returns the newest CLI release the server named in a
// response to this process, or an empty string when no response named one. One
// leading "v" is already stripped, so the value is a bare version.
func LatestAdvertised() string {
	latestMu.Lock()
	defer latestMu.Unlock()
	return latestAdvertised
}

// recordLatestAdvertised keeps the release named by raw, which is the header
// exactly as it arrived. A value longer than the bound is dropped here, where
// it enters the process, rather than by whatever reads it later: refusing at
// the boundary means it is never stored, never held for the length of the
// command, and out of reach of anything that consumes the getter. An absent
// header and an overlong one are the same non-event, and neither clears what an
// earlier response named.
//
// One leading "v" is dropped after the check rather than before it, so a value
// that would fit only once shortened is still refused. The server publishes the
// bare form, and dropping a "v" if one turns up keeps the getter single-valued.
func recordLatestAdvertised(raw string) {
	if raw == "" || len(raw) > maxLatestVersionBytes {
		return
	}

	latestMu.Lock()
	defer latestMu.Unlock()
	latestAdvertised = strings.TrimPrefix(raw, "v")
}

// Client talks to the Kamakiri API.
type Client struct {
	BaseURL string
	APIKey  string
	// Version is the token stated in the User-Agent; NewClient stamps the
	// current package value onto it.
	Version    string
	HTTPClient *http.Client
}

// NewClient creates a new API client pointing at the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		Version:    version,
		HTTPClient: &http.Client{},
	}
}

// Site is the response from site endpoints.
type Site struct {
	ID                       string  `json:"id"`
	Subdomain                string  `json:"subdomain"`
	SubdomainEnabled         bool    `json:"subdomain_enabled"`
	SubdomainObserved        *string `json:"subdomain_observed,omitempty"`         // nil = never reconciled
	SubdomainEnabledObserved *bool   `json:"subdomain_enabled_observed,omitempty"` // nil = never reconciled
	// Status is the desired site status, "active" or "deleted"; StatusObserved
	// is the reconciler's teardown progress, "active" | "soft_deleted" |
	// "deleted". Together they are the teardown lifecycle.
	Status         string `json:"status,omitempty"`
	StatusObserved string `json:"status_observed,omitempty"`
	URL            string `json:"url"`
	SiteID         string `json:"site_id,omitempty"`
	SyncAttemptID  int64  `json:"sync_attempt_id,omitempty"` // present only on 202 mutation responses
	Sync           *Sync  `json:"sync,omitempty"`

	// RetainedWebAccelResources is populated only on the DELETE (teardown) 202.
	// WebAccel resources live in the customer's own Sakura account, so teardown
	// leaves them for the user to remove; a Cloudflare hostname, which we own,
	// goes automatically.
	RetainedWebAccelResources []WebAccelResource `json:"retained_webaccel_resources,omitempty"`

	// Quota carries usage against the account's plan limits, present only on the
	// GET /v1/pages/sites/:id read and nil against an older server.
	Quota *Quota `json:"quota,omitempty"`

	// Freshness is the server's site-level "is the new site actually live"
	// verdict, emitted only on the GET /v1/pages/sites/:id read where the domains
	// are preloaded. A pointer, so an absent block decodes to nil, which the
	// freshness wait treats as a hard error rather than as pending or fresh.
	Freshness *Freshness `json:"freshness,omitempty"`

	// Site-level edge cache purge health, emitted alongside Freshness: the
	// server-derived verdict ("ok" | "failing" | "broken"), the short tag behind
	// a failing one, and the anchor for the "since" age. The raw
	// requested/flushed timestamps are deliberately off the wire, their verdict
	// carried by Freshness.Axes.Edge and their age by the edge Pending entry.
	EdgePurgeHealth      string `json:"edge_purge_health,omitempty"`
	EdgePurgeErrorReason string `json:"edge_purge_error_reason,omitempty"`
	EdgePurgeLastOkAt    string `json:"edge_purge_last_ok_at,omitempty"`
}

// Freshness is the site-level freshness verdict: has every cache a visitor
// could hit (our origin, our edge, each fronting CDN edge) caught up to the
// current deploy. The freshness wait polls it until State is "fresh".
type Freshness struct {
	// State is the tri-state verdict and the only field the wait's return
	// decision reads: "fresh" (return live), "pending" (keep waiting) or
	// "blocked" (a terminal flush failure the user must act on). Any other
	// value, "" included, is a hard error, never treated as pending or fresh.
	State string `json:"state"`

	// LiveURL is the address the verdict is about: the serving canonical custom
	// domain, else the managed subdomain when enabled, else "" (the milestone
	// then renders bare rather than naming a host that serves nothing). Site.URL
	// cannot stand in for it, since that one hardcodes the managed subdomain.
	LiveURL string `json:"live_url,omitempty"`

	// Axes are the per-axis facts behind the verdict; the wait itself reads
	// State, Pending and Blockers, not these. All three keys ship regardless, so
	// folding further axes in later adds keys rather than reshaping the block.
	Axes FreshnessAxes `json:"axes"`

	// Pending is one entry per outstanding target, always present and empty when
	// State is "fresh", ordered origin, edge, then host-sorted CDN. The CLI
	// cannot derive it: identifying a pending CDN domain is a server-side
	// requested-versus-flushed comparison. Disjoint from Blockers, so a blocked
	// domain is never reported twice.
	Pending []PendingTarget `json:"pending"`

	// Blockers is one entry per terminal flush failure, always present and empty
	// unless State is "blocked".
	Blockers []Blocker `json:"blockers"`
}

// FreshnessAxes carries the three freshness axes as their read-time verdicts.
// Origin is "satisfied" | "pending" (does the edge serve the current live
// deploy), Edge is "flushed" | "flushing" (our own cache), and Cdn is
// "flushed" | "flushing" | "blocked" (every fronting active-CDN domain).
type FreshnessAxes struct {
	Origin string `json:"origin"`
	Edge   string `json:"edge"`
	Cdn    string `json:"cdn"`
}

// PendingTarget is one outstanding flush the wait may name. Axis is "origin" |
// "edge" | "cdn"; Provider and Host are set only for a CDN entry, since origin
// and edge are ours and single. OwedSince (RFC3339) is that axis's own
// owed-since anchor: the origin sync attempt's start, the edge purge request, or
// the domain's purge request. An empty OwedSince means the age parenthetical is
// dropped rather than computed from nothing.
type PendingTarget struct {
	Axis      string `json:"axis"`
	Provider  string `json:"provider,omitempty"`
	Host      string `json:"host,omitempty"`
	OwedSince string `json:"owed_since,omitempty"`
}

// Blocker is one terminal flush failure, rendered as an actionable entry by the
// freshness wait and by `kamakiri status`.
// Reason is the closed set "credentials_rejected" | "resource_deleted" |
// "provider_rejected" | "site_torn_down", and the CLI branches on it to pick the
// remediation. Provider ("webaccel" | "cloudflare") and Host name the affected
// CDN domain, both empty for the site-level "site_torn_down" arm. Detail carries
// the provider's own rejection message, server-truncated and sanitized.
type Blocker struct {
	Provider string `json:"provider,omitempty"`
	Host     string `json:"host,omitempty"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
}

// Quota is the per-account storage usage block on the site read. SitesUsed and
// SitesMax are account-scoped; SnapshotsUsed and SnapshotsMax are for the site
// being read; MaxSnapshotBytes is the per-deploy size ceiling.
type Quota struct {
	SitesUsed        int   `json:"sites_used"`
	SitesMax         int   `json:"sites_max"`
	SnapshotsUsed    int   `json:"snapshots_used"`
	SnapshotsMax     int   `json:"snapshots_max"`
	MaxSnapshotBytes int64 `json:"max_snapshot_bytes"`
}

// Sync is the site's latest reconcile attempt. Error carries the reconciler's
// own failure reason, which WaitForSync surfaces rather than replacing with a
// generic message; an empty one falls back to pointing at `kamakiri status`.
type Sync struct {
	LatestAttemptID int64    `json:"latest_attempt_id"`
	Outcome         string   `json:"outcome"`
	AttemptedAt     string   `json:"attempted_at"`
	FinishedAt      string   `json:"finished_at,omitempty"`
	TriggeredBy     string   `json:"triggered_by,omitempty"`
	Changes         []string `json:"changes,omitempty"`
	Error           string   `json:"error,omitempty"` // populated when Outcome == "error"
}

// DeployResult is the 202 response from POST /v1/pages/deploy and POST
// /v1/pages/rollback. Liveness is gated on the freshness wait polling GET
// /v1/pages/sites/:id, not on this response. There is no site_id: the CLI
// already holds it locally.
type DeployResult struct {
	URL      string `json:"url"`
	DeployID string `json:"deploy_id"`
	// SyncAttemptID is on the wire but unread: deploy and rollback poll
	// site.freshness rather than this handle.
	SyncAttemptID int64 `json:"sync_attempt_id,omitempty"`
}

// Deploy is a single deploy entry from GET /v1/pages/deploys.
type Deploy struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// DeployList is the response from GET /v1/pages/deploys.
type DeployList struct {
	Deploys []Deploy `json:"deploys"`
}

// CdnHealth is the read-time CDN health verdict the server derives from the
// row's lifecycle position and observed facts, the CDN-axis analogue of
// CertVerdict. The CLI dispatches verdict-first on it: a problem verdict
// (degraded | broken | timed_out | misconfigured | unverified) renders from
// CdnHealth, otherwise the lifecycle position from CdnState does.
//
// Verdict is the closed set off | healthy | pending | degraded | broken |
// timed_out | misconfigured | unverified, or "" from a server too old to derive
// it. Reason is a closed-set problem tag or "", the ones this CLI branches on
// being named in internal/cdn. Since (RFC3339) is the age anchor for
// whichever clock the verdict is about, "" when none applies.
type CdnHealth struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Since   string `json:"since"`
}

// Domain is the response from domain endpoints.
type Domain struct {
	Domain         string `json:"domain"`
	Role           string `json:"role"`
	RedirectStatus int    `json:"redirect_status,omitempty"`
	DnsTarget      string `json:"dns_target,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`

	// SyncAttemptID is populated on the 202 from SetCanonical and AddDomain;
	// RemoveDomain returns it via RemoveDomainResult instead.
	SyncAttemptID int64 `json:"sync_attempt_id,omitempty"`

	// State is the derived per-domain state, one of awaiting_dns |
	// awaiting_edge | awaiting_cert | serving | degraded | tearing_down.
	State string `json:"state,omitempty"`

	// HasLiveDeploy is the site-level fact that the domain's site has a published
	// deploy. A deploy-less site emits no edge route, so a domain past DNS rests
	// in awaiting_edge and can get neither a route nor a certificate until the
	// site is deployed. A pointer, so nil (an older server that omits the field)
	// is distinguishable from an explicit false: the copy flips to deploy-first
	// only on the explicit false, never on the unknown case.
	HasLiveDeploy *bool `json:"has_live_deploy,omitempty"`

	DeleteRequestedAt string `json:"delete_requested_at,omitempty"`

	// Customer-DNS axis, server-derived. DnsVerdict compares the observed value
	// against the current expected, one of "correct" | "present_but_wrong" |
	// "absent" | "unreachable"; the CLI renders it rather than re-implementing
	// apex/alternative matching. "unreachable" tracks the delivery (primary)
	// record alone, and each record's own transient error rides per-entry in
	// DNSRecordsObserved, so there is no top-level DNS error field.
	// DnsMatchedAlternative is the apex user-intent signal ("a" |
	// "alias_or_aname" | ""); DnsCheckedAt is the last probe attempt.
	DnsVerdict            string `json:"dns_verdict,omitempty"`
	DnsCheckedAt          string `json:"dns_checked_at,omitempty"`
	DnsMatchedAlternative string `json:"dns_matched_alternative,omitempty"`

	// Cert axis, server-derived like the DNS one: the CLI renders the verdict
	// rather than deriving one from the latch columns. CdnMode (the site row's
	// mode) drives the terminal copy on go-live: direct mode claims the
	// certificate, Cloudflare and WebAccel say the domain is routed via that
	// vendor instead, since we issue no certificate in those modes.
	//
	// CertVerdict is the read-time verdict against the current expected:
	// "correct" | "wrong" | "absent" | "unreachable", or "" from an older server.
	// CertError is the closed-set category naming why ("cert_unavailable",
	// "wrong_subject", "expired", "renewal_overdue", "chain_invalid") or "",
	// never a raw error term. CertObservedLastAt is the last confirmation of the
	// observed cert; CertCheckedAt is the last probe attempt and anchors the
	// degraded "last checked" copy.
	CdnMode            string `json:"cdn_mode,omitempty"`
	CertRequired       bool   `json:"cert_required,omitempty"`
	CertVerdict        string `json:"cert_verdict,omitempty"`
	CertError          string `json:"cert_error,omitempty"`
	CertObservedLastAt string `json:"cert_observed_last_at,omitempty"`
	CertCheckedAt      string `json:"cert_checked_at,omitempty"`
	CertObserveError   string `json:"cert_observe_error,omitempty"`
	CertObserveErrorAt string `json:"cert_observe_error_at,omitempty"`

	// Per-domain CDN lifecycle position plus the derived health verdict.
	// CdnState is the lifecycle position: off | awaiting_provisioning |
	// awaiting_verification | awaiting_cf_validation | ready_to_flip | active
	// (the constants live in internal/cdn/state.go). The "is it any good"
	// verdict lives in CdnHealth, which the CLI dispatches on first, falling back
	// to the position; a problem condition arrives as a CdnHealth verdict, so the
	// CLI never re-derives one from the position. CfLastCheckedAt and
	// WebaccelLastCheckedAt are the last probe attempt; the degraded "since" and
	// the validation age come from CdnHealth.Since instead.
	CdnState              string    `json:"cdn_state,omitempty"`
	CdnStateObserved      string    `json:"cdn_state_observed,omitempty"`
	CdnHealth             CdnHealth `json:"cdn_health"`
	CfLastCheckedAt       string    `json:"cf_last_checked_at,omitempty"`
	WebaccelLastCheckedAt string    `json:"webaccel_last_checked_at,omitempty"`

	// DNSRecordsExpected is the source of truth for the DNS records the customer
	// must add at their registrar; DNSRecordsObserved is the most recent probe's
	// per-record observation. The match verdict lives on the top-level
	// DnsVerdict and DnsMatchedAlternative, never on the entries, though each
	// entry does carry its own transient probe error.
	DNSRecordsExpected []DNSRecord         `json:"dns_records_expected,omitempty"`
	DNSRecordsObserved []DNSRecordObserved `json:"dns_observed,omitempty"`

	// Per-domain CDN purge-health axis, ok | failing | broken. An empty value is
	// a server that predates the axis, and the renderer treats it as healthy
	// rather than as an unrecognised value to warn about.
	// CdnPurgeErrorReason is a short tag ("http_401", "network_timeout") the CLI
	// maps to a user-facing hint.
	CdnPurgeHealth              string `json:"cdn_purge_health"`
	CdnPurgeLastOkAt            string `json:"cdn_purge_last_ok_at,omitempty"`
	CdnPurgeLastFailedAt        string `json:"cdn_purge_last_failed_at,omitempty"`
	CdnPurgeConsecutiveFailures int    `json:"cdn_purge_consecutive_failures,omitempty"`
	CdnPurgeErrorReason         string `json:"cdn_purge_error_reason,omitempty"`

	// CdnPurgeBlocked says this domain appears in its site's freshness Blockers.
	// The CLI deliberately does not consume it for per-domain suppression:
	// `kamakiri status` reads the site payload and the domain rows in two
	// separate requests, so this flag could disagree with the site response
	// within one frame and drop a blocker line nothing site-level then renders.
	// Suppression keys off the site payload's own blocker-host set instead.
	CdnPurgeBlocked bool `json:"cdn_purge_blocked,omitempty"`
}

// WebAccelResource names a customer-owned WebAccel resource that teardown leaves
// in place. Subdomain is the delivery hostname the user recognizes in their
// Sakura panel; ID is the WebAccel "Site" resource id, the fallback label when
// the subdomain was never captured.
type WebAccelResource struct {
	ID        string `json:"id"`
	Subdomain string `json:"subdomain,omitempty"`
}

// UnsetCanonicalResult is the response from DELETE /v1/pages/domains/canonical.
// The async unset returns the queued sync_attempt_id to wait on.
type UnsetCanonicalResult struct {
	SyncAttemptID int64 `json:"sync_attempt_id"`
}

// RemoveDomainResult is the response from DELETE /v1/pages/domains/:domain. The
// async remove returns site_id as well, since the CLI knows only the domain name
// and WaitForSync needs the site.
type RemoveDomainResult struct {
	SiteID        string `json:"site_id"`
	SyncAttemptID int64  `json:"sync_attempt_id"`
}

// DomainList is the response from GET /v1/pages/domains.
type DomainList struct {
	Domains []Domain `json:"domains"`
}

// RegistrationTXT is the `_kamakiri-verify` record the user must publish to
// prove ownership, returned on the register response.
type RegistrationTXT struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// RegistrationAttachment is one of the account's active site attachments at or
// under a registration's name. RedirectStatus is 0 for non-redirect roles.
type RegistrationAttachment struct {
	SiteID         string `json:"site_id"`
	Host           string `json:"host"`
	Role           string `json:"role"`
	RedirectStatus int    `json:"redirect_status,omitempty"`
}

// Registration is an account-level domain ownership row. One struct serves both
// responses: the POST populates Name/Status/VerifyToken/TXT/VerifiedAt, a GET
// list entry Name/Status/VerifiedAt/Attachments, and the unused fields are zero.
// Status is "awaiting" or "verified"; a registration claims no exclusivity, so
// another account can never spoil one.
type Registration struct {
	Name        string                   `json:"name"`
	Status      string                   `json:"status"`
	VerifyToken string                   `json:"verify_token,omitempty"`
	VerifiedAt  string                   `json:"verified_at,omitempty"`
	TXT         RegistrationTXT          `json:"txt"`
	Attachments []RegistrationAttachment `json:"attachments,omitempty"`
}

// RegistrationList is the response from GET /v1/pages/registrations.
type RegistrationList struct {
	Registrations []Registration `json:"registrations"`
}

// CDNSetResponse is the 202 response from POST /v1/pages/cdn/set, an async
// write-and-enqueue: the new desired cdn_mode plus the queued sync_attempt_id to
// wait on. Cloudflare DCV records and per-domain WebAccel state are not on this
// body; they surface afterwards via GET /v1/pages/cdn/status.
//
// ApexARecordsToUpdate carries the post-flip A IPs for every apex domain whose
// observed matched_alternative is "a", so the user can update their registrar;
// empty when no apex is on A records. ApexARecordsPending carries the apexes
// whose IPs are not known yet because provisioning has not finished, which the
// CLI names without IPs, telling the user to re-run `kamakiri status` shortly
// rather than misadvising cluster IPs that would bypass the CDN.
type CDNSetResponse struct {
	CDNMode              string                 `json:"cdn_mode"`
	SyncAttemptID        int64                  `json:"sync_attempt_id,omitempty"`
	ApexARecordsToUpdate []ApexARecordsToUpdate `json:"apex_a_records_to_update,omitempty"`
	ApexARecordsPending  []ApexARecordsPending  `json:"apex_a_records_pending,omitempty"`
}

// ApexARecordsToUpdate carries the per-apex A-record advice the server computes
// after a mode flip: the apex hostname and what the customer must set at their
// registrar for the new mode.
type ApexARecordsToUpdate struct {
	Domain           string   `json:"domain"`
	ExpectedARecords []string `json:"expected_a_records"`
}

// ApexARecordsPending carries an apex whose post-flip A IPs are not available
// yet because the reconciler has not captured them (under WebAccel, before the
// per-domain Subdomain capture runs). Reason is a stable identifier
// ("webaccel_pending") the CLI may switch on.
type ApexARecordsPending struct {
	Domain string `json:"domain"`
	Reason string `json:"reason"`
}

// CDNPurgeResponse is the 202 response from POST /v1/pages/cdn/purge, a manual
// cache purge. site_id and sync_attempt_id let the CLI wait on the reconciler's
// purge step via WaitForSync.
type CDNPurgeResponse struct {
	SiteID        string `json:"site_id"`
	SyncAttemptID int64  `json:"sync_attempt_id"`
}

// CDNCleanupResponse is the 202 response from POST /v1/pages/cdn/cleanup: the
// count of orphaned WebAccel resources the server will delete and how many are
// terminally blocked on invalid credentials, a pre-arm snapshot the CLI echoes.
// Completion is tracked by polling CDNStatus's cleanup_orphan_count.
type CDNCleanupResponse struct {
	SiteID        string `json:"site_id"`
	SyncAttemptID int64  `json:"sync_attempt_id"`
	OrphanCount   int    `json:"orphan_count"`
	BlockedCount  int    `json:"blocked_count"`
}

// CDNStatusResponse is the response from GET /v1/pages/cdn/status.
type CDNStatusResponse struct {
	CDNMode         string            `json:"cdn_mode"`
	CDNModeObserved string            `json:"cdn_mode_observed,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	Domains         []CDNDomainStatus `json:"domains,omitempty"`

	// Site-level WebAccel credential-health axis. It is site-level, not
	// per-domain, because the WebAccel API token gates our control plane (purge,
	// probe, rotate) rather than Sakura's edge: a failing token never collapses
	// into a per-domain cdn_state, and the rows still render as routed via
	// WebAccel.
	CdnCredentialsHealth              string `json:"cdn_credentials_health"`
	CdnCredentialsLastOkAt            string `json:"cdn_credentials_last_ok_at,omitempty"`
	CdnCredentialsLastFailedAt        string `json:"cdn_credentials_last_failed_at,omitempty"`
	CdnCredentialsConsecutiveFailures int    `json:"cdn_credentials_consecutive_failures,omitempty"`
	CdnCredentialsErrorReason         string `json:"cdn_credentials_error_reason,omitempty"`

	// WebAccel resources awaiting `cdn cleanup`, counted over both active
	// non-webaccel-mode rows and tombstoned rows (so a removed domain's orphan
	// still counts), plus the subset blocked by credentials that are no longer
	// valid.
	CleanupOrphanCount  int `json:"cleanup_orphan_count,omitempty"`
	CleanupBlockedCount int `json:"cleanup_blocked_count,omitempty"`
}

// DNSRecord is one entry in dns_records_expected, a record the customer must
// (or for Cloudflare DCV, may) add at their registrar.
//
// Type is one of "a" | "cname" | "txt" | "alias_or_aname". The "alias_or_aname"
// type is virtual: the customer's provider resolves it to A records, so probes
// never query it directly and renderers offer it as ALIAS or ANAME, whichever
// that provider supports.
//
// Purpose is "primary" (gates the DNS-axis state to "serving"), "validation"
// (Cloudflare DCV, orthogonal to state, gates cdn_state) or "ownership" (the
// WebAccel TXT proving domain ownership before the flip-last go-live).
//
// AlternativeGroup is "" for a standalone record, or a group name
// ("apex_primary") meaning this record is one option in a pick-one set,
// satisfied as soon as any required member matches. Apex domains use it, where A
// records and ALIAS/ANAME are mutually exclusive at the same name.
type DNSRecord struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	Value            string `json:"value"`
	Purpose          string `json:"purpose"`
	Required         bool   `json:"required"`
	AlternativeGroup string `json:"alternative_group,omitempty"`
}

// DNSRecordObserved is one entry in the server's dns_observed value, the most
// recent probe's resolver answers grouped by (Name, Type). It is a pure
// observation: the match verdict is derived server-side and surfaced on
// Domain.DnsVerdict and Domain.DnsMatchedAlternative, never baked into the
// entries.
//
// Values is the union of resolver answers (deduped, sorted), nil for
// alias_or_aname placeholders since the type is not queryable and the sibling A
// entry carries the wire-level result. AliasResolved is the resolved dns_target
// A set carried alongside the apex (name, a) entry, the comparison source for
// the ALIAS/ANAME path.
//
// ObserveError and ObserveErrorAt carry this record's last transient probe
// failure, where the resolver could not be reached (SERVFAIL, timeout, network).
// The server clears both on the next definitive observation, so a set error
// always means the latest probe of this record could not confirm it. The
// alias_or_aname placeholders never carry them; the sibling A entry does.
type DNSRecordObserved struct {
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	Values           []string `json:"values"`
	AlternativeGroup string   `json:"alternative_group,omitempty"`
	AliasResolved    []string `json:"alias_resolved,omitempty"`
	ObserveError     string   `json:"observe_error,omitempty"`
	ObserveErrorAt   string   `json:"observe_error_at,omitempty"`
}

// CDNDomainStatus is a single domain entry in the CDN status response. CDNID is
// the per-domain provider-specific identifier (a WebAccel site ID, say), present
// only when the site uses a CDN mode that requires per-domain IDs. DCV records
// arrive in DNSRecordsExpected with Purpose "validation"; there is no dedicated
// field for them.
type CDNDomainStatus struct {
	Domain string `json:"domain"`
	CDNID  string `json:"cdn_id,omitempty"`

	CdnState              string    `json:"cdn_state,omitempty"`
	CdnStateObserved      string    `json:"cdn_state_observed,omitempty"`
	CdnHealth             CdnHealth `json:"cdn_health"`
	CfLastCheckedAt       string    `json:"cf_last_checked_at,omitempty"`
	WebaccelLastCheckedAt string    `json:"webaccel_last_checked_at,omitempty"`

	// WebAccel go-live observed facts: per-domain timestamp latches plus an
	// ownership status, which the CLI projects into the go-live milestones
	// (resource ready, ownership verified, enabled, routed).
	//
	// Under BYO, WebaccelRoutedObservedAt means the customer's delivery CNAME
	// resolves directly to the subdomain, a record they published and we observe,
	// not that our own indirection flip landed; no consumer may read it that way.
	// WebaccelEnabledObservedAt is also the key the renderer dispatches on to
	// tell the two awaiting_verification sub-states apart, publish-TXT from
	// publish-delivery. The cert latch is best-effort: the SSL milestone gates on
	// the client's own check. All the latches are forward-only, so a Ctrl-C and
	// re-run resumes where it left off.
	WebaccelSubdomain           string `json:"webaccel_subdomain,omitempty"`
	WebaccelOwnershipObservedAt string `json:"webaccel_ownership_observed_at,omitempty"`
	WebaccelOwnershipStatus     string `json:"webaccel_ownership_status,omitempty"`
	WebaccelEnabledObservedAt   string `json:"webaccel_enabled_observed_at,omitempty"`
	WebaccelRoutedObservedAt    string `json:"webaccel_routed_observed_at,omitempty"`
	WebaccelCertObservedAt      string `json:"webaccel_cert_observed_at,omitempty"`

	// The customer-DNS axis fields the cdn status endpoint shares with the domain
	// payload, carrying the same server-derived verdict.
	DNSRecordsExpected    []DNSRecord         `json:"dns_records_expected,omitempty"`
	DNSRecordsObserved    []DNSRecordObserved `json:"dns_observed,omitempty"`
	DnsVerdict            string              `json:"dns_verdict,omitempty"`
	DnsMatchedAlternative string              `json:"dns_matched_alternative,omitempty"`

	// Per-domain purge-health fields, the same five Domain carries and with the
	// same meaning.
	CdnPurgeHealth              string `json:"cdn_purge_health"`
	CdnPurgeLastOkAt            string `json:"cdn_purge_last_ok_at,omitempty"`
	CdnPurgeLastFailedAt        string `json:"cdn_purge_last_failed_at,omitempty"`
	CdnPurgeConsecutiveFailures int    `json:"cdn_purge_consecutive_failures,omitempty"`
	CdnPurgeErrorReason         string `json:"cdn_purge_error_reason,omitempty"`
}

// ErrorResponse is returned by the API on errors.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	SiteID  string `json:"site_id,omitempty"`
}

func (e *ErrorResponse) Error() string {
	return e.Message
}

// ErrUpgradeRequired is returned when the server refuses this CLI version. Its
// text is the whole message the user sees, so callers that print an error
// verbatim need no arm of their own; callers that render per-code copy match it
// with errors.Is. The message is looked up when it is printed rather than here,
// since this value is built during package initialization, before the language
// is resolved.
var ErrUpgradeRequired = i18n.NewError("api.err_upgrade_required")

// MapError translates common API error codes into user-friendly error messages,
// looked up from the message catalog so they render in the language the CLI
// settled on. It returns the original error unchanged if it is not an
// *ErrorResponse or its code is unrecognized.
//
// Several arms return the server's own message verbatim rather than a local
// copy: that message names a particular thing this side does not hold (a size,
// a cap, a host, a colliding id, or what made an upload invalid), which a copy
// here would lose.
func MapError(err error) error {
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		return err
	}
	switch apiErr.Code {
	case "site_not_found":
		return errors.New(i18n.T("api.err_site_not_found"))
	case "forbidden":
		return errors.New(i18n.T("api.err_forbidden"))
	case "unauthorized":
		return UnauthorizedError()
	case "tarball_invalid":
		return errors.New(apiErr.Message)
	case "deploy_too_large", "deploy_too_many_files", "config_too_large":
		return errors.New(apiErr.Message)
	case "site_limit_reached":
		return errors.New(apiErr.Message)
	case "invalid_subdomain":
		return errors.New(i18n.T("api.err_invalid_subdomain"))
	case "subdomain_reserved":
		return errors.New(i18n.T("api.err_subdomain_reserved"))
	case "subdomain_taken":
		return errors.New(i18n.T("api.err_subdomain_taken"))
	case "domain_taken":
		// The server never says whose site holds the host, since a foreign one
		// must stay invisible, so the copy can only name the remedy for the case
		// the user can act on.
		return errors.New(i18n.T("api.err_domain_taken"))
	case "domain_not_registered":
		// The set/add gate: attaching requires a verified registration covering
		// the host.
		return errors.New(apiErr.Message)
	case "domain_in_use":
		// Unregister blocked while a site still attaches a host under the
		// registration.
		return errors.New(apiErr.Message)
	case "registration_limit":
		return errors.New(i18n.T("api.err_registration_limit"))
	case "domain_reserved":
		return errors.New(i18n.T("api.err_domain_reserved"))
	case "canonical_exists":
		return errors.New(i18n.T("api.err_canonical_exists"))
	case "canonical_via_add":
		return errors.New(i18n.T("api.err_canonical_via_add"))
	case "no_canonical":
		return errors.New(i18n.T("api.err_no_canonical"))
	case "has_redirect_domains":
		return errors.New(i18n.T("api.err_has_redirect_domains"))
	case "is_canonical":
		return errors.New(i18n.T("api.err_is_canonical"))
	case "domain_not_found":
		return errors.New(i18n.T("api.err_domain_not_found"))
	case "rate_limited":
		// The per-account recheck limiter. Automatic checks keep running
		// regardless, so this is not a failure the user must act on.
		return errors.New(i18n.T("api.err_rate_limited"))
	case "deploy_rate_limited":
		// A distinct code from rate_limited so a deploy flood never inherits the
		// recheck copy above.
		return errors.New(i18n.T("api.err_deploy_rate_limited"))
	case "register_rate_limited":
		// Distinct again, so a register flood inherits neither copy above.
		return errors.New(i18n.T("api.err_register_rate_limited"))
	case "invalid_domain":
		return errors.New(i18n.T("api.err_invalid_domain"))
	case "deploy_not_found":
		return errors.New(i18n.T("api.err_deploy_not_found"))
	case "already_live":
		return errors.New(i18n.T("api.err_already_live"))
	case "deploy_id_conflict":
		return errors.New(apiErr.Message)
	case "invalid_mode":
		return errors.New(i18n.T("api.err_invalid_mode"))
	case "invalid_credentials":
		return errors.New(i18n.T("api.err_invalid_credentials"))
	case "no_cdn_configured":
		return errors.New(i18n.T("api.err_no_cdn_configured"))
	case "no_live_deploy":
		return errors.New(i18n.T("api.err_no_live_deploy"))
	case "apex_unsupported_for_cloudflare":
		// Cloudflare for SaaS cannot route an apex content-serving domain on our
		// plan. The server's message lists each offending domain with its www
		// suggestion.
		return errors.New(apiErr.Message)
	case "requires_webaccel_mode":
		return errors.New(i18n.T("api.err_requires_webaccel_mode"))
	case "cdn_error":
		// A CLI-authored frame over the provider's own words: the frame is
		// localized, the message it carries is passed through as delivered.
		return errors.New(i18n.Tf("api.err_cdn_error", apiErr.Message))
	case "invalid_webaccel_credentials":
		// Eager validation: the token was checked at set time and Sakura rejected
		// it (401), so the copy can say exactly where to re-mint one.
		return errors.New(i18n.T("api.err_invalid_webaccel_credentials"))
	default:
		return apiErr
	}
}

// RegisterResponse is the response from POST /v1/auth/register.
type RegisterResponse struct {
	Message string `json:"message"`
}

// VerifyResponse is the response from POST /v1/auth/verify.
type VerifyResponse struct {
	APIKey string `json:"api_key"`
}

// Register calls POST /v1/auth/register.
func (c *Client) Register(email string, tosAccepted bool, secretHash string) (*RegisterResponse, error) {
	body := map[string]any{
		"email":        email,
		"tos_accepted": tosAccepted,
		"secret_hash":  secretHash,
	}

	var result RegisterResponse
	if err := c.doJSON("POST", "/v1/auth/register", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Verify calls POST /v1/auth/verify.
func (c *Client) Verify(email, code, secret string) (*VerifyResponse, error) {
	body := map[string]any{
		"email":  email,
		"code":   code,
		"secret": secret,
	}

	var result VerifyResponse
	if err := c.doJSON("POST", "/v1/auth/verify", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CreateSite calls POST /v1/pages/sites (authenticated).
func (c *Client) CreateSite(subdomain string) (*Site, error) {
	body := map[string]string{"subdomain": subdomain}
	var result Site
	if err := c.doJSON("POST", "/v1/pages/sites", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetSite calls GET /v1/pages/sites/:id (authenticated).
func (c *Client) GetSite(id string) (*Site, error) {
	var result Site
	if err := c.doJSON("GET", "/v1/pages/sites/"+id, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteSite calls DELETE /v1/pages/sites/:id (authenticated). It returns the
// 202 body so callers can read sync_attempt_id: teardown is async, and the CLI
// polls that handle until status_observed reaches "deleted".
func (c *Client) DeleteSite(id string) (*Site, error) {
	var result Site
	if err := c.doJSON("DELETE", "/v1/pages/sites/"+id, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UpdateSubdomain calls PUT /v1/pages/sites/subdomain (authenticated).
func (c *Client) UpdateSubdomain(siteID, subdomain string) (*Site, error) {
	body := map[string]string{"site_id": siteID, "subdomain": subdomain}
	var result Site
	if err := c.doJSON("PUT", "/v1/pages/sites/subdomain", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SetCanonical calls PUT /v1/pages/domains/canonical (authenticated).
// Under cdn_mode=webaccel the server auto-provisions the WebAccel resource
// for the domain (managed mode), so no per-domain CDN id is sent.
func (c *Client) SetCanonical(siteID, domain string) (*Domain, error) {
	body := map[string]any{"site_id": siteID, "domain": domain}
	var result Domain
	if err := c.doJSON("PUT", "/v1/pages/domains/canonical", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UnsetCanonical calls DELETE /v1/pages/domains/canonical (authenticated). It
// returns the queued sync_attempt_id from the 202 response.
func (c *Client) UnsetCanonical(siteID string) (*UnsetCanonicalResult, error) {
	body := map[string]string{"site_id": siteID}
	var result UnsetCanonicalResult
	if err := c.doJSON("DELETE", "/v1/pages/domains/canonical", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AddDomain calls POST /v1/pages/domains (authenticated).
// Under cdn_mode=webaccel the server auto-provisions the WebAccel resource
// for the domain (managed mode), so no per-domain CDN id is sent.
func (c *Client) AddDomain(siteID, domain, role string, redirectStatus int) (*Domain, error) {
	body := map[string]any{
		"site_id": siteID,
		"domain":  domain,
		"role":    role,
	}
	if redirectStatus > 0 {
		body["redirect_status"] = redirectStatus
	}
	var result Domain
	if err := c.doJSON("POST", "/v1/pages/domains", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RemoveDomain calls DELETE /v1/pages/domains/:domain (authenticated). It
// returns the queued sync_attempt_id and site_id to wait on.
func (c *Client) RemoveDomain(domain string) (*RemoveDomainResult, error) {
	var result RemoveDomainResult
	if err := c.doJSON("DELETE", "/v1/pages/domains/"+url.PathEscape(domain), nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RecheckDomain calls POST /v1/pages/domains/:domain/recheck (authenticated).
// It does not probe: it only opens or extends the domain's verify-priority
// (Burst) window server-side, so the next one-minute verification tick picks the
// domain up. The response has the same shape as a domain list entry, so the CLI
// can render fresh status straight after.
//
// A 404 domain_not_found covers both an unknown and a non-owned domain, so the
// :domain path param cannot leak another tenant's domain existence; 429
// rate_limited comes from the per-account recheck limiter.
func (c *Client) RecheckDomain(domain string) (*Domain, error) {
	var result Domain
	if err := c.doJSON("POST", "/v1/pages/domains/"+url.PathEscape(domain)+"/recheck", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListDomains calls GET /v1/pages/domains?site_id=:id (authenticated).
func (c *Client) ListDomains(siteID string) (*DomainList, error) {
	params := url.Values{"site_id": {siteID}}.Encode()
	var result DomainList
	if err := c.doJSON("GET", "/v1/pages/domains?"+params, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RegisterDomain calls POST /v1/pages/registrations (authenticated,
// account-scoped, no site). Idempotent: it creates or finds the account's
// registration of name and returns it with the `_kamakiri-verify` TXT to
// publish. A name already covered by a verified ancestor comes back as that
// ancestor. Named so as not to collide with the auth-side Register.
func (c *Client) RegisterDomain(name string) (*Registration, error) {
	body := map[string]string{"name": name}
	var result Registration
	if err := c.doJSON("POST", "/v1/pages/registrations", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UnregisterDomain calls DELETE /v1/pages/registrations/:name (authenticated,
// account-scoped). Synchronous and idempotent: an unknown name is a no-op
// success. Fails with `409 domain_in_use` while a host under it is attached.
func (c *Client) UnregisterDomain(name string) error {
	return c.doJSON("DELETE", "/v1/pages/registrations/"+url.PathEscape(name), nil, nil)
}

// ListRegistrations calls GET /v1/pages/registrations (authenticated,
// account-scoped, no project required). It returns every registration the
// account owns with its derived status and the account's attachments under it.
func (c *Client) ListRegistrations() (*RegistrationList, error) {
	var result RegistrationList
	if err := c.doJSON("GET", "/v1/pages/registrations", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DisableSubdomain calls POST /v1/pages/sites/subdomain/disable (authenticated).
func (c *Client) DisableSubdomain(siteID string) (*Site, error) {
	body := map[string]string{"site_id": siteID}
	var result Site
	if err := c.doJSON("POST", "/v1/pages/sites/subdomain/disable", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// EnableSubdomain calls POST /v1/pages/sites/subdomain/enable (authenticated).
func (c *Client) EnableSubdomain(siteID string) (*Site, error) {
	body := map[string]string{"site_id": siteID}
	var result Site
	if err := c.doJSON("POST", "/v1/pages/sites/subdomain/enable", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Deploy calls POST /v1/pages/deploy with a multipart form (authenticated).
func (c *Client) Deploy(config []byte, tarball io.Reader) (*DeployResult, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// The body is built in a goroutine so the tarball streams through the pipe
	// into the request instead of being buffered in memory first.
	go func() {
		defer pw.Close()

		configField, err := mw.CreateFormField("config")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := configField.Write(config); err != nil {
			pw.CloseWithError(err)
			return
		}

		tarballField, err := mw.CreateFormFile("tarball", "deploy.tar.gz")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(tarballField, tarball); err != nil {
			pw.CloseWithError(err)
			return
		}

		mw.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/pages/deploy", pr)
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf("%s: %w", i18n.T("api.err_create_request"), err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	var result DeployResult
	if err := c.doRaw(req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListDeploys calls GET /v1/pages/deploys?site_id=:id (authenticated).
func (c *Client) ListDeploys(siteID string) ([]Deploy, error) {
	params := url.Values{"site_id": {siteID}}.Encode()
	var result DeployList
	if err := c.doJSON("GET", "/v1/pages/deploys?"+params, nil, &result); err != nil {
		return nil, err
	}
	return result.Deploys, nil
}

// Rollback calls POST /v1/pages/rollback (authenticated).
func (c *Client) Rollback(siteID, deployID string) (*DeployResult, error) {
	body := map[string]string{"site_id": siteID, "deploy_id": deployID}
	var result DeployResult
	if err := c.doJSON("POST", "/v1/pages/rollback", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SetCDN calls POST /v1/pages/cdn/set for modes that need no extra params
// (cloudflare, none). For webaccel use SetCDNWebAccel.
//
// A mode flip only stops routing; it never deletes a WebAccel resource, so a
// `cdn none` from webaccel leaves the resource in the customer's Sakura account
// and surfaces it for the dedicated `cdn cleanup` command.
func (c *Client) SetCDN(siteID, mode string) (*CDNSetResponse, error) {
	body := map[string]any{"site_id": siteID, "mode": mode}
	var result CDNSetResponse
	if err := c.doJSON("POST", "/v1/pages/cdn/set", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CleanupCDN calls POST /v1/pages/cdn/cleanup (authenticated), the dedicated
// command that deletes the site's orphaned WebAccel resources. The server arms
// the cleanup marker, enqueues a reconcile that deletes each orphan, and returns
// 202 with the orphan count and the blocked subset. The wait then polls
// CDNStatus until the orphan count reaches zero, or every remaining orphan is
// blocked.
func (c *Client) CleanupCDN(siteID string) (*CDNCleanupResponse, error) {
	body := map[string]string{"site_id": siteID}
	var result CDNCleanupResponse
	if err := c.doJSON("POST", "/v1/pages/cdn/cleanup", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SetCDNWebAccel calls POST /v1/pages/cdn/set with mode=webaccel and the
// per-site WebAccel credentials. token and secret may be empty when the server
// already has credentials on file: it reuses the stored pair when both are
// omitted from the body.
func (c *Client) SetCDNWebAccel(siteID, token, secret string) (*CDNSetResponse, error) {
	body := map[string]any{
		"site_id": siteID,
		"mode":    "webaccel",
	}
	if token != "" {
		body["token"] = token
	}
	if secret != "" {
		body["secret"] = secret
	}
	var result CDNSetResponse
	if err := c.doJSON("POST", "/v1/pages/cdn/set", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RotateCDNCredentials calls POST /v1/pages/cdn/credentials (authenticated),
// rotating the stored WebAccel token and secret while the server preserves the
// domain-to-resource-id mapping. Async write-and-enqueue: it returns 202 with
// the same body shape as `set`, and the new credentials are validated lazily on
// the enqueued reconcile. A rotate changes no per-domain cdn_state, so waiting
// on that one sync attempt is the whole wait.
//
// A 409 requires_webaccel_mode means the site is not in webaccel mode or has no
// stored mapping.
func (c *Client) RotateCDNCredentials(siteID, token, secret string) (*CDNSetResponse, error) {
	body := map[string]string{"site_id": siteID, "token": token, "secret": secret}
	var result CDNSetResponse
	if err := c.doJSON("POST", "/v1/pages/cdn/credentials", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// PurgeCDN calls POST /v1/pages/cdn/purge (authenticated), a manual cache purge.
// It returns 202 with site_id and sync_attempt_id, which WaitForSync waits on.
func (c *Client) PurgeCDN(siteID string) (*CDNPurgeResponse, error) {
	body := map[string]string{"site_id": siteID}
	var result CDNPurgeResponse
	if err := c.doJSON("POST", "/v1/pages/cdn/purge", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CDNStatus calls GET /v1/pages/cdn/status (authenticated).
func (c *Client) CDNStatus(siteID string) (*CDNStatusResponse, error) {
	params := url.Values{"site_id": {siteID}}.Encode()
	var result CDNStatusResponse
	if err := c.doJSON("GET", "/v1/pages/cdn/status?"+params, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) doJSON(method, path string, body any, result any) error {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.T("api.err_marshal_request"), err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("api.err_create_request"), err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	return c.doRaw(req, result)
}

// userAgent is what a request states about this client: its version and the
// platform the binary was built for. A Client built as a bare struct literal
// carries no version and falls back to the dev token rather than stating none,
// which a server enforcing a minimum version refuses.
func (c *Client) userAgent() string {
	token := c.Version
	if token == "" {
		token = devVersion
	}
	return fmt.Sprintf("kamakiri/%s (%s/%s)", token, runtime.GOOS, runtime.GOARCH)
}

// doRaw executes req and unmarshals a success body into result. A 4xx or 5xx
// whose body does not parse as JSON degrades to a line naming the status code
// and nothing else, so not every failure from here carries a server message. It
// is the one place every request passes through, JSON calls and the deploy
// multipart alike, so the User-Agent is stamped, a version refusal is
// recognized, and the release the server advertises is picked up here rather
// than per endpoint.
func (c *Client) doRaw(req *http.Request, result any) error {
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("api.err_request_failed"), err)
	}
	defer resp.Body.Close()

	// Before the status is looked at, since a command can exit 0 having seen a
	// response that was not a success: a best-effort read whose failure changes
	// nothing (the prior-mode read before a CDN flip, the nudge inside a watch),
	// a watch poll that is retried, and a `kamakiri status --recheck` whose nudge
	// the server did not perform all do, so recording only on success would drop
	// the release such a run was told about.
	recordLatestAdvertised(resp.Header.Get(latestVersionHeader))

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("api.err_read_response"), err)
	}

	if resp.StatusCode >= 400 {
		// The refusal is recognized by status before the body is looked at, so a
		// 426 that carries no JSON still renders the upgrade copy instead of the
		// bare status-code line; the code check below catches the same refusal
		// arriving under any other error status.
		if resp.StatusCode == http.StatusUpgradeRequired {
			return ErrUpgradeRequired
		}

		var apiErr ErrorResponse
		if err := json.Unmarshal(respBody, &apiErr); err != nil {
			return errors.New(i18n.Tf("api.err_server_returned", resp.StatusCode))
		}
		if apiErr.Code == "upgrade_required" {
			return ErrUpgradeRequired
		}
		return &apiErr
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("%s: %w", i18n.T("api.err_parse_response"), err)
		}
	}
	return nil
}
