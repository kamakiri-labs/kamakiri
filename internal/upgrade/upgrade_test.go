package upgrade

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the copy compared below holds whatever
// locale the suite runs under. Load rather than Setup: nothing here reports
// which language is in force, only renders in it. The catalog is an
// unsynchronized map, so no test in this package takes t.Parallel.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

// repoPath is the owner/repo part of the releases site. The stand-in server
// carries it so the resolution's path rules are exercised against a base URL
// shaped like the real one rather than against a bare host, where a prefix bug
// would go unnoticed.
const repoPath = "/kamakiri-labs/kamakiri"

// oneMB is the cap the asset tests drive in place of the 200 MB a release
// carries, so a body that runs past it is refused end to end without that many
// bytes crossing the loopback.
const oneMB = 1 << 20

// release is the stand-in for the releases site: it answers the
// `releases/latest` redirect, the asset download and `checksums.txt`, and
// records every path it was asked for.
type release struct {
	tag    string
	assets map[string][]byte
	// checksums replaces the body served for checksums.txt; empty means one
	// built from assets.
	checksums string
	// checksumsStatus replaces that body with a bare status.
	checksumsStatus int

	mu    sync.Mutex
	paths []string
}

// start serves the release and returns the base URL a caller passes to run.
func (r *release) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(server.Close)
	return server.URL + repoPath
}

func (r *release) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()

	download := repoPath + "/releases/download/" + r.tag + "/"
	switch {
	case req.URL.Path == repoPath+"/releases/latest":
		http.Redirect(w, req, repoPath+"/releases/tag/"+r.tag, http.StatusFound)
	case req.URL.Path == download+"checksums.txt":
		if r.checksumsStatus != 0 {
			w.WriteHeader(r.checksumsStatus)
			return
		}
		io.WriteString(w, r.checksumsBody())
	case strings.HasPrefix(req.URL.Path, download):
		body, ok := r.assets[strings.TrimPrefix(req.URL.Path, download)]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	default:
		http.NotFound(w, req)
	}
}

func (r *release) checksumsBody() string {
	if r.checksums != "" {
		return r.checksums
	}
	var body strings.Builder
	for name, contents := range r.assets {
		fmt.Fprintf(&body, "%x  %s\n", sha256.Sum256(contents), name)
	}
	return body.String()
}

// requested reports the paths the server was asked for, in order.
func (r *release) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

// linuxRelease is the release every test that gets as far as a download runs
// against: one tag newer than v0.1.1, carrying the one asset a linux/amd64
// build asks for.
func linuxRelease(contents string) *release {
	return &release{tag: "v0.2.0", assets: map[string][]byte{"kamakiri-linux-amd64": []byte(contents)}}
}

// invocation is one set of arguments for run, so a table case can arrange the
// world it needs and hand back the call that exercises it.
type invocation struct {
	version  string
	execPath string
	base     string
	goos     string
	goarch   string
	maxAsset int64
}

// call runs the invocation, writing to out.
func (i invocation) call(out io.Writer) error {
	return run(i.version, i.execPath, i.base, i.goos, i.goarch, i.maxAsset, out)
}

// upgradeFrom returns the invocation of a linux/amd64 build at v0.1.1 installed
// at execPath, under the cap the released binary carries.
func upgradeFrom(execPath, base string) invocation {
	return invocation{
		version:  "v0.1.1",
		execPath: execPath,
		base:     base,
		goos:     "linux",
		goarch:   "amd64",
		maxAsset: maxAssetBytes,
	}
}

// installedAt writes a stand-in for an installed binary and returns its path.
func installedAt(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolateConfigDir points the install-marker read at an empty directory, so a
// marker on the machine running the tests cannot decide anything here.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// pinLanguage renders in lang for the length of one test and puts the package
// pin back afterwards.
func pinLanguage(t *testing.T, lang string) {
	t.Helper()
	i18n.Load(lang)
	t.Cleanup(func() { i18n.Load("en") })
}

// assertDirHolds fails unless dir holds exactly the named entries, which is
// what catches a temp file left behind in the user's install directory.
func assertDirHolds(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

func hexSum(contents string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(contents)))
}

// outputCase is one branch that ends the command without an error, and the
// whole block it writes in each language. Whole blocks rather than phrases: the
// catalog gates cannot tell one key from another of the same shape, and a
// fragment assertion leaves every lookup it does not quote free to change.
type outputCase struct {
	name string
	// arrange sets up whatever the branch needs and returns the call plus the
	// two blocks it must produce.
	arrange func(t *testing.T) (call invocation, en, ja string)
}

