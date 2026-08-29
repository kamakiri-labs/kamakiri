package timeago

import (
	"os"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under: the unit words in every span come from the message
// catalog. Load rather than Setup: nothing here reports which language is in
// force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

// ago renders a timestamp d in the past, in a format the helpers parse.
func ago(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

func TestCoarse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"unparseable", "not-a-time", ""},
		{"future", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), ""},
		{"sub-minute dropped", ago(30 * time.Second), ""},
		{"minutes", ago(90 * time.Second), "1m"},
		{"hours", ago(2 * time.Hour), "2h"},
		{"days", ago(50 * time.Hour), "2d"},
		{"fractional seconds parse", time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano), "2h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Coarse(tc.in); got != tc.want {
				t.Errorf("Coarse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPrecise(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"unparseable", "not-a-time", ""},
		{"future", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), ""},
		{"seconds shown", ago(30 * time.Second), "30s"},
		{"minutes and seconds", ago(90 * time.Second), "1m 30s"},
		{"hours", ago(2 * time.Hour), "2h"},
		{"days", ago(50 * time.Hour), "2d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Precise(tc.in); got != tc.want {
				t.Errorf("Precise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	if _, ok := Parse(""); ok {
		t.Error("Parse(empty) should not be ok")
	}
	if _, ok := Parse("garbage"); ok {
		t.Error("Parse(garbage) should not be ok")
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05"} {
		ts := time.Now().UTC().Format(layout)
		if _, ok := Parse(ts); !ok {
			t.Errorf("Parse(%q) via layout %q should be ok", ts, layout)
		}
	}
}
