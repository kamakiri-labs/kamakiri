package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// extractTarballEntries reads a tarball and returns sorted entry names.
func extractTarballEntries(t *testing.T, r io.Reader) []string {
	t.Helper()
	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	return names
}

func TestCreateBasicTarball(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hi</h1>"), 0644)
	os.MkdirAll(filepath.Join(dir, "css"), 0755)
	os.WriteFile(filepath.Join(dir, "css", "style.css"), []byte("body{}"), 0644)

	r, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer r.Close()

	if stats.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", stats.FileCount)
	}
	if stats.TotalSize != int64(len("<h1>Hi</h1>")+len("body{}")) {
		t.Errorf("TotalSize = %d, want %d", stats.TotalSize, len("<h1>Hi</h1>")+len("body{}"))
	}

	entries := extractTarballEntries(t, r)
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(entries))
	}

	found := map[string]bool{}
	for _, e := range entries {
		found[e] = true
	}
	if !found["index.html"] {
		t.Error("missing index.html in tarball")
	}
	if !found["css/style.css"] {
		t.Error("missing css/style.css in tarball")
	}
}

func TestCreateSkipsAllDotfiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hi"), 0644)
	os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("ds"), 0644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=x"), 0644)
	os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0755)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte("ci"), 0644)

	r, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer r.Close()

	entries := extractTarballEntries(t, r)
	for _, e := range entries {
		if strings.HasPrefix(filepath.Base(e), ".") {
			t.Errorf("dotfile should not be in tarball: %q", e)
		}
	}

	if stats.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1 (only index.html)", stats.FileCount)
	}
}

func TestCreateEmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	r, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer r.Close()

	if stats.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0", stats.FileCount)
	}
	if stats.TotalSize != 0 {
		t.Errorf("TotalSize = %d, want 0", stats.TotalSize)
	}
}

func TestCreateReturnsCorrectStats(t *testing.T) {
	dir := t.TempDir()
	content := "hello world"
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte(content), 0644)

	_, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if stats.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", stats.FileCount)
	}
	wantSize := int64(len(content) * 2)
	if stats.TotalSize != wantSize {
		t.Errorf("TotalSize = %d, want %d", stats.TotalSize, wantSize)
	}
}

func TestCreateNonexistentDirectory(t *testing.T) {
	_, _, err := Create("/nonexistent/path/that/does/not/exist", MaxTotalSize)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestCreateRejectsSymlinkOutsideDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "legit.txt"), []byte("ok"), 0644)

	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("sensitive"), 0644)

	// A symlink whose target escapes the upload root must be rejected, or a
	// crafted tree could exfiltrate files outside the deploy directory.
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "escape.txt"))

	_, _, err := Create(dir, MaxTotalSize)
	if err == nil {
		t.Fatal("expected error for symlink pointing outside directory")
	}
}

func TestCreateAllowsSymlinkInsideDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("content"), 0644)
	os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt"))

	r, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer r.Close()

	if stats.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2 (real + symlink)", stats.FileCount)
	}

	entries := extractTarballEntries(t, r)
	found := map[string]bool{}
	for _, e := range entries {
		found[e] = true
	}
	if !found["real.txt"] || !found["link.txt"] {
		t.Errorf("expected both real.txt and link.txt, got %v", entries)
	}
}

func TestCreateEnforcesFileCountLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= MaxFileCount; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("file_%d.txt", i)), []byte("x"), 0644)
	}

	_, stats, err := Create(dir, MaxTotalSize)
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("Create() error = %v, want ErrTooManyFiles", err)
	}
	// The count is checked before a file is written, so the stats stop at the
	// cap rather than counting the file that was refused.
	if stats.FileCount != MaxFileCount {
		t.Errorf("stats.FileCount = %d, want %d", stats.FileCount, MaxFileCount)
	}

	// The sentinel above never reaches the user: the deploy path translates it
	// into its own message, which nothing else asserts on. The same directory
	// serves both, since walking it twice is cheaper than building a second one.
	_, prepErr := prepareDirTarball(dir, MaxTotalSize)
	if prepErr == nil {
		t.Fatal("prepareDirTarball() = nil, want the file-count message")
	}
	if want := "upload exceeds 20,000 file limit. Reduce your build output and try again"; prepErr.Error() != want {
		t.Errorf("prepareDirTarball() error = %q, want %q", prepErr.Error(), want)
	}
}