func outputCases() []outputCase {
	return []outputCase{
		{
			name: "a completed upgrade",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				base := linuxRelease("new binary").start(t)
				return upgradeFrom(target, base),
					"Checking for the latest release.\n" +
						"v0.2.0 is available; you are on v0.1.1.\n" +
						"Downloading kamakiri-linux-amd64 into " + dir + ".\n" +
						"Checksum verified against checksums.txt.\n" +
						"Updated to v0.2.0 at " + target + ".\n",
					"最新リリースを確認しています。\n" +
						"v0.2.0 が利用可能です。現在のバージョンは v0.1.1 です。\n" +
						"kamakiri-linux-amd64 を " + dir + " にダウンロードしています。\n" +
						"チェックサムが checksums.txt と一致することを確認しました。\n" +
						target + " を v0.2.0 に更新しました。\n"
			},
		},
		{
			name: "the running version is the latest release",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "current binary")
				base := linuxRelease("newer binary").start(t)
				call := upgradeFrom(target, base)
				call.version = "v0.2.0"
				return call,
					"Checking for the latest release.\n" +
						"You are on v0.2.0, the latest release.\n",
					"最新リリースを確認しています。\n" +
						"現在のバージョン v0.2.0 は最新リリースです。\n"
			},
		},
		{
			name: "a Homebrew install",
			arrange: func(t *testing.T) (invocation, string, string) {
				base := linuxRelease("new binary").start(t)
				return upgradeFrom("/home/linuxbrew/.linuxbrew/Cellar/kamakiri/0.1.1/bin/kamakiri", base),
					"Checking for the latest release.\n" +
						"v0.2.0 is available; you are on v0.1.1.\n" +
						"Homebrew installed this copy of kamakiri. Run `brew upgrade kamakiri` to update it.\n",
					"最新リリースを確認しています。\n" +
						"v0.2.0 が利用可能です。現在のバージョンは v0.1.1 です。\n" +
						"この kamakiri は Homebrew でインストールされています。`brew upgrade kamakiri` を実行して更新してください。\n"
			},
		},
		{
			name: "a global npm install",
			arrange: func(t *testing.T) (invocation, string, string) {
				const installed = "/usr/local/lib/node_modules/kamakiri/node_modules/@kamakiri-labs/cli-linux-x64/bin/kamakiri"
				base := linuxRelease("new binary").start(t)
				return upgradeFrom(installed, base),
					"Checking for the latest release.\n" +
						"v0.2.0 is available; you are on v0.1.1.\n" +
						"npm installed this copy of kamakiri (" + installed + "). For a global install, run `npm install -g kamakiri@latest`. For a copy a project depends on, update that project's `kamakiri` dependency. For a copy npx fetched, there is nothing to update in place, and `npx kamakiri@latest` fetches the newest release.\n",
					"最新リリースを確認しています。\n" +
						"v0.2.0 が利用可能です。現在のバージョンは v0.1.1 です。\n" +
						"この kamakiri は npm でインストールされています (" + installed + ")。グローバルインストールの場合は `npm install -g kamakiri@latest` を実行してください。プロジェクトの依存関係としてインストールされている場合は、そのプロジェクトの `kamakiri` を更新してください。npx で取得した場合は、その場で更新するものはなく、`npx kamakiri@latest` が最新リリースを取得します。\n"
			},
		},
		{
			// The npm defer line has to hold for a copy a project owns as much
			// as for a global one: `npm install -g kamakiri@latest` would not touch
			// this binary, so the line names the path it is talking about and says
			// what updates a copy a project depends on. The path it names is the
			// platform package the launcher pulled in, not the `kamakiri`
			// dependency the project itself records, which is why the line names
			// that dependency rather than pointing at the path.
			name: "an npm install inside a project",
			arrange: func(t *testing.T) (invocation, string, string) {
				const installed = "/home/user/project/node_modules/@kamakiri-labs/cli-linux-x64/bin/kamakiri"
				base := linuxRelease("new binary").start(t)
				return upgradeFrom(installed, base),
					"Checking for the latest release.\n" +
						"v0.2.0 is available; you are on v0.1.1.\n" +
						"npm installed this copy of kamakiri (" + installed + "). For a global install, run `npm install -g kamakiri@latest`. For a copy a project depends on, update that project's `kamakiri` dependency. For a copy npx fetched, there is nothing to update in place, and `npx kamakiri@latest` fetches the newest release.\n",
					"最新リリースを確認しています。\n" +
						"v0.2.0 が利用可能です。現在のバージョンは v0.1.1 です。\n" +
						"この kamakiri は npm でインストールされています (" + installed + ")。グローバルインストールの場合は `npm install -g kamakiri@latest` を実行してください。プロジェクトの依存関係としてインストールされている場合は、そのプロジェクトの `kamakiri` を更新してください。npx で取得した場合は、その場で更新するものはなく、`npx kamakiri@latest` が最新リリースを取得します。\n"
			},
		},
		{
			// The install-method rules read the platform this flow was handed
			// rather than the one the tests run on, and a Windows path is what
			// says so: a backslash separates only there, so a detection reading
			// the host's platform would see one long segment, find no channel in
			// it, and go on to replace a copy npm owns.
			name: "an npm install on Windows",
			arrange: func(t *testing.T) (invocation, string, string) {
				const installed = `C:\Users\user\project\node_modules\@kamakiri-labs\cli-win32-x64\bin\kamakiri.exe`
				base := linuxRelease("new binary").start(t)
				call := upgradeFrom(installed, base)
				call.goos = "windows"
				return call,
					"Checking for the latest release.\n" +
						"v0.2.0 is available; you are on v0.1.1.\n" +
						"npm installed this copy of kamakiri (" + installed + "). For a global install, run `npm install -g kamakiri@latest`. For a copy a project depends on, update that project's `kamakiri` dependency. For a copy npx fetched, there is nothing to update in place, and `npx kamakiri@latest` fetches the newest release.\n",
					"最新リリースを確認しています。\n" +
						"v0.2.0 が利用可能です。現在のバージョンは v0.1.1 です。\n" +
						"この kamakiri は npm でインストールされています (" + installed + ")。グローバルインストールの場合は `npm install -g kamakiri@latest` を実行してください。プロジェクトの依存関係としてインストールされている場合は、そのプロジェクトの `kamakiri` を更新してください。npx で取得した場合は、その場で更新するものはなく、`npx kamakiri@latest` が最新リリースを取得します。\n"
			},
		},
	}
}

