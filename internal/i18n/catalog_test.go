package i18n

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// These tests are the catalog's gates. Every later string that is keyed leans on
// them, so each one has to be able to fail: removing a key from one catalog,
// repeating a key in one file, retyping a verb, adding an unkeyed lookup,
// handing a call site the wrong number of arguments, leaving a key with no call
// site and importing the rest of the CLI from here each have to turn one of them
// red.

const (
	cliModule      = "github.com/kamakiri-labs/kamakiri"
	i18nImportPath = `"` + cliModule + `/internal/i18n"`
)

func TestCatalogsCarryTheSameKeys(t *testing.T) {
	en := loadCatalog(t, "messages_en.json")
	ja := loadCatalog(t, "messages_ja.json")

	for _, key := range slices.Sorted(maps.Keys(en)) {
		if _, ok := ja[key]; !ok {
			t.Errorf("key %q is in messages_en.json but missing from messages_ja.json", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(ja)) {
		if _, ok := en[key]; !ok {
			t.Errorf("key %q is in messages_ja.json but missing from messages_en.json", key)
		}
	}
}

// Tf builds its format string from the catalog, which go vet's printf analysis
// cannot see through, so this test is what stands in for it: it holds the two
// languages to the same arguments, while still letting Japanese reorder them
// with explicit indexes.
func TestCatalogsAgreeOnFormatVerbs(t *testing.T) {
	en := loadCatalog(t, "messages_en.json")
	ja := loadCatalog(t, "messages_ja.json")

	for _, key := range slices.Sorted(maps.Keys(en)) {
		jaValue, ok := ja[key]
		if !ok {
			continue
		}

		enArgs, _, err := scanFormat(en[key])
		if err != nil {
			t.Errorf("%s: messages_en.json: %v (%q)", key, err, en[key])
			continue
		}
		jaArgs, _, err := scanFormat(jaValue)
		if err != nil {
			t.Errorf("%s: messages_ja.json: %v (%q)", key, err, jaValue)
			continue
		}

		if !maps.Equal(enArgs, jaArgs) {
			t.Errorf("%s: the two languages consume different arguments: en %s, ja %s",
				key, describeArgs(enArgs), describeArgs(jaArgs))
			continue
		}

		// The two are equal by here, so one of them stands for both. An index
		// below the highest one that no verb consumes is a hole rather than an
		// error fmt reports: it counts its way past the argument to reach the
		// ones that are consumed, and the value passed for it goes nowhere.
		needed := argumentsNeeded(enArgs)
		for index := 1; index <= needed; index++ {
			if _, consumed := enArgs[index]; !consumed {
				t.Errorf("%s: no verb consumes argument %d, yet the call has to pass %d to reach the ones that are, so the value it passes for %d is dropped: %q",
					key, index, needed, index, en[key])
			}
		}
	}
}

func TestEveryKeyHasACallSiteAndEveryCallSiteHasAKey(t *testing.T) {
	en := loadCatalog(t, "messages_en.json")
	ja := loadCatalog(t, "messages_ja.json")

	used := map[string]bool{}
	for _, site := range callSitesUnderCLI(t) {
		if !site.literal {
			t.Errorf("%s: %s.%s is called with a computed key; this check reads call sites, so the key has to be a literal",
				site.pos, site.local, site.fn)
			continue
		}
		used[site.key] = true
		if _, ok := en[site.key]; !ok {
			t.Errorf("%s: key %q is not in messages_en.json", site.pos, site.key)
		}
		if _, ok := ja[site.key]; !ok {
			t.Errorf("%s: key %q is not in messages_ja.json", site.pos, site.key)
		}
		if site.runsAtInit != "" && site.fn != "NewError" {
			t.Errorf("%s: %s.%s(%q) sits in %s, which runs before the language is resolved and would freeze the message in whatever the environment suggested; for a sentinel error use %s.NewError(%q), otherwise move the lookup to where the string is printed",
				site.pos, site.local, site.fn, site.key, site.runsAtInit, site.local, site.key)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(en)) {
		if !used[key] {
			t.Errorf("catalog key %q has no call site; wire it up or delete it from both catalogs", key)
		}
	}
}

// The other half of standing in for go vet: the verb-parity test holds the two
// catalogs to the same arguments, and this one holds every call site to the
// arguments its key actually consumes, so a missing or surplus one is caught
// where it is written rather than printed to a user as %!s(MISSING).
func TestCallSitesPassTheArgumentsTheirKeysNeed(t *testing.T) {
	en := loadCatalog(t, "messages_en.json")

	for _, site := range callSitesUnderCLI(t) {
		// A computed key, a key the English catalog does not carry and a
		// malformed value are each another gate's to report.
		if !site.literal {
			continue
		}
		value, ok := en[site.key]
		if !ok {
			continue
		}
		if site.spread {
			t.Errorf("%s: %s.%s(%q, ...) spreads a slice, so nothing can count its arguments against the message; write them out",
				site.pos, site.local, site.fn, site.key)
			continue
		}
		args, _, err := scanFormat(value)
		if err != nil {
			continue
		}

		want := argumentsNeeded(args)
		if site.args != want {
			t.Errorf("%s: %s.%s is called with key %q and %d argument(s), but the message consumes %d: %q",
				site.pos, site.local, site.fn, site.key, site.args, want, value)
		}
	}
}

func TestJapaneseCatalogIsActuallyJapanese(t *testing.T) {
	en := loadCatalog(t, "messages_en.json")
	ja := loadCatalog(t, "messages_ja.json")

	for _, key := range slices.Sorted(maps.Keys(en)) {
		jaValue, ok := ja[key]
		if !ok {
			continue
		}
		_, literal, err := scanFormat(en[key])
		if err != nil {
			continue
		}
		// A value that is only verbs and punctuation carries no prose to
		// translate, so it is allowed to be identical in both languages.
		if !strings.ContainsFunc(literal, func(r rune) bool {
			return r <= unicode.MaxASCII && unicode.IsLetter(r)
		}) {
			continue
		}

		if jaValue == en[key] {
			t.Errorf("%s: the ja value is the en value verbatim: %q", key, jaValue)
		}
		if !strings.ContainsFunc(jaValue, func(r rune) bool { return r > unicode.MaxASCII }) {
			t.Errorf("%s: the ja value has no character outside ASCII, so it is not Japanese: %q", key, jaValue)
		}
	}
}

// Nothing else keeps this package clear of the rest of the CLI. Every package it
// might reach for is itself localized and calls back into here, so an import
// that looks harmless today is an import cycle at the next conversion, which the
// compiler then reports far from whoever wrote it.
func TestI18nImportsNothingFromTheRestOfTheCLI(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Test files are out: a test compiled into this package would hit the
		// cycle at once, and an external test package can import whatever it
		// likes without putting this package in one.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, parseErr := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("%s: %v", name, parseErr)
		}
		checked++

		for _, imported := range file.Imports {
			path, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				t.Fatalf("%s: import path %s: %v", name, imported.Path.Value, unquoteErr)
			}
			if strings.HasPrefix(path, cliModule+"/") {
				t.Errorf("%s imports %s; this package has to stay importable from every other one, so the standard library and golang.org/x/text are all it may use",
					name, path)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no non-test file in the package directory; the check is reading the wrong place")
	}
}

// Verb parity, call-site arity and the Japanese check all read format strings
// through scanFormat, so it is pinned directly rather than through whichever
// shapes the catalogs happen to carry today. Each accepted case is also rendered
// through fmt, with exactly the arguments scanFormat says it needs and then with
// one fewer, which is what keeps the parser's model of an argument count from
// drifting away from the package it is modelling.
func TestScanFormat(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		args    map[int]byte
		literal string
		needed  int
		wantErr string
		// fmtRenders marks a rejected format that fmt itself prints without a
		// %! marker, so the rejection is this parser's own call rather than a
		// rendering the user would be shown.
		fmtRenders bool
	}{
		{
			name:    "no verb at all",
			format:  "Language set.",
			args:    map[int]byte{},
			literal: "Language set.",
		},
		{
			name:    "implicit verbs take successive arguments",
			format:  "%s and %d",
			args:    map[int]byte{1: 's', 2: 'd'},
			literal: " and ",
			needed:  2,
		},
		{
			name:    "a doubled percent consumes nothing",
			format:  "100%% sure",
			args:    map[int]byte{},
			literal: "100% sure",
		},
		{
			name:    "explicit indexes reorder",
			format:  "%[2]s before %[1]s",
			args:    map[int]byte{1: 's', 2: 's'},
			literal: " before ",
			needed:  2,
		},
		{
			// An implicit verb resumes from the last explicit index rather than
			// from where it would have been, so a format naming two verbs can
			// still reach a third argument.
			name:    "an implicit verb continues from an explicit index",
			format:  "%[2]s then %s",
			args:    map[int]byte{2: 's', 3: 's'},
			literal: " then ",
			needed:  3,
		},
		{
			name:    "an index gap leaves an argument unconsumed",
			format:  "%s %[3]s",
			args:    map[int]byte{1: 's', 3: 's'},
			literal: " ",
			needed:  3,
		},
		{
			name:    "a repeated index consumes one argument twice",
			format:  "%[1]s %[1]s",
			args:    map[int]byte{1: 's'},
			literal: " ",
			needed:  1,
		},
		{
			name:    "flags, width and precision belong to the verb",
			format:  "%-8.2f|",
			args:    map[int]byte{1: 'f'},
			literal: "|",
			needed:  1,
		},
		{
			// A reordered value can still pad: what stands ahead of the index
			// is read as the flags, the width and the precision, while the
			// same bytes written after it are not.
			name:    "flags, width and precision may precede an explicit index",
			format:  "%-5.2[2]f|",
			args:    map[int]byte{2: 'f'},
			literal: "|",
			needed:  2,
		},
		{
			name:    "a width before an explicit index is legal",
			format:  "%5[2]s|",
			args:    map[int]byte{2: 's'},
			literal: "|",
			needed:  2,
		},
		{
			name:    "an index after the precision dot is legal",
			format:  "%.[2]f|",
			args:    map[int]byte{2: 'f'},
			literal: "|",
			needed:  2,
		},
		{
			name:    "a width with an index after the precision dot is legal",
			format:  "%5.[2]f|",
			args:    map[int]byte{2: 'f'},
			literal: "|",
			needed:  2,
		},
		{
			// The dot's index is fmt's second index position, and the digits
			// after it are still the precision.
			name:    "a precision may be written after the dot's index",
			format:  "%.[2]3f|",
			args:    map[int]byte{2: 'f'},
			literal: "|",
			needed:  2,
		},
		{
			// The other side of the index-on-a-percent boundary: a % verb with
			// no index of its own leaves fmt's place in the arguments alone, so
			// the verb after it takes the next argument rather than an earlier
			// one.
			name:    "a doubled percent between two verbs shifts nothing",
			format:  "%s %% %s",
			args:    map[int]byte{1: 's', 2: 's'},
			literal: " % ",
			needed:  2,
		},
		{
			// A width on a % verb is dead weight, fmt ignoring both the width
			// and the precision there, but it is the index alone that makes
			// such a verb a problem, so this one is admitted.
			name:    "a width on a percent verb is accepted",
			format:  "%5%",
			args:    map[int]byte{},
			literal: "%",
		},
		{
			name:    "flags may repeat and stand in any order among themselves",
			format:  "%0-5s|",
			args:    map[int]byte{1: 's'},
			literal: "|",
			needed:  1,
		},
		{
			name:    "a width after an explicit index is rejected",
			format:  "%[2]5s",
			wantErr: `fmt reads the "5" after the argument index as a width, and then discards the index; write the width before the index, as in %-5.2[2]f`,
		},
		{
			name:    "zero padding after an explicit index is rejected",
			format:  "%[2]08d",
			wantErr: `fmt reads the "08" after the argument index as a width, and then discards the index; write the width before the index, as in %-5.2[2]f`,
		},
		{
			name:    "a precision after an explicit index is rejected",
			format:  "%[1].2f",
			wantErr: `fmt reads the "." after the argument index as a precision, and then discards the index; write the precision before the index, as in %-5.2[2]f`,
		},
		{
			// The verb here is %, which absorbs no operand and so renders in
			// spite of the discarded index. The parser turns it away all the
			// same, which is why the message describes what fmt reads rather
			// than promising a broken rendering.
			name:       "a width after an explicit index is rejected even where fmt would render it",
			format:     "%[1]5%",
			wantErr:    `fmt reads the "5" after the argument index as a width, and then discards the index; write the width before the index, as in %-5.2[2]f`,
			fmtRenders: true,
		},
		{
			// The verb consumes nothing, so the index cannot be reordering
			// anything; what it does is move fmt on to that argument, and the
			// verbs after it then start from there. A value carrying one
			// renders without a %! marker while silently printing the wrong
			// arguments, which is the whole reason this is rejected.
			name:       "an explicit index on a percent verb is rejected",
			format:     "%[1]%",
			wantErr:    "%[1]% prints a bare % and consumes no argument, yet the index can still move fmt's place in the arguments, so the next verb without an index of its own may take argument 1; write %% for a literal percent sign",
			fmtRenders: true,
		},
		{
			// The index written after the precision dot reaches fmt's argument
			// cursor by the same route, so a percent verb is rejected there too.
			name:       "an index after the precision dot on a percent verb is rejected",
			format:     "%.[2]%",
			wantErr:    "%[2]% prints a bare % and consumes no argument, yet the index can still move fmt's place in the arguments, so the next verb without an index of its own may take argument 2; write %% for a literal percent sign",
			fmtRenders: true,
		},
		{
			name:    "a flag after an explicit index is rejected",
			format:  "%[2]-5s",
			wantErr: `"-" is a flag, and fmt reads flags only at the start of a verb; write it directly after the %, as in %-5.2f`,
		},
		{
			name:    "a flag after a width is rejected",
			format:  "%5-s",
			wantErr: `"-" is a flag, and fmt reads flags only at the start of a verb; write it directly after the %, as in %-5.2f`,
		},
		{
			name:    "a flag after a precision is rejected",
			format:  "%.2+s",
			wantErr: `"+" is a flag, and fmt reads flags only at the start of a verb; write it directly after the %, as in %-5.2f`,
		},
		{
			// The space is the flag byte least likely to be read as one, and it
			// pads exactly like the width it follows, so a value carrying it
			// looks right until fmt renders it.
			name:    "a space flag after a width is rejected",
			format:  "%5 s",
			wantErr: `" " is a flag, and fmt reads flags only at the start of a verb; write it directly after the %, as in %-5.2f`,
		},
		{
			name:    "a flag after the last index is rejected",
			format:  "%.2[2]0f",
			wantErr: `"0" is a flag, and fmt reads flags only at the start of a verb; write it directly after the %, as in %-5.2f`,
		},
		{
			name:    "a second explicit index stands where the verb belongs",
			format:  "%[1][2]s",
			wantErr: "fmt reads one argument index per verb, and this is a second one standing where the verb belongs; write a single index, as in %[2]s",
		},
		{
			name:       "a starred width is rejected",
			format:     "%*d",
			wantErr:    "a * reads the width or the precision from an argument of its own; write the number out",
			fmtRenders: true,
		},
		{
			name:       "a starred precision is rejected",
			format:     "%.*f",
			wantErr:    "a * reads the width or the precision from an argument of its own; write the number out",
			fmtRenders: true,
		},
		{
			// A C habit: fmt has no %i, and prints %!i(string=x) rather than the
			// value, so the letter has to be checked against fmt's own set.
			name:    "a letter fmt has no verb for is rejected",
			format:  "%i",
			wantErr: "%i is not one of the verbs Sprintf renders (bcdefgopqstvxEFGOTUX)",
		},
		{
			name:    "the uppercase spelling of a verb is not a verb",
			format:  "%S",
			wantErr: "%S is not one of the verbs Sprintf renders (bcdefgopqstvxEFGOTUX)",
		},
		{
			// %w is a verb, and one a Go author reaches for out of habit, but
			// only Errorf wraps with it; a catalog value carrying it reaches the
			// user as %!w(string=x) through Sprintf.
			name:    "a verb only Errorf understands is rejected",
			format:  "%w",
			wantErr: "%w is not one of the verbs Sprintf renders (bcdefgopqstvxEFGOTUX)",
		},
		{
			name:    "a bare percent before a multibyte rune names its cure",
			format:  "50%です",
			wantErr: `%\xe3 is not a verb; write %% for a literal percent sign`,
		},
		{
			name:    "a string ending inside a verb is rejected",
			format:  "progress: %",
			wantErr: "the string ends inside a verb",
		},
		{
			name:    "one index consumed by two verbs is rejected",
			format:  "%[1]s and %[1]d",
			wantErr: "argument 1 is consumed by both %s and %d",
		},
		{
			name:    "an unclosed index is rejected",
			format:  "%[1s",
			wantErr: "unclosed argument index",
		},
		{
			name:    "a zero index is rejected",
			format:  "%[0]s",
			wantErr: `bad argument index "[0]"`,
		},
		{
			name:    "a signed index is rejected",
			format:  "%[+2]s",
			wantErr: `bad argument index "[+2]"`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			args, literal, err := scanFormat(testCase.format)

			if testCase.wantErr != "" {
				if err == nil {
					t.Fatalf("scanFormat(%q) = %v, %q, nil; want the error %q",
						testCase.format, args, literal, testCase.wantErr)
				}
				if err.Error() != testCase.wantErr {
					t.Fatalf("scanFormat(%q) error = %q, want %q", testCase.format, err, testCase.wantErr)
				}
				// The rejected half of the table is held to fmt as well, so a
				// shape can only be listed here because fmt breaks on it or
				// because the case says outright that fmt does not.
				if rendered, renders := fmtRendering(testCase.format); renders != testCase.fmtRenders {
					if renders {
						t.Errorf("scanFormat rejects %q, yet fmt renders it as %q; either the rejection is wrong or the case has to say so",
							testCase.format, rendered)
					} else {
						t.Errorf("the case says fmt renders %q, but every argument count leaves a %%! marker in it", testCase.format)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("scanFormat(%q) error = %v", testCase.format, err)
			}
			if !maps.Equal(args, testCase.args) {
				t.Errorf("scanFormat(%q) args = %s, want %s",
					testCase.format, describeArgs(args), describeArgs(testCase.args))
			}
			if literal != testCase.literal {
				t.Errorf("scanFormat(%q) literal = %q, want %q", testCase.format, literal, testCase.literal)
			}
			if got := argumentsNeeded(args); got != testCase.needed {
				t.Fatalf("argumentsNeeded for %q = %d, want %d", testCase.format, got, testCase.needed)
			}

			if rendered := fmt.Sprintf(testCase.format, sampleArgs(testCase.args, testCase.needed)...); strings.Contains(rendered, "%!") {
				t.Errorf("fmt is not satisfied by the %d argument(s) scanFormat asks for: %q with %d renders as %q",
					testCase.needed, testCase.format, testCase.needed, rendered)
			}
			if testCase.needed == 0 {
				return
			}
			if rendered := fmt.Sprintf(testCase.format, sampleArgs(testCase.args, testCase.needed-1)...); !strings.Contains(rendered, "%!") {
				t.Errorf("fmt is satisfied by one argument fewer than scanFormat asks for: %q with %d renders as %q",
					testCase.format, testCase.needed-1, rendered)
			}
		})
	}
}

// sampleArgs builds count arguments for a format whose verbs are args, each of a
// type its verb accepts, so a rendering that carries a %! marker is a disagreement
// about how many arguments the format needs rather than about their types. An
// index no verb consumes gets a string, which nothing reads.
func sampleArgs(args map[int]byte, count int) []any {
	sample := make([]any, 0, count)
	for index := 1; index <= count; index++ {
		switch args[index] {
		case 'd':
			sample = append(sample, 7)
		case 'f':
			sample = append(sample, 1.5)
		default:
			sample = append(sample, "x")
		}
	}
	return sample
}

// fmtRendering reports whether fmt prints format without a %! marker for some
// small number of arguments, and the first rendering that manages it. It stands
// in for a count and a set of types scanFormat never worked out, the format
// having been rejected before it got that far, so it searches for them: every
// assignment of the three types a catalog value can ask for over every length up
// to four. A format that needs more or other arguments than that reads here as
// one fmt breaks on, which is why this only ever backs a case that already
// carries the error it expects.
func fmtRendering(format string) (string, bool) {
	values := []any{"x", 7, 1.5}
	for count := 0; count <= 4; count++ {
		assignments := 1
		for range count {
			assignments *= len(values)
		}
		for assignment := range assignments {
			args := make([]any, count)
			for i := range args {
				args[i] = values[assignment%len(values)]
				assignment /= len(values)
			}
			if rendered := fmt.Sprintf(format, args...); !strings.Contains(rendered, "%!") {
				return rendered, true
			}
		}
	}
	return "", false
}

func loadCatalog(t *testing.T, filename string) map[string]string {
	t.Helper()

	data, err := messagesFS.ReadFile(filename)
	if err != nil {
		t.Fatalf("reading %s: %v", filename, err)
	}
	catalog := map[string]string{}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	if len(catalog) == 0 {
		t.Fatalf("%s is empty", filename)
	}
	// Unmarshal keeps the last of a repeated key and says nothing, so a
	// hand-edited file can carry two values for one key and lose one of them
	// with every gate below still green.
	if repeated := repeatedKeys(t, filename, data); len(repeated) > 0 {
		t.Fatalf("%s: repeated keys: %s. JSON keeps the last value of a repeated key and drops the rest",
			filename, strings.Join(repeated, ", "))
	}
	return catalog
}

// repeatedKeys reports the keys that occur more than once in a catalog file, in
// the order they repeat, by walking the JSON tokens rather than the decoded map.
func repeatedKeys(t *testing.T, filename string, data []byte) []string {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		t.Fatalf("reading %s: %v", filename, err)
	}
	if opening != json.Delim('{') {
		t.Fatalf("%s does not hold a JSON object", filename)
	}

	seen := map[string]bool{}
	var repeated []string
	for decoder.More() {
		next, err := decoder.Token()
		if err != nil {
			t.Fatalf("reading %s: %v", filename, err)
		}
		key, ok := next.(string)
		if !ok {
			t.Fatalf("%s: %v where a key was expected", filename, next)
		}
		if seen[key] {
			repeated = append(repeated, strconv.Quote(key))
		}
		seen[key] = true

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("reading %s: %v", filename, err)
		}
	}
	return repeated
}

