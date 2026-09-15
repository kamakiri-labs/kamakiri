package domain

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

func registrationListFixture() *api.RegistrationList {
	return &api.RegistrationList{Registrations: []api.Registration{
		{Name: "example.com", Status: RegStatusVerified,
			Attachments: []api.RegistrationAttachment{{Host: "www.example.com", Role: "canonical"}}},
		{Name: "a-much-longer-domain.example", Status: RegStatusAwaiting},
		{Name: "third.example", Status: "some_future_status",
			Attachments: []api.RegistrationAttachment{
				{Host: "one.third.example", Role: "redirect", RedirectStatus: 301},
				{Host: "two.third.example", Role: "alias"},
			}},
	}}
}

func renderRegistrationList(t *testing.T) string {
	t.Helper()
	list := registrationListFixture()
	client := &mockClient{listRegistrationsFn: func() (*api.RegistrationList, error) { return list, nil }}
	var out bytes.Buffer
	if err := ListRegistrations(client, &out); err != nil {
		t.Fatalf("ListRegistrations() error = %v", err)
	}
	return out.String()
}

func renderRegisterInstructions() string {
	var out bytes.Buffer
	printRegisterInstructions(&out, &api.Registration{
		Name: "example.com",
		TXT: api.RegistrationTXT{
			Type:  "TXT",
			Name:  "_kamakiri-verify.example.com",
			Value: "kamakiri-site-verification=abc123",
		},
	})
	return out.String()
}

// Both registration tables are laid out by hand rather than through
// text/tabwriter, which counts a cell in runes and so reads a Japanese header or
// status as half the columns a terminal gives it. These goldens pin the geometry
// that layout keeps: each column is as wide as its widest cell plus two, and the
// last cell of a row is never padded.
func TestRegistrationTablesLayout(t *testing.T) {
	setupCredentials(t)

	wantList := strings.Join([]string{
		"DOMAIN                        STATUS                 SITES",
		"example.com                   verified               www.example.com (canonical)",
		"a-much-longer-domain.example  awaiting verification  -",
		"third.example                 some_future_status     one.third.example (redirect 301), two.third.example (alias)",
		"",
	}, "\n")
	if got := renderRegistrationList(t); got != wantList {
		t.Errorf("ListRegistrations() =\n%q\nwant\n%q", got, wantList)
	}

	wantInstructions := strings.Join([]string{
		"",
		"Publish this DNS record to prove ownership:",
		"  TYPE  NAME                          VALUE",
		"  TXT   _kamakiri-verify.example.com  kamakiri-site-verification=abc123",
		"  Add this value; keep any other `_kamakiri-verify` value already there.",
		"",
	}, "\n")
	if got := renderRegisterInstructions(); got != wantInstructions {
		t.Errorf("printRegisterInstructions() =\n%q\nwant\n%q", got, wantInstructions)
	}
}

// The same two tables with Japanese cells: the header row and the status column
// both become wide, and neither is last in its row.
func TestRegistrationTablesJapanese(t *testing.T) {
	setupCredentials(t)

	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	list := strings.Split(strings.TrimSuffix(renderRegistrationList(t), "\n"), "\n")
	statusAnchors := []string{
		i18n.T("domain.reg_col_status"),
		i18n.T("domain.reg_status_verified"),
		i18n.T("domain.reg_status_awaiting"),
		"some_future_status",
	}
	assertCellsStartTogether(t, "ListRegistrations status column", list, statusAnchors)
	siteCells := []string{
		i18n.T("domain.reg_col_sites"),
		"www.example.com (canonical)",
		"-",
		"one.third.example (redirect 301), two.third.example (alias)",
	}
	assertLastCellsStartTogether(t, "ListRegistrations sites column", list, siteCells)

	// A localized header proves the columns are lining up around what the reader
	// actually sees rather than around leftover ASCII.
	if !strings.HasPrefix(list[0], i18n.T("domain.reg_col_domain")) {
		t.Errorf("the header is not localized: %q", list[0])
	}

	instructions := strings.Split(strings.TrimSuffix(renderRegisterInstructions(), "\n"), "\n")[2:4]
	assertCellsStartTogether(t, "printRegisterInstructions name column", instructions,
		[]string{i18n.T("domain.reg_header_name"), "_kamakiri-verify.example.com"})
	assertLastCellsStartTogether(t, "printRegisterInstructions value column", instructions,
		[]string{i18n.T("domain.reg_header_value"), "kamakiri-site-verification=abc123"})
}