func TestRunOutputCopy(t *testing.T) {
	for _, testCase := range outputCases() {
		for _, lang := range []string{"en", "ja"} {
			t.Run(testCase.name+" in "+lang, func(t *testing.T) {
				isolateConfigDir(t)
				pinLanguage(t, lang)
				call, en, ja := testCase.arrange(t)
				want := en
				if lang == "ja" {
					want = ja
				}

				var out bytes.Buffer
				if err := call.call(&out); err != nil {
					t.Fatalf("run() error = %v, want nil", err)
				}
				if got := out.String(); got != want {
					t.Errorf("run() wrote:\n%q\nwant:\n%q", got, want)
				}
			})
		}
	}
}

// failureCase is one branch that ends the command with an error, and the whole
// line that error carries in each language.
type failureCase struct {
	name string
	// framed marks a branch whose copy is a frame around a reason from
	// elsewhere. Only the frame and its separator are compared then, since the
	// reason after it comes from the operating system or the network stack and
	// reads differently from one machine to the next.
	framed  bool
	arrange func(t *testing.T) (call invocation, en, ja string)
}

func failureCases() []failureCase {
	return []failureCase{
		{
			name: "an unstamped build",
			arrange: func(t *testing.T) (invocation, string, string) {
				// No request is made on this path, so the base URL names a host
				// that does not resolve rather than a server.
				const base = "https://releases.invalid" + repoPath
				call := upgradeFrom("/usr/local/bin/kamakiri", base)
				call.version = "(dev)"
				return call,
					"this build did not come from a release, so it cannot update itself. Install a release from " + base + "/releases",
					"このビルドはリリースから作成されたものではないため、自身を更新できません。" + base + "/releases からリリースをインストールしてください"
			},
		},
		{
			name: "no release to be found",
			arrange: func(t *testing.T) (invocation, string, string) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, repoPath+"/releases", http.StatusFound)
				}))
				t.Cleanup(server.Close)
				base := server.URL + repoPath
				return upgradeFrom("/usr/local/bin/kamakiri", base),
					"could not determine the latest release. Check " + base + "/releases",
					"最新リリースを確認できませんでした。" + base + "/releases をご確認ください"
			},
		},
		{
			name: "a build stamped with a version this CLI cannot read",
			arrange: func(t *testing.T) (invocation, string, string) {
				base := linuxRelease("new binary").start(t)
				call := upgradeFrom("/usr/local/bin/kamakiri", base)
				// A four-part stamp is the shape a mis-stamped build has
				// reported, and it is refused rather than guessed at. The
				// releases page answered perfectly well here, so the line says
				// which side could not be read.
				call.version = "v0.1.7.10"
				return call,
					"could not read this build's version (v0.1.7.10). Install a release from " + base + "/releases",
					"このビルドのバージョン (v0.1.7.10) を読み取れませんでした。" + base + "/releases からリリースをインストールしてください"
			},
		},
		{
			name:   "the releases site out of reach",
			framed: true,
			arrange: func(t *testing.T) (invocation, string, string) {
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				base := server.URL + repoPath
				server.Close()
				return upgradeFrom("/usr/local/bin/kamakiri", base),
					"request failed: ",
					"リクエストの送信: "
			},
		},
		{
			name: "an install directory that cannot be written to",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := readOnlyDir(t)
				target := filepath.Join(dir, "kamakiri")
				base := linuxRelease("new binary").start(t)
				return upgradeFrom(target, base),
					"could not write to " + dir + ". Re-run this command with the privileges the CLI was installed with",
					dir + " に書き込めませんでした。CLIをインストールしたときと同じ権限でもう一度実行してください"
			},
		},
		{
			name: "an asset the release does not carry",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				base := (&release{tag: "v0.2.0", assets: map[string][]byte{}}).start(t)
				return upgradeFrom(target, base),
					"could not download kamakiri-linux-amd64. The server returned 404",
					"kamakiri-linux-amd64 をダウンロードできませんでした。サーバーが404を返しました"
			},
		},
		{
			name: "checksums.txt missing from the release",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				serving.checksumsStatus = http.StatusNotFound
				return upgradeFrom(target, serving.start(t)),
					"could not download checksums.txt. The server returned 404",
					"checksums.txt をダウンロードできませんでした。サーバーが404を返しました"
			},
		},
		{
			name: "an asset past its cap",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				// Driven at a cap of 1 MB rather than the 200 MB a release
				// carries, so the refusal is exercised end to end without moving
				// the real cap's worth of bytes over the loopback.
				call := upgradeFrom(target, linuxRelease(strings.Repeat("x", oneMB+1)).start(t))
				call.maxAsset = oneMB
				return call,
					"kamakiri-linux-amd64 is too large (limit 1 MB)",
					"kamakiri-linux-amd64 が大きすぎます（上限 1 MB）"
			},
		},
		{
			name: "a checksums.txt past its cap",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				serving.checksums = strings.Repeat("x", maxChecksumsBytes+1)
				return upgradeFrom(target, serving.start(t)),
					"checksums.txt is too large (limit 1 MB)",
					"checksums.txt が大きすぎます（上限 1 MB）"
			},
		},
		{
			name: "a checksums.txt whose lines only nearly name the asset",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				// Three well-formed lines, none of them this asset's: a
				// neighbouring asset, a name the asset's own name begins with,
				// and a name that begins with the asset's own. The name field is
				// held to full equality, so neither near miss may stand in for
				// it however the two are compared.
				sum := hexSum("new binary")
				serving.checksums = sum + "  kamakiri-linux-arm64\n" +
					sum + "  kamakiri-linux-amd6\n" +
					sum + "  kamakiri-linux-amd64.sig\n"
				return upgradeFrom(target, serving.start(t)),
					"no line in checksums.txt names kamakiri-linux-amd64",
					"checksums.txt に kamakiri-linux-amd64 を示す行がありません"
			},
		},
		{
			name: "a checksums.txt whose sum for the asset is not a digest",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				// Both lines name the asset, and neither field is a sha256
				// digest: one is escape sequences a terminal would act on, the
				// other is the right length and the wrong alphabet. A sum that
				// reaches the mismatch line is printed to the user as it stands,
				// so a line is only a sum once it looks like one.
				serving.checksums = "\x1b[2J\x1b[H  kamakiri-linux-amd64\n" +
					strings.Repeat("z", 64) + "  kamakiri-linux-amd64\n"
				return upgradeFrom(target, serving.start(t)),
					"the line for kamakiri-linux-amd64 in checksums.txt does not carry a checksum (64 hexadecimal characters)",
					"checksums.txt の kamakiri-linux-amd64 の行に有効なチェックサム（16進数64文字）がありません"
			},
		},
		{
			name: "a download that does not match its checksum",
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				published := strings.Repeat("0", 64)
				serving.checksums = published + "  kamakiri-linux-amd64\n"
				return upgradeFrom(target, serving.start(t)),
					"kamakiri-linux-amd64 does not match its checksum. checksums.txt lists " + published + ", the download is " + hexSum("new binary"),
					"kamakiri-linux-amd64 がチェックサムと一致しません。checksums.txt の値は " + published + "、ダウンロードした値は " + hexSum("new binary") + " です"
			},
		},
		{
			name:   "a binary that cannot be replaced",
			framed: true,
			arrange: func(t *testing.T) (invocation, string, string) {
				dir := t.TempDir()
				// A non-empty directory standing where the binary belongs, so
				// the rename that would put the download in its place is
				// refused. It reaches the branch on any platform, without
				// needing a file this test does not own.
				target := filepath.Join(dir, "kamakiri")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				installedAt(t, target, "occupied", "in the way")
				return upgradeFrom(target, linuxRelease("new binary").start(t)),
					"could not replace the binary: ",
					"バイナリを置き換えられませんでした: "
			},
		},
	}
}

