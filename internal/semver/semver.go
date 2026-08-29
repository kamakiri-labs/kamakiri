// Package semver parses and orders version strings under semver 2.0.0.
//
// The order is the specification's own, section 11: the numeric triple first,
// then a prerelease tail ordering below the plain triple it qualifies, then
// identifier by identifier, numerically for the all-digit ones and in ASCII
// order for the rest.
//
// The grammar accepted is not the specification's. The package is a gate as
// much as a comparison: what it accepts can end up in a download URL, and what
// it refuses is a string a caller then knows better than to use or to print.
//
// In one place it is more tolerant. One lowercase leading v is accepted and
// carries no meaning, so v1.0.0 and 1.0.0 are the same version, which is what
// lets a caller hand over a tag as published. Only that one spelling: an
// uppercase V and a second v are refused, since nothing produces them and a
// gate that guesses is a gate that lets a wrong tag through.
//
// In two places it is narrower, and both are deliberate deviations from the
// specification whose name this package carries, worth knowing before reading
// the name as a promise. Section 10 allows a plus-sign build metadata suffix
// and requires precedence to ignore it; here such a version is refused rather
// than ignored, because nothing we publish carries one and accepting the form
// would widen the gate for no caller. And every number is read into a 64-bit
// unsigned value, so a triple part or a numeric prerelease identifier past
// that bound is refused, where the specification sets no maximum on either.
// Anything that needs full semver 2.0.0 needs a different package.
package semver

import (
	"cmp"
	"errors"
	"strconv"
	"strings"
)

// The parse errors. They are static values holding no part of the input, and
// that is a contract rather than an accident: the input can be a value the CLI
// does not control, such as a header from a server, and keeping such a value
// off the terminal is half of why this gate exists. An error that embedded it
// would hand the next caller a way to print it anyway. The errors are meant to
// be branched on, never rendered, so they carry no user-facing copy.
var (
	errTriple              = errors.New("a version needs exactly three numeric parts")
	errNumberEmpty         = errors.New("a numeric field is empty")
	errNumberCharacter     = errors.New("a numeric field holds something other than digits")
	errNumberLeadingZero   = errors.New("a numeric field has a leading zero")
	errNumberRange         = errors.New("a numeric field is too large")
	errBuildMetadata       = errors.New("build metadata is not accepted")
	errIdentifierEmpty     = errors.New("a prerelease identifier is empty")
	errIdentifierCharacter = errors.New("a prerelease identifier holds something other than letters, digits and hyphens")
)

// identifier is one dot-separated field of a prerelease tail, classified as
// numeric or not at parse time. Semver 2.0.0 section 11 rule 4.3 orders two
// identifiers on that classification alone, and rules 4.1 and 4.2 order within
// one. The bound on a numeric identifier is this package's own, not the
// specification's: it is read into a 64-bit unsigned value so an oversized
// field is refused rather than wrapping into a small number. Both the flag and
// the value are therefore settled where the input can still be refused.
type identifier struct {
	text    string
	num     uint64
	numeric bool
}

// Version is a parsed version, ordered by Compare. Only Parse produces a valid
// one.
//
// The zero Version is not a version. It is what Parse returns beside an error,
// and it orders below every parsed version, including 0.0.0. That ordering is a
// guarantee on one side only. When the value that failed to parse is the latest
// version, it never reads as newer, so a string the CLI does not control cannot
// make a caller announce or fetch a release. When it is the running version,
// the same ordering makes every release read as newer, exactly as a plausible
// 0.0.0 would, and the ordering protects nothing. What covers that side is the
// caller's own contract: branch on the parse error, and neither announce nor
// fetch anything when either side failed to parse.
type Version struct {
	parsed bool
	major  uint64
	minor  uint64
	patch  uint64
	pre    []identifier
}

// Parse reads [v]MAJOR.MINOR.PATCH[-PRERELEASE] and refuses anything else.
//
// Refusals include a fourth numeric part, a leading zero anywhere a number is
// expected (two spellings of one version is exactly what a gate feeding a
// download URL must not have), a number too large for the 64-bit unsigned
// value it is read into, build metadata, an uppercase V, and any surrounding
// whitespace, which is never trimmed.
//
// The returned error names the fault and never quotes the input.
func Parse(s string) (Version, error) {
	// Up front, so the one form the specification allows and this package does
	// not is named by its own error rather than surfacing as a stray character
	// deeper in.
	if strings.ContainsRune(s, '+') {
		return Version{}, errBuildMetadata
	}

	// Exactly one lowercase v is tolerated, and no explicit check is needed for
	// the rest: TrimPrefix takes no more than one, so a second v or an
	// uppercase V survives into the major part, where it is not a digit.
	s = strings.TrimPrefix(s, "v")

	// Cut at the first hyphen: the triple holds digits and dots only, so the
	// first one can only be the hyphen introducing the tail, and every later
	// one belongs to an identifier, which is what makes x-y-z a single
	// identifier. Whether there was a hyphen at all is what separates no tail
	// from an empty one, and an empty one is a refusal.
	triple, pre, hasPre := strings.Cut(s, "-")

	// Split returns one field more than there are separators, so counting the
	// dots decides exactly what the split length would, and deciding it first
	// keeps an input that is one long run of dots from allocating a string
	// header per dot before being refused. The input can be a value the CLI
	// does not control, such as a header from a server, and nothing bounds its
	// length here.
	if strings.Count(triple, ".") != 2 {
		return Version{}, errTriple
	}
	parts := strings.Split(triple, ".")
	v := Version{parsed: true}
	var err error
	if v.major, err = parseNumber(parts[0]); err != nil {
		return Version{}, err
	}
	if v.minor, err = parseNumber(parts[1]); err != nil {
		return Version{}, err
	}
	if v.patch, err = parseNumber(parts[2]); err != nil {
		return Version{}, err
	}

	if hasPre {
		if v.pre, err = parsePrerelease(pre); err != nil {
			return Version{}, err
		}
	}
	return v, nil
}

