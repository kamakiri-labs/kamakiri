package cdn

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

func statusTableFixture() *api.CDNStatusResponse {
	dcv := []api.DNSRecord{
		{Name: "_acme.a.example.com", Type: "txt", Purpose: "validation", Value: "cf-validation-1"},
		{Name: "_acme2.a.example.com", Type: "cname", Purpose: "validation", Value: "cf-validation-2.dcv.cloudflare.com"},
	}
	return &api.CDNStatusResponse{CDNMode: "cloudflare", Domains: []api.CDNDomainStatus{
		{Domain: "a.example.com", CDNID: "cf-1", CdnState: CdnStateActive},
		{Domain: "much-longer-name.example.com", CdnState: CdnStateAwaitingCFValidation, DNSRecordsExpected: dcv},
		{Domain: "c.example.com", CDNID: "cf-333333", CdnState: CdnStateActive},
		{Domain: "d.example.com"},
		{Domain: "e.example.com", CDNID: "cf-4", CdnState: CdnStateActive},
	}}
}

func renderStatusTable(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site1")

	res := statusTableFixture()
	client := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) { return res, nil }}
	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	return out.String()
}

// The status table is laid out by hand rather than through text/tabwriter, so
// this golden pins the geometry that layout has to keep: a column is as wide as
// the widest cell of the block it belongs to plus two, a row carrying no
// tab-terminated cell in a column ends that column's block, and a row's last
// cell is never padded. The fixture is shaped to exercise all four of the loop's
// row shapes plus an interleaved validation sub-block.
func TestStatusTableLayout(t *testing.T) {
	want := strings.Join([]string{
		"CDN:  cloudflare",
		"  a.example.com                 cf-1  ✓ via Cloudflare",
		"  much-longer-name.example.com  ⧗ awaiting CF validation",
		"    add TXT                     _acme.a.example.com → cf-validation-1",
		"    add CNAME                   _acme2.a.example.com → cf-validation-2.dcv.cloudflare.com",
		"  c.example.com                 cf-333333  ✓ via Cloudflare",
		"  d.example.com",
		"  e.example.com  cf-4  ✓ via Cloudflare",
		"",
	}, "\n")

	if got := renderStatusTable(t); got != want {
		t.Errorf("Status() table =\n%q\nwant\n%q", got, want)
	}
}

// The same table with Japanese cells. The middle column carries a localized CDN
// status that is not last in its row, which is exactly what text/tabwriter's
// rune counting reads as half its width, so every column has to be measured in
// terminal columns instead.
func TestStatusTableJapanese(t *testing.T) {
	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	output := renderStatusTable(t)
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")[1:] // drop the CDN: header

	// Every row of the first block starts its second cell at one column: the
	// four domain rows and the two validation rows share it.
	anchors := []string{"cf-1", i18n.T("cdn.status_cf_validation"), "_acme.a.example.com",
		"_acme2.a.example.com", "cf-333333"}
	want := assertCellsStartTogetherAt(t, lines, anchors)

	// The one-cell row ends the block, so the row after it measures its own
	// column and starts earlier: block-scoped columns keep one wide cell from
	// inflating rows it shares no block with.
	last := lines[len(lines)-1]
	if idx := strings.Index(last, "cf-4"); idx < 0 || i18n.Width(last[:idx]) >= want {
		t.Errorf("the row after the one-cell row should start a fresh column block: %q", last)
	}

	// The localized status has to be the Japanese one, or the columns line up
	// around a value the reader never sees.
	if !strings.Contains(output, i18n.Tf("cdn.status_live_via", "Cloudflare")) ||
		!strings.Contains(output, i18n.T("cdn.status_cf_validation")) {
		t.Errorf("missing the localized CDN status: %q", output)
	}
}

// The table above is at its widest in ASCII in every padded column, so it cannot
// tell a display-width column from a rune-counted one. This one can: with short
// domain names, the widest cell in the first column is the validation block's
// localized instruction, which a rune count reads as three columns short.
func TestStatusTableSizesColumnsByDisplayWidth(t *testing.T) {
	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site1")

	res := &api.CDNStatusResponse{CDNMode: "cloudflare", Domains: []api.CDNDomainStatus{
		{Domain: "a.jp", CDNID: "cf-1", CdnState: CdnStateActive},
		{Domain: "b.jp", CdnState: CdnStateAwaitingCFValidation, DNSRecordsExpected: []api.DNSRecord{
			{Name: "_acme.b.jp", Type: "cname", Purpose: "validation", Value: "cf-validation-1"},
		}},
	}}
	client := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) { return res, nil }}
	var out bytes.Buffer
	if err := Status(client, &out); err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	rows := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")[1:]

	anchors := []string{"cf-1", i18n.T("cdn.status_cf_validation"), "_acme.b.jp"}
	assertCellsStartTogetherAt(t, rows, anchors)
}

