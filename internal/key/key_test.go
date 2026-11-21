package key

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// testLang is the catalog every test here renders against. A test that switches
// away restores this rather than a literal of its own, so the pin moves in one
// place.
const testLang = "en"

// The catalog is pinned so the assertions below hold whatever locale the suite
// runs under. Load rather than Setup: nothing here reports which language is in
// force, only renders in it. The test that checks the Japanese table swaps the
// catalog itself and swaps it back; none of them is parallel, since the catalog
// is an unsynchronized map.
//
// KAMAKIRI_API_KEY is cleared for a reason of its own: it outranks the
// credentials file, so on a machine that exports it a test that writes a key
// into its own config home would still load the developer's, and a test that
// states there is no credential would find one. There is no *testing.T here,
// hence os.Unsetenv rather than t.Setenv.
func TestMain(m *testing.M) {
	i18n.Load(testLang)
	os.Unsetenv(core.EnvAPIKey)
	os.Exit(m.Run())
}

// fakeClient answers the key calls from fixed values and records each call, so
// a test can hold both what was printed and what was sent.
type fakeClient struct {
	keys      []api.Key
	listErr   error
	renamed   *api.Key
	renameErr error
	revoked   *api.KeyInfo
	revokeErr error
	created   *api.CreatedKey
	createErr error
	rotated   *api.RotatedKey
	rotateErr error

	calls []string
}

func (f *fakeClient) ListKeys() ([]api.Key, error) {
	f.calls = append(f.calls, "ListKeys")
	return f.keys, f.listErr
}

func (f *fakeClient) RenameKey(id, name string) (*api.Key, error) {
	f.calls = append(f.calls, fmt.Sprintf("RenameKey %s %q", id, name))
	return f.renamed, f.renameErr
}

func (f *fakeClient) RevokeKey(id string) (*api.KeyInfo, error) {
	f.calls = append(f.calls, "RevokeKey "+id)
	return f.revoked, f.revokeErr
}

func (f *fakeClient) CreateKey(name string, sites []string) (*api.CreatedKey, error) {
	f.calls = append(f.calls, fmt.Sprintf("CreateKey %q %q", name, sites))
	return f.created, f.createErr
}

func (f *fakeClient) RotateKey(id string) (*api.RotatedKey, error) {
	f.calls = append(f.calls, "RotateKey "+id)
	return f.rotated, f.rotateErr
}

func assertCalls(t *testing.T, f *fakeClient, want ...string) {
	t.Helper()
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

func loggedIn(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}

func useLanguage(t *testing.T, lang string) {
	t.Helper()
	i18n.Load(lang)
	t.Cleanup(func() { i18n.Load(testLang) })
}

const (
	unnamedID = "0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
	laptopID  = "5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65"
	ciID      = "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c"
)

// now is the moment every age here is measured from.
var now = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

func stamp(t time.Time) *string {
	s := t.Format(time.RFC3339)
	return &s
}

// accountKeys is the account of the listing the owner chose: an old key last
// used two months ago, the key this command runs with, and one never used.
func accountKeys(laptopName string) []api.Key {
	return []api.Key{
		{ID: unnamedID, Name: "unnamed", Kind: "login", ResourceIDs: []string{}, CreatedAt: "2026-09-02T08:14:37Z", LastUsedAt: stamp(now.Add(-67 * 24 * time.Hour))},
		{ID: laptopID, Name: laptopName, Kind: "login", ResourceIDs: []string{}, CreatedAt: "2026-11-12T10:30:00.123456Z", LastUsedAt: stamp(now.Add(-10 * time.Second)), Current: true},
		{ID: ciID, Name: "github-ci", Kind: "login", ResourceIDs: []string{}, CreatedAt: "2026-11-13T02:05:59Z"},
	}
}

func TestListRendersTheTableByteForByte(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{keys: accountKeys("alexis-laptop")}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := "" +
		"  ID                                    Name           Kind   Sites  Created           Last used\n" +
		"  0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40  unnamed        login  all    2026-09-02 08:14  2 months ago\n" +
		"* 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65  alexis-laptop  login  all    2026-11-12 10:30  just now\n" +
		"  c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c  github-ci      login  all    2026-11-13 02:05  never\n" +
		"\n" +
		"* the key this command ran with\n"
	if got := out.String(); got != want {
		t.Errorf("List() printed:\n%s\nwant:\n%s", got, want)
	}
	assertCalls(t, client, "ListKeys")
}

// The Japanese headers, the kind, the age and the name are wide, so a column
// padded by rune count rather than by display width would come out ragged. The
// widest name is the wide one, so its column is sized by it, and the kind
// column is sized by ログイン rather than by its header.
func TestListAlignsTheJapaneseTableByDisplayWidth(t *testing.T) {
	loggedIn(t)
	useLanguage(t, "ja")
	client := &fakeClient{keys: accountKeys("ビルド機-東京")}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := "" +
		"  キーID                                名前           種類      サイト  作成日時          最終使用\n" +
		"  0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40  unnamed        ログイン  すべて  2026-09-02 08:14  2か月前\n" +
		"* 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65  ビルド機-東京  ログイン  すべて  2026-11-12 10:30  たった今\n" +
		"  c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c  github-ci      ログイン  すべて  2026-11-13 02:05  未使用\n" +
		"\n" +
		"* このコマンドが使用したAPIキー\n"
	if got := out.String(); got != want {
		t.Errorf("List() printed:\n%s\nwant:\n%s", got, want)
	}
}

// The legend explains the marker, so a listing with no marked row has none. The
// server marks the key the request came with, so this is a server that did not.
func TestListPrintsNoLegendWhenNoRowIsMarked(t *testing.T) {
	loggedIn(t)
	keys := accountKeys("alexis-laptop")
	keys[1].Current = false
	client := &fakeClient{keys: keys[1:2]}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := "" +
		"  ID                                    Name           Kind   Sites  Created           Last used\n" +
		"  5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65  alexis-laptop  login  all    2026-11-12 10:30  just now\n"
	if got := out.String(); got != want {
		t.Errorf("List() printed:\n%s\nwant:\n%s", got, want)
	}
}

// deployKeys is an account holding the login key this command runs with and
// two deploy keys: one bound to a canonical domain, and one bound to that
// domain, a full subdomain host, and the bare ID of a site that is no longer
// active.
func deployKeys() []api.Key {
	return []api.Key{{
		ID:         laptopID,
		Name:       "alexis-laptop",
		Kind:       "login",
		CreatedAt:  "2026-11-12T10:30:00Z",
		LastUsedAt: stamp(now.Add(-10 * time.Second)),
		Current:    true,
		// A login key shows all whatever the server sent with it.
		ResourceIDs: []string{"k3j9x2m4p7q1w8e5"},
		Sites:       []api.LabeledSite{{ID: "k3j9x2m4p7q1w8e5", Label: "example.com"}},
	}, {
		ID:          ciID,
		Name:        "github-actions",
		Kind:        "deploy",
		ResourceIDs: []string{"k3j9x2m4p7q1w8e5"},
		Sites:       []api.LabeledSite{{ID: "k3j9x2m4p7q1w8e5", Label: "example.com"}},
		CreatedAt:   "2026-11-13T02:05:59Z",
	}, {
		ID:          unnamedID,
		Name:        "docs-ci",
		Kind:        "deploy",
		ResourceIDs: []string{"k3j9x2m4p7q1w8e5", "a1b2c3d4e5f6g7h8", "m4n5p6q7r8s9t0u1"},
		Sites: []api.LabeledSite{
			{ID: "k3j9x2m4p7q1w8e5", Label: "example.com"},
			{ID: "a1b2c3d4e5f6g7h8", Label: "docs.kamakiri-pages.jp"},
			{ID: "m4n5p6q7r8s9t0u1", Label: "m4n5p6q7r8s9t0u1"},
		},
		CreatedAt: "2026-11-14T08:00:00Z",
	}}
}

// A deploy key's Sites cell names its sites as the server labels them, in the
// server's order. A login key's cell still reads all, and the column is as wide
// as the longest list.
func TestListShowsADeployKeysSitesByLabel(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{keys: deployKeys()}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := "" +
		"  ID                                    Name            Kind    Sites                                                  Created           Last used\n" +
		"* 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65  alexis-laptop   login   all                                                    2026-11-12 10:30  just now\n" +
		"  c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c  github-actions  deploy  example.com                                            2026-11-13 02:05  never\n" +
		"  0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40  docs-ci         deploy  example.com, docs.kamakiri-pages.jp, m4n5p6q7r8s9t0u1  2026-11-14 08:00  never\n" +
		"\n" +
		"* the key this command ran with\n"
	if got := out.String(); got != want {
		t.Errorf("List() printed:\n%s\nwant:\n%s", got, want)
	}
}

// In Japanese both kinds are worded, ログイン and デプロイ, each eight columns
// wide, so the 種類 column is sized by them rather than by its header.
func TestListWordsBothKindsInJapanese(t *testing.T) {
	loggedIn(t)
	useLanguage(t, "ja")
	client := &fakeClient{keys: deployKeys()}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := "" +
		"  キーID                                名前            種類      サイト                                                 作成日時          最終使用\n" +
		"* 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65  alexis-laptop   ログイン  すべて                                                 2026-11-12 10:30  たった今\n" +
		"  c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c  github-actions  デプロイ  example.com                                            2026-11-13 02:05  未使用\n" +
		"  0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40  docs-ci         デプロイ  example.com、docs.kamakiri-pages.jp、m4n5p6q7r8s9t0u1  2026-11-14 08:00  未使用\n" +
		"\n" +
		"* このコマンドが使用したAPIキー\n"
	if got := out.String(); got != want {
		t.Errorf("List() printed:\n%s\nwant:\n%s", got, want)
	}
}

// A deploy key whose server sent no usable label is listed by the resource IDs
// it names, escaped and joined as labels are: when the sites are absent, empty,
// or every entry has neither label nor ID, and in the language's separator.
func TestListFallsBackToResourceIDsWhenNoLabelIsUsable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lang  string
		sites []api.LabeledSite
		ids   []string
		want  string
	}{
		{"absent", "en", nil, []string{"k3j9x2m4p7q1w8e5", "a1b2c3d4e5f6g7h8"},
			"  deploy  k3j9x2m4p7q1w8e5, a1b2c3d4e5f6g7h8  2026-11-13 02:05"},
		{"empty", "en", []api.LabeledSite{}, []string{"k3j9x2m4p7q1w8e5"},
			"  deploy  k3j9x2m4p7q1w8e5  2026-11-13 02:05"},
		{"all unusable", "en", []api.LabeledSite{{}, {}}, []string{"k3j9x2m4p7q1w8e5"},
			"  deploy  k3j9x2m4p7q1w8e5  2026-11-13 02:05"},
		{"escaped", "en", nil, []string{"k3j9\u202ex2m4"},
			`  deploy  k3j9\u{202e}x2m4  2026-11-13 02:05`},
		{"japanese separator", "ja", nil, []string{"k3j9x2m4p7q1w8e5", "a1b2c3d4e5f6g7h8"},
			"  デプロイ  k3j9x2m4p7q1w8e5、a1b2c3d4e5f6g7h8  2026-11-13 02:05"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			useLanguage(t, tc.lang)
			client := &fakeClient{keys: []api.Key{{
				ID: ciID, Name: "ci", Kind: "deploy", ResourceIDs: tc.ids, Sites: tc.sites,
				CreatedAt: "2026-11-13T02:05:59Z",
			}}}

			var out bytes.Buffer
			if err := List(client, &out, now); err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if got := out.String(); !strings.Contains(got, tc.want) {
				t.Errorf("List() printed:\n%s\nwant a row holding %q", got, tc.want)
			}
		})
	}
}

