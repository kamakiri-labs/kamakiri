package semver

import (
	"slices"
	"testing"
)

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q) = %v, want no error", s, err)
	}
	return v
}

// preTexts renders a parsed prerelease tail as the identifiers it holds, so an
// assertion reads as the input does.
func preTexts(v Version) []string {
	texts := make([]string, len(v.pre))
	for i, id := range v.pre {
		texts[i] = id.text
	}
	return texts
}

func TestParseAccepts(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		major uint64
		minor uint64
		patch uint64
		pre   []string
	}{
		{name: "plain triple", in: "1.2.3", major: 1, minor: 2, patch: 3},
		{name: "leading v", in: "v1.2.3", major: 1, minor: 2, patch: 3},
		{name: "all zeros", in: "0.0.0"},
		{name: "a part that is exactly zero is not a leading zero", in: "0.1.0", minor: 1},
		{name: "multi-digit parts", in: "10.20.30", major: 10, minor: 20, patch: 30},
		{name: "the largest part a 64-bit unsigned holds", in: "18446744073709551615.0.0", major: 18446744073709551615},
		{name: "one prerelease identifier", in: "0.1.0-snapshot", minor: 1, pre: []string{"snapshot"}},
		{name: "several prerelease identifiers", in: "1.0.0-alpha.1", major: 1, pre: []string{"alpha", "1"}},
		// Both examples come from semver 2.0.0 section 9, and both turn on the
		// hyphen being an ordinary identifier character once the tail has begun.
		{name: "hyphens inside identifiers", in: "1.0.0-x-y-z.--", major: 1, pre: []string{"x-y-z", "--"}},
		{name: "numeric identifiers including zero", in: "1.0.0-0.3.7", major: 1, pre: []string{"0", "3", "7"}},
		{name: "leading v with a prerelease tail", in: "v0.1.7-snapshot.2", minor: 1, patch: 7, pre: []string{"snapshot", "2"}},
		{name: "the largest numeric identifier a 64-bit unsigned holds", in: "1.0.0-18446744073709551615", major: 1, pre: []string{"18446744073709551615"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := mustParse(t, tt.in)
			if v.major != tt.major || v.minor != tt.minor || v.patch != tt.patch {
				t.Errorf("Parse(%q) triple = %d.%d.%d, want %d.%d.%d",
					tt.in, v.major, v.minor, v.patch, tt.major, tt.minor, tt.patch)
			}
			if got := preTexts(v); !slices.Equal(got, tt.pre) {
				t.Errorf("Parse(%q) prerelease = %q, want %q", tt.in, got, tt.pre)
			}
			if !v.parsed {
				t.Errorf("Parse(%q) returned a version that reads as unparsed", tt.in)
			}
		})
	}
}