// The table is the answer the command was asked for, so a write that fails
// part way through has to reach the exit code rather than be swallowed into a
// silent success.
func TestStatusReportsAWriteFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site1")

	res := statusTableFixture()
	client := &mockClient{cdnStatusFn: func(string) (*api.CDNStatusResponse, error) { return res, nil }}
	broken := errors.New("stdout closed")
	err := Status(client, writerFunc(func(p []byte) (int, error) {
		if strings.HasPrefix(string(p), "  a.example.com") {
			return 0, broken
		}
		return len(p), nil
	}))
	if !errors.Is(err, broken) {
		t.Errorf("Status() error = %v, want the writer's own error", err)
	}
}

// writerFunc adapts a function to io.Writer, so a test can fail one write.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// alignRows has to leave the last cell of a row unpadded, so a line ends in
// whitespace only where that cell is itself empty.
func TestAlignRowsLeavesTheLastCellBare(t *testing.T) {
	lines := alignRows([][]string{
		{"aaa", "b"},
		{"a", "bbbb"},
		{"a"},
	})
	for i, line := range lines {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line %d ends in whitespace: %q", i, line)
		}
	}
	if lines[0] != "aaa  b" || lines[1] != "a    bbbb" || lines[2] != "a" {
		t.Errorf("alignRows = %q", lines)
	}
}

// The blocks below keep text/tabwriter, which is only safe while no cell that is
// not last in its row changes width with the language. These four write nothing
// but a DNS name, a record type and a value, so the record rows come out with
// the same bytes in either language; a localized cell slipping into one of them
// would break that at once.
func TestGuidedRecordBlocksCarryNoLocalizedCell(t *testing.T) {
	own := []api.DNSRecord{
		{Name: "_webaccel.a.example.com", Type: "txt", Purpose: "ownership", Value: "webaccel=abc"},
		{Name: "_webaccel.much-longer.example.com", Type: "cname", Purpose: "ownership", Value: "webaccel=def"},
	}
	prim := []api.DNSRecord{
		{Name: "a.example.com", Type: "cname", Purpose: "primary", Value: "h1.kamakiri-pages.site."},
		{Name: "much-longer-name.example.com", Type: "alias", Purpose: "primary", Value: "h2.kamakiri-pages.site."},
	}
	snap := cdnSnapshot{
		delivery: []deliveryTarget{
			{domain: "a.example.com", subdomain: "abc.user.webaccel.jp"},
			{domain: "much-longer-name.example.com", subdomain: "def.user.webaccel.jp", apex: true},
		},
		mismatch: own,
	}
	blocks := map[string]func() []string{
		"deliveryGuidanceBlock": func() []string { return deliveryGuidanceBlock(prim) },
		"ownershipBlock":        func() []string { return ownershipBlock(own) },
		"deliveryBlock":         func() []string { return deliveryBlock(snap) },
		"webaccelTerminalBlock": func() []string {
			var b bytes.Buffer
			printWebAccelTerminalBlock(&b, cdnSnapshot{
				cliTerminal: []string{"a.example.com: webaccel_txt_mismatch"}, mismatch: own})
			return strings.Split(b.String(), "\n")
		},
	}

	t.Cleanup(func() { i18n.Load(testLang) })
	for name, render := range blocks {
		t.Run(name, func(t *testing.T) {
			english := recordRows(render())
			if len(english) < 2 {
				t.Fatalf("%s rendered %d record rows, want at least 2", name, len(english))
			}

			i18n.Load("ja")
			japanese := recordRows(render())
			i18n.Load(testLang)

			if strings.Join(english, "\n") != strings.Join(japanese, "\n") {
				t.Errorf("%s record rows differ between languages, so a cell in them is localized:\nen %q\nja %q",
					name, english, japanese)
			}
			assertColumnsAligned(t, english)
		})
	}
}