// The size limit is the caller's, measured on the uncompressed total: a tree
// exactly at it packs and one byte over it is refused. The deploy path words
// the refusal with the limit it was given, so the user reads the figure the
// check actually used.
func TestCreateEnforcesTheSizeLimitItIsGiven(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.bin"), bytes.Repeat([]byte("x"), 1536), 0644)
	os.WriteFile(filepath.Join(dir, "b.bin"), bytes.Repeat([]byte("y"), 512), 0644)

	r, stats, err := Create(dir, 2048)
	if err != nil {
		t.Fatalf("Create(dir, 2048) error = %v, want nil at exactly the limit", err)
	}
	r.Close()
	if stats.TotalSize != 2048 {
		t.Errorf("TotalSize = %d, want 2048", stats.TotalSize)
	}

	if _, _, err := Create(dir, 2047); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Create(dir, 2047) error = %v, want ErrTooLarge", err)
	}

	_, prepErr := prepareDirTarball(dir, 1024)
	if prepErr == nil {
		t.Fatal("prepareDirTarball(dir, 1024) = nil, want the size message")
	}
	if want := "upload exceeds 0 MB limit. Reduce your build output and try again"; prepErr.Error() != want {
		t.Errorf("prepareDirTarball(dir, 1024) error = %q, want %q", prepErr.Error(), want)
	}
}

func TestCreateSkipsSymlinkToHiddenFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=x"), 0644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hi</h1>"), 0644)
	// A non-hidden name symlinked to a dotfile must still be skipped,
	// otherwise the dotfile filter could be bypassed to leak a .env secret.
	os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "config"))

	r, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer r.Close()

	if stats.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1 (only index.html)", stats.FileCount)
	}

	entries := extractTarballEntries(t, r)
	for _, e := range entries {
		if e == "config" {
			t.Error("symlink to hidden file should not be in tarball")
		}
	}
}

func TestTempTarballCloseRemovesTheFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "kamakiri-deploy-*.tar.gz")
	if err != nil {
		t.Fatalf("os.CreateTemp: %v", err)
	}
	name := f.Name()

	// The wrapper is driven against a file that is still on disk, which is what
	// Create leaves behind on Windows, where its unlink of the open file fails.
	// Going through Create here would prove nothing on Unix: the early unlink
	// has already removed the name before Close runs.
	archive := &tempTarball{File: f, name: name}
	if err := archive.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) error = %v, want a not-exist error: Close left the temp file behind", name, err)
	}
}

func TestPrepareDirTarballReturnsAMeasuredRewoundArchive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hi</h1>"), 0644)

	info, err := prepareDirTarball(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("prepareDirTarball() error = %v", err)
	}
	defer info.reader.Close()

	if info.fileCount != 1 {
		t.Errorf("fileCount = %d, want 1", info.fileCount)
	}

	// The measuring seeks run inside prepareDirTarball, so the size it reports
	// and the reader it hands to the upload must agree: everything readable from
	// the current position is what gets uploaded under that content length.
	body, err := io.ReadAll(info.reader)
	if err != nil {
		t.Fatalf("io.ReadAll(reader): %v", err)
	}
	if int64(len(body)) != info.size {
		t.Errorf("read %d bytes from the reader, size = %d: the archive was not rewound to the start", len(body), info.size)
	}

	entries := extractTarballEntries(t, bytes.NewReader(body))
	if len(entries) != 1 || entries[0] != "index.html" {
		t.Errorf("entries = %v, want [index.html]", entries)
	}
}

func TestCreateReturnsStatsOnLimitError(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= MaxFileCount; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("file_%d.txt", i)), []byte("x"), 0644)
	}

	_, stats, err := Create(dir, MaxTotalSize)
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("Create() error = %v, want ErrTooManyFiles", err)
	}
	if stats.FileCount != MaxFileCount {
		t.Errorf("stats.FileCount = %d, want %d: the stats hold what was packed before the refusal", stats.FileCount, MaxFileCount)
	}
}

