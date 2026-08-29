package cdn

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/timeago"
)

// Per-domain CDN lifecycle positions, as the server reports them.
const (
	CdnStateOff                  = "off"
	CdnStateAwaitingProvisioning = "awaiting_provisioning"
	CdnStateAwaitingVerification = "awaiting_verification"
	CdnStateAwaitingCFValidation = "awaiting_cf_validation"
	CdnStateReadyToFlip          = "ready_to_flip"
	CdnStateActive               = "active"
)

// Per-domain CDN health verdicts. Rendering dispatches on the verdict first and
// falls back to the lifecycle position; the five problem verdicts below claim a
// row outright.
const (
	CdnVerdictOff           = "off"
	CdnVerdictHealthy       = "healthy"
	CdnVerdictPending       = "pending"
	CdnVerdictDegraded      = "degraded"
	CdnVerdictBroken        = "broken"
	CdnVerdictTimedOut      = "timed_out"
	CdnVerdictMisconfigured = "misconfigured"
	CdnVerdictUnverified    = "unverified"
)

// Reason tags carried by the degraded verdict when a live site's delivery
// record stops pointing at the assigned target. They let the row render the
// actionable re-point line instead of the generic unhealthy one.
const (
	CdnReasonWebaccelDeliveryLost   = "webaccel_delivery_lost"
	CdnReasonCloudflareDeliveryLost = "cloudflare_delivery_lost"
)

// CdnLiveStates is the set of positions where the CDN is fronting traffic. A
// live row reporting unhealthy is still live: it stays here and carries a
// degraded verdict instead.
var CdnLiveStates = map[string]struct{}{
	CdnStateActive: {},
}

// CdnKnownStates is the set of positions this CLI understands. A state outside
// it comes from a newer server: renderers warn once and pass it through
// verbatim rather than treating it as an error.
var CdnKnownStates = map[string]struct{}{
	CdnStateOff:                  {},
	CdnStateAwaitingProvisioning: {},
	CdnStateAwaitingVerification: {},
	CdnStateAwaitingCFValidation: {},
	CdnStateReadyToFlip:          {},
	CdnStateActive:               {},
}

// CdnIsLive reports whether a domain in the given state is being fronted.
func CdnIsLive(state string) bool {
	_, ok := CdnLiveStates[state]
	return ok
}

// statusSource is the subset of fields formatCdnStatus needs, so that one
// renderer serves both of the shapes the server returns.
type statusSource struct {
	cdnState string
	health   api.CdnHealth
	cdnMode  string

	// The WebAccel ownership TXT, surfaced only on the unverified verdict so a
	// typo'd record can be compared against the expected value.
	ownershipName  string
	ownershipValue string

	// The customer's delivery record and the target it should point at, surfaced
	// on the delivery-lost verdict. The target is provider-specific; the copy
	// built from it is not.
	deliveryName   string
	deliveryTarget string

	// webaccelEnabledObservedAt latches WebAccel reaching enabled. The
	// awaiting_verification position covers two different customer-DNS waits,
	// the ownership TXT and then the delivery CNAME, and this latch is the only
	// fact that tells them apart: unset means still on the TXT, set means
	// ownership is proved and the delivery record is what is missing.
	//
	// webaccelLatchesAvailable separates a genuinely unset latch from a source
	// that cannot see the latch at all. Without it the renderer would read a
	// missing field as "TXT not published yet" and tell a user to publish a
	// record they already published.
	webaccelEnabledObservedAt string
	webaccelLatchesAvailable  bool
}

func ownershipRecord(expected []api.DNSRecord) (string, string) {
	for _, r := range expected {
		if r.Purpose == "ownership" {
			return r.Name, r.Value
		}
	}
	return "", ""
}

// primaryRecord returns the customer's delivery record. The first primary entry
// is that record, whether a standalone CNAME or an apex group's ALIAS member,
// and its trailing dot is trimmed for inline display.
func primaryRecord(expected []api.DNSRecord) (string, string) {
	for _, r := range expected {
		if r.Purpose == "primary" {
			return r.Name, strings.TrimSuffix(r.Value, ".")
		}
	}
	return "", ""
}

// FormatCdnStatusFromDomain renders the one-line CDN status for a domain as it
// appears in a domain listing. cdnMode is the site's desired CDN mode, which
// picks the provider named in the rendered line. This shape carries no WebAccel
// latches, so the line it produces cannot name a specific verification phase.
func FormatCdnStatusFromDomain(d api.Domain, cdnMode string) string {
	ownName, ownValue := ownershipRecord(d.DNSRecordsExpected)
	delName, delTarget := primaryRecord(d.DNSRecordsExpected)
	return formatCdnStatus(statusSource{
		cdnState:       d.CdnState,
		health:         d.CdnHealth,
		cdnMode:        cdnMode,
		ownershipName:  ownName,
		ownershipValue: ownValue,
		deliveryName:   delName,
		deliveryTarget: delTarget,
	})
}