// scanFormat walks format once and reports which argument each verb consumes,
// along with the text left when the verbs are removed. It follows fmt's rules:
// an implicit verb takes the next argument, an explicit %[k]s takes the k-th and
// the implicit verbs after it continue from there, and %% consumes none.
//
// What a verb may carry ahead of its letter is fixed in order:
//
//	% flags* [index]? width? ( '.' [index]? precision? )? [index]? verb
//
// A byte written out of that order is not read as the thing it looks like. fmt
// takes the flags in a loop of their own before it looks at anything else, so a
// flag after a width or a precision is left standing where the verb belongs, and
// it forgets an index as soon as a width or a precision follows it. Either way
// the value never reaches the user, who sees a %! marker in its place, so the
// parser holds to the order rather than to the character classes.
//
// Where it deliberately stops short of fmt, so the boundary does not have to be
// re-derived. Three shapes fmt renders are rejected here anyway, because a
// message catalog has no use for them and reporting them is cheaper than
// modelling them: a * that reads the width or precision from an argument, an
// argument index on a % verb, and one index consumed by two different verbs. One
// shape is accepted that fmt breaks on: a width, precision or index whose digits
// run past fmt's own number limit, as in %10000011s, which prints %!(NOVERB).
// Reaching that from prose takes a stray %, a digit run of seven or more, and a
// real verb straight after it, and an index large enough to overflow is caught
// by the call-site argument count instead.
func scanFormat(format string) (args map[int]byte, literal string, err error) {
	args = map[int]byte{}
	var text strings.Builder
	next := 1

	for i := 0; i < len(format); {
		if format[i] != '%' {
			text.WriteByte(format[i])
			i++
			continue
		}
		i++

		// The flags, in a loop of their own ahead of everything else, which is
		// what makes this the only place they are flags.
		for i < len(format) && isFormatFlag(format[i]) {
			i++
		}

		var explicit int
		explicit, i, err = scanArgumentIndex(format, i)
		if err != nil {
			return nil, "", err
		}

		if i < len(format) && format[i] == '*' {
			return nil, "", errStarArgument
		}
		widthStart := i
		for i < len(format) && isASCIIDigit(format[i]) {
			i++
		}
		if i > widthStart && explicit != 0 {
			return nil, "", fmt.Errorf("fmt reads the %q after the argument index as a width, and then discards the index; write the width before the index, as in %%-5.2[2]f", format[widthStart:i])
		}

		// fmt takes a "." for a precision only when a byte follows it, and lets
		// the index be written after the dot instead of before it, which is how
		// %.[2]f reaches its argument.
		if i+1 < len(format) && format[i] == '.' {
			if explicit != 0 {
				return nil, "", fmt.Errorf("fmt reads the \".\" after the argument index as a precision, and then discards the index; write the precision before the index, as in %%-5.2[2]f")
			}
			i++
			var precisionIndex int
			precisionIndex, i, err = scanArgumentIndex(format, i)
			if err != nil {
				return nil, "", err
			}
			if precisionIndex != 0 {
				explicit = precisionIndex
			}
			if i < len(format) && format[i] == '*' {
				return nil, "", errStarArgument
			}
			for i < len(format) && isASCIIDigit(format[i]) {
				i++
			}
		}

		// The last place an index may stand. fmt looks here only when it has not
		// read one already, so the second index of %[1][2]s falls where the verb
		// belongs and is rejected there.
		if explicit == 0 {
			explicit, i, err = scanArgumentIndex(format, i)
			if err != nil {
				return nil, "", err
			}
		}

		if i >= len(format) {
			return nil, "", fmt.Errorf("the string ends inside a verb")
		}
		verb := format[i]
		i++
		if verb == '%' {
			// fmt takes the index into its argument cursor before it reads the
			// verb, and a % verb consumes nothing and leaves the cursor where
			// the index put it, so the verbs after %[1]% start again from
			// argument 1. It only moves the cursor when the call passes at
			// least that many arguments, which is the count being worked out
			// here, so the shape is turned away rather than modelled.
			if explicit != 0 {
				return nil, "", fmt.Errorf("%%[%d]%% prints a bare %% and consumes no argument, yet the index can still move fmt's place in the arguments, so the next verb without an index of its own may take argument %d; write %%%% for a literal percent sign", explicit, explicit)
			}
			text.WriteByte('%')
			continue
		}
		if !isFormatVerb(verb) {
			if isFormatFlag(verb) {
				return nil, "", fmt.Errorf("%q is a flag, and fmt reads flags only at the start of a verb; write it directly after the %%, as in %%-5.2f", string(verb))
			}
			if isASCIILetter(verb) {
				return nil, "", fmt.Errorf("%%%c is not one of the verbs Sprintf renders (%s)", verb, fmtVerbs)
			}
			// A "[" can only be a second index here: fmt reads one per verb, and
			// the scan above has taken the first from wherever it stood.
			if verb == '[' {
				return nil, "", fmt.Errorf("fmt reads one argument index per verb, and this is a second one standing where the verb belongs; write a single index, as in %%[2]s")
			}
			return nil, "", fmt.Errorf("%%%s is not a verb; write %%%% for a literal percent sign", describeByte(verb))
		}

		index := next
		if explicit != 0 {
			index = explicit
		}
		if previous, taken := args[index]; taken && previous != verb {
			return nil, "", fmt.Errorf("argument %d is consumed by both %%%c and %%%c", index, previous, verb)
		}
		args[index] = verb
		next = index + 1
	}

	return args, text.String(), nil
}