// A site label comes from the server and lands in a terminal, so it goes
// through the escaping a key name does; the list is joined with the separator
// of the language in force.
func TestListEscapesSiteLabelsAndJoinsThemInTheLanguageInForce(t *testing.T) {
	keys := []api.Key{{
		ID:          ciID,
		Name:        "ci",
		Kind:        "deploy",
		ResourceIDs: []string{"k3j9x2m4p7q1w8e5", "a1b2c3d4e5f6g7h8"},
		Sites: []api.LabeledSite{
			{ID: "k3j9x2m4p7q1w8e5", Label: "exa‮mple.com"},
			{ID: "a1b2c3d4e5f6g7h8", Label: "docs.kamakiri-pages.jp"},
		},
		CreatedAt: "2026-11-13T02:05:59Z",
	}}

	for _, tc := range []struct {
		lang string
		want string
	}{
		{"en", `  deploy  exa\u{202e}mple.com, docs.kamakiri-pages.jp  2026-11-13 02:05`},
		{"ja", `  デプロイ  exa\u{202e}mple.com、docs.kamakiri-pages.jp  2026-11-13 02:05`},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			loggedIn(t)
			useLanguage(t, tc.lang)
			client := &fakeClient{keys: keys}

			var out bytes.Buffer
			if err := List(client, &out, now); err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if got := out.String(); !strings.Contains(got, tc.want) {
				t.Errorf("List() printed:\n%s\nwant a row holding %q", got, tc.want)
			}
		})
	}
}

// A deploy key from a server that sends no labelled sites is listed by the
// resource IDs it names instead. The name goes through the escaping every
// printed key name does, and is sized by what it prints as. A timestamp that
// does not parse is shown as the server sent it rather than replaced with a
// made-up time.
func TestListShowsResourceIDsEscapedNamesAndUnparsedTimesAsTheyAre(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{keys: []api.Key{{
		ID:          ciID,
		Name:        "ci‮box",
		Kind:        "deploy",
		ResourceIDs: []string{"site-a", "site-b"},
		CreatedAt:   "yesterday",
		LastUsedAt:  stamp(now.Add(-2 * time.Hour)),
		Current:     true,
	}, {
		ID:         unnamedID,
		Name:       "unnamed",
		Kind:       "login",
		CreatedAt:  "2026-09-02T08:14:00Z",
		LastUsedAt: func() *string { s := "soon"; return &s }(),
	}}}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := "" +
		"  ID                                    Name           Kind    Sites           Created           Last used\n" +
		`* c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c  ci\u{202e}box  deploy  site-a, site-b  yesterday         2 hours ago` + "\n" +
		"  0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40  unnamed        login   all             2026-09-02 08:14  soon\n" +
		"\n" +
		"* the key this command ran with\n"
	if got := out.String(); got != want {
		t.Errorf("List() printed:\n%s\nwant:\n%s", got, want)
	}
}