// writeFiles creates each file under root with its contents, making the
// directories on the way. Names use forward slashes.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("os.MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
	}
}

// symlinkOrSkip makes a link at root/link pointing at target, both written
// with forward slashes, and skips the test where the platform refuses to create
// symlinks (Windows without the privilege).
func symlinkOrSkip(t *testing.T, root, link, target string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(link))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := os.Symlink(filepath.FromSlash(target), path); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
}

// readTarball returns the entry names in the order they were written, and each
// entry's contents by name.
func readTarball(t *testing.T, r io.Reader) ([]string, map[string]string) {
	t.Helper()
	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	var names []string
	contents := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		names = append(names, hdr.Name)
		contents[hdr.Name] = string(body)
	}
	return names, contents
}

// packTree packs dir under MaxTotalSize, failing the test on any error, and
// returns what readTarball reads back along with the stats Create reported.
func packTree(t *testing.T, dir string) ([]string, map[string]string, FileStats) {
	t.Helper()
	r, stats, err := Create(dir, MaxTotalSize)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer r.Close()
	names, contents := readTarball(t, r)
	return names, contents, stats
}

// lowerMaxLinkedDirs lowers the linked-directory bound for one test, so a
// fan-out can be exercised without building thousands of directories. The bound
// is package state, so a test that lowers it must not run in parallel.
func lowerMaxLinkedDirs(t *testing.T, n int) {
	t.Helper()
	saved := maxLinkedDirs
	maxLinkedDirs = n
	t.Cleanup(func() { maxLinkedDirs = saved })
}

// loopLine and fanoutLine are the two directory-link refusals in full, written
// out rather than looked up so that a change to the catalog's wording fails
// here. Create names a link with the platform's separator.
func loopLine(link string) string {
	return fmt.Sprintf("symlink %s leads back to a directory already being packed, so packing would never end. Remove the link or point it elsewhere and try again", filepath.FromSlash(link))
}

func fanoutLine(limit int, link string) string {
	return fmt.Sprintf("symlink %s leads past the limit of %d directories reached through links. Use fewer links, or link smaller directories, and try again", filepath.FromSlash(link), limit)
}

func pathTooLongLine(link string) string {
	return fmt.Sprintf("symlink %s leads to a packed path longer than 4,096 bytes. Link a shallower directory, or shorten the names below the link, and try again", filepath.FromSlash(link))
}

// createError runs Create on dir, expecting it to fail, and returns the error.
func createError(t *testing.T, dir string) error {
	t.Helper()
	r, _, err := Create(dir, MaxTotalSize)
	if err == nil {
		r.Close()
		t.Fatal("Create() error = nil, want a refusal")
	}
	return err
}

func TestCreateFollowsADirectoryLink(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"shared/a.txt":     "shared a",
		"shared/sub/b.txt": "shared b",
	})
	symlinkOrSkip(t, dir, "site", "shared")

	_, contents, stats := packTree(t, dir)

	want := map[string]string{
		"shared/a.txt":     "shared a",
		"shared/sub/b.txt": "shared b",
		"site/a.txt":       "shared a",
		"site/sub/b.txt":   "shared b",
	}
	if !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
	if stats.FileCount != 4 {
		t.Errorf("FileCount = %d, want 4", stats.FileCount)
	}
}

func TestCreateRefusesALinkToADirectoryItIsInside(t *testing.T) {
	t.Run("the root", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"index.html": "hi"})
		symlinkOrSkip(t, dir, "loop", ".")

		if err := createError(t, dir); err.Error() != loopLine("loop") {
			t.Errorf("Create() error = %q, want %q", err, loopLine("loop"))
		}
	})

	t.Run("a mid-level ancestor", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"index.html":     "hi",
			"a/b/c/page.txt": "page",
		})
		symlinkOrSkip(t, dir, "a/b/c/up", "../..")

		if err := createError(t, dir); err.Error() != loopLine("a/b/c/up") {
			t.Errorf("Create() error = %q, want %q", err, loopLine("a/b/c/up"))
		}
	})
}

