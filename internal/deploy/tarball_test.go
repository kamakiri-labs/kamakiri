package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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

	r, stats, err := Create(dir)
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

	r, stats, err := Create(dir)
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

	r, stats, err := Create(dir)
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

	_, stats, err := Create(dir)
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
	_, _, err := Create("/nonexistent/path/that/does/not/exist")
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

	_, _, err := Create(dir)
	if err == nil {
		t.Fatal("expected error for symlink pointing outside directory")
	}
}

func TestCreateAllowsSymlinkInsideDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("content"), 0644)
	os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt"))

	r, stats, err := Create(dir)
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

	_, _, err := Create(dir)
	if err == nil {
		t.Fatal("expected error when exceeding file count limit")
	}

	// The sentinel above never reaches the user: the deploy path translates it
	// into its own message, which nothing else asserts on. The same directory
	// serves both, since walking it twice is cheaper than building a second one.
	_, prepErr := prepareDirTarball(dir)
	if prepErr == nil {
		t.Fatal("prepareDirTarball() = nil, want the file-count message")
	}
	if want := "upload exceeds 50,000 file limit. Reduce your build output and try again"; prepErr.Error() != want {
		t.Errorf("prepareDirTarball() error = %q, want %q", prepErr.Error(), want)
	}
}

func TestCreateSkipsSymlinkToHiddenFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=x"), 0644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hi</h1>"), 0644)
	// A non-hidden name symlinked to a dotfile must still be skipped,
	// otherwise the dotfile filter could be bypassed to leak a .env secret.
	os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "config"))

	r, stats, err := Create(dir)
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

	info, err := prepareDirTarball(dir)
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

	_, stats, err := Create(dir)
	if err == nil {
		t.Fatal("expected error when exceeding file count limit")
	}
	if stats.FileCount == 0 {
		t.Error("stats.FileCount should be non-zero on limit error")
	}
}