// The Created column is always UTC, whatever offset the server sent.
func TestListShowsAnOffsetCreatedTimeInUTC(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{keys: []api.Key{{
		ID:        ciID,
		Name:      "ci",
		Kind:      "login",
		CreatedAt: "2026-11-12T19:30:00+09:00",
	}}}

	var out bytes.Buffer
	if err := List(client, &out, now); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "2026-11-12 10:30") {
		t.Errorf("List() printed:\n%s\nwant the created time 2026-11-12 10:30", got)
	}
}

func TestListMapsAnAPIError(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{listErr: &api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"}}

	var out bytes.Buffer
	err := List(client, &out, now)
	if want := api.UnauthorizedError().Error(); err == nil || err.Error() != want {
		t.Fatalf("List() error = %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Errorf("List() printed %q on failure, want nothing", out.String())
	}
}

// With no credential the command says so before anything is sent:
// setAPIKey only refuses a credential it failed to read, not a missing one.
func TestEveryCommandRefusesWithoutACredential(t *testing.T) {
	const notLoggedIn = "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key"
	cases := []struct {
		name string
		run  func(*fakeClient, *bytes.Buffer) error
	}{
		{"List", func(c *fakeClient, out *bytes.Buffer) error { return List(c, out, now) }},
		{"Rename", func(c *fakeClient, out *bytes.Buffer) error { return Rename(c, laptopID, "ci-box", out) }},
		{"Revoke", func(c *fakeClient, out *bytes.Buffer) error { return Revoke(c, ciID, out) }},
		{"Create", func(c *fakeClient, out *bytes.Buffer) error {
			return Create(c, CreateOptions{Sites: []string{"example.com"}}, out, out)
		}},
		{"Rotate", func(c *fakeClient, out *bytes.Buffer) error { return Rotate(c, deployID, out, out) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			client := &fakeClient{
				keys:    accountKeys("alexis-laptop"),
				revoked: &api.KeyInfo{ID: ciID, Name: "github-ci"},
				created: createdKey("github-actions"),
				rotated: rotatedKey("github-actions"),
			}

			var out bytes.Buffer
			err := tc.run(client, &out)
			if err == nil || err.Error() != notLoggedIn {
				t.Fatalf("error = %v, want %q", err, notLoggedIn)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q, want nothing", out.String())
			}
			assertCalls(t, client)
		})
	}
}

// Each unit turns over at its boundary, and one of a unit reads in the
// singular. A month is 30 days and a year 365, and the months stop at 11, so
// the last days of a year never read as 12 months.
func TestAgeAtEachBoundary(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{-time.Hour, "just now"},
		{-time.Second, "just now"},
		{0, "just now"},
		{59 * time.Second, "just now"},
		{60 * time.Second, "1 minute ago"},
		{119 * time.Second, "1 minute ago"},
		{2 * time.Minute, "2 minutes ago"},
		{59 * time.Minute, "59 minutes ago"},
		{59*time.Minute + 59*time.Second, "59 minutes ago"},
		{time.Hour, "1 hour ago"},
		{2 * time.Hour, "2 hours ago"},
		{23 * time.Hour, "23 hours ago"},
		{23*time.Hour + 59*time.Minute, "23 hours ago"},
		{day, "1 day ago"},
		{2 * day, "2 days ago"},
		{29 * day, "29 days ago"},
		{29*day + 23*time.Hour, "29 days ago"},
		{30 * day, "1 month ago"},
		{59 * day, "1 month ago"},
		{60 * day, "2 months ago"},
		{359 * day, "11 months ago"},
		{360 * day, "11 months ago"},
		{364 * day, "11 months ago"},
		{364*day + 23*time.Hour, "11 months ago"},
		{365 * day, "1 year ago"},
		{729 * day, "1 year ago"},
		{730 * day, "2 years ago"},
	}
	for _, tc := range cases {
		t.Run(tc.ago.String(), func(t *testing.T) {
			if got := age(now.Add(-tc.ago), now); got != tc.want {
				t.Errorf("age(now - %s) = %q, want %q", tc.ago, got, tc.want)
			}
		})
	}
}

// The Japanese ages are their own catalog values, singular and plural alike.
func TestAgeInJapanese(t *testing.T) {
	useLanguage(t, "ja")
	const day = 24 * time.Hour
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "たった今"},
		{time.Minute, "1分前"},
		{5 * time.Minute, "5分前"},
		{time.Hour, "1時間前"},
		{5 * time.Hour, "5時間前"},
		{day, "1日前"},
		{5 * day, "5日前"},
		{30 * day, "1か月前"},
		{150 * day, "5か月前"},
		{365 * day, "1年前"},
		{5 * 365 * day, "5年前"},
	}
	for _, tc := range cases {
		t.Run(tc.ago.String(), func(t *testing.T) {
			if got := age(now.Add(-tc.ago), now); got != tc.want {
				t.Errorf("age(now - %s) = %q, want %q", tc.ago, got, tc.want)
			}
		})
	}
}

func TestRenameSendsTheTrimmedNameAndPrintsTheStoredOne(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{renamed: &api.Key{ID: laptopID, Name: "ci\u009b2Jbox‮ (stored)", Kind: "login"}}

	var out bytes.Buffer
	if err := Rename(client, strings.ToUpper(laptopID), " \t ci-box \n", &out); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}

	assertCalls(t, client, fmt.Sprintf("RenameKey %s %q", strings.ToUpper(laptopID), "ci-box"))
	want := `Renamed key 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65 to ci\u{009b}2Jbox\u{202e} (stored).` + "\n"
	if got := out.String(); got != want {
		t.Errorf("Rename() printed %q, want %q", got, want)
	}
}

// The ID is checked for the shape of a key ID before anything is sent, and the
// refusal never repeats what was typed: the likeliest wrong argument is an API
// key, which would then land in a terminal's scrollback or a CI log.
func TestRenameRefusesAnArgumentThatIsNotAKeyID(t *testing.T) {
	const refusal = `invalid key ID. A key ID looks like 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65; run "kamakiri key list" to see yours`
	cases := []struct {
		name string
		id   string
	}{
		{"an API key", "kk_live_3f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c"},
		{"empty", ""},
		{"current", "current"},
		{"a prefix", ciID[:8]},
		{"one character short", ciID[:35]},
		{"one character long", ciID + "0"},
		{"no hyphens", strings.ReplaceAll(ciID, "-", "") + "0000"},
		{"a hyphen moved", "c3e94b701-a2d-4e5f-b6c7-8d9e0f1a2b3c"},
		{"a letter past f", "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3g"},
		{"braces", "{c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b}"},
		{"a path", "../c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{}

			var out bytes.Buffer
			err := Rename(client, tc.id, "ci-box", &out)
			if err == nil || err.Error() != refusal {
				t.Fatalf("Rename() error = %v, want %q", err, refusal)
			}
			if tc.id != "" && strings.Contains(err.Error(), tc.id) {
				t.Errorf("the refusal repeats the argument: %q", err)
			}
			if out.Len() != 0 {
				t.Errorf("Rename() printed %q, want nothing", out.String())
			}
			assertCalls(t, client)
		})
	}
}