// FormatCdnStatusFromCDN renders the one-line CDN status for a domain as it
// appears in a CDN status response, through the same renderer
// FormatCdnStatusFromDomain uses. This shape carries the WebAccel latches, so
// its lines can name the exact verification phase.
func FormatCdnStatusFromCDN(d api.CDNDomainStatus, cdnMode string) string {
	ownName, ownValue := ownershipRecord(d.DNSRecordsExpected)
	delName, delTarget := primaryRecord(d.DNSRecordsExpected)
	return formatCdnStatus(statusSource{
		cdnState:                  d.CdnState,
		health:                    d.CdnHealth,
		cdnMode:                   cdnMode,
		ownershipName:             ownName,
		ownershipValue:            ownValue,
		deliveryName:              delName,
		deliveryTarget:            delTarget,
		webaccelEnabledObservedAt: d.WebaccelEnabledObservedAt,
		webaccelLatchesAvailable:  true,
	})
}

// formatCdnStatus renders one domain's CDN status line.
//
// It dispatches on the health verdict first, because a problem verdict is
// position-independent: a broken hostname is broken whether the row is
// validating or live. Only when the verdict claims nothing does the lifecycle
// position decide the line:
//
//	off                                 -> ""
//	pending @ awaiting_provisioning     -> "⧗ provisioning <provider>"
//	pending @ awaiting_verification     -> the TXT, delivery-CNAME or neutral wait
//	pending @ awaiting_cf_validation    -> "⧗ awaiting CF validation (<age>)"
//	pending @ ready_to_flip             -> "⧗ DNS flip pending"
//	pending @ ready_to_flip (webaccel)  -> "⧗ provisioning WebAccel"
//	healthy @ active                    -> "✓ via Cloudflare" or "✓ via WebAccel"
//	degraded (delivery_lost)            -> the actionable re-point line
//	degraded (panel unhealthy)          -> "⚠ <provider> reports unhealthy since <age>"
//	broken / timed_out                  -> "✗ <reason>"
//	misconfigured / unverified          -> the actionable WebAccel line
func formatCdnStatus(src statusSource) string {
	switch src.health.Verdict {
	case CdnVerdictDegraded:
		return degradedLine(src)
	case CdnVerdictBroken, CdnVerdictTimedOut:
		return brokenLine(src.health.Reason)
	case CdnVerdictMisconfigured:
		// Only the user can clear this one, in their own WebAccel panel, so the
		// line names the fix rather than showing a spinner that will never end.
		return i18n.T("cdn.status_misconfigured")
	case CdnVerdictUnverified:
		return txtMismatchLine(src)
	}

	if _, known := CdnKnownStates[src.cdnState]; !known && src.cdnState != "" {
		warnUnknownStateOnce(src.cdnState)
		return src.cdnState
	}

	switch src.cdnState {
	case "", CdnStateOff:
		return ""

	case CdnStateAwaitingProvisioning:
		return i18n.Tf("cdn.status_provisioning", ProviderLabel(src.cdnMode))

	case CdnStateAwaitingVerification:
		// Under BYO WebAccel this one position covers two different customer-DNS
		// waits, phase 1's ownership TXT and phase 2's delivery CNAME, and the
		// enabled latch is the only fact that tells them apart. A source without
		// the latch gets the neutral line: naming the wrong phase would send the
		// user to republish a TXT they already published.
		if src.cdnMode == "webaccel" {
			if !src.webaccelLatchesAvailable {
				return i18n.T("cdn.status_awaiting_dns_generic")
			}
			if src.webaccelEnabledObservedAt != "" {
				return i18n.T("cdn.status_awaiting_delivery_cname")
			}
		}
		return i18n.T("cdn.status_awaiting_verification")

	case CdnStateAwaitingCFValidation:
		// The age anchors on when the row entered this position, not on the last
		// probe: a probe time resets on every check, so the rendered age would
		// ratchet down each tick instead of growing.
		//
		// Each of the three shapes is a whole message: where an age and a
		// still-running note attach to the line is a matter of grammar, not of
		// appending clauses in the order English happens to want them. The
		// still-validating note is informational rather than an error, since the
		// checks keep running on their own.
		//
		// A still-validating line with no age is not one of the shapes: the note
		// only appears for a timestamp old enough to be past the threshold, and
		// such a timestamp always renders an age too.
		age := timeago.Precise(src.health.Since)
		switch {
		case age != "" && isOlderThan(src.health.Since, 2*time.Hour):
			return i18n.Tf("cdn.status_cf_validation_aged_still", age)
		case age != "":
			return i18n.Tf("cdn.status_cf_validation_aged", age)
		default:
			return i18n.T("cdn.status_cf_validation")
		}

	case CdnStateReadyToFlip:
		// This position is Cloudflare-only; a WebAccel row reaching it is a stale
		// reading. Under BYO WebAccel the customer owns the delivery record, so
		// the neutral spinner is the honest line: a pending-flip line would claim
		// we are about to change DNS we do not control.
		if src.cdnMode == "webaccel" {
			return i18n.Tf("cdn.status_provisioning", ProviderLabel(src.cdnMode))
		}
		return i18n.T("cdn.status_flip_pending")

	case CdnStateActive:
		// A real live tick, not an optimistic one: under BYO WebAccel the server
		// only reaches this position once it has seen the certificate issued, so
		// the row is cert-confirmed live. A live-but-unhealthy row carries the
		// degraded verdict and never reaches here.
		return i18n.Tf("cdn.status_live_via", ProviderLabel(src.cdnMode))
	}

	return ""
}