// Which identifiers count as numeric is what decides two of the four
// prerelease rules in semver 2.0.0 section 11, so it is asserted on the parsed
// value rather than only through the order it produces. Section 11 counts an
// identifier as non-numeric as soon as it holds a letter or a hyphen.
func TestParseClassifiesPrereleaseIdentifiers(t *testing.T) {
	tests := []struct {
		identifier string
		numeric    bool
		num        uint64
	}{
		{identifier: "0", numeric: true},
		{identifier: "11", numeric: true, num: 11},
		{identifier: "18446744073709551615", numeric: true, num: 18446744073709551615},
		{identifier: "alpha"},
		{identifier: "1a"},
		{identifier: "a1"},
		// Section 9 bans a leading zero in a numeric identifier only, so these
		// two are valid and the rule must not reach them.
		{identifier: "0a"},
		{identifier: "01a"},
		{identifier: "-"},
		{identifier: "1-1"},
		{identifier: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.identifier, func(t *testing.T) {
			in := "1.0.0-" + tt.identifier
			v := mustParse(t, in)
			if len(v.pre) != 1 {
				t.Fatalf("Parse(%q) prerelease = %q, want one identifier", in, preTexts(v))
			}
			if v.pre[0].text != tt.identifier {
				t.Errorf("Parse(%q) identifier text = %q, want %q", in, v.pre[0].text, tt.identifier)
			}
			if v.pre[0].numeric != tt.numeric {
				t.Errorf("Parse(%q) identifier numeric = %v, want %v", in, v.pre[0].numeric, tt.numeric)
			}
			if v.pre[0].num != tt.num {
				t.Errorf("Parse(%q) identifier number = %d, want %d", in, v.pre[0].num, tt.num)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "empty", in: "", wantErr: errTriple},
		{name: "bare v", in: "v", wantErr: errTriple},
		{name: "one part", in: "1", wantErr: errTriple},
		{name: "two parts", in: "1.0", wantErr: errTriple},
		{name: "four parts", in: "v0.1.7.10", wantErr: errTriple},
		{name: "garbage", in: "not-a-version", wantErr: errTriple},
		{name: "no triple before the prerelease", in: "-1.0.0", wantErr: errTriple},
		{name: "empty part", in: "1..0", wantErr: errNumberEmpty},
		{name: "second v", in: "vv1.0.0", wantErr: errNumberCharacter},
		{name: "uppercase V", in: "V1.0.0", wantErr: errNumberCharacter},
		{name: "letter in the triple", in: "1.0.x", wantErr: errNumberCharacter},
		{name: "leading space", in: " 1.0.0", wantErr: errNumberCharacter},
		{name: "trailing space", in: "1.0.0 ", wantErr: errNumberCharacter},
		{name: "trailing newline", in: "1.0.0\n", wantErr: errNumberCharacter},
		{name: "non-ASCII digit", in: "１.0.0", wantErr: errNumberCharacter},
		{name: "leading zero in major", in: "01.0.0", wantErr: errNumberLeadingZero},
		{name: "leading zero in minor", in: "1.01.0", wantErr: errNumberLeadingZero},
		{name: "leading zero in patch", in: "1.0.01", wantErr: errNumberLeadingZero},
		{name: "leading zero in a numeric identifier", in: "1.0.0-01", wantErr: errNumberLeadingZero},
		{name: "part past what a 64-bit unsigned holds", in: "18446744073709551616.0.0", wantErr: errNumberRange},
		{name: "numeric identifier past what a 64-bit unsigned holds", in: "1.0.0-18446744073709551616", wantErr: errNumberRange},
		{name: "forty-digit part", in: "1111111111111111111111111111111111111111.0.0", wantErr: errNumberRange},
		{name: "build metadata", in: "1.0.0+build.1", wantErr: errBuildMetadata},
		{name: "build metadata after a prerelease", in: "1.0.0-alpha+001", wantErr: errBuildMetadata},
		{name: "trailing hyphen", in: "1.0.0-", wantErr: errIdentifierEmpty},
		{name: "empty identifier between two others", in: "1.0.0-a..b", wantErr: errIdentifierEmpty},
		{name: "empty leading identifier", in: "1.0.0-.a", wantErr: errIdentifierEmpty},
		{name: "empty trailing identifier", in: "1.0.0-alpha.", wantErr: errIdentifierEmpty},
		{name: "underscore in an identifier", in: "1.0.0-alpha_beta", wantErr: errIdentifierCharacter},
		{name: "non-ASCII in an identifier", in: "1.0.0-α", wantErr: errIdentifierCharacter},
		{name: "space in an identifier", in: "1.0.0-alpha ", wantErr: errIdentifierCharacter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Parse(tt.in)
			// Identity, not errors.Is: the package's errors are static values,
			// and that is what keeps the input out of them. A wrapped error
			// carrying the offending string would satisfy errors.Is and defeat
			// the rule that no error quotes an input the CLI does not control.
			if err != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if v.parsed {
				t.Errorf("Parse(%q) refused but returned a version that reads as parsed", tt.in)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{name: "equal triples", a: "1.2.3", b: "1.2.3", want: 0},
		{name: "major", a: "1.0.0", b: "2.0.0", want: -1},
		{name: "minor", a: "2.0.0", b: "2.1.0", want: -1},
		{name: "patch", a: "2.1.0", b: "2.1.1", want: -1},
		{name: "major outranks minor and patch", a: "1.99.99", b: "2.0.0", want: -1},
		{name: "minor outranks patch", a: "1.1.99", b: "1.2.0", want: -1},
		// A lexical comparison would order these the other way, which is the
		// whole reason the parts are compared as numbers.
		{name: "parts compare numerically", a: "2.0.0", b: "10.0.0", want: -1},
		{name: "leading v is tolerance, not identity", a: "v1.0.0", b: "1.0.0", want: 0},
		{name: "leading v on the higher side", a: "1.0.0", b: "v1.0.1", want: -1},
		{name: "leading v on the lower side", a: "v1.0.0", b: "1.0.1", want: -1},
		// Semver 2.0.0 section 11, rule 3.
		{name: "a prerelease orders below its triple", a: "0.1.0-snapshot", b: "0.1.0", want: -1},
		{name: "equal prerelease tails", a: "1.0.0-alpha.1", b: "1.0.0-alpha.1", want: 0},
		{name: "the triple is compared before the tail", a: "1.0.0", b: "0.9.9-alpha", want: 1},
		// Section 11, rule 4.1.
		{name: "numeric identifiers compare numerically", a: "1.0.0-2", b: "1.0.0-11", want: -1},
		// Section 11, rule 4.2.
		{name: "alphanumeric identifiers compare in ASCII order", a: "1.0.0-Alpha", b: "1.0.0-alpha", want: -1},
		{name: "ASCII order puts a hyphen below a letter", a: "1.0.0--", b: "1.0.0-a", want: -1},
		// Section 11, rule 4.3. The numeric identifier is the larger number in
		// both rows, so a comparison that forgot the rule would order them the
		// other way.
		{name: "a numeric identifier orders below an alphanumeric one", a: "1.0.0-99", b: "1.0.0-alpha", want: -1},
		{name: "a hyphen makes an identifier alphanumeric", a: "1.0.0-99", b: "1.0.0--", want: -1},
		{name: "the rule applies at any position", a: "1.0.0-alpha.99", b: "1.0.0-alpha.beta", want: -1},
		// Section 11, rule 4.4.
		{name: "a longer tail orders above a shorter one", a: "1.0.0-alpha", b: "1.0.0-alpha.1", want: -1},
		{name: "the longer tail only wins when the shared identifiers are equal", a: "1.0.0-alpha.99", b: "1.0.0-beta", want: -1},
		{name: "a numeric tail extended", a: "1.0.0-1", b: "1.0.0-1.0", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := mustParse(t, tt.a), mustParse(t, tt.b)
			if got := Compare(a, b); got != tt.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
			// The relation is antisymmetric, so the reverse of every row is
			// asserted here rather than written out again as its own row.
			if got := Compare(b, a); got != -tt.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tt.b, tt.a, got, -tt.want)
			}
			if got := Compare(a, a); got != 0 {
				t.Errorf("Compare(%q, %q) = %d, want 0", tt.a, tt.a, got)
			}
			if got := Compare(b, b); got != 0 {
				t.Errorf("Compare(%q, %q) = %d, want 0", tt.b, tt.b, got)
			}
		})
	}
}

// The worked example from semver 2.0.0 section 11, asserted over every pair
// rather than only consecutive ones, so the order is transitive and not just
// locally right.
func TestPrecedenceExampleFromTheSpecification(t *testing.T) {
	ascending := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
	}

	for i, lower := range ascending {
		for _, higher := range ascending[i+1:] {
			a, b := mustParse(t, lower), mustParse(t, higher)
			if got := Compare(a, b); got != -1 {
				t.Errorf("Compare(%q, %q) = %d, want -1", lower, higher, got)
			}
			if got := Compare(b, a); got != 1 {
				t.Errorf("Compare(%q, %q) = %d, want 1", higher, lower, got)
			}
		}
	}
}

// A caller that ignores a parse error holds the zero Version. Ordering it below
// every parsed version is a guarantee on one side only: an unparsed latest
// version never reads as newer, so nothing announces or fetches a release the
// package refused to parse. Held as the running version it protects nothing,
// reading as older than everything exactly as a plausible 0.0.0 would; the
// guarantee there is the caller's, to branch on the parse error.
func TestZeroVersionOrdersBelowEveryParsedVersion(t *testing.T) {
	var zero Version

	if got := Compare(zero, zero); got != 0 {
		t.Errorf("Compare(zero, zero) = %d, want 0", got)
	}
	for _, s := range []string{"0.0.0", "0.0.0-alpha", "1.0.0", "18446744073709551615.0.0"} {
		v := mustParse(t, s)
		if got := Compare(zero, v); got != -1 {
			t.Errorf("Compare(zero, %q) = %d, want -1", s, got)
		}
		if got := Compare(v, zero); got != 1 {
			t.Errorf("Compare(%q, zero) = %d, want 1", s, got)
		}
	}
}