// The cycle closes at b's link back to a: a is on the descent path by then,
// having been entered as a plain directory at the start of the walk.
func TestCreateRefusesATwoLinkCycle(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a/one.txt": "one",
		"b/two.txt": "two",
	})
	symlinkOrSkip(t, dir, "a/to_b", "../b")
	symlinkOrSkip(t, dir, "b/to_a", "../a")

	if err := createError(t, dir); err.Error() != loopLine("a/to_b/to_a") {
		t.Errorf("Create() error = %q, want %q", err, loopLine("a/to_b/to_a"))
	}
}

// x sorts before z, so the walk reaches z/b first through x and z through
// x/y. Entering z's plain subdirectory b then repeats z/b, and the link named
// is the innermost one on the descent path, x/y, rather than the plain
// directory the repeat was found at.
func TestCreateRefusesACycleClosedByAPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"z/b/page.txt": "page"})
	symlinkOrSkip(t, dir, "x", "z/b")
	symlinkOrSkip(t, dir, "z/b/y", "..")

	if err := createError(t, dir); err.Error() != loopLine("x/y") {
		t.Errorf("Create() error = %q, want %q", err, loopLine("x/y"))
	}
}

func TestCreateSkipsALinkToADotDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html":         "hi",
		".hidden/secret.txt": "secret",
	})
	symlinkOrSkip(t, dir, "visible", ".hidden")

	_, contents, _ := packTree(t, dir)

	if want := map[string]string{"index.html": "hi"}; !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// Only the link's own name and the target's last component are checked, so a
// build output inside a dot-directory can be published through a plain link,
// while the dot-directory itself stays out at its own path.
func TestCreateFollowsALinkIntoADotDirectorysSubtree(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".vitepress/config.mts":      "config",
		".vitepress/dist/index.html": "docs home",
	})
	symlinkOrSkip(t, dir, "docs", ".vitepress/dist")

	_, contents, _ := packTree(t, dir)

	if want := map[string]string{"docs/index.html": "docs home"}; !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

func TestCreateRefusesADirectoryLinkLeavingTheTree(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.txt": "sensitive"})
	symlinkOrSkip(t, dir, "ext", filepath.ToSlash(outside))

	resolved, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks: %v", err)
	}
	want := fmt.Sprintf("symlink ext points outside directory: %s", resolved)
	if err := createError(t, dir); err.Error() != want {
		t.Errorf("Create() error = %q, want %q", err, want)
	}
}

func TestCreateSkipsDotEntriesInsideALinkedDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"shared/a.txt":          "a",
		"shared/.env":           "SECRET=x",
		"shared/.git/config":    "git",
		"shared/sub/b.txt":      "b",
		"shared/sub/.DS_Store":  "ds",
		"shared/sub/.cache/c.x": "c",
	})
	symlinkOrSkip(t, dir, "site", "shared")

	_, contents, _ := packTree(t, dir)

	want := map[string]string{
		"shared/a.txt":     "a",
		"shared/sub/b.txt": "b",
		"site/a.txt":       "a",
		"site/sub/b.txt":   "b",
	}
	if !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// The plain tree holds 1000 bytes; the link packs a second copy, so the deploy
// carries 2000. The limit is checked before a file is written, so on refusal
// the stats hold only what was packed before it: the first copy.
func TestCreateCountsFilesReachedThroughLinksTowardTheCaps(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"shared/a.bin": strings.Repeat("x", 1000)})
	symlinkOrSkip(t, dir, "copy", "shared")

	r, stats, err := Create(dir, 2000)
	if err != nil {
		t.Fatalf("Create(dir, 2000) error = %v", err)
	}
	r.Close()
	if want := (FileStats{FileCount: 2, TotalSize: 2000}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	_, stats, err = Create(dir, 1999)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Create(dir, 1999) error = %v, want ErrTooLarge", err)
	}
	if want := (FileStats{FileCount: 1, TotalSize: 1000}); stats != want {
		t.Errorf("stats on refusal = %+v, want %+v", stats, want)
	}
}