// The status column above is at its widest in ASCII, so it cannot tell a
// display-width column from a rune-counted one. This listing can: with only the
// two localized statuses in it, the widest cell in that column is Japanese, and
// a rune count reads it as half the terminal columns it takes.
func TestRegistrationListSizesColumnsByDisplayWidth(t *testing.T) {
	setupCredentials(t)

	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	client := &mockClient{listRegistrationsFn: func() (*api.RegistrationList, error) {
		return &api.RegistrationList{Registrations: []api.Registration{
			{Name: "a.jp", Status: RegStatusVerified},
			{Name: "bb.jp", Status: RegStatusAwaiting},
		}}, nil
	}}
	var out bytes.Buffer
	if err := ListRegistrations(client, &out); err != nil {
		t.Fatalf("ListRegistrations() error = %v", err)
	}
	rows := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	assertLastCellsStartTogether(t, "narrow listing", rows, []string{
		i18n.T("domain.reg_col_sites"), "-", "-",
	})
}

// No row of either table ends in whitespace, since the last cell is never
// padded.
func TestRegistrationTablesLeaveTheLastCellBare(t *testing.T) {
	setupCredentials(t)

	t.Cleanup(func() { i18n.Load(testLang) })
	for _, lang := range []string{"en", "ja"} {
		i18n.Load(lang)
		for _, line := range strings.Split(strings.TrimSuffix(renderRegistrationList(t), "\n"), "\n") {
			if strings.HasSuffix(line, " ") {
				t.Errorf("%s: line ends in whitespace: %q", lang, line)
			}
		}
	}
	i18n.Load(testLang)
}

// The wrong-target frame sizes its label column from the labels themselves, so a
// longer Japanese label widens the column instead of colliding with its value.
// Both renderings are pinned literally: the indent, the gap and the padding are
// all visible in them, and Japanese padded by rune count would put 実際の値: six
// spaces from its value rather than two.
func TestWrongTargetLabelColumn(t *testing.T) {
	t.Cleanup(func() { i18n.Load(testLang) })
	d := &api.Domain{Domain: "a.example.com", DnsVerdict: "present_but_wrong",
		DNSRecordsExpected: []api.DNSRecord{{Name: "a.example.com", Type: "cname", Purpose: "primary", Value: "h1.kamakiri-pages.site."}},
		DNSRecordsObserved: []api.DNSRecordObserved{{Name: "a.example.com", Type: "cname", Values: []string{"old.host.example."}}},
	}
	want := map[string][2]string{
		"en": {"      expected:  h1.kamakiri-pages.site.", "      got:       old.host.example."},
		"ja": {"      期待値:    h1.kamakiri-pages.site.", "      実際の値:  old.host.example."},
	}

	for _, lang := range []string{"en", "ja"} {
		i18n.Load(lang)
		lines := transientStatus(d, phaseWrong, "under a minute", false, false)
		if lines[1] != want[lang][0] || lines[2] != want[lang][1] {
			t.Errorf("%s: expected/got pair =\n%q\n%q\nwant\n%q\n%q",
				lang, lines[1], lines[2], want[lang][0], want[lang][1])
		}
		// The two values start at one terminal column, whatever bytes it took
		// each label to get there.
		first := i18n.Width(lines[1][:strings.Index(lines[1], "h1.kamakiri-pages.site.")])
		second := i18n.Width(lines[2][:strings.Index(lines[2], "old.host.example.")])
		if first != second {
			t.Errorf("%s: values start at columns %d and %d", lang, first, second)
		}
	}
	i18n.Load(testLang)
}

// assertLastCellsStartTogether checks that each row starts its final cell at the
// same terminal column. That cell is never padded, so it is matched as a suffix
// rather than searched for.
func assertLastCellsStartTogether(t *testing.T, what string, rows, cells []string) {
	t.Helper()
	if len(rows) != len(cells) {
		t.Fatalf("%s: %d rows against %d cells: %q", what, len(rows), len(cells), rows)
	}
	want := -1
	for i, cell := range cells {
		if !strings.HasSuffix(rows[i], cell) {
			t.Fatalf("%s: row %d = %q, want it to end in %q", what, i, rows[i], cell)
		}
		got := i18n.Width(rows[i][:len(rows[i])-len(cell)])
		if want < 0 {
			want = got
		}
		if got != want {
			t.Errorf("%s: row %d starts its last cell at column %d, want %d: %q", what, i, got, want, rows[i])
		}
	}
}

// assertCellsStartTogether checks that each row starts the named cell at the
// same terminal column.
func assertCellsStartTogether(t *testing.T, what string, rows, anchors []string) {
	t.Helper()
	if len(rows) != len(anchors) {
		t.Fatalf("%s: %d rows against %d anchors: %q", what, len(rows), len(anchors), rows)
	}
	want := -1
	for i, anchor := range anchors {
		idx := strings.Index(rows[i], anchor)
		if idx < 0 {
			t.Fatalf("%s: row %d = %q, want it to carry %q", what, i, rows[i], anchor)
		}
		got := i18n.Width(rows[i][:idx])
		if want < 0 {
			want = got
		}
		if got != want {
			t.Errorf("%s: row %d starts at column %d, want %d: %q", what, i, got, want, rows[i])
		}
	}
}