func TestRunFailureCopy(t *testing.T) {
	for _, testCase := range failureCases() {
		for _, lang := range []string{"en", "ja"} {
			t.Run(testCase.name+" in "+lang, func(t *testing.T) {
				isolateConfigDir(t)
				pinLanguage(t, lang)
				call, en, ja := testCase.arrange(t)
				want := en
				if lang == "ja" {
					want = ja
				}

				var out bytes.Buffer
				err := call.call(&out)
				if err == nil {
					t.Fatalf("run() = nil, want an error carrying %q", want)
				}
				if testCase.framed {
					if !strings.HasPrefix(err.Error(), want) {
						t.Errorf("run() error = %q, want it to open with %q", err.Error(), want)
					}
					return
				}
				if err.Error() != want {
					t.Errorf("run() error = %q, want %q", err.Error(), want)
				}
			})
		}
	}
}

// readOnlyDir returns a directory nothing may be created in, restoring its
// permissions afterwards so the test framework can remove it.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory whatever its permissions say")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	return dir
}

// The temp file is created before the download starts, so an install directory
// the user cannot write to fails in a moment rather than after bytes that then
// have nowhere to land. Nothing claims a download either: the line announcing
// one comes after the file it will be written into exists.
func TestRunFailsToWriteBeforeItDownloadsAnything(t *testing.T) {
	isolateConfigDir(t)
	dir := readOnlyDir(t)
	serving := linuxRelease("new binary")
	base := serving.start(t)

	var out bytes.Buffer
	if err := run("v0.1.1", filepath.Join(dir, "kamakiri"), base, "linux", "amd64", maxAssetBytes, &out); err == nil {
		t.Fatal("run() = nil, want the write failure")
	}

	want := "Checking for the latest release.\n" +
		"v0.2.0 is available; you are on v0.1.1.\n"
	if got := out.String(); got != want {
		t.Errorf("run() wrote %q, want %q: nothing may announce a download that never started", got, want)
	}
	for _, path := range serving.requested() {
		if strings.Contains(path, "/releases/download/") {
			t.Errorf("the release was asked for %q, so bytes were fetched before the directory was proven writable", path)
		}
	}
}

// The whole point of resolving through the redirect is that anything but a
// release tag is refused, including the redirect to the releases index that a
// repository with no qualifying release answers.
func TestRunRefusesEveryAnswerThatIsNotAReleaseTag(t *testing.T) {
	redirectTo := func(location string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, http.StatusFound)
		}
	}

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "the releases index a repository with no release answers", handler: redirectTo(repoPath + "/releases")},
		{name: "no such page", handler: http.NotFound},
		{
			name:    "a page rather than a redirect",
			handler: func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html></html>") },
		},
		{
			name:    "a redirect carrying no Location",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusFound) },
		},
		// The scheme and the host are compared separately, so each is refused on
		// its own: a Location differing in both would leave either comparison
		// free to be dropped without a test noticing.
		{name: "a Location on another host, under the scheme the request used", handler: redirectTo("http://example.invalid" + repoPath + "/releases/tag/v9.9.9")},
		{
			name: "a Location on the host the request went to, under another scheme",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+r.Host+repoPath+"/releases/tag/v9.9.9", http.StatusFound)
			},
		},
		{name: "a Location outside the releases path", handler: redirectTo(repoPath + "/tags/v9.9.9")},
		{name: "a Location naming another repository", handler: redirectTo("/someone-else/kamakiri/releases/tag/v9.9.9")},
		{name: "a tag segment that is not a version", handler: redirectTo(repoPath + "/releases/tag/latest")},
		{name: "a tag segment carrying a path of its own", handler: redirectTo(repoPath + "/releases/tag/v1.0.0/assets")},
		{name: "no tag segment at all", handler: redirectTo(repoPath + "/releases/tag/")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateConfigDir(t)
			server := httptest.NewServer(testCase.handler)
			t.Cleanup(server.Close)
			base := server.URL + repoPath

			var out bytes.Buffer
			err := run("v0.1.1", "/usr/local/bin/kamakiri", base, "linux", "amd64", maxAssetBytes, &out)

			want := "could not determine the latest release. Check " + base + "/releases"
			if err == nil || err.Error() != want {
				t.Errorf("run() error = %v, want %q", err, want)
			}
			// The milestone printed before the request stays on stdout, and
			// nothing after it is printed.
			if got := out.String(); got != "Checking for the latest release.\n" {
				t.Errorf("run() wrote %q, want only the checking line", got)
			}
		})
	}
}