// Two links to one directory are not on each other's descent path, so both
// pack; a global set of directories already visited would drop the second.
func TestCreatePacksDuplicateLinksToOneDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"assets/logo.png": "png",
		"en/index.html":   "en",
		"ja/index.html":   "ja",
	})
	symlinkOrSkip(t, dir, "en/assets", "../assets")
	symlinkOrSkip(t, dir, "ja/assets", "../assets")

	_, contents, _ := packTree(t, dir)

	want := map[string]string{
		"assets/logo.png":    "png",
		"en/assets/logo.png": "png",
		"en/index.html":      "en",
		"ja/assets/logo.png": "png",
		"ja/index.html":      "ja",
	}
	if !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// Each of l0 to l29 holds two links, a and b, to the next level, so an
// unbounded walk would enter about two billion directories while holding no
// file for either cap to count. With the bound at 31 the walk goes down the a
// links, entering l1 to l30 (30 linked directories), backs up one level and
// takes l29's b link to l30 (the 31st), then backs up to l28 and takes its b
// link to l29, the 32nd, which is refused.
func TestCreateBoundsAFanOutOfDirectoryLinks(t *testing.T) {
	const depth = 30
	dir := t.TempDir()
	for level := 0; level < depth; level++ {
		next := fmt.Sprintf("../l%d", level+1)
		symlinkOrSkip(t, dir, fmt.Sprintf("l%d/a", level), next)
		symlinkOrSkip(t, dir, fmt.Sprintf("l%d/b", level), next)
	}
	if err := os.Mkdir(filepath.Join(dir, fmt.Sprintf("l%d", depth)), 0755); err != nil {
		t.Fatalf("os.Mkdir: %v", err)
	}
	lowerMaxLinkedDirs(t, 31)

	// Create runs on its own goroutine so that a walk the bound fails to stop
	// fails this test within a minute instead of hanging the whole package.
	done := make(chan error, 1)
	go func() {
		r, _, err := Create(dir, MaxTotalSize)
		if r != nil {
			r.Close()
		}
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute):
		t.Fatal("Create() still walking after a minute: the fan-out was not bounded")
	}

	want := fanoutLine(31, "l0"+strings.Repeat("/a", 28)+"/b")
	if err == nil || err.Error() != want {
		t.Errorf("Create() error = %v, want %q", err, want)
	}
}

// The bound counts every directory entered below a followed link, by any
// route: l1 enters t and t/u, and l2 enters t/u again, which is three. A tree
// of eleven plain directories and no links is never counted.
func TestCreateLinkedDirectoryBoundIsInclusive(t *testing.T) {
	lowerMaxLinkedDirs(t, 3)

	t.Run("exactly the bound packs", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"t/a.txt": "a", "t/u/b.txt": "b"})
		symlinkOrSkip(t, dir, "l1", "t")
		symlinkOrSkip(t, dir, "l2", "t/u")

		_, contents, _ := packTree(t, dir)

		want := map[string]string{
			"l1/a.txt":   "a",
			"l1/u/b.txt": "b",
			"l2/b.txt":   "b",
			"t/a.txt":    "a",
			"t/u/b.txt":  "b",
		}
		if !reflect.DeepEqual(contents, want) {
			t.Errorf("packed = %v, want %v", contents, want)
		}
	})

	t.Run("one more is refused", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"t/a.txt": "a", "t/u/b.txt": "b"})
		symlinkOrSkip(t, dir, "l1", "t")
		symlinkOrSkip(t, dir, "l2", "t/u")
		symlinkOrSkip(t, dir, "l3", "t/u")

		if err := createError(t, dir); err.Error() != fanoutLine(3, "l3") {
			t.Errorf("Create() error = %q, want %q", err, fanoutLine(3, "l3"))
		}
	})

	t.Run("plain directories are not counted", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"a/one.txt":              "1",
			"a/b/c/d/e/deep.txt":     "5",
			"f/g/h/wide.txt":         "3",
			"f/g/h/i/j/k/deeper.txt": "6",
		})

		_, contents, _ := packTree(t, dir)

		want := map[string]string{
			"a/one.txt":              "1",
			"a/b/c/d/e/deep.txt":     "5",
			"f/g/h/wide.txt":         "3",
			"f/g/h/i/j/k/deeper.txt": "6",
		}
		if !reflect.DeepEqual(contents, want) {
			t.Errorf("packed = %v, want %v", contents, want)
		}
	})
}