// A name the server would refuse is refused here first, with nothing sent.
func TestRenameRefusesABadNameBeforeSending(t *testing.T) {
	const refusal = "invalid key name: it must be 1 to 64 characters with no control characters"
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", " \t\n "},
		{"65 code points", strings.Repeat("名", 65)},
		{"an escape character", "ci\x1bbox"},
		{"the 8-bit CSI", "ci\u009bbox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{}

			var out bytes.Buffer
			err := Rename(client, laptopID, tc.in, &out)
			if err == nil || err.Error() != refusal {
				t.Fatalf("Rename() error = %v, want %q", err, refusal)
			}
			if out.Len() != 0 {
				t.Errorf("Rename() printed %q, want nothing", out.String())
			}
			assertCalls(t, client)
		})
	}
}

// The server answers a key of another account exactly as one that does not
// exist, so the line says only that this account holds no such key. Any other
// refusal is worded as everywhere else.
func TestRenameWordsTheServersRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"key_not_found",
			&api.ErrorResponse{Code: "key_not_found", Message: "Key not found."},
			"no key with ID " + laptopID + " in this account",
		},
		{
			"unauthorized",
			&api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"},
			api.UnauthorizedError().Error(),
		},
		{
			"invalid_request",
			&api.ErrorResponse{Code: "invalid_request", Message: "name must be 1 to 64 characters with no control characters."},
			"name must be 1 to 64 characters with no control characters.",
		},
		{
			"a transport failure",
			errors.New("request failed: connection refused"),
			"request failed: connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{renameErr: tc.err}

			var out bytes.Buffer
			err := Rename(client, laptopID, "ci-box", &out)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Rename() error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("Rename() printed %q, want nothing", out.String())
			}
		})
	}
}

// The line names the key by what the server answered, so the ID is the stored,
// lower-case one even when it was typed in capitals, and the name is escaped as
// every printed key name is.
func TestRevokeSendsTheIDAndPrintsTheRevokedKey(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{revoked: &api.KeyInfo{ID: ciID, Name: "ci‮box"}}

	var out bytes.Buffer
	if err := Revoke(client, strings.ToUpper(ciID), &out); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	assertCalls(t, client, "RevokeKey "+strings.ToUpper(ciID))
	want := `Revoked key ci\u{202e}box (c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c). It no longer works anywhere it was used.` + "\n"
	if got := out.String(); got != want {
		t.Errorf("Revoke() printed %q, want %q", got, want)
	}
}

func TestRevokePrintsTheRevokedKeyInJapanese(t *testing.T) {
	loggedIn(t)
	useLanguage(t, "ja")
	client := &fakeClient{revoked: &api.KeyInfo{ID: ciID, Name: "ビルド機"}}

	var out bytes.Buffer
	if err := Revoke(client, ciID, &out); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	want := "キー「ビルド機」(c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c) を失効させました。このキーは、使われていたどの場所でも使えなくなりました。\n"
	if got := out.String(); got != want {
		t.Errorf("Revoke() printed %q, want %q", got, want)
	}
}

// The ID is checked as Rename checks it, so a word such as "current" never
// reaches a route of its own, and the refusal never repeats what was typed.
func TestRevokeRefusesAnArgumentThatIsNotAKeyID(t *testing.T) {
	const refusal = `invalid key ID. A key ID looks like 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65; run "kamakiri key list" to see yours`
	cases := []struct {
		name string
		id   string
	}{
		{"an API key", "kk_live_3f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c"},
		{"empty", ""},
		{"current", "current"},
		{"a prefix", ciID[:8]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{}

			var out bytes.Buffer
			err := Revoke(client, tc.id, &out)
			if err == nil || err.Error() != refusal {
				t.Fatalf("Revoke() error = %v, want %q", err, refusal)
			}
			if tc.id != "" && strings.Contains(err.Error(), tc.id) {
				t.Errorf("the refusal repeats the argument: %q", err)
			}
			if out.Len() != 0 {
				t.Errorf("Revoke() printed %q, want nothing", out.String())
			}
			assertCalls(t, client)
		})
	}
}

// A key of another account is answered as one that does not exist, so the line
// says only that this account holds no such key; the server refuses the key
// the command runs with, and the CLI says how to revoke it instead. Anything
// else is worded as everywhere else.
func TestRevokeWordsTheServersRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"key_not_found",
			&api.ErrorResponse{Code: "key_not_found", Message: "Key not found."},
			"no key with ID " + ciID + " in this account",
		},
		{
			"current_key",
			&api.ErrorResponse{Code: "current_key", Message: "This is the key the request was made with."},
			`this is the key this machine is logged in with. Run "kamakiri logout" to revoke it`,
		},
		{
			"unauthorized",
			&api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"},
			api.UnauthorizedError().Error(),
		},
		{
			"a transport failure",
			errors.New("request failed: connection refused"),
			"request failed: connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{revokeErr: tc.err}

			var out bytes.Buffer
			err := Revoke(client, ciID, &out)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Revoke() error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("Revoke() printed %q, want nothing", out.String())
			}
			assertCalls(t, client, "RevokeKey "+ciID)
		})
	}
}

// The refusal of the key the command runs with is worded by where that key came
// from, read from the environment itself rather than from the api package's own
// record of it: each case below sets that record to the opposite answer, so a
// refusal worded from it would come out wrong. Not parallel: t.Setenv, and the
// api package's record is unsynchronized package state.
func TestRevokeOfTheCurrentKeyWordsItBySource(t *testing.T) {
	refusal := &api.ErrorResponse{Code: "current_key", Message: "This is the key the request was made with."}
	cases := []struct {
		name        string
		environment bool
		lang        string
		want        string
	}{
		{"from the file", false, "en", `this is the key this machine is logged in with. Run "kamakiri logout" to revoke it`},
		{"from the environment", true, "en", "this is the key in KAMAKIRI_API_KEY, which this command runs with. Revoke it using another key, such as the one on your own machine"},
		{"from the file, in Japanese", false, "ja", "このマシンがログインに使っているキーです。失効させるには \"kamakiri logout\" を実行してください"},
		{"from the environment, in Japanese", true, "ja", "このコマンドが使っている KAMAKIRI_API_KEY のキーです。自分のマシンのキーなど、別のキーを使って失効させてください"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			if tc.environment {
				t.Setenv(core.EnvAPIKey, "kk_live_fromenv")
			}
			api.SetKeyFromEnvironment(!tc.environment)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
			useLanguage(t, tc.lang)
			client := &fakeClient{revokeErr: refusal}

			var out bytes.Buffer
			err := Revoke(client, ciID, &out)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Revoke() error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("Revoke() printed %q, want nothing", out.String())
			}
		})
	}
}

const (
	deployID   = "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
	deployKey  = "kk_deploy_4Wq8ZtR2mN7xK1pL9sB3vC6yH0jD5fGa"
	linkedSite = "k3j9x2m4p7q1w8e5"
)

// createdKey is the answer to a creation: a deploy key bound to two sites, one
// shown by its canonical domain and one by its full subdomain host.
func createdKey(name string) *api.CreatedKey {
	return &api.CreatedKey{
		Key: api.Key{
			ID:          deployID,
			Name:        name,
			Kind:        "deploy",
			ResourceIDs: []string{linkedSite, "a1b2c3d4e5f6g7h8"},
			Sites: []api.LabeledSite{
				{ID: linkedSite, Label: "example.com"},
				{ID: "a1b2c3d4e5f6g7h8", Label: "docs.kamakiri-pages.jp"},
			},
			CreatedAt: "2026-11-17T10:30:00Z",
		},
		APIKey: deployKey,
	}
}

