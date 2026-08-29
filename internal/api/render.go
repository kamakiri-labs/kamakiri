package api

import (
	"fmt"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// PagesDomain is the public hostname suffix for managed sites. It is a constant
// because the server's equivalent is a config knob with the same default;
// promote it to runtime config when the CLI talks to more than one environment.
const PagesDomain = "kamakiri-pages.jp"

// SubdomainLine renders the desired/observed/sync triple for the subdomain row.
// It is shared so `kamakiri subdomain get` and `kamakiri status` agree on every
// state. Below, <host> is `<desired>.kamakiri-pages.jp`, <observed-host> is the
// same with <observed> in place of <desired>, and <suffix> is the disabled
// marker when `subdomain_enabled = false` and empty otherwise. The display is
// diff-only, and each annotation's wording comes from the message catalog, so
// the shapes below are the English rendering of it:
//
//   - desired == observed, with a sync row whose outcome is ok →
//     `<host><suffix>  ✓`.
//   - observed unset, no sync row → `<host><suffix>` (a first-ever reconcile
//     pending, or a site predating the observed columns).
//   - observed unset, sync row queued or running →
//     `<host><suffix>  ⧗ never reconciled`.
//   - desired != observed →
//     `<desired> → <observed-host><suffix>` and the sync's verb.
//   - latest sync error →
//     `<desired> → <observed-host><suffix> ✗ failed (<reason>)`.
func SubdomainLine(site *Site) string {
	desired := site.Subdomain
	observed := ""
	if site.SubdomainObserved != nil {
		observed = *site.SubdomainObserved
	}

	suffix := ""
	if !site.SubdomainEnabled {
		suffix = i18n.T("api.subdomain_suffix_disabled")
	}

	host := desired + "." + PagesDomain
	syncOutcome := ""
	if site.Sync != nil {
		syncOutcome = site.Sync.Outcome
	}

	switch {
	case observed != "" && observed == desired && syncOutcome == "ok":
		return fmt.Sprintf("%s%s  ✓", host, suffix)

	case observed == "" && site.Sync == nil:
		return fmt.Sprintf("%s%s", host, suffix)

	case observed == "" && (syncOutcome == "queued" || syncOutcome == "running"):
		// Frame a first-ever reconcile as "never reconciled" rather than
		// "syncing": syncing implies a previously synced value is being updated,
		// which is a different situation for the reader.
		return fmt.Sprintf("%s%s  %s", host, suffix, i18n.T("api.subdomain_never_reconciled"))

	case observed != "" && observed != desired && syncOutcome == "error":
		// The parenthetical holds either the reconciler's own reason, which the
		// server delivers in English whoever asks, or the CLI's own localized
		// pointer at `kamakiri status`. It can therefore come out in Japanese,
		// which is why the Japanese value brackets it full-width.
		return fmt.Sprintf("%s → %s.%s%s %s",
			desired, observed, PagesDomain, suffix,
			i18n.Tf("api.subdomain_failed", errorMessage(site)))

	case observed != "" && observed != desired:
		// queued renders as ⧗ pending and running as ⧗ syncing, falling back to
		// ⧗ syncing on a nil or unknown outcome (a fresh run that has not
		// transitioned the row yet).
		return fmt.Sprintf("%s → %s.%s%s %s", desired, observed, PagesDomain, suffix,
			syncVerb(syncOutcome))

	default:
		// The catch-all: observed agrees with desired but the sync row is nil or
		// not ok, and also an unset observed whose sync row has left
		// queued/running. Show the value plain: a sync-row problem surfaces on
		// `kamakiri status`'s own sync line.
		return fmt.Sprintf("%s%s", host, suffix)
	}
}

func syncVerb(outcome string) string {
	switch outcome {
	case "queued":
		return i18n.T("api.sync_verb_pending")
	default:
		return i18n.T("api.sync_verb_syncing")
	}
}

func errorMessage(site *Site) string {
	if site.Sync == nil || site.Sync.Error == "" {
		return i18n.T("api.see_status")
	}
	return site.Sync.Error
}
