package cdn

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/timeago"
)

// Purge health is its own axis, separate from the DNS and CDN lifecycles: it
// tracks whether purges against the provider are landing. A healthy row prints
// no line at all.
//
// The server reports only a short tag for the failure and the CLI owns the
// wording, so a copy fix ships with a CLI release instead of a server deploy.
const (
	purgeHealthOk      = "ok"
	purgeHealthFailing = "failing"
	purgeHealthBroken  = "broken"
)

// purgeHint is the copy one failure tag renders as: label is the short
// descriptor shown in parentheses, hint the message while purges are merely
// failing, and suffix the remediation that replaces hint once they are broken.
// An empty suffix means there is nothing the user can do but wait.
type purgeHint struct {
	label  string
	hint   string
	suffix string
}

// purgeHints maps a failure tag to the copy that renders it. Each entry is a
// function rather than a resolved struct because the catalog is read when a line
// is printed: package initialization runs before the language is resolved, so a
// table of strings would freeze in whatever the environment suggested.
//
// Holding key names in the table and resolving them in one shared place is not
// the simplification it looks like: the catalog checks require a literal key at
// every lookup, so keys arriving there as variables fail that check and leave
// their catalog entries looking like orphans with no call site at all. The
// fields do not all hold the same kind of thing either, since label is a literal
// for the HTTP tags and a key for the rest.
//
// The HTTP labels stay literal. They are a protocol status rather than prose, so
// they read the same in every language.
var purgeHints = map[string]func() purgeHint{
	"http_401": func() purgeHint {
		return purgeHint{
			label:  "HTTP 401",
			hint:   i18n.T("cdn.purge_hint_key_invalid"),
			suffix: i18n.T("cdn.purge_fix_rotate_key"),
		}
	},
	"http_403": func() purgeHint {
		return purgeHint{
			label:  "HTTP 403",
			hint:   i18n.T("cdn.purge_hint_key_forbidden"),
			suffix: i18n.T("cdn.purge_fix_rotate_key"),
		}
	},
	"http_404": func() purgeHint {
		return purgeHint{
			label: "HTTP 404",
			hint:  i18n.T("cdn.purge_hint_not_found"),
			// The tag carries no provider, so the remediation names both commands
			// rather than guessing which one applies.
			suffix: i18n.T("cdn.purge_fix_reprovision"),
		}
	},
	"http_408": func() purgeHint {
		return purgeHint{
			label: "HTTP 408",
			hint:  i18n.T("cdn.purge_hint_timed_out"),
			// No suffix: a request timeout is transient and clears on retry.
		}
	},
	"http_429": func() purgeHint {
		return purgeHint{
			label: "HTTP 429",
			hint:  i18n.T("cdn.purge_hint_rate_limited"),
			// No suffix: rate limiting is transient and clears on its own.
		}
	},
	"http_5xx_persistent": func() purgeHint {
		return purgeHint{
			label: "HTTP 5xx",
			hint:  i18n.T("cdn.purge_hint_server_error"),
			// No suffix: a provider outage has no operator remediation.
		}
	},
	"network_timeout": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_network_timeout"),
			hint:   i18n.T("cdn.purge_hint_unreachable"),
			suffix: i18n.T("cdn.purge_fix_status_page"),
		}
	},
	"network_error": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_network_error"),
			hint:   i18n.T("cdn.purge_hint_unreachable"),
			suffix: i18n.T("cdn.purge_fix_status_page"),
		}
	},
	"connection_refused": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_connection_refused"),
			hint:   i18n.T("cdn.purge_hint_refused"),
			suffix: i18n.T("cdn.purge_fix_status_page"),
		}
	},
	"lock_timeout": func() purgeHint {
		return purgeHint{
			label: i18n.T("cdn.purge_label_lock_timeout"),
			hint:  i18n.T("cdn.purge_hint_contended"),
			// No suffix: the contention is ours, not theirs, and clears on its own.
		}
	},
	"missing_credentials": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_missing_credentials"),
			hint:   i18n.T("cdn.purge_hint_no_credentials"),
			suffix: i18n.T("cdn.purge_fix_credentials"),
		}
	},
	"provider_rejected": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_provider_rejected"),
			hint:   i18n.T("cdn.purge_hint_rejected"),
			suffix: i18n.T("cdn.purge_fix_check_status"),
		}
	},
	"invalid_cdn_mode": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_invalid_mode"),
			hint:   i18n.T("cdn.purge_hint_mode_unsupported"),
			suffix: i18n.T("cdn.purge_fix_cdn_status"),
		}
	},
	"unknown": func() purgeHint {
		return purgeHint{
			label:  i18n.T("cdn.purge_label_unknown"),
			hint:   i18n.T("cdn.purge_hint_unknown"),
			suffix: i18n.T("cdn.purge_fix_support"),
		}
	},
}