// inDirectory runs the rest of the test from a fresh directory, linked to
// siteID when it is not empty, so what core.LoadProject finds is the test's.
func inDirectory(t *testing.T, siteID string) {
	t.Helper()
	t.Chdir(t.TempDir())
	if siteID != "" {
		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: siteID}); err != nil {
			t.Fatal(err)
		}
	}
}

// assertStreams compares both streams whole: stdout is the one a pipe such as
// `| gh secret set` reads, so anything but the key on it, or the key on a
// failure, is a fault.
func assertStreams(t *testing.T, stdout, stderr *bytes.Buffer, wantStdout, wantStderr string) {
	t.Helper()
	if got := stdout.String(); got != wantStdout {
		t.Errorf("stdout = %q, want %q", got, wantStdout)
	}
	if got := stderr.String(); got != wantStderr {
		t.Errorf("stderr:\n%s\nwant:\n%s", got, wantStderr)
	}
}

// The key alone goes to stdout, so it can be piped into a secret store; the
// four lines a person reads go to stderr, naming the key in full and its sites
// by the labels the server gave them. Repeated sites go out in the order typed.
func TestCreatePrintsTheKeyAloneOnStdoutAndTheLinesOnStderr(t *testing.T) {
	loggedIn(t)
	inDirectory(t, "")
	client := &fakeClient{created: createdKey("github-actions")}

	var stdout, stderr bytes.Buffer
	err := Create(client, CreateOptions{Name: "github-actions", NameGiven: true, Sites: []string{"example.com", "docs"}}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, deployKey+"\n",
		"Created deploy key \"github-actions\" ("+deployID+").\n"+
			"Sites: example.com, docs.kamakiri-pages.jp\n"+
			"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n"+
			"A notice is being sent to the account's email address.\n")
	assertCalls(t, client, `CreateKey "github-actions" ["example.com" "docs"]`)
}

func TestCreatePrintsTheLinesInJapanese(t *testing.T) {
	loggedIn(t)
	inDirectory(t, "")
	useLanguage(t, "ja")
	client := &fakeClient{created: createdKey("github-actions")}

	var stdout, stderr bytes.Buffer
	if err := Create(client, CreateOptions{Sites: []string{"example.com"}}, &stdout, &stderr); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, deployKey+"\n",
		"デプロイキー「github-actions」("+deployID+") を作成しました。\n"+
			"サイト: example.com、docs.kamakiri-pages.jp\n"+
			"このキーが表示されるのは今回だけです。CIのシークレットに KAMAKIRI_API_KEY として保存してください。\n"+
			"アカウントのメールアドレスに通知を送信します。\n")
}

// The name and the labels come from the server and may hold characters that
// reorder or hide what follows, so they print escaped as every key name does.
func TestCreateEscapesTheNameAndTheLabels(t *testing.T) {
	loggedIn(t)
	inDirectory(t, "")
	created := createdKey("ci‮box")
	created.Key.Sites[1].Label = "evil .example"
	client := &fakeClient{created: created}

	var stdout, stderr bytes.Buffer
	if err := Create(client, CreateOptions{Sites: []string{"example.com"}}, &stdout, &stderr); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, deployKey+"\n",
		"Created deploy key \"ci\\u{202e}box\" ("+deployID+").\n"+
			"Sites: example.com, evil\\u{2028}.example\n"+
			"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n"+
			"A notice is being sent to the account's email address.\n")
}

// Without --name no name is sent, so the server's default applies; a given name
// is sent trimmed, as rename sends one.
func TestCreateSendsTheNameOnlyWhenGiven(t *testing.T) {
	cases := []struct {
		label string
		opts  CreateOptions
		call  string
	}{
		{"no --name", CreateOptions{Sites: []string{"example.com"}}, `CreateKey "" ["example.com"]`},
		{"--name", CreateOptions{Name: "  github-actions\t", NameGiven: true, Sites: []string{"example.com"}}, `CreateKey "github-actions" ["example.com"]`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, "")
			client := &fakeClient{created: createdKey("deploy")}

			var stdout, stderr bytes.Buffer
			if err := Create(client, tc.opts, &stdout, &stderr); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			assertCalls(t, client, tc.call)
			if got := stdout.String(); got != deployKey+"\n" {
				t.Errorf("stdout = %q, want the key alone", got)
			}
		})
	}
}

// With no --site the key is bound to the linked site, named by its ID; a --site
// given in a linked directory names the sites instead, and the linked one is
// not added to them.
func TestCreateBindsTheLinkedSiteWithoutSite(t *testing.T) {
	cases := []struct {
		label string
		sites []string
		call  string
	}{
		{"no --site", nil, `CreateKey "" ["` + linkedSite + `"]`},
		{"--site", []string{"docs", "example.com"}, `CreateKey "" ["docs" "example.com"]`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, linkedSite)
			client := &fakeClient{created: createdKey("deploy")}

			var stdout, stderr bytes.Buffer
			if err := Create(client, CreateOptions{Sites: tc.sites}, &stdout, &stderr); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			assertCalls(t, client, tc.call)
			if got := stdout.String(); got != deployKey+"\n" {
				t.Errorf("stdout = %q, want the key alone", got)
			}
		})
	}
}

