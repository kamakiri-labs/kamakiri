package deploys

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// testLang is the catalog every test here renders against. A test that switches
// away restores this rather than a literal of its own, so the pin moves in one
// place.
const testLang = "en"

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under: the API error copy they match on renders from the
// message catalog. Load rather than Setup: nothing here reports which language
// is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load(testLang)
	os.Exit(m.Run())
}

type mockClient struct {
	listDeploysFn func(siteID string) ([]api.Deploy, error)
}

func (m *mockClient) ListDeploys(siteID string) ([]api.Deploy, error) {
	return m.listDeploysFn(siteID)
}

func setupCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}

func setupProject(t *testing.T) {
	t.Helper()
	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
		t.Fatal(err)
	}
}

func TestDeploysHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		listDeploysFn: func(siteID string) ([]api.Deploy, error) {
			if siteID != "site123" {
				t.Errorf("siteID = %q, want site123", siteID)
			}
			return []api.Deploy{
				{ID: "20260401-120000", Status: "live", CreatedAt: "2026-04-01T12:00:00Z"},
				{ID: "20260401-100000", Status: "", CreatedAt: "2026-04-01T10:00:00Z"},
				{ID: "20260331-180000", Status: "", CreatedAt: "2026-03-31T18:00:00Z"},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := out.String()

	if !strings.Contains(output, "ID") || !strings.Contains(output, "Status") || !strings.Contains(output, "Created") {
		t.Errorf("missing table header: %q", output)
	}

	if !strings.Contains(output, "20260401-120000") {
		t.Errorf("missing deploy ID: %q", output)
	}

	// Only "live" surfaces in the Status column; every other server status
	// (superseded, reverted, rolled_back) renders as an empty cell.
	if !strings.Contains(output, "live") {
		t.Errorf("missing 'live' status: %q", output)
	}
	if strings.Contains(output, "superseded") || strings.Contains(output, "reverted") || strings.Contains(output, "rolled_back") {
		t.Errorf("non-live deploys should display as empty: %q", output)
	}

	if !strings.Contains(output, "2026-04-01 12:00:00 UTC") {
		t.Errorf("missing formatted date: %q", output)
	}
}

// tableFixture is deliberately awkward: a status cell that renders empty, and an
// ID wider than every other cell in its column, so a column width taken from the
// header alone or a padded last cell shows up as a byte difference.
func tableFixture() []api.Deploy {
	return []api.Deploy{
		{ID: "20260401-120000", Status: "live", CreatedAt: "2026-04-01T12:00:00Z"},
		{ID: "20260401-100000", Status: "superseded", CreatedAt: "2026-04-01T10:00:00Z"},
		{ID: "20260331-180000-rebuild", Status: "rolled_back", CreatedAt: "2026-03-31T18:00:00Z"},
	}
}

func runTable(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		listDeploysFn: func(_ string) ([]api.Deploy, error) { return tableFixture(), nil },
	}
	var out bytes.Buffer
	if err := Run(client, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return out.String()
}

// The English listing is a published layout, so it is pinned byte for byte:
// every column is as wide as its widest cell, two spaces separate two columns,
// and the last cell on a line is never padded.
func TestDeploysTableLayout(t *testing.T) {
	want := "ID                       Status  Created\n" +
		"20260401-120000          live    2026-04-01 12:00:00 UTC\n" +
		"20260401-100000                  2026-04-01 10:00:00 UTC\n" +
		"20260331-180000-rebuild          2026-03-31 18:00:00 UTC\n"

	if got := runTable(t); got != want {
		t.Errorf("Run() table =\n%q\nwant\n%q", got, want)
	}
}

// Japanese headers are twice as wide per rune as the ASCII ones, so the columns
// hold only if the widths are counted in terminal columns. Every line's run up to
// the last column has to measure the same, whatever bytes it took to get there.
func TestDeploysTableLayoutJapanese(t *testing.T) {
	i18n.Load("ja")
	t.Cleanup(func() { i18n.Load(testLang) })

	output := runTable(t)
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	lastCells := []string{
		i18n.T("deploys.header_created"),
		"2026-04-01 12:00:00 UTC",
		"2026-04-01 10:00:00 UTC",
		"2026-03-31 18:00:00 UTC",
	}
	if len(lines) != len(lastCells) {
		t.Fatalf("Run() rendered %d lines, want %d: %q", len(lines), len(lastCells), output)
	}

	var prefixWidth int
	for i, line := range lines {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line %d ends in whitespace: %q", i, line)
		}
		if !strings.HasSuffix(line, lastCells[i]) {
			t.Fatalf("line %d = %q, want it to end in %q", i, line, lastCells[i])
		}
		got := i18n.Width(strings.TrimSuffix(line, lastCells[i]))
		if i == 0 {
			prefixWidth = got
		}
		if got != prefixWidth {
			t.Errorf("line %d starts its last column at width %d, want %d: %q", i, got, prefixWidth, line)
		}
	}

	// The live cell has to be the Japanese one, or the columns line up around a
	// value the reader never sees.
	if !strings.Contains(output, i18n.T("deploys.status_live")) {
		t.Errorf("missing the localized live status: %q", output)
	}
}

func TestDeploysEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		listDeploysFn: func(_ string) ([]api.Deploy, error) {
			return []api.Deploy{}, nil
		},
	}

	var out bytes.Buffer
	if err := Run(client, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !strings.Contains(out.String(), "No deploys yet.") {
		t.Errorf("expected 'No deploys yet.', got: %q", out.String())
	}
}

func TestDeploysNotLoggedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	client := &mockClient{}
	var out bytes.Buffer
	err := Run(client, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("expected 'not logged in', got: %q", err.Error())
	}
}

func TestDeploysNoConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{}
	var out bytes.Buffer
	err := Run(client, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no site linked") {
		t.Errorf("expected 'no site linked', got: %q", err.Error())
	}
}

func TestDeploysSiteNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		listDeploysFn: func(_ string) ([]api.Deploy, error) {
			return nil, &api.ErrorResponse{Code: "site_not_found", Message: "Site not found."}
		},
	}

	var out bytes.Buffer
	err := Run(client, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "site not found") {
		t.Errorf("expected mapped error, got: %q", err.Error())
	}
}

func TestDeploysForbidden(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		listDeploysFn: func(_ string) ([]api.Deploy, error) {
			return nil, &api.ErrorResponse{Code: "forbidden", Message: "Not your site."}
		},
	}

	var out bytes.Buffer
	err := Run(client, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "don't own") {
		t.Errorf("expected mapped error, got: %q", err.Error())
	}
}

func TestDeploysUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProject(t)

	client := &mockClient{
		listDeploysFn: func(_ string) ([]api.Deploy, error) {
			return nil, &api.ErrorResponse{Code: "unauthorized", Message: "Invalid API key."}
		},
	}

	var out bytes.Buffer
	err := Run(client, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("expected mapped error, got: %q", err.Error())
	}
}

func TestDisplayStatus(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"live", "live"},
		{"rolled_back", ""},
		{"superseded", ""},
		{"unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := displayStatus(tt.input)
			if got != tt.want {
				t.Errorf("displayStatus(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatCreatedAt(t *testing.T) {
	got := formatCreatedAt("2026-03-01T14:30:22Z")
	want := "2026-03-01 14:30:22 UTC"
	if got != want {
		t.Errorf("formatCreatedAt() = %q, want %q", got, want)
	}
}