// The tag is held to a length, and the two edges are driven together on
// purpose: a case at one of them alone would still pass with the bound set to
// another number, and only the pair says what that number is. The lengths are
// written out rather than derived from the constant for that same reason, since
// tags built from it would follow it to whatever it became. Both tags are versions by
// every other rule and differ in nothing but their length.
func TestRunBoundsTheTagLength(t *testing.T) {
	const prefix = "v0.2.0-"
	atCap := prefix + strings.Repeat("a", 64-len(prefix))
	pastCap := prefix + strings.Repeat("a", 65-len(prefix))

	t.Run("a tag the length of the cap", func(t *testing.T) {
		isolateConfigDir(t)
		dir := t.TempDir()
		target := installedAt(t, dir, "kamakiri", "old binary")
		serving := &release{tag: atCap, assets: map[string][]byte{"kamakiri-linux-amd64": []byte("new binary")}}
		base := serving.start(t)

		var out bytes.Buffer
		if err := upgradeFrom(target, base).call(&out); err != nil {
			t.Fatalf("run() error = %v, want the tag accepted and the upgrade carried out", err)
		}
		if !strings.Contains(out.String(), "Updated to "+atCap) {
			t.Errorf("run() wrote %q, want the closing line naming %q", out.String(), atCap)
		}
	})

	t.Run("a tag one byte past the cap", func(t *testing.T) {
		isolateConfigDir(t)
		dir := t.TempDir()
		target := installedAt(t, dir, "kamakiri", "old binary")
		serving := &release{tag: pastCap, assets: map[string][]byte{"kamakiri-linux-amd64": []byte("new binary")}}
		base := serving.start(t)

		var out bytes.Buffer
		err := upgradeFrom(target, base).call(&out)

		want := "could not determine the latest release. Check " + base + "/releases"
		if err == nil || err.Error() != want {
			t.Errorf("run() error = %v, want %q", err, want)
		}
		if got := out.String(); got != "Checking for the latest release.\n" {
			t.Errorf("run() wrote %q, want only the checking line", got)
		}
	})
}

// The edges above pin that the bound exists, but not where it sits: a check on
// either side of the parse refuses exactly the same tags, so nothing the
// command returns or prints tells the two apart. Where it sits is the point of
// the bound, since the parser allocates many times what it is handed and a
// check reading the length afterwards would have run too late. This reads the
// source instead.
func TestTagFromRedirectHoldsTheLengthBeforeTheParse(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "upgrade.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing upgrade.go: %v", err)
	}

	var lengthCheck, versionParse token.Pos
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if !ok || decl.Recv != nil || decl.Name.Name != "tagFromRedirect" {
			return true
		}
		ast.Inspect(decl.Body, func(inner ast.Node) bool {
			switch expr := inner.(type) {
			case *ast.Ident:
				if expr.Name == "maxTagBytes" && !lengthCheck.IsValid() {
					lengthCheck = expr.Pos()
				}
			case *ast.CallExpr:
				if types.ExprString(expr.Fun) == "semver.Parse" && !versionParse.IsValid() {
					versionParse = expr.Pos()
				}
			}
			return true
		})
		return false
	})

	if !versionParse.IsValid() {
		t.Fatal("tagFromRedirect does not call semver.Parse; this check is reading the wrong function")
	}
	if !lengthCheck.IsValid() {
		t.Fatal("tagFromRedirect does not read maxTagBytes; the tag reaches the parser unbounded")
	}
	if lengthCheck > versionParse {
		t.Error("tagFromRedirect reads maxTagBytes after calling semver.Parse, want the length held ahead of the parse")
	}
}

// A release at or below the running version ends the command without fetching
// anything.
func TestRunDownloadsNothingWhenItIsAlreadyCurrent(t *testing.T) {
	cases := []struct {
		name    string
		running string
		tag     string
	}{
		{name: "the same version", running: "v0.2.0", tag: "v0.2.0"},
		{name: "a release older than the running build", running: "v0.3.0", tag: "v0.2.0"},
		{name: "a prerelease of the version already running", running: "v0.2.0", tag: "v0.2.0-rc.1"},
		{name: "a stamp written without its leading v", running: "0.2.0", tag: "v0.2.0"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateConfigDir(t)
			dir := t.TempDir()
			target := installedAt(t, dir, "kamakiri", "current binary")
			serving := &release{tag: testCase.tag, assets: map[string][]byte{"kamakiri-linux-amd64": []byte("newer binary")}}
			base := serving.start(t)

			var out bytes.Buffer
			if err := run(testCase.running, target, base, "linux", "amd64", maxAssetBytes, &out); err != nil {
				t.Fatalf("run() error = %v, want nil", err)
			}

			want := "Checking for the latest release.\n" +
				"You are on " + testCase.running + ", the latest release.\n"
			if got := out.String(); got != want {
				t.Errorf("run() wrote %q, want %q", got, want)
			}
			for _, path := range serving.requested() {
				if strings.Contains(path, "/releases/download/") {
					t.Errorf("the release was asked for %q, and nothing needed downloading", path)
				}
			}
			assertDirHolds(t, dir, "kamakiri")
		})
	}
}