// Each refusal made before the request leaves both streams empty, returns the
// one line for main to print, and sends nothing.
func TestCreateRefusesBeforeSending(t *testing.T) {
	cases := []struct {
		label  string
		linked string
		config string
		opts   CreateOptions
		want   string
	}{
		{
			label: "no --site and no linked site",
			want:  "no site linked here. Run this in a linked directory, or name a site with --site",
		},
		{
			label:  "no --site and a linked config with no ID",
			config: `{"version":1,"kind":"pages","id":""}`,
			want:   "no site linked here. Run this in a linked directory, or name a site with --site",
		},
		{
			label:  "no --site and a config that does not parse",
			config: `{"id":`,
			want:   "parse project config: unexpected end of JSON input",
		},
		{
			label: "an empty name",
			opts:  CreateOptions{NameGiven: true, Sites: []string{"example.com"}},
			want:  "invalid key name: it must be 1 to 64 characters with no control characters",
		},
		{
			label:  "a whitespace name, in a linked directory",
			linked: linkedSite,
			opts:   CreateOptions{Name: " \t ", NameGiven: true},
			want:   "invalid key name: it must be 1 to 64 characters with no control characters",
		},
		{
			label: "a name with a control character",
			opts:  CreateOptions{Name: "ci\x1bbox", NameGiven: true, Sites: []string{"example.com"}},
			want:  "invalid key name: it must be 1 to 64 characters with no control characters",
		},
		{
			label: "a name of 65 code points",
			opts:  CreateOptions{Name: strings.Repeat("名", 65), NameGiven: true, Sites: []string{"example.com"}},
			want:  "invalid key name: it must be 1 to 64 characters with no control characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, tc.linked)
			if tc.config != "" {
				if err := os.MkdirAll(".kamakiri", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(core.ProjectPath(), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			client := &fakeClient{created: createdKey("deploy")}

			var stdout, stderr bytes.Buffer
			err := Create(client, tc.opts, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Create() error = %v, want %q", err, tc.want)
			}
			assertStreams(t, &stdout, &stderr, "", "")
			assertCalls(t, client)
		})
	}
}

// Every refusal of the request itself is worded as every command words it,
// with the key nowhere: stdout stays empty, and so does stderr, which main
// writes the returned line to.
func TestCreateWordsTheServersRefusals(t *testing.T) {
	cases := []struct {
		label string
		err   error
		want  string
	}{
		{
			"key_site_not_found",
			&api.ErrorResponse{Code: "key_site_not_found", Message: "No site of this account matches docs.kamakiri-pages.jp.", Reference: "docs.kamakiri-pages.jp"},
			"no site of this account matches docs.kamakiri-pages.jp. Name a site by its subdomain, one of its domains, or its site ID",
		},
		{
			"deploy_key_limit",
			&api.ErrorResponse{Code: "deploy_key_limit", Message: "This account already holds 10 deploy keys, the most it can have.", Limit: 10},
			`this account already holds 10 deploy keys, the most it can have. Revoke one with "kamakiri key revoke <id>" first`,
		},
		{
			"key_create_rate_limited",
			&api.ErrorResponse{Code: "key_create_rate_limited", Message: "Too many keys created recently. Try again later."},
			"too many keys created recently. Try again later",
		},
		{
			"deploy_key_refused",
			&api.ErrorResponse{
				Code:    "deploy_key_refused",
				Message: "This is a deploy key.",
				Key:     &api.KeyInfo{ID: deployID, Name: "github-actions"},
				Sites:   []api.LabeledSite{{ID: linkedSite, Label: "example.com"}},
			},
			`this is a deploy key ("github-actions"): it can deploy to example.com and nothing else. Use a login key for this command`,
		},
		{
			"invalid_request",
			&api.ErrorResponse{Code: "invalid_request", Message: "sites must be a list of 1 to 100 site references, each a non-empty string of at most 253 bytes."},
			"sites must be a list of 1 to 100 site references, each a non-empty string of at most 253 bytes.",
		},
		{
			"unauthorized",
			&api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"},
			api.UnauthorizedError().Error(),
		},
		{
			"a transport failure",
			errors.New("request failed: connection refused"),
			"request failed: connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, "")
			client := &fakeClient{createErr: tc.err}

			var stdout, stderr bytes.Buffer
			err := Create(client, CreateOptions{Sites: []string{"docs"}}, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Create() error = %v, want %q", err, tc.want)
			}
			assertStreams(t, &stdout, &stderr, "", "")
			assertCalls(t, client, `CreateKey "" ["docs"]`)
		})
	}
}

// With no --site, in a linked directory, only key_site_not_found gets the
// linked-site wording; every other refusal keeps the wording it has with a
// --site, so the special case is tied to the code and not to the missing flag.
func TestCreateWithoutSiteWordsOnlyAnUnmatchedSiteAsTheLinkedOne(t *testing.T) {
	cases := []struct {
		label string
		err   error
		want  string
	}{
		{
			"key_site_not_found",
			&api.ErrorResponse{Code: "key_site_not_found", Message: "No site of this account matches " + linkedSite + ".kamakiri-pages.jp.", Reference: linkedSite + ".kamakiri-pages.jp"},
			"the site linked here is not an active site of this account. Name a site with --site",
		},
		{
			"deploy_key_limit",
			&api.ErrorResponse{Code: "deploy_key_limit", Message: "This account already holds 10 deploy keys, the most it can have.", Limit: 10},
			`this account already holds 10 deploy keys, the most it can have. Revoke one with "kamakiri key revoke <id>" first`,
		},
		{
			"key_create_rate_limited",
			&api.ErrorResponse{Code: "key_create_rate_limited", Message: "Too many keys created recently. Try again later."},
			"too many keys created recently. Try again later",
		},
		{
			"deploy_key_refused",
			&api.ErrorResponse{
				Code:    "deploy_key_refused",
				Message: "This is a deploy key.",
				Key:     &api.KeyInfo{ID: deployID, Name: "github-actions"},
				Sites:   []api.LabeledSite{{ID: linkedSite, Label: "example.com"}},
			},
			`this is a deploy key ("github-actions"): it can deploy to example.com and nothing else. Use a login key for this command`,
		},
		{
			"invalid_request",
			&api.ErrorResponse{Code: "invalid_request", Message: "sites must be a list of 1 to 100 site references, each a non-empty string of at most 253 bytes."},
			"sites must be a list of 1 to 100 site references, each a non-empty string of at most 253 bytes.",
		},
		{
			"unauthorized",
			&api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"},
			api.UnauthorizedError().Error(),
		},
		{
			"a transport failure",
			errors.New("request failed: connection refused"),
			"request failed: connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, linkedSite)
			client := &fakeClient{createErr: tc.err}

			var stdout, stderr bytes.Buffer
			err := Create(client, CreateOptions{}, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Create() error = %v, want %q", err, tc.want)
			}
			assertStreams(t, &stdout, &stderr, "", "")
			assertCalls(t, client, `CreateKey "" ["`+linkedSite+`"]`)
		})
	}
}

// failingWriter refuses every write, as a full disk or a closed pipe does.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// When the key cannot be written, the run fails instead of exiting 0 with the
// key lost: the error names the key ID so the user can revoke it, and never the
// key itself. The four lines had already gone to stderr by then.
func TestCreateFailsWhenTheKeyCannotBeWritten(t *testing.T) {
	cases := []struct {
		lang string
		want string
	}{
		{"en", `the key was created but could not be written to stdout (broken pipe). It cannot be shown again: run "kamakiri key rotate ` + deployID + `" to get another`},
		{"ja", `キーは作成されましたが、標準出力に書き込めませんでした (broken pipe)。このキーは再表示できません。"kamakiri key rotate ` + deployID + `" を実行して、別のキーを取得してください`},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, "")
			useLanguage(t, tc.lang)
			client := &fakeClient{created: createdKey("github-actions")}

			var stderr bytes.Buffer
			err := Create(client, CreateOptions{Sites: []string{"example.com"}}, failingWriter{errors.New("broken pipe")}, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Create() error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), deployKey) || strings.Contains(stderr.String(), deployKey) {
				t.Errorf("the key reached stderr or the error:\n%s\n%v", stderr.String(), err)
			}
			if got := strings.Count(stderr.String(), "\n"); got != 4 {
				t.Errorf("stderr holds %d lines, want the 4 created lines:\n%s", got, stderr.String())
			}
		})
	}
}

// The key ID in the not-written line is escaped like the one on the created
// line, so an ID holding a control character cannot drive the terminal.
func TestCreateNotWrittenErrorEscapesTheKeyID(t *testing.T) {
	loggedIn(t)
	inDirectory(t, "")
	created := createdKey("github-actions")
	created.Key.ID = "3f2a\x1b[31m-id"
	client := &fakeClient{created: created}

	var stderr bytes.Buffer
	err := Create(client, CreateOptions{Sites: []string{"example.com"}}, failingWriter{errors.New("broken pipe")}, &stderr)
	if err == nil {
		t.Fatal("Create() error = nil, want the not-written error")
	}
	want := `run "kamakiri key rotate 3f2a\u{001b}[31m-id" to get another`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Create() error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("the error holds a raw escape character: %q", err)
	}
}