// The Cloudflare validation block is the one table that keeps text/tabwriter
// while carrying a localized first cell. It is safe because every row of it
// comes from one renderer, so the Japanese half of that cell is the same phrase
// on each row and only the ASCII record type varies: the gap between a cell's
// rune count and its display width is then the same everywhere, and tabwriter's
// rune-counted padding still lands the next column in one place.
func TestValidationBlockKeepsTabwriterUnderJapanese(t *testing.T) {
	records := []api.DNSRecord{
		{Name: "_acme.a.example.com", Type: "txt", Purpose: "validation", Value: "cf-validation-1"},
		{Name: "_acme2.a.example.com", Type: "cname", Purpose: "validation", Value: "cf-validation-2"},
		{Name: "_acme3.a.example.com", Type: "a", Purpose: "validation", Value: "203.0.113.1"},
	}

	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	lines := dcvBlock(records)
	rows := recordRows(lines)
	if len(rows) != len(records) {
		t.Fatalf("dcvBlock rendered %d record rows, want %d: %q", len(rows), len(records), lines)
	}

	// The invariant that makes tabwriter's rune count stand in for the display
	// width here: every first cell is the same distance from one to the other.
	cells := make([]string, len(records))
	for i, r := range records {
		cells[i] = i18n.Tf("dns.validation_add", strings.ToUpper(r.Type))
	}
	if !skewIsUniform(cells) {
		t.Errorf("the first cells %q no longer share one skew between display width and rune count; the block can no longer keep tabwriter", cells)
	}

	assertColumnsAligned(t, rows)
	if !strings.Contains(rows[0], i18n.Tf("dns.validation_add", "TXT")) {
		t.Errorf("the first cell is not the localized one: %q", rows[0])
	}
}

// skewIsUniform reports whether every cell is the same number of terminal
// columns wider than its rune count. That is the condition under which
// tabwriter's rune-counted padding still lands the next column in one place,
// so a column of such cells may sit in a non-last position.
func skewIsUniform(cells []string) bool {
	if len(cells) == 0 {
		return true
	}
	skew := func(s string) int { return i18n.Width(s) - utf8.RuneCountInString(s) }
	for _, cell := range cells {
		if skew(cell) != skew(cells[0]) {
			return false
		}
	}
	return true
}

// skewIsUniform is not vacuous: cells carrying different numbers of wide
// characters have skews that differ, and it has to refuse them.
func TestUniformSkewCheckBitesOnMixedWidthCells(t *testing.T) {
	if skewIsUniform([]string{"あ TXT", "ああああ CNAME"}) {
		t.Error("cells with one and four wide characters read as one skew; the check cannot bite")
	}
	if !skewIsUniform([]string{"あ TXT", "あ CNAME"}) {
		t.Error("cells with the same wide characters must read as one skew")
	}
}

// assertColumnsAligned is not vacuous either: a column whose cells differ in how
// many wide characters they carry does come out ragged once tabwriter pads it by
// rune count, which is what keeps every other table off tabwriter. The two rows
// below are what it would emit for a one-rune and a four-rune cell.
func TestColumnAlignmentCheckBitesOnAMixedWidthColumn(t *testing.T) {
	starts := secondColumnStarts([]string{
		"あ     value-one",
		"ああああ  value-two",
	})
	if starts[0] == starts[1] {
		t.Errorf("a mixed-width column reads as aligned at %d; the check cannot bite", starts[0])
	}
}

// recordRows returns the lines of a guided block that carry a rendered DNS
// record, which are the only ones a column runs through.
func recordRows(lines []string) []string {
	var rows []string
	for _, line := range lines {
		if strings.HasPrefix(line, "    ") && strings.TrimSpace(line) != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// secondColumnStarts returns, per row, the terminal column its second cell
// begins at, taking the run of two or more spaces after the first cell as the
// gap. A row with no such run reports -1.
func secondColumnStarts(rows []string) []int {
	starts := make([]int, len(rows))
	for i, row := range rows {
		trimmed := strings.TrimLeft(row, " ")
		indent := i18n.Width(row) - i18n.Width(trimmed)
		gap := strings.Index(trimmed, "  ")
		if gap < 0 {
			starts[i] = -1
			continue
		}
		rest := strings.TrimLeft(trimmed[gap:], " ")
		starts[i] = indent + i18n.Width(trimmed[:len(trimmed)-len(rest)])
	}
	return starts
}

// assertCellsStartTogetherAt checks that each row starts the named cell at the
// same terminal column, and returns that column so a caller can compare a row
// outside the block against it.
func assertCellsStartTogetherAt(t *testing.T, rows, anchors []string) int {
	t.Helper()
	want := -1
	for i, anchor := range anchors {
		idx := strings.Index(rows[i], anchor)
		if idx < 0 {
			t.Fatalf("row %d = %q, want it to carry %q", i, rows[i], anchor)
		}
		got := i18n.Width(rows[i][:idx])
		if want < 0 {
			want = got
		}
		if got != want {
			t.Errorf("row %d starts its second cell at column %d, want %d: %q", i, got, want, rows[i])
		}
	}
	return want
}

func assertColumnsAligned(t *testing.T, rows []string) {
	t.Helper()
	starts := secondColumnStarts(rows)
	for i, got := range starts {
		if got < 0 {
			t.Errorf("row %d has no column gap: %q", i, rows[i])
			continue
		}
		if got != starts[0] {
			t.Errorf("row %d starts its second column at %d, want %d: %q", i, got, starts[0], rows[i])
		}
	}
}
