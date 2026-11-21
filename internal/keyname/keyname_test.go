package keyname

import (
	"strings"
	"testing"
)

// What could move the cursor, end the line, or reorder what follows on a
// terminal is shown as the escape of its code point; everything else, a
// backslash included, is printed as it is.
func TestEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ASCII", "ci-box", "ci-box"},
		{"non-ASCII, emoji and a combining mark", "ビルド機 🚀 e\u0301", "ビルド機 🚀 e\u0301"},
		{"backslash", `C:\box`, `C:\box`},
		{"ESC", "a\x1bb", `a\u{001b}b`},
		{"DEL", "a\x7fb", `a\u{007f}b`},
		{"NEL", "a\u0085b", `a\u{0085}b`},
		{"8-bit CSI", "a\u009bb", `a\u{009b}b`},
		{"last C1 control", "a\u009fb", `a\u{009f}b`},
		{"no-break space", "a\u00a0b", "a\u00a0b"},
		{"zero-width space", "a\u200bb", `a\u{200b}b`},
		{"right-to-left override", "a\u202eb", `a\u{202e}b`},
		{"left-to-right isolate", "a\u2066b", `a\u{2066}b`},
		{"byte order mark", "a\ufeffb", `a\u{feff}b`},
		{"line separator", "a\u2028b", `a\u{2028}b`},
		{"paragraph separator", "a\u2029b", `a\u{2029}b`},
		{"language tag", "a\U000e0001b", `a\u{e0001}b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Escape(tc.in); got != tc.want {
				t.Errorf("Escape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The rule refuses control characters only, and counts code points, so the
// characters on either side of the C0 and C1 ranges pass and 64 wide characters
// fit where 65 do not.
func TestValid(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain", "ci-box", true},
		{"an inner space", "ci box", true},
		{"64 non-ASCII code points", strings.Repeat("名", 64), true},
		{"a tilde", "ci~box", true},
		{"a no-break space", "ci\u00a0box", true},
		{"a zero-width joiner", "ci\u200dbox", true},
		{"a bidi override", "ci\u202ebox", true},
		{"a line separator", "ci\u2028box", true},
		{"64 invalid UTF-8 bytes", strings.Repeat("\xff", 64), true},
		{"empty", "", false},
		{"65 code points", strings.Repeat("名", 65), false},
		{"65 invalid UTF-8 bytes", strings.Repeat("\xff", 65), false},
		{"an escape character", "ci\x1bbox", false},
		{"DEL", "ci\x7fbox", false},
		{"a newline inside", "ci\nbox", false},
		{"the first C1 control", "ci\u0080box", false},
		{"NEL inside", "ci\u0085box", false},
		{"the last C1 control", "ci\u009fbox", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.in); got != tc.want {
				t.Errorf("Valid(%q) = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}
