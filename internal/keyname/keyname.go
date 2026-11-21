// Package keyname holds the rule an API key's name must meet and the escaping
// every printed key name goes through. The name of a key is chosen by whoever
// minted or renamed it and comes back from the server, so the one place that
// decides what a name may hold and how it reaches a terminal is shared by every
// command that sends or prints one. The same escaping serves the other text a
// key's lines carry that the CLI did not write: the labels of the sites a
// deploy key is bound to, and a site reference the server echoes back.
package keyname

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRunes is the most code points a key's name may hold. The server applies
// the same limit, counted the same way, independently.
const MaxRunes = 64

// Valid reports whether a trimmed name is one the server accepts: 1 to 64 code
// points, none a control character (category Cc, U+0000 to U+001F and U+007F to
// U+009F). Format characters and the line and paragraph separators are not
// controls and pass, as they do on the server. A byte that is not UTF-8 counts
// as one code point here, and goes out in the request as one U+FFFD, so the two
// counts agree.
func Valid(name string) bool {
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= MaxRunes && !strings.ContainsFunc(name, Refused)
}

// Refused reports whether the name rule refuses r anywhere in a name.
func Refused(r rune) bool {
	return unicode.Is(unicode.Cc, r)
}

// Escape makes a key name safe to print on a terminal. The name comes from the
// server, and the name rule lets through characters that reorder or hide what
// follows: format characters (bidi overrides and isolates, zero-width
// characters, tag characters) and the line and paragraph separators. A site's
// label and an echoed site reference go through it too: they come from the
// server or from what the user typed, the CLI cannot vouch for what they hold,
// and they print in the same sentences as a key's name, so one escaping keeps
// every part of such a line equally safe. Every rune in the Unicode categories
// Cc, Cf, Zl and Zp, that is every control character, every format character
// and the line and paragraph separators, is shown as \u{...} with its code
// point in at least four lowercase hex digits. The rule refuses controls, but
// they are escaped anyway so the line does not depend on the server for what
// reaches the terminal (a C1 control such as U+009B, the 8-bit CSI, is one a
// terminal acts on). Every rune outside those categories, a backslash included,
// is copied as it is, so an ordinary name prints unchanged; a name that holds
// the text of an escape therefore reads the same as one holding the character,
// which is acceptable on a line meant for a person.
func Escape(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			fmt.Fprintf(&b, `\u{%04x}`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