// Entries are written depth first, each directory's entries in name order, a
// linked directory in the position of the link's own name.
func TestCreateWritesEntriesInADeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"d.txt":     "d",
		"b.txt":     "b",
		"a/z.txt":   "z",
		"a/m/k.txt": "k",
		"e/f/g.txt": "g",
	})
	symlinkOrSkip(t, dir, "c", "a")

	want := []string{
		"a/m/k.txt",
		"a/z.txt",
		"b.txt",
		"c/m/k.txt",
		"c/z.txt",
		"d.txt",
		"e/f/g.txt",
	}
	for run := 1; run <= 2; run++ {
		names, _, _ := packTree(t, dir)
		if !reflect.DeepEqual(names, want) {
			t.Errorf("run %d: entry order = %v, want %v", run, names, want)
		}
	}
}

// A file that cannot be opened is refused for its size rather than for the
// open, which proves the limit is checked before the file is read.
func TestCreateChecksTheSizeLimitBeforeOpeningAFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not deny reading on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can open a file whatever its mode")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"big.bin": strings.Repeat("x", 2048)})
	if err := os.Chmod(filepath.Join(dir, "big.bin"), 0); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}

	_, stats, err := Create(dir, 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Create(dir, 1024) error = %v, want ErrTooLarge", err)
	}
	if stats != (FileStats{}) {
		t.Errorf("stats on refusal = %+v, want nothing packed", stats)
	}
}

// A dot-named directory outside the walk holds a chain of directories, each
// with one link, named with linkName, to the next. The root's link x enters the
// chain, so the file at the end is packed under x, then one linkName per level,
// then fileName. The real paths stay short while the pack path grows, which is
// the shape the path-length bound exists for. It returns the pack path of the
// innermost link before the file.
func buildLinkChain(t *testing.T, root string, levels int, linkName, fileName string) (filePath, lastLink string) {
	t.Helper()
	symlinkOrSkip(t, root, "x", ".d/d0")
	last := "x"
	for i := 0; i < levels; i++ {
		symlinkOrSkip(t, root, fmt.Sprintf(".d/d%d/%s", i, linkName), fmt.Sprintf("../d%d", i+1))
		last += "/" + linkName
	}
	writeFiles(t, root, map[string]string{fmt.Sprintf(".d/d%d/%s", levels, fileName): "end"})
	return last + "/" + fileName, last
}

func TestCreateBoundsThePackPathBelowLinks(t *testing.T) {
	linkName := strings.Repeat("n", 250)
	const levels = 16 // x plus 16 names of 250 bytes is 4017 bytes before the file

	t.Run("exactly the bound packs", func(t *testing.T) {
		dir := t.TempDir()
		fileName := strings.Repeat("f", 4096-4017-1)
		filePath, _ := buildLinkChain(t, dir, levels, linkName, fileName)
		if len(filePath) != 4096 {
			t.Fatalf("test tree pack path is %d bytes, want 4096", len(filePath))
		}

		names, contents, _ := packTree(t, dir)

		if want := []string{filepath.ToSlash(filePath)}; !reflect.DeepEqual(names, want) {
			t.Errorf("packed = %v, want %v", names, want)
		}
		if contents[filepath.ToSlash(filePath)] != "end" {
			t.Errorf("the file at the end of the chain was not packed with its contents")
		}
	})

	t.Run("one byte over is refused naming the innermost link", func(t *testing.T) {
		dir := t.TempDir()
		fileName := strings.Repeat("f", 4096-4017)
		filePath, lastLink := buildLinkChain(t, dir, levels, linkName, fileName)
		if len(filePath) != 4097 {
			t.Fatalf("test tree pack path is %d bytes, want 4097", len(filePath))
		}

		if err := createError(t, dir); err.Error() != pathTooLongLine(lastLink) {
			t.Errorf("Create() error = %q, want %q", err, pathTooLongLine(lastLink))
		}
	})

	t.Run("a long chain is refused at the first level past the bound", func(t *testing.T) {
		dir := t.TempDir()
		_, _ = buildLinkChain(t, dir, 60, linkName, "end.txt")
		lastLink := "x" + strings.Repeat("/"+linkName, levels)

		start := time.Now()
		err := createError(t, dir)

		if err.Error() != pathTooLongLine(lastLink) {
			t.Errorf("Create() error = %q, want %q", err, pathTooLongLine(lastLink))
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("Create() took %v, want a quick refusal", d)
		}
	})
}

