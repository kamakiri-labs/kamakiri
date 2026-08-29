// Package timeago formats an RFC3339 timestamp as a short "time since now"
// span, at either of two granularities.
//
// Coarse drops anything under a minute, for milestone copy where a
// second-by-second age reads as noise ("couldn't reach your DNS, ~4m ago").
// Precise keeps the seconds, for a live age the user watches tick ("degraded,
// 2m 30s").
//
// A span is a bare duration: the "ago" or "since" framing around it belongs to
// whichever call site prints it, which is where a language that words the two
// differently gets to say so. The unit words come from the message catalog, so
// the English examples above are one language's rendering, not the format.
package timeago

import (
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// Parse reads an RFC3339 timestamp, tolerating fractional seconds and the
// zone-less form some sources emit. An empty or unparseable input is not an
// error but a false ok, so a caller renders nothing instead of a made-up time.
func Parse(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Coarse renders the span since ts at minute resolution, and nothing at all for
// a timestamp that is empty, unparseable, in the future, or under a minute old.
// The sub-minute case is dropped so milestone copy never fabricates a "~0m ago".
func Coarse(ts string) string {
	t, ok := Parse(ts)
	if !ok {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return i18n.Tf("timeago.minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return i18n.Tf("timeago.hours", int(d.Hours()))
	default:
		return i18n.Tf("timeago.days", int(d.Hours()/24))
	}
}

// Precise renders the span since ts at second resolution, and nothing at all for
// a timestamp that is empty, unparseable, or in the future.
func Precise(ts string) string {
	t, ok := Parse(ts)
	if !ok {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return ""
	case d < time.Minute:
		return i18n.Tf("timeago.seconds", int(d.Seconds()))
	case d < time.Hour:
		return i18n.Tf("timeago.minutes_seconds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return i18n.Tf("timeago.hours", int(d.Hours()))
	default:
		return i18n.Tf("timeago.days", int(d.Hours())/24)
	}
}
