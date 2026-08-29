package i18n

import "testing"

func TestWidth(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "Version:", 8},
		{"ascii with spaces", "no site linked", 14},
		{"japanese", "日本語", 6},
		{"mixed", "CDN: 有効", 9},
		{"fullwidth latin", "ＡＢ", 4},
		{"halfwidth katakana", "ｱｲｳ", 3},
		{"combining acute adds nothing", "é", 1},
		{"combining marks on a base of two", "が́", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Width(tt.s); got != tt.want {
				t.Errorf("Width(%q) = %d, want %d", tt.s, got, tt.want)
			}
		})
	}
}

// The glyphs the CLI already prints must each stay one column: the check marks
// are Neutral, and the arrow and ellipsis are East Asian Ambiguous, which this
// package deliberately counts as one.
func TestWidthOfTheGlyphsInUse(t *testing.T) {
	for _, glyph := range []string{"✓", "✗", "⚠", "⧗", "→", "…"} {
		if got := Width(glyph); got != 1 {
			t.Errorf("Width(%q) = %d, want 1", glyph, got)
		}
	}
}

func TestPadReachesTheRequestedColumns(t *testing.T) {
	tests := []struct {
		name string
		s    string
		w    int
		want string
	}{
		{"ascii", "Version:", 14, "Version:      "},
		{"japanese", "言語:", 14, "言語:         "},
		{"already exact", "Credentials:", 12, "Credentials:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Pad(tt.s, tt.w)
			if got != tt.want {
				t.Errorf("Pad(%q, %d) = %q, want %q", tt.s, tt.w, got, tt.want)
			}
			if w := Width(got); w != tt.w {
				t.Errorf("Width(Pad(%q, %d)) = %d, want %d", tt.s, tt.w, w, tt.w)
			}
		})
	}
}

// The point of the helper: an English label and its Japanese twin land in the
// same column, which byte padding cannot do for a multibyte string.
func TestPadAlignsAcrossLanguages(t *testing.T) {
	en := Pad("Language:", 14)
	ja := Pad("言語:", 14)

	if Width(en) != Width(ja) {
		t.Errorf("Width(%q) = %d, Width(%q) = %d, want equal", en, Width(en), ja, Width(ja))
	}
}

// Pad never truncates, matching the %-14s it replaces: an over-wide cell pushes
// its row out rather than losing characters.
func TestPadLeavesAnOverWideStringAlone(t *testing.T) {
	if got := Pad("a much longer label", 8); got != "a much longer label" {
		t.Errorf("Pad() = %q, want the string unchanged", got)
	}
	if got := Pad("日本語日本語", 8); got != "日本語日本語" {
		t.Errorf("Pad() = %q, want the string unchanged", got)
	}
}