// The self-replace, driven at a binary in a directory of its own rather than at
// the test binary: what lands is the published bytes, executable, with nothing
// left beside it.
func TestRunReplacesTheBinaryInPlace(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri", "old binary")
	base := linuxRelease("new binary").start(t)

	var out bytes.Buffer
	if err := run("v0.1.1", target, base, "linux", "amd64", maxAssetBytes, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new binary" {
		t.Errorf("%s holds %q, want the downloaded bytes", target, contents)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("%s is mode %v, want 0755: the replacement has to be runnable", target, perm)
	}
	assertDirHolds(t, dir, "kamakiri")
}

// The tag reaches the download URLs exactly as the release names it. Stripping
// the leading v would build a URL for a tag that does not exist, and the 404
// would read as a missing release rather than as a bug here.
func TestRunFetchesTheTagAsPublished(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri", "old binary")
	serving := linuxRelease("new binary")
	base := serving.start(t)

	var out bytes.Buffer
	if err := run("v0.1.1", target, base, "linux", "amd64", maxAssetBytes, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	want := []string{
		repoPath + "/releases/latest",
		repoPath + "/releases/download/v0.2.0/kamakiri-linux-amd64",
		repoPath + "/releases/download/v0.2.0/checksums.txt",
	}
	if got := serving.requested(); !slices.Equal(got, want) {
		t.Errorf("the release was asked for %v, want %v", got, want)
	}
}

// A digest spelled in uppercase is the same digest. Nothing published writes it
// that way, sha256sum's own output being lowercase, but a case-sensitive
// comparison would refuse a good download over a spelling.
func TestRunAcceptsAnUppercaseChecksum(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri", "old binary")
	serving := linuxRelease("new binary")
	serving.checksums = strings.ToUpper(hexSum("new binary")) + "  kamakiri-linux-amd64\n"
	base := serving.start(t)

	var out bytes.Buffer
	if err := run("v0.1.1", target, base, "linux", "amd64", maxAssetBytes, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
}

// A line naming the asset with a first field that is not a digest is skipped
// rather than taken as the file's answer for it, so the first usable line wins
// wherever it sits. Refusing on the unusable one would turn a release whose
// checksums file carries one bad line into a release nobody can upgrade to.
func TestRunTakesTheFirstUsableChecksumLine(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri", "old binary")
	serving := linuxRelease("new binary")
	serving.checksums = "not-a-digest  kamakiri-linux-amd64\n" +
		hexSum("new binary") + "  kamakiri-linux-amd64\n"
	base := serving.start(t)

	var out bytes.Buffer
	if err := run("v0.1.1", target, base, "linux", "amd64", maxAssetBytes, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new binary" {
		t.Errorf("%s holds %q, want the downloaded bytes", target, contents)
	}
}

// Every way phases 5 and 6 end short of a completed replace leaves the install
// directory as it found it. The cleanup is a property of the flow rather than
// of any one branch, and this is what says so.
func TestRunLeavesNoTempFileBehind(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T) (call invocation, dir string, want []string)
	}{
		{
			name: "an asset the release does not carry",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				base := (&release{tag: "v0.2.0", assets: map[string][]byte{}}).start(t)
				return upgradeFrom(target, base), dir, []string{"kamakiri"}
			},
		},
		{
			name: "checksums.txt missing from the release",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				serving.checksumsStatus = http.StatusNotFound
				return upgradeFrom(target, serving.start(t)), dir, []string{"kamakiri"}
			},
		},
		{
			name: "an asset past its cap",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				call := upgradeFrom(target, linuxRelease(strings.Repeat("x", oneMB+1)).start(t))
				call.maxAsset = oneMB
				return call, dir, []string{"kamakiri"}
			},
		},
		{
			name: "a checksums.txt past its cap",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				serving.checksums = strings.Repeat("x", maxChecksumsBytes+1)
				return upgradeFrom(target, serving.start(t)), dir, []string{"kamakiri"}
			},
		},
		{
			name: "no line naming the asset",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				serving.checksums = "\n"
				return upgradeFrom(target, serving.start(t)), dir, []string{"kamakiri"}
			},
		},
		{
			name: "a download that does not match its checksum",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := installedAt(t, dir, "kamakiri", "old binary")
				serving := linuxRelease("new binary")
				serving.checksums = strings.Repeat("0", 64) + "  kamakiri-linux-amd64\n"
				return upgradeFrom(target, serving.start(t)), dir, []string{"kamakiri"}
			},
		},
		{
			name: "a binary that cannot be replaced",
			arrange: func(t *testing.T) (invocation, string, []string) {
				dir := t.TempDir()
				target := filepath.Join(dir, "kamakiri")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				installedAt(t, target, "occupied", "in the way")
				return upgradeFrom(target, linuxRelease("new binary").start(t)), dir, []string{"kamakiri"}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateConfigDir(t)
			call, dir, want := testCase.arrange(t)

			var out bytes.Buffer
			if err := call.call(&out); err == nil {
				t.Fatal("run() = nil, want the failure this case arranges")
			}
			assertDirHolds(t, dir, want...)
		})
	}
}