// A star takes an argument of its own, and one written as %[2]*[1]d takes it
// out of order, so counting a * correctly means reimplementing that corner of
// fmt. Rejecting it undercounts nothing and costs the catalogs a construction
// they never need. Both places fmt looks for a * report this, the width and the
// precision alike, so the message names neither on its own.
var errStarArgument = errors.New("a * reads the width or the precision from an argument of its own; write the number out")

// scanArgumentIndex reads the explicit argument index written at i, as in the
// "[2]" of %[2]s, and returns it with the offset of the byte after it. A
// position that carries no index is not an error: the index is optional at each
// of the three places fmt looks for one, and the returned index is then 0.
func scanArgumentIndex(format string, i int) (index, next int, err error) {
	if i >= len(format) || format[i] != '[' {
		return 0, i, nil
	}
	end := strings.IndexByte(format[i:], ']')
	if end < 0 {
		return 0, 0, fmt.Errorf("unclosed argument index")
	}
	// Digits only. strconv.Atoi would take a leading sign that fmt's own number
	// parser rejects, so "%[+2]s" would pass here and render as a bad index.
	digits := format[i+1 : i+end]
	for j := 0; j < len(digits); j++ {
		if !isASCIIDigit(digits[j]) {
			return 0, 0, fmt.Errorf("bad argument index %q", format[i:i+end+1])
		}
	}
	n, convErr := strconv.Atoi(digits)
	if convErr != nil || n < 1 {
		return 0, 0, fmt.Errorf("bad argument index %q", format[i:i+end+1])
	}
	return n, i + end + 1, nil
}