// A link into a directory that holds another link is still checked against the
// inside guard below the first link, not only at the top of the walk.
func TestCreateRefusesALinkLeavingTheTreeBelowALink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "site")
	writeFiles(t, dir, map[string]string{"z/keep.txt": "keep"})
	writeFiles(t, base, map[string]string{"outside/secret.txt": "sensitive"})
	// a sorts before z, so z/esc is first reached through a.
	symlinkOrSkip(t, dir, "a", "z")
	symlinkOrSkip(t, dir, "z/esc", "../../outside")

	resolved, err := filepath.EvalSymlinks(filepath.Join(base, "outside"))
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks: %v", err)
	}
	want := fmt.Sprintf("symlink %s points outside directory: %s", filepath.Join("a", "esc"), resolved)
	if err := createError(t, dir); err.Error() != want {
		t.Errorf("Create() error = %q, want %q", err, want)
	}
}

func TestCreateSkipsALinkToADotNameBelowALink(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"z/ok.txt": "ok", ".hidden": "secret"})
	symlinkOrSkip(t, dir, "a", "z")
	symlinkOrSkip(t, dir, "z/vis", "../.hidden")

	_, contents, _ := packTree(t, dir)

	if want := map[string]string{"a/ok.txt": "ok", "z/ok.txt": "ok"}; !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// The inside guard compares against the root plus a separator, so a sibling
// whose name merely starts with the root's name is outside.
func TestCreateRefusesALinkToASiblingSharingTheRootsNamePrefix(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "site")
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	writeFiles(t, base, map[string]string{"site-evil/secret.txt": "sensitive"})
	symlinkOrSkip(t, dir, "ext", "../site-evil")

	resolved, err := filepath.EvalSymlinks(filepath.Join(base, "site-evil"))
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks: %v", err)
	}
	want := fmt.Sprintf("symlink ext points outside directory: %s", resolved)
	if err := createError(t, dir); err.Error() != want {
		t.Errorf("Create() error = %q, want %q", err, want)
	}
}

// A link whose own name starts with a dot is skipped even when its target is a
// plain directory.
func TestCreateSkipsADotNamedLinkToAPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"shared/f.txt": "shared"})
	symlinkOrSkip(t, dir, ".link", "shared")

	_, contents, _ := packTree(t, dir)

	if want := map[string]string{"shared/f.txt": "shared"}; !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// An absolute link that stays inside the tree packs like a relative one, at
// every depth below it.
func TestCreateFollowsAnAbsoluteDirectoryLinkInsideTheTree(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"shared/sub/a.txt": "a"})
	symlinkOrSkip(t, dir, "site", filepath.ToSlash(filepath.Join(dir, "shared")))

	_, contents, _ := packTree(t, dir)

	want := map[string]string{"shared/sub/a.txt": "a", "site/sub/a.txt": "a"}
	if !reflect.DeepEqual(contents, want) {
		t.Errorf("packed = %v, want %v", contents, want)
	}
}

// swapBeforeOpen makes the walk run swap once, after it has checked the file at
// target (relative to dir, forward slashes) and before it opens it, standing in
// for another process changing the tree in that window. The returned function
// reports whether the swap ran, so a test can tell a refusal of the swap from a
// walk that never reached it.
func swapBeforeOpen(t *testing.T, dir, target string, swap func()) (ran func() bool) {
	t.Helper()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks: %v", err)
	}
	want := filepath.Join(resolvedDir, filepath.FromSlash(target))
	swapped := false
	saved := beforeOpen
	beforeOpen = func(resolved string) {
		if resolved == want && !swapped {
			swapped = true
			swap()
		}
	}
	t.Cleanup(func() { beforeOpen = saved })
	return func() bool { return swapped }
}