// The aside a Windows upgrade leaves behind is cleared at the start of the next
// `upgrade` run whatever that run goes on to do, which is what makes a user who
// upgrades once and then only checks stop carrying it. A cleanup moved into the
// replace phase passes every other test here and fails this one.
func TestRunClearsTheStaleAsideOnAnInvocationThatUpgradesNothing(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri.exe", "current binary")
	installedAt(t, dir, "kamakiri.exe.old", "the binary a previous upgrade replaced")
	base := (&release{tag: "v0.2.0", assets: map[string][]byte{}}).start(t)

	var out bytes.Buffer
	if err := run("v0.2.0", target, base, "windows", "amd64", maxAssetBytes, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	assertDirHolds(t, dir, "kamakiri.exe")
}

// The aside only exists on Windows, so nothing else may delete a neighbouring
// file that happens to be named that way.
func TestRunLeavesAnAsideAloneOffWindows(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri", "current binary")
	installedAt(t, dir, "kamakiri.old", "a file of the user's own")
	base := (&release{tag: "v0.2.0", assets: map[string][]byte{}}).start(t)

	var out bytes.Buffer
	if err := run("v0.2.0", target, base, "linux", "amd64", maxAssetBytes, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	assertDirHolds(t, dir, "kamakiri", "kamakiri.old")
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{goos: "darwin", goarch: "amd64", want: "kamakiri-darwin-amd64"},
		{goos: "darwin", goarch: "arm64", want: "kamakiri-darwin-arm64"},
		{goos: "linux", goarch: "amd64", want: "kamakiri-linux-amd64"},
		{goos: "linux", goarch: "arm64", want: "kamakiri-linux-arm64"},
		{goos: "windows", goarch: "amd64", want: "kamakiri-windows-amd64.exe"},
		{goos: "windows", goarch: "arm64", want: "kamakiri-windows-arm64.exe"},
	}

	for _, testCase := range cases {
		t.Run(testCase.goos+"/"+testCase.goarch, func(t *testing.T) {
			if got := assetName(testCase.goos, testCase.goarch); got != testCase.want {
				t.Errorf("assetName(%q, %q) = %q, want %q", testCase.goos, testCase.goarch, got, testCase.want)
			}
		})
	}
}

// Off Windows a replacement is one rename inside the install directory, so it
// never crosses a filesystem and no window exists where the binary is missing.
func TestReplaceBinaryRenamesOverTheTarget(t *testing.T) {
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri", "old binary")
	tmp := installedAt(t, dir, ".tmp-download", "new binary")

	if err := replaceBinary(tmp, target, "linux"); err != nil {
		t.Fatalf("replaceBinary() error = %v", err)
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new binary" {
		t.Errorf("%s holds %q, want the downloaded bytes", target, contents)
	}
	assertDirHolds(t, dir, "kamakiri")
}

// The Windows dance, driven against plain files: a running image can be renamed
// but not overwritten, so the binary is moved aside first. Its interaction with
// a genuinely running exe is out of reach here, and that is what the aside
// delete is about; on any other platform the delete succeeds and leaves nothing.
func TestReplaceBinaryMovesTheTargetAsideOnWindows(t *testing.T) {
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri.exe", "old binary")
	tmp := installedAt(t, dir, ".tmp-download", "new binary")

	if err := replaceBinary(tmp, target, "windows"); err != nil {
		t.Fatalf("replaceBinary() error = %v", err)
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new binary" {
		t.Errorf("%s holds %q, want the downloaded bytes", target, contents)
	}
	assertDirHolds(t, dir, "kamakiri.exe")
}

// If the second rename fails the user still has a working binary: the aside is
// moved back. Without the compensating move they would be left with no
// `kamakiri.exe` at all.
func TestReplaceBinaryPutsTheTargetBackWhenTheSecondRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := installedAt(t, dir, "kamakiri.exe", "old binary")
	missing := filepath.Join(dir, ".tmp-never-written")

	if err := replaceBinary(missing, target, "windows"); err == nil {
		t.Fatal("replaceBinary() = nil, want the failure of the second rename")
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading %s: %v; the original binary was not put back", target, err)
	}
	if string(contents) != "old binary" {
		t.Errorf("%s holds %q, want the original bytes", target, contents)
	}
	assertDirHolds(t, dir, "kamakiri.exe")
}

// failingWriter stands in for a disk with no room left: it takes nothing and
// reports the same failure every time. Its close reports nothing, so the copy's
// own failure is the one that surfaces.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func (f failingWriter) Close() error { return nil }

// A copy reports one error whether the body could not be read or the file could
// not be written, and the two branches need different copy: naming the network
// for a disk that is full sends the user looking in the wrong place, and the
// cause itself is what says which local problem it was.
func TestDownloadNamesTheDirectoryWhenItCannotWrite(t *testing.T) {
	cases := []struct{ lang, want string }{
		{lang: "en", want: "could not write the download to /install/dir: no space left on device"},
		{lang: "ja", want: "ダウンロードしたファイルを /install/dir に書き込めませんでした: no space left on device"},
	}

	for _, testCase := range cases {
		t.Run(testCase.lang, func(t *testing.T) {
			pinLanguage(t, testCase.lang)
			base := linuxRelease("new binary").start(t)
			full := failingWriter{errors.New("no space left on device")}

			_, err := download(full, "/install/dir", base, "v0.2.0", "kamakiri-linux-amd64", maxAssetBytes)
			if err == nil {
				t.Fatalf("download() = nil, want %q", testCase.want)
			}
			if err.Error() != testCase.want {
				t.Errorf("download() error = %q, want %q", err.Error(), testCase.want)
			}
		})
	}
}