// The four lines come before the key when both streams are one terminal or one
// file, as a person running the command without redirection sees them.
func TestCreateWritesTheLinesBeforeTheKey(t *testing.T) {
	loggedIn(t)
	inDirectory(t, "")
	client := &fakeClient{created: createdKey("github-actions")}

	var both bytes.Buffer
	if err := Create(client, CreateOptions{Sites: []string{"example.com"}}, &both, &both); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	want := "Created deploy key \"github-actions\" (" + deployID + ").\n" +
		"Sites: example.com, docs.kamakiri-pages.jp\n" +
		"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n" +
		"A notice is being sent to the account's email address.\n" +
		deployKey + "\n"
	if got := both.String(); got != want {
		t.Errorf("combined output:\n%s\nwant:\n%s", got, want)
	}
}

// With no --site the reference the server could not match is the linked site's
// ID, which the user never typed, so its echo (the ID expanded into a host) is
// not shown; the line says the linked site is the trouble instead. A --site the
// server could not match is echoed as the server read it.
func TestCreateWordsAnUnmatchedSiteByWhereItCameFrom(t *testing.T) {
	refusal := &api.ErrorResponse{
		Code:      "key_site_not_found",
		Message:   "No site of this account matches " + linkedSite + ".kamakiri-pages.jp.",
		Reference: linkedSite + ".kamakiri-pages.jp",
	}
	cases := []struct {
		label string
		sites []string
		lang  string
		want  string
	}{
		{"the linked site", nil, "en", "the site linked here is not an active site of this account. Name a site with --site"},
		{"the linked site, in Japanese", nil, "ja", "このディレクトリにリンクされているサイトは、このアカウントの有効なサイトではありません。--site でサイトを指定してください"},
		{"a --site", []string{linkedSite}, "en", "no site of this account matches " + linkedSite + ".kamakiri-pages.jp. Name a site by its subdomain, one of its domains, or its site ID"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			loggedIn(t)
			inDirectory(t, linkedSite)
			useLanguage(t, tc.lang)
			client := &fakeClient{createErr: refusal}

			var stdout, stderr bytes.Buffer
			err := Create(client, CreateOptions{Sites: tc.sites}, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Create() error = %v, want %q", err, tc.want)
			}
			assertStreams(t, &stdout, &stderr, "", "")
			assertCalls(t, client, `CreateKey "" ["`+linkedSite+`"]`)
		})
	}
}

const (
	newDeployID  = "7b2e4d10-9a3c-4f1e-8b20-5c6d7e8f9a01"
	newDeployKey = "kk_deploy_9Hc2Lm5Qx8Tn1Vb4Zr7Kp0Wd3Fs6Gj"
)

// rotatedKey is the answer to a rotation of the key createdKey describes: a new
// key of the same name and sites, replacing deployID.
func rotatedKey(name string) *api.RotatedKey {
	created := createdKey(name)
	created.Key.ID = newDeployID
	created.Key.CreatedAt = "2026-11-18T09:00:00Z"
	return &api.RotatedKey{
		Key:      created.Key,
		APIKey:   newDeployKey,
		Replaced: api.KeyInfo{ID: deployID, Name: name},
	}
}

// rotatedLines is what a rotation prints on stderr in English, before the key.
const rotatedLines = "Rotated deploy key \"github-actions\": " + newDeployID + " replaces " + deployID + ", which no longer works.\n" +
	"Sites: example.com, docs.kamakiri-pages.jp\n" +
	"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n" +
	"A notice is being sent to the account's email address.\n"

// The new key alone goes to stdout, so a rotation can be piped into the secret
// store the old key sat in; the lines a person reads go to stderr. The old ID
// on the first line is the server's, lower-case even when typed in capitals.
func TestRotatePrintsTheKeyAloneOnStdoutAndTheLinesOnStderr(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{rotated: rotatedKey("github-actions")}

	var stdout, stderr bytes.Buffer
	if err := Rotate(client, strings.ToUpper(deployID), &stdout, &stderr); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, newDeployKey+"\n", rotatedLines)
	assertCalls(t, client, "RotateKey "+strings.ToUpper(deployID))
}

func TestRotatePrintsTheLinesInJapanese(t *testing.T) {
	loggedIn(t)
	useLanguage(t, "ja")
	client := &fakeClient{rotated: rotatedKey("github-actions")}

	var stdout, stderr bytes.Buffer
	if err := Rotate(client, deployID, &stdout, &stderr); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, newDeployKey+"\n",
		"デプロイキー「github-actions」をローテーションしました。"+deployID+" は "+newDeployID+" に置き換えられ、使えなくなりました。\n"+
			"サイト: example.com、docs.kamakiri-pages.jp\n"+
			"このキーが表示されるのは今回だけです。CIのシークレットに KAMAKIRI_API_KEY として保存してください。\n"+
			"アカウントのメールアドレスに通知を送信します。\n")
}

// The name, the IDs and the labels come from the server, so they print escaped
// as every key name does.
func TestRotateEscapesWhatTheServerSent(t *testing.T) {
	loggedIn(t)
	rotated := rotatedKey("ci‮box")
	rotated.Key.ID = "7b2e\x1b[31m-new"
	rotated.Replaced.ID = "3f2a\x1b[31m-old"
	rotated.Key.Sites[1].Label = "evil .example"
	client := &fakeClient{rotated: rotated}

	var stdout, stderr bytes.Buffer
	if err := Rotate(client, deployID, &stdout, &stderr); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, newDeployKey+"\n",
		"Rotated deploy key \"ci\\u{202e}box\": 7b2e\\u{001b}[31m-new replaces 3f2a\\u{001b}[31m-old, which no longer works.\n"+
			"Sites: example.com, evil\\u{2028}.example\n"+
			"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n"+
			"A notice is being sent to the account's email address.\n")
}

// An answer that leaves out the replaced key's ID still names the old key: the
// typed ID, which has passed the ID check and so is safe to repeat, rather than
// a line with a hole in it.
func TestRotateNamesTheTypedIDWhenTheAnswerNamesNoReplacedKey(t *testing.T) {
	loggedIn(t)
	rotated := rotatedKey("github-actions")
	rotated.Replaced = api.KeyInfo{}
	client := &fakeClient{rotated: rotated}

	var stdout, stderr bytes.Buffer
	if err := Rotate(client, deployID, &stdout, &stderr); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	assertStreams(t, &stdout, &stderr, newDeployKey+"\n", rotatedLines)
}

// The ID is checked as revoke checks it, before anything is sent, and the
// refusal never repeats what was typed nor says the key may have been rotated.
func TestRotateRefusesAnArgumentThatIsNotAKeyID(t *testing.T) {
	const refusal = `invalid key ID. A key ID looks like 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65; run "kamakiri key list" to see yours`
	cases := []struct {
		name string
		id   string
	}{
		{"an API key", "kk_live_3f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c"},
		{"empty", ""},
		{"current", "current"},
		{"a prefix", deployID[:8]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{rotated: rotatedKey("github-actions")}

			var stdout, stderr bytes.Buffer
			err := Rotate(client, tc.id, &stdout, &stderr)
			if err == nil || err.Error() != refusal {
				t.Fatalf("Rotate() error = %v, want %q", err, refusal)
			}
			if tc.id != "" && strings.Contains(err.Error(), tc.id) {
				t.Errorf("the refusal repeats the argument: %q", err)
			}
			assertStreams(t, &stdout, &stderr, "", "")
			assertCalls(t, client)
		})
	}
}