// mustSucceed fails the test at once when a step of a swap fails, so a broken
// setup is never read as the walk refusing the swap.
func mustSucceed(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A component swapped for a link leaving the tree after the walk's checks must
// not be read. The root refuses the open, so the error is the open's own path
// error; a plain open would read the outside file, and only the identity check
// would then stop the deploy, which this test counts as a failure of the
// confinement.
func TestCreateRefusesAComponentSwappedForALinkLeavingTheTree(t *testing.T) {
	const secret = "outside-secret-bytes"
	// The swaps make links from inside Create, where the test cannot skip, so
	// the platform's support for links is probed up front.
	symlinkOrSkip(t, t.TempDir(), "probe", "target")

	for _, tc := range []struct {
		name, target string
		swap         func(t *testing.T, dir, outside string)
	}{
		{
			name:   "the file itself",
			target: "page.html",
			swap: func(t *testing.T, dir, outside string) {
				path := filepath.Join(dir, "page.html")
				mustSucceed(t, os.Remove(path))
				mustSucceed(t, os.Symlink(filepath.Join(outside, "page.html"), path))
			},
		},
		{
			name:   "a directory above it",
			target: "sub/page.html",
			swap: func(t *testing.T, dir, outside string) {
				path := filepath.Join(dir, "sub")
				mustSucceed(t, os.Rename(path, filepath.Join(dir, ".moved")))
				mustSucceed(t, os.Symlink(outside, path))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"index.html": "hi", tc.target: "inside"})
			outside := t.TempDir()
			writeFiles(t, outside, map[string]string{"page.html": secret})
			ran := swapBeforeOpen(t, dir, tc.target, func() { tc.swap(t, dir, outside) })

			r, _, err := Create(dir, MaxTotalSize)
			if err == nil {
				_, contents := readTarball(t, r)
				r.Close()
				for name, body := range contents {
					if strings.Contains(body, secret) {
						t.Errorf("%s was packed with the outside file's bytes", name)
					}
				}
				t.Fatalf("Create() error = nil, packed %v; want a refusal", contents)
			}
			if !ran() {
				t.Fatalf("Create() error = %v before the swap ran", err)
			}
			if errors.Is(err, errFileChanged) {
				t.Errorf("Create() error = %v: the outside file was opened, and only the identity check stopped it", err)
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) {
				t.Errorf("Create() error = %v, want the open's path error", err)
			}
			if prefix := "open " + filepath.FromSlash(tc.target) + ": "; !strings.HasPrefix(err.Error(), prefix) {
				t.Errorf("Create() error = %q, want it to start %q", err, prefix)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Create() error = %q carries the outside file's bytes", err)
			}
		})
	}
}

// A file replaced inside the tree after the walk's checks is a different file
// from the one checked, and the deploy stops rather than pack it. The
// replacement comes from a dot-directory, which the walk skips, and has the
// checked file's length, so nothing but the identity check can notice the
// change.
func TestCreateRefusesAFileReplacedAfterItWasChecked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.SameFile reads a stat's file identity only when it compares, after the swap, so Windows cannot tell the two files apart")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html":         "hi",
		"page.html":          "checked",
		".stash/swapped.txt": "swapped",
	})
	ran := swapBeforeOpen(t, dir, "page.html", func() {
		mustSucceed(t, os.Rename(filepath.Join(dir, ".stash", "swapped.txt"), filepath.Join(dir, "page.html")))
	})

	r, _, err := Create(dir, MaxTotalSize)
	if err == nil {
		_, contents := readTarball(t, r)
		r.Close()
		t.Fatalf("Create() error = nil, packed %v; want a refusal", contents)
	}
	if !ran() {
		t.Fatalf("Create() error = %v before the swap ran", err)
	}
	if !errors.Is(err, errFileChanged) {
		t.Errorf("Create() error = %v, want errFileChanged", err)
	}
	if want := "open page.html: file changed while packing"; err.Error() != want {
		t.Errorf("Create() error = %q, want %q", err, want)
	}
}

func TestGroupThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 20000: "20,000", 1234567: "1,234,567"} {
		if got := groupThousands(n); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", n, got, want)
		}
	}
}