// closingFailsWriter takes every byte and fails only when it is closed, which is
// how a filesystem that buffers writes reports a disk with no room left.
type closingFailsWriter struct{ err error }

func (c closingFailsWriter) Write(p []byte) (int, error) { return len(p), nil }

func (c closingFailsWriter) Close() error { return c.err }

// Some filesystems only report a full disk at close, by which point the digest
// has already been taken from the bytes the copy handed over. A close failure
// left unreported would therefore pass verification and rename a short file over
// the user's binary, so it ends the command with the same line a write failure
// mid-copy carries.
func TestDownloadReportsAFailureToClose(t *testing.T) {
	cases := []struct{ lang, want string }{
		{lang: "en", want: "could not write the download to /install/dir: no space left on device"},
		{lang: "ja", want: "ダウンロードしたファイルを /install/dir に書き込めませんでした: no space left on device"},
	}

	for _, testCase := range cases {
		t.Run(testCase.lang, func(t *testing.T) {
			pinLanguage(t, testCase.lang)
			base := linuxRelease("new binary").start(t)
			full := closingFailsWriter{errors.New("no space left on device")}

			sum, err := download(full, "/install/dir", base, "v0.2.0", "kamakiri-linux-amd64", maxAssetBytes)
			if err == nil {
				t.Fatalf("download() = %q, nil; want the error %q", sum, testCase.want)
			}
			if err.Error() != testCase.want {
				t.Errorf("download() error = %q, want %q", err.Error(), testCase.want)
			}
		})
	}
}

// failingBothWriter fails both the write and the close, with different
// errors, so a test can tell which one the reported line actually came from.
type failingBothWriter struct{ writeErr, closeErr error }

func (f failingBothWriter) Write([]byte) (int, error) { return 0, f.writeErr }

func (f failingBothWriter) Close() error { return f.closeErr }

// When both the write and the close fail, the write's error is the one the
// user sees: it names what actually went wrong, and the close failure adds
// nothing once the download itself has already failed.
func TestDownloadPrefersTheWriteFailureOverTheCloseFailure(t *testing.T) {
	cases := []struct{ lang, want string }{
		{lang: "en", want: "could not write the download to /install/dir: no space left on device"},
		{lang: "ja", want: "ダウンロードしたファイルを /install/dir に書き込めませんでした: no space left on device"},
	}

	for _, testCase := range cases {
		t.Run(testCase.lang, func(t *testing.T) {
			pinLanguage(t, testCase.lang)
			base := linuxRelease("new binary").start(t)
			both := failingBothWriter{
				writeErr: errors.New("no space left on device"),
				closeErr: errors.New("file already closed"),
			}

			_, err := download(both, "/install/dir", base, "v0.2.0", "kamakiri-linux-amd64", maxAssetBytes)
			if err == nil {
				t.Fatalf("download() = nil, want %q", testCase.want)
			}
			if err.Error() != testCase.want {
				t.Errorf("download() error = %q, want %q", err.Error(), testCase.want)
			}
		})
	}
}

// A cap is a refusal, not a truncation: the bytes land in the user's install
// directory, so a body that keeps coming has to fail rather than be cut short.
func TestCopyCapped(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		limit int64
		want  string
	}{
		{name: "under the cap", body: "abc", limit: 8, want: "abc"},
		{name: "exactly at the cap", body: "abcdefgh", limit: 8, want: "abcdefgh"},
		{name: "one byte past the cap", body: "abcdefghi", limit: 8},
		{name: "an empty body", body: "", limit: 8},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var got bytes.Buffer
			err := copyCapped(&got, strings.NewReader(testCase.body), testCase.limit)

			if int64(len(testCase.body)) > testCase.limit {
				if err != errTooLarge {
					t.Fatalf("copyCapped() error = %v, want errTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("copyCapped() error = %v", err)
			}
			if got.String() != testCase.want {
				t.Errorf("copyCapped() wrote %q, want %q", got.String(), testCase.want)
			}
		})
	}
}

// Run's whole job beyond run is to bind the four values a test cannot drive:
// the releases site, the platform this binary was built for, and the cap the
// download is held to. Nothing it prints or returns depends on them before a
// request leaves the machine, so no comparison of its output can reach the
// binding; this reads the source instead, and replacing any of them with a
// literal turns it red.
func TestRunBindsTheReleasesSiteAndTheRunningPlatform(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "upgrade.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing upgrade.go: %v", err)
	}

	var got []string
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if !ok || decl.Recv != nil || decl.Name.Name != "Run" {
			return true
		}
		ast.Inspect(decl.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || name.Name != "run" {
				return true
			}
			found = true
			for _, arg := range call.Args {
				got = append(got, types.ExprString(arg))
			}
			return false
		})
		return false
	})

	if !found {
		t.Fatal("Run does not call run; this check is reading the wrong function")
	}
	want := []string{"version", "execPath", "releasesBaseURL", "runtime.GOOS", "runtime.GOARCH", "maxAssetBytes", "out"}
	if !slices.Equal(got, want) {
		t.Errorf("Run calls run(%s), want run(%s)", strings.Join(got, ", "), strings.Join(want, ", "))
	}
}
