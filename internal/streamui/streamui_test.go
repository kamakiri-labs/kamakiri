package streamui

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: the humanized durations render from the
// message catalog. Load rather than Setup: nothing here reports which
// language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

// The rewind escape a Renderer emits to erase N transient lines on a TTY.
func rewind(n int) string {
	return "\033[" + itoa(n) + "A\033[J"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// The rewind escape must appear exactly where a TTY would rewind, and nowhere
// at all off a TTY.
func TestRenderer(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  bool
		want string
	}{
		{
			name: "tty rewinds the transient block",
			tty:  true,
			want: "  a\n  b\n" + rewind(2) + "  c\n" + rewind(1) + "done\n" + "note: 1\n",
		},
		{
			name: "non-tty keeps every line, no rewind",
			tty:  false,
			want: "  a\n  b\n  c\ndone\nnote: 1\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := New(&buf, tc.tty)
			r.Transient("  a", "  b")
			r.Transient("  c")
			r.Commit("done")
			r.Commit("note: 1")
			if got := buf.String(); got != tc.want {
				t.Fatalf("output mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// A first Transient must not rewind into whatever preceded it: Clear is a no-op
// at zero transient lines.
func TestRendererFirstTransientDoesNotRewind(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, true)
	r.Transient("  first")
	if got := buf.String(); got != "  first\n" {
		t.Fatalf("first transient should not rewind: %q", got)
	}
}

func TestHumanizeElapsed(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "under a minute"},
		{59 * time.Second, "under a minute"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h 00m"},
		{90 * time.Minute, "1h 30m"},
		{2*time.Hour + 5*time.Minute, "2h 05m"},
	} {
		if got := HumanizeElapsed(tc.d); got != tc.want {
			t.Errorf("HumanizeElapsed(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestHumanizeSpan(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		// Sub-minute spans keep their real seconds.
		{5 * time.Second, "5s"},
		{29 * time.Second, "29s"},
		{42 * time.Second, "42s"},
		{time.Minute, "1m"},
		{78 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h0m"},
		{90 * time.Minute, "1h30m"},
	} {
		if got := HumanizeSpan(tc.d); got != tc.want {
			t.Errorf("HumanizeSpan(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