// parseNumber reads one unsigned decimal number, under the rules semver 2.0.0
// sections 2 and 9 put on the triple's parts and on a numeric prerelease
// identifier alike.
func parseNumber(s string) (uint64, error) {
	if s == "" {
		return 0, errNumberEmpty
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errNumberCharacter
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, errNumberLeadingZero
	}
	// Syntax is settled above, so the only failure left is the range one, and
	// refusing it is what keeps a forty-digit field from wrapping into a small
	// number that would then compare as a real version.
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, errNumberRange
	}
	return n, nil
}

// parsePrerelease reads the tail after the introducing hyphen: a dot-separated
// list of one or more identifiers, per semver 2.0.0 section 9.
//
// The fields are cut off one at a time rather than split up front, and the
// slice is grown rather than sized from the field count, so that refusing a
// tail costs no more than the field that earned the refusal. The tail is
// whatever followed the first hyphen of a value the CLI does not control, such
// as a header from a server, and nothing bounds its length here: splitting a
// long run of dots would allocate a string header per dot, and sizing the slice
// from that count an identifier per dot on top, all before the first field is
// looked at. Only the last field is cut with no separator left after it, so an
// empty tail is one empty identifier and is refused as one.
func parsePrerelease(s string) ([]identifier, error) {
	var ids []identifier
	for {
		field, rest, more := strings.Cut(s, ".")
		id, err := parseIdentifier(field)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		if !more {
			return ids, nil
		}
		s = rest
	}
}

// parseIdentifier reads one prerelease identifier and classifies it. Semver
// 2.0.0 section 11 rule 4.2 counts an identifier as non-numeric as soon as it
// holds a letter or a hyphen, so -1 and 1-1 are alphanumeric, not numbers.
func parseIdentifier(s string) (identifier, error) {
	if s == "" {
		return identifier{}, errIdentifierEmpty
	}
	numeric := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' {
			numeric = false
			continue
		}
		// The set is ASCII, so any byte of a multi-byte character lands here.
		if c < '0' || c > '9' {
			return identifier{}, errIdentifierCharacter
		}
	}
	if !numeric {
		return identifier{text: s}, nil
	}
	n, err := parseNumber(s)
	if err != nil {
		return identifier{}, err
	}
	return identifier{text: s, num: n, numeric: true}, nil
}

// Compare orders a against b under semver 2.0.0 section 11, returning -1 when
// a has the lower precedence, +1 when it has the higher, and 0 when the two
// are equal. An unparsed (zero) Version orders below every parsed one, and two
// unparsed ones compare equal.
func Compare(a, b Version) int {
	if a.parsed != b.parsed {
		if a.parsed {
			return 1
		}
		return -1
	}
	if !a.parsed {
		return 0
	}
	if c := cmp.Compare(a.major, b.major); c != 0 {
		return c
	}
	if c := cmp.Compare(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmp.Compare(a.patch, b.patch); c != 0 {
		return c
	}
	return comparePrerelease(a.pre, b.pre)
}

// comparePrerelease orders two prerelease tails, either of which may be empty.
func comparePrerelease(a, b []identifier) int {
	// Section 11 rule 3: a version with a tail has lower precedence than the
	// same triple without one, which is the reverse of the length rule below
	// and the reason the empty case is settled before any identifier is read.
	if len(a) == 0 || len(b) == 0 {
		switch {
		case len(a) == len(b):
			return 0
		case len(a) == 0:
			return 1
		default:
			return -1
		}
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	// Section 11 rule 4.4: every shared identifier is equal, so the larger set
	// wins.
	return cmp.Compare(len(a), len(b))
}

// compareIdentifier orders two prerelease identifiers under semver 2.0.0
// section 11 rules 4.1 to 4.3.
func compareIdentifier(a, b identifier) int {
	// Rule 4.3, applied before either comparison below, because it decides on
	// the kind of identifier alone: a number is always lower than a word, and
	// however large the number is does not enter into it.
	if a.numeric != b.numeric {
		if a.numeric {
			return -1
		}
		return 1
	}
	if a.numeric {
		// Rule 4.1. Comparing these as text would put 11 below 2.
		return cmp.Compare(a.num, b.num)
	}
	// Rule 4.2. The identifiers are ASCII by construction, so comparing bytes
	// is comparing in ASCII order.
	return cmp.Compare(a.text, b.text)
}