// argumentsNeeded returns how many arguments a call has to supply for the args
// scanFormat found. It is the highest index referenced rather than the count of
// them: an explicit index reaches past the ones before it, which still have to
// be there for fmt to count its way to it.
func argumentsNeeded(args map[int]byte) int {
	if len(args) == 0 {
		return 0
	}
	return slices.Max(slices.Collect(maps.Keys(args)))
}

func describeArgs(args map[int]byte) string {
	if len(args) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(args))
	for _, index := range slices.Sorted(maps.Keys(args)) {
		parts = append(parts, fmt.Sprintf("%d:%%%c", index, args[index]))
	}
	return strings.Join(parts, " ")
}

// describeByte renders one byte of a format string for a failure message. A
// Japanese value often carries a stray % in front of a multibyte rune, whose
// leading byte is not a character on its own and reads as mojibake when it is
// printed as one, so anything but printable ASCII is shown as hex.
func describeByte(b byte) string {
	if b > ' ' && b < unicode.MaxASCII {
		return string(b)
	}
	return fmt.Sprintf("\\x%02x", b)
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// fmtFlags is every byte fmt treats as a flag. They are only flags where fmt
// reads them, directly after the %: one written later is taken for whatever
// stands in that position instead, which for "%5-s" is the verb.
const fmtFlags = "#0+- "

func isFormatFlag(b byte) bool {
	return strings.IndexByte(fmtFlags, b) >= 0
}

// fmtVerbs is every verb Sprintf renders, which is the whole of what a catalog
// value can use: Tf formats through Sprintf, so a letter outside this set
// reaches the user as %!i(string=x) instead of the value. %w is not in it:
// only Errorf wraps an error, and Sprintf prints %!w(...) for one.
const fmtVerbs = "bcdefgopqstvxEFGOTUX"

func isFormatVerb(b byte) bool {
	return strings.IndexByte(fmtVerbs, b) >= 0
}

// i18nLocalName returns the name this file calls the i18n package by, or an
// empty string when it does not import it. Resolving it per file rather than
// assuming "i18n" means an aliased import cannot slip past the scan.
func i18nLocalName(file *ast.File) string {
	for _, imported := range file.Imports {
		if imported.Path.Value != i18nImportPath {
			continue
		}
		if imported.Name != nil {
			return imported.Name.Name
		}
		return "i18n"
	}
	return ""
}

type catalogCall struct {
	local   string
	fn      string
	key     string
	literal bool
	// args counts the arguments after the key, and spread says the call passes
	// them as a slice with ..., in which case args says nothing.
	args   int
	spread bool
	// runsAtInit names the package-initialization context the call sits in, and
	// is empty for a call that runs later.
	runsAtInit string
	pos        string
}

// callSitesUnderCLI parses every non-test file of the CLI and returns the T, Tf
// and NewError calls in them.
//
// It reads the source rather than the call graph, so two shapes are invisible to
// it: a package-level declaration whose initializer calls a helper that looks a
// key up in turn, and a call made through a copy of the function, as in
// t := i18n.T followed by t(key). Chasing those would mean real call-graph
// analysis, which is out of proportion to how likely they are; the gate covers
// the shapes anyone actually writes, not every shape that exists.
func callSitesUnderCLI(t *testing.T) []catalogCall {
	t.Helper()

	var sites []catalogCall
	scanned := 0

	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests legitimately look up keys that do not exist, and a key only a
		// test reaches is an orphan by any useful measure, so they stay out.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Errorf("%s: %v", path, parseErr)
			return nil
		}

		local := i18nLocalName(file)
		if local == "" {
			return nil
		}
		if local == "." || local == "_" {
			t.Errorf("%s: the i18n import is %s-qualified, which hides its call sites from this check", path, local)
			return nil
		}
		scanned++

		sites = append(sites, catalogCallSites(fset, file, local)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// A scan that reached nothing would pass every assertion its callers make
	// while checking nothing at all.
	if scanned == 0 {
		t.Fatalf("no file under %s imports the i18n package; the scan root is wrong", root)
	}
	return sites
}

// catalogCallSites collects every T, Tf and NewError call in file, flagging the
// ones that run while the package is initializing.
func catalogCallSites(fset *token.FileSet, file *ast.File, local string) []catalogCall {
	runsAtInit := map[*ast.CallExpr]string{}
	for _, decl := range file.Decls {
		switch declaration := decl.(type) {
		case *ast.GenDecl:
			// Only var: a const initializer has to be a constant expression, so
			// a call cannot appear in one and still compile.
			if declaration.Tok == token.VAR {
				markInitCalls(declaration, "a package-level declaration", runsAtInit)
			}
		case *ast.FuncDecl:
			// func init runs before main for the same reason a package-level
			// declaration does, so a lookup in one freezes just as early.
			if declaration.Recv == nil && declaration.Name.Name == "init" && declaration.Body != nil {
				markInitCalls(declaration.Body, "func init", runsAtInit)
			}
		}
	}

	var sites []catalogCall
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != local {
			return true
		}
		fn := selector.Sel.Name
		if fn != "T" && fn != "Tf" && fn != "NewError" {
			return true
		}

		site := catalogCall{
			local:      local,
			fn:         fn,
			args:       len(call.Args) - 1,
			spread:     call.Ellipsis.IsValid(),
			runsAtInit: runsAtInit[call],
			pos:        fset.Position(call.Pos()).String(),
		}
		if len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if key, err := strconv.Unquote(lit.Value); err == nil {
					site.key = key
					site.literal = true
				}
			}
		}
		sites = append(sites, site)
		return true
	})
	return sites
}

// markInitCalls records every call under node that runs while the package is
// initializing, labelling it with context. A function literal is skipped unless
// it is invoked where it is written, and parentheses around it are stripped
// first, since (func() string { ... })() is the shape an immediate invocation is
// usually written in and it runs just as early as the bare one. A literal stored
// in a variable and then called through that variable does run at initialization
// and is missed, the same call-graph blind spot callSitesUnderCLI has: chasing
// it is out of proportion to how likely anyone is to write it.
func markInitCalls(node ast.Node, context string, marked map[*ast.CallExpr]string) {
	invoked := map[*ast.FuncLit]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if literal, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
				invoked[literal] = true
			}
		}
		return true
	})

	ast.Inspect(node, func(n ast.Node) bool {
		switch value := n.(type) {
		case *ast.FuncLit:
			return invoked[value]
		case *ast.CallExpr:
			marked[value] = context
		}
		return true
	})
}
