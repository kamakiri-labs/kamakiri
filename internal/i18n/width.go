package i18n

import (
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// Width returns the number of terminal columns s occupies. Japanese needs it:
// counting bytes or runes both misjudge a wide glyph, so a column laid out with
// either drifts as soon as the label is not ASCII.
func Width(s string) int {
	columns := 0
	for _, r := range s {
		columns += runeWidth(r)
	}
	return columns
}

// Pad appends spaces to s until it occupies at least w columns, and returns s
// untouched when it is already wider. Never truncating matches the byte padding
// it replaces: an over-wide cell pushes its row out rather than losing text.
func Pad(s string, w int) string {
	if missing := w - Width(s); missing > 0 {
		return s + strings.Repeat(" ", missing)
	}
	return s
}

func runeWidth(r rune) int {
	// A combining mark renders on top of the character before it and takes no
	// column of its own. It needs its own check because the width tables have
	// no kind for it.
	if unicode.Is(unicode.Mn, r) {
		return 0
	}

	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	default:
		// East Asian Ambiguous lands here deliberately, not by oversight: the
		// arrow and the ellipsis the CLI prints are Ambiguous, and one column
		// is what the terminals it targets give them. Neutral, Narrow and
		// Halfwidth are one column by definition, and the check and warning
		// marks are Neutral.
		return 1
	}
}