// maybeRotated is the line that follows a failure leaving it unknown whether
// the rotation happened.
const maybeRotated = `The key may have been rotated. Run "kamakiri key list" to see whether it was replaced.`

// A refusal the server makes before it changes anything gets its own line and
// nothing more: the key was not rotated. Every other failure may have followed
// a rotation whose answer was lost, so its line is followed by one saying the
// key may have been rotated, since the old key may then be dead with no new
// one in hand.
func TestRotateWordsTheServersRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"key_not_found",
			&api.ErrorResponse{Code: "key_not_found", Message: "Key not found."},
			"no key with ID " + deployID + " in this account",
		},
		{
			"key_not_rotatable",
			&api.ErrorResponse{Code: "key_not_rotatable", Message: "This is a login key, which cannot be rotated. Log in again on the machine that uses it, then revoke this one."},
			`this is a login key, which cannot be rotated. Run "kamakiri login" on the machine that uses it, then revoke this one with "kamakiri key revoke ` + deployID + `"`,
		},
		{
			"unauthorized",
			&api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"},
			api.UnauthorizedError().Error(),
		},
		{
			"deploy_key_refused",
			&api.ErrorResponse{
				Code:    "deploy_key_refused",
				Message: "This is a deploy key.",
				Key:     &api.KeyInfo{ID: deployID, Name: "github-actions"},
				Sites:   []api.LabeledSite{{ID: linkedSite, Label: "example.com"}},
			},
			`this is a deploy key ("github-actions"): it can deploy to example.com and nothing else. Use a login key for this command`,
		},
		{
			"key_create_rate_limited",
			&api.ErrorResponse{Code: "key_create_rate_limited", Message: "Too many keys created recently. Try again later."},
			"too many keys created recently. Try again later",
		},
		{
			"an upgrade refusal",
			api.ErrUpgradeRequired,
			`this version of the CLI is no longer supported. Run "kamakiri upgrade" to move to the latest release`,
		},
		{
			"server_error",
			&api.ErrorResponse{Code: "server_error", Message: "Failed to rotate the key."},
			"Failed to rotate the key.\n" + maybeRotated,
		},
		{
			"an unrecognised code",
			&api.ErrorResponse{Code: "key_rotation_paused", Message: "Rotation is paused."},
			"Rotation is paused.\n" + maybeRotated,
		},
		{
			"an empty code",
			&api.ErrorResponse{Message: "server returned 500"},
			"server returned 500\n" + maybeRotated,
		},
		{
			"no answer",
			fmt.Errorf("request failed: %w", errors.New("context deadline exceeded")),
			"request failed: context deadline exceeded\n" + maybeRotated,
		},
		{
			"an answer that is not JSON",
			errors.New("server returned 502"),
			"server returned 502\n" + maybeRotated,
		},
		{
			"a success that carries no key",
			fmt.Errorf("parse response: %w", errors.New("the response names no key")),
			"parse response: the response names no key\n" + maybeRotated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			client := &fakeClient{rotateErr: tc.err}

			var stdout, stderr bytes.Buffer
			err := Rotate(client, deployID, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Rotate() error = %v, want %q", err, tc.want)
			}
			assertStreams(t, &stdout, &stderr, "", "")
			assertCalls(t, client, "RotateKey "+deployID)
		})
	}
}

func TestRotateWordsALoginKeyAndAnUnknownOutcomeInJapanese(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"key_not_rotatable",
			&api.ErrorResponse{Code: "key_not_rotatable", Message: "This is a login key."},
			`これはログインキーのため、ローテーションできません。このキーを使っているマシンで "kamakiri login" を実行してから、"kamakiri key revoke ` + deployID + `" でこのキーを失効させてください`,
		},
		{
			"server_error",
			&api.ErrorResponse{Code: "server_error", Message: "Failed to rotate the key."},
			"Failed to rotate the key.\nキーはローテーションされた可能性があります。置き換えられたかどうかは \"kamakiri key list\" で確認してください。",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loggedIn(t)
			useLanguage(t, "ja")
			client := &fakeClient{rotateErr: tc.err}

			var stdout, stderr bytes.Buffer
			err := Rotate(client, deployID, &stdout, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Rotate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

// When the new key cannot be written, the run fails instead of exiting 0 with
// the key lost. The old key is already gone, so the line says so and names the
// new ID to rotate again by; it never holds the key itself. The four lines had
// gone to stderr by then, and the rotation is known to have happened, so the
// unknown-outcome line does not follow.
func TestRotateFailsWhenTheKeyCannotBeWritten(t *testing.T) {
	cases := []struct {
		lang string
		want string
	}{
		{"en", `the key was rotated but could not be written to stdout (broken pipe). It cannot be shown again, and the old key no longer works: run "kamakiri key rotate ` + newDeployID + `" to get another`},
		{"ja", `キーはローテーションされましたが、標準出力に書き込めませんでした (broken pipe)。このキーは再表示できず、古いキーはもう使えません。"kamakiri key rotate ` + newDeployID + `" を実行して、別のキーを取得してください`},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			loggedIn(t)
			useLanguage(t, tc.lang)
			client := &fakeClient{rotated: rotatedKey("github-actions")}

			var stderr bytes.Buffer
			err := Rotate(client, deployID, failingWriter{errors.New("broken pipe")}, &stderr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Rotate() error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), newDeployKey) || strings.Contains(stderr.String(), newDeployKey) {
				t.Errorf("the key reached stderr or the error:\n%s\n%v", stderr.String(), err)
			}
			if got := strings.Count(stderr.String(), "\n"); got != 4 {
				t.Errorf("stderr holds %d lines, want the 4 rotated lines:\n%s", got, stderr.String())
			}
		})
	}
}

// The new ID in the not-written line is escaped like the one on the rotated
// line, so an ID holding a control character cannot drive the terminal.
func TestRotateNotWrittenErrorEscapesTheKeyID(t *testing.T) {
	loggedIn(t)
	rotated := rotatedKey("github-actions")
	rotated.Key.ID = "7b2e\x1b[31m-id"
	client := &fakeClient{rotated: rotated}

	var stderr bytes.Buffer
	err := Rotate(client, deployID, failingWriter{errors.New("broken pipe")}, &stderr)
	if err == nil {
		t.Fatal("Rotate() error = nil, want the not-written error")
	}
	want := `run "kamakiri key rotate 7b2e\u{001b}[31m-id" to get another`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Rotate() error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("the error holds a raw escape character: %q", err)
	}
}

// The four lines come before the key when both streams are one terminal or one
// file.
func TestRotateWritesTheLinesBeforeTheKey(t *testing.T) {
	loggedIn(t)
	client := &fakeClient{rotated: rotatedKey("github-actions")}

	var both bytes.Buffer
	if err := Rotate(client, deployID, &both, &both); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if got, want := both.String(), rotatedLines+newDeployKey+"\n"; got != want {
		t.Errorf("combined output:\n%s\nwant:\n%s", got, want)
	}
}