// brokenLine renders a broken or timed-out verdict from its reason tag. The
// fallback covers a verdict that arrives without one, so the user still gets a
// failure line rather than a bare mark.
func brokenLine(reason string) string {
	if reason != "" {
		// The reason is the server's own tag, passed through as delivered; only
		// the mark around it belongs to the CLI.
		return i18n.Tf("cdn.status_broken_reason", reason)
	}
	return i18n.T("cdn.status_broken")
}

// degradedLine renders the degraded verdict: the actionable re-point line when
// the reason is a lost delivery record, the generic provider-unhealthy line
// otherwise.
func degradedLine(src statusSource) string {
	if src.health.Reason == CdnReasonWebaccelDeliveryLost ||
		src.health.Reason == CdnReasonCloudflareDeliveryLost {
		return deliveryLostLine(src)
	}

	// The age is when unhealthy was last confirmed, so the line reads "since we
	// last saw this", which is all we can honestly claim. Aged or not, each is a
	// whole message: an age is not a clause every language appends at the end.
	if age := timeago.Precise(src.health.Since); age != "" {
		return i18n.Tf("cdn.status_unhealthy_since", ProviderLabel(src.cdnMode), age)
	}
	return i18n.Tf("cdn.status_unhealthy", ProviderLabel(src.cdnMode))
}

// deliveryLostLine renders the delivery-lost demotion: traffic is no longer
// reaching the CDN. Naming the record and its target keeps the line
// provider-agnostic; the fallbacks cover a payload that omits the record.
func deliveryLostLine(src statusSource) string {
	if src.deliveryName != "" && src.deliveryTarget != "" {
		return i18n.Tf("cdn.delivery_lost_named", src.deliveryName, src.deliveryTarget)
	}

	switch src.cdnMode {
	case "cloudflare":
		return i18n.T("cdn.delivery_lost_cloudflare")
	case "webaccel":
		return i18n.T("cdn.delivery_lost_webaccel")
	default:
		return i18n.T("cdn.delivery_lost_generic")
	}
}

// txtMismatchLine renders the unverified verdict. It surfaces the expected
// value when the payload has it, because nothing times this gate out and a
// neutral waiting line would leave a typo'd record stuck with no signal.
func txtMismatchLine(src statusSource) string {
	if src.ownershipValue != "" {
		return i18n.Tf("cdn.txt_mismatch_named", src.ownershipName, src.ownershipValue)
	}
	return i18n.T("cdn.txt_mismatch_generic")
}

func isOlderThan(ts string, dur time.Duration) bool {
	if ts == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(t) > dur
}

// Warn-once registry for unknown states, guarded so a renderer may be called
// from any goroutine. One warning per process, not one per rendered row: a
// listing of many domains would otherwise bury its own output.
var (
	warnedStatesMu         sync.Mutex
	warnedStates                     = map[string]struct{}{}
	unknownStateWarnWriter io.Writer = os.Stderr
)

func warnUnknownStateOnce(state string) {
	warnedStatesMu.Lock()
	defer warnedStatesMu.Unlock()
	if _, ok := warnedStates[state]; ok {
		return
	}
	warnedStates[state] = struct{}{}
	fmt.Fprintln(unknownStateWarnWriter, i18n.Tf("cdn.warn_unknown_state", state))
}

// WarnedStates returns a snapshot of the states already warned about. It is
// exported for the package's own tests and is not part of the CLI surface.
func WarnedStates() map[string]struct{} {
	warnedStatesMu.Lock()
	defer warnedStatesMu.Unlock()
	out := make(map[string]struct{}, len(warnedStates))
	for k := range warnedStates {
		out[k] = struct{}{}
	}
	return out
}

func resetWarnedStatesForTest() {
	warnedStatesMu.Lock()
	defer warnedStatesMu.Unlock()
	warnedStates = map[string]struct{}{}
}