// FormatPurgeStateLine renders the purge line that sits under a domain's row,
// indented to match it, or "" when the row is healthy and the caller should
// print nothing:
//
//	failing → "  Purges: ⚠ failing (N failures, last error: <label> (<hint>))"
//	broken  → "  Purges: ✗ broken since <age> ago (<label>: <suffix-or-hint>)"
//
// An unrecognised failure tag renders as the unknown mapping rather than
// leaking the raw tag, which would be a debug string in the user's face; an
// http_<status> is the exception and renders as that status. An unrecognised
// health value warns once and then passes through verbatim, the same
// forward-compatibility policy the lifecycle positions follow.
func FormatPurgeStateLine(d *api.Domain) string {
	if d == nil {
		return ""
	}
	switch d.CdnPurgeHealth {
	case "", purgeHealthOk:
		return ""
	case purgeHealthFailing:
		return formatPurgeFailing(d)
	case purgeHealthBroken:
		return formatPurgeBroken(d)
	default:
		warnUnknownPurgeHealthOnce(d.CdnPurgeHealth)
		return i18n.Tf("cdn.purge_state_raw", d.CdnPurgeHealth)
	}
}

func formatPurgeFailing(d *api.Domain) string {
	hint := purgeHintFor(d.CdnPurgeErrorReason)
	// The failure count is a whole message per branch rather than a noun chosen
	// by number and dropped into a frame: Japanese has no plural to pick, so a
	// count and a noun the sentence does not need are not the same thing.
	if n := d.CdnPurgeConsecutiveFailures; n > 0 {
		failures := i18n.Tf("cdn.purge_failures_many", n)
		if n == 1 {
			failures = i18n.Tf("cdn.purge_failures_one", n)
		}
		return i18n.Tf("cdn.purge_failing_counted", failures, hint.label, hint.hint)
	}
	return i18n.Tf("cdn.purge_failing", hint.label, hint.hint)
}

func formatPurgeBroken(d *api.Domain) string {
	hint := purgeHintFor(d.CdnPurgeErrorReason)
	msg := hint.suffix
	if msg == "" {
		msg = hint.hint
	}

	// The age anchors on the last success, not the last failure. Retries keep
	// running while a row is broken, so a last-failure anchor would reset on
	// every attempt and the age would never grow. Aged or not, each shape is a
	// whole message: an age is not a clause every language inserts mid-sentence.
	if age := timeago.Precise(d.CdnPurgeLastOkAt); age != "" {
		return i18n.Tf("cdn.purge_broken_since", age, hint.label, msg)
	}
	return i18n.Tf("cdn.purge_broken", hint.label, msg)
}

// purgeHintFor maps a failure tag to the copy that renders it; an unrecognised
// or empty tag lands on the unknown mapping, unless it is an http_<status>,
// which renders as that status. The result always carries a label and a hint,
// so the formatters need no emptiness guards.
func purgeHintFor(tag string) purgeHint {
	if h, ok := purgeHints[tag]; ok {
		return h()
	}
	// An HTTP status with no entry of its own still renders as that status
	// rather than as "unknown error": the number is information the user can act
	// on, and the provider's own words arrive separately.
	if strings.HasPrefix(tag, "http_") {
		if status := strings.TrimPrefix(tag, "http_"); isAllDigits(status) {
			return purgeHint{
				label:  "HTTP " + status,
				hint:   i18n.T("cdn.purge_hint_rejected"),
				suffix: i18n.T("cdn.purge_fix_check_status"),
			}
		}
	}
	return purgeHints["unknown"]()
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Warn-once registry for unknown purge-health values, guarded so a renderer may
// be called from any goroutine. It is deliberately separate from the
// lifecycle-position registry: sharing one would let a new value on either axis
// silence the warning on the other.
var (
	warnedHealthsMu         sync.Mutex
	warnedHealths                     = map[string]struct{}{}
	unknownHealthWarnWriter io.Writer = os.Stderr
)

func warnUnknownPurgeHealthOnce(health string) {
	warnedHealthsMu.Lock()
	defer warnedHealthsMu.Unlock()
	if _, ok := warnedHealths[health]; ok {
		return
	}
	warnedHealths[health] = struct{}{}
	fmt.Fprintln(unknownHealthWarnWriter, i18n.Tf("cdn.warn_unknown_purge_health", health))
}

// WarnedHealths returns a snapshot of the health values already warned about.
// It is exported for the package's own tests and is not part of the CLI surface.
func WarnedHealths() map[string]struct{} {
	warnedHealthsMu.Lock()
	defer warnedHealthsMu.Unlock()
	out := make(map[string]struct{}, len(warnedHealths))
	for k := range warnedHealths {
		out[k] = struct{}{}
	}
	return out
}

func resetWarnedHealthsForTest() {
	warnedHealthsMu.Lock()
	defer warnedHealthsMu.Unlock()
	warnedHealths = map[string]struct{}{}
}
