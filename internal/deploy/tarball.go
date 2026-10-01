package deploy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// ErrTooManyFiles reports a directory over MaxFileCount.
var ErrTooManyFiles = errors.New("exceeds file count limit")

// ErrTooLarge reports a directory over the size limit Create was given.
var ErrTooLarge = errors.New("exceeds size limit")

// errFileChanged reports a file that is no longer the one the walk checked by
// the time it is opened: something replaced it in between.
var errFileChanged = errors.New("file changed while packing")

// beforeOpen runs in pack between the walk's checks on a file and its open. It
// does nothing in a real deploy; tests set it to change the tree inside that
// window.
var beforeOpen = func(resolved string) {}

// FileStats counts the files packed and their total size, uncompressed.
type FileStats struct {
	FileCount int
	TotalSize int64
}

// MaxTotalSize is the size limit a directory deploy is checked against,
// uncompressed, when the site's own per-deploy limit is not known: the site
// read failed, or the server did not send a limit. The server enforces the
// real limit either way, and failing here, like failing on MaxFileCount, only
// saves the upload.
const MaxTotalSize = 500 * 1024 * 1024

// MaxFileCount is the largest number of files a deploy may carry.
const MaxFileCount = 20000

// maxLinkedDirs bounds how many directories one Create call may enter below a
// followed directory link. Links fanning out over directories that hold
// nothing to pack trip neither cap, and two links per level already double the
// walk at each level, so without it a tree of a few dozen directories could
// keep a deploy walking for hours. It is a variable only so tests can lower it.
var maxLinkedDirs = MaxFileCount

// maxPackPathLen bounds the byte length of a pack path below a followed
// directory link. The real path of a linked directory stays short while the
// pack path grows with every level, so nothing else limits it, and memory grows
// with the square of the depth. Linux refuses any path of 4096 bytes or more,
// so the server's extraction onto disk could not hold a longer one anyway.
const maxPackPathLen = 4096

// tempTarball is the archive Create hands back: the temp file it packed into,
// with a Close that also removes the file from disk. It embeds *os.File so the
// value stays an io.ReadSeeker, which callers rely on to measure the archive
// before uploading it.
type tempTarball struct {
	*os.File
	name string
}

// Close closes the file and then removes it, reporting the close error only.
// The order is what makes the removal work: Windows locks an open file against
// deletion, so the unlink Create attempts while the file is still open fails
// there, and only this second attempt, after the handle is gone, succeeds. On
// Unix the early unlink already removed the name and this one fails ENOENT, so
// the removal is best-effort either way and its error is discarded.
func (t *tempTarball) Close() error {
	err := t.File.Close()
	os.Remove(t.name)
	return err
}

// Create packs dir into a gzipped tar and returns it for reading. The reader
// is seekable, so a caller can measure the archive. The caller must close it,
// which is also what removes the temp file from disk.
//
// The walk is depth first, each directory's entries in name order, and these
// rules hold at every depth, below a followed link included:
//
//   - An entry whose own name starts with a dot is skipped, and a
//     dot-directory is not entered.
//   - Every other entry is resolved through its symlinks. A target outside dir
//     is an error naming the entry, and a target whose own name starts with a
//     dot is skipped. Only the entry's name and the target's last component
//     are checked, so a plain link into a dot-directory (docs pointing at
//     .vitepress/dist) is followed.
//   - A file is packed as a regular entry under the entry's path, with the
//     target's contents. No directory entries are written.
//   - A target that is neither a regular file nor a directory (a named pipe,
//     a socket, a device) is an error naming the entry, raised before anything
//     opens it, since opening a named pipe blocks until a writer appears.
//   - A directory is entered and its entries packed under the entry's path, so
//     a directory link's target is packed under the link's name.
//   - Entering a directory already on the current descent path, which only a
//     link can lead back to, is an error naming the innermost directory link
//     on that path. Two links to one directory from different places both
//     pack.
//   - Entering more than maxLinkedDirs directories below followed directory
//     links is an error naming the innermost link.
//   - An entry below a followed directory link whose path is longer than
//     maxPackPathLen bytes is an error naming the innermost link. Entries with
//     no link above them are not checked, since the filesystem bounds their
//     path.
//   - Both caps are checked before a file is written: the file after
//     MaxFileCount is ErrTooManyFiles, and one that takes the uncompressed
//     total past maxTotalSize bytes is ErrTooLarge, so a tree at exactly the
//     limit packs. The stats returned with either hold what was packed before
//     it.
func Create(dir string, maxTotalSize int64) (io.ReadCloser, FileStats, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_resolve_directory"), err)
	}
	absDir, err = filepath.EvalSymlinks(absDir)
	if err != nil {
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_resolve_directory"), err)
	}
	// The root serves only the walk's file opens. The archive outlives it,
	// through the upload, so it closes here rather than with the archive.
	root, err := os.OpenRoot(absDir)
	if err != nil {
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_resolve_directory"), err)
	}
	defer root.Close()

	tmpFile, err := os.CreateTemp("", "kamakiri-deploy-*.tar.gz")
	if err != nil {
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_create_temp_file"), err)
	}
	archive := &tempTarball{File: tmpFile, name: tmpFile.Name()}
	// Removal is attempted twice, and each attempt covers a platform. Unlinking
	// while the file is still open works on Unix and is the stronger guarantee
	// there: the OS reclaims the file even if the process dies before closing it.
	// Windows locks an open file against deletion, so that first attempt fails
	// there and tempTarball.Close makes the second one after closing the handle.
	os.Remove(archive.name)

	gw := gzip.NewWriter(archive)
	tw := tar.NewWriter(gw)

	w := &walker{tw: tw, absDir: absDir, root: root, maxTotalSize: maxTotalSize}
	if err := w.walk(absDir, "", map[string]bool{absDir: true}, ""); err != nil {
		archive.Close()
		return nil, w.stats, err
	}

	if err := tw.Close(); err != nil {
		archive.Close()
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_close_tar"), err)
	}
	if err := gw.Close(); err != nil {
		archive.Close()
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_close_gzip"), err)
	}

	if _, err := archive.Seek(0, 0); err != nil {
		archive.Close()
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_seek_temp_file"), err)
	}

	return archive, w.stats, nil
}

// walker is the state one Create call shares across its recursive walk. root
// is opened on absDir, and every file is read through it. linkedDirs counts
// the directories entered below a followed directory link, for maxLinkedDirs,
// and lives here rather than in the package so that each call counts its own.
type walker struct {
	tw           *tar.Writer
	absDir       string
	root         *os.Root
	maxTotalSize int64
	stats        FileStats
	linkedDirs   int
}

// walk packs the entries of dirPath, a directory whose path has every symlink
// resolved, under prefix, their path relative to the deploy root. onPath holds
// the resolved directories on the current descent path, and link is the path
// of the innermost directory link on it, "" while the descent has followed
// none.
func (w *walker) walk(dirPath, prefix string, onPath map[string]bool, link string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		// Dotfiles are never published: .git, .env and the like would put
		// history and secrets on a public site.
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		rel := filepath.Join(prefix, name)
		if link != "" && len(rel) > maxPackPathLen {
			return errors.New(i18n.Tf("deploy.err_symlink_path_too_long", link))
		}
		path := filepath.Join(dirPath, name)

		// Symlinks are followed, so the target gets both guards: it must stay
		// under the source directory, and the dotfile rule is re-checked on the
		// target's own name, since a plain-looking link can point at .env or
		// .git. Only that last component is re-checked, so a link into a
		// dot-directory (.git/config, .vitepress/dist) still packs.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_resolve_symlink", rel), err)
		}
		if !strings.HasPrefix(resolved, w.absDir+string(filepath.Separator)) && resolved != w.absDir {
			return errors.New(i18n.Tf("deploy.err_symlink_outside", rel, resolved))
		}
		if strings.HasPrefix(filepath.Base(resolved), ".") {
			continue
		}

		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_stat_file", rel), err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New(i18n.Tf("deploy.err_not_regular", rel))
		}

		if info.IsDir() {
			// dirPath is resolved, so a plain directory resolves to its own
			// path and anything else was reached through a link.
			inner := link
			if resolved != path {
				inner = rel
			}
			if err := w.enter(resolved, rel, onPath, inner); err != nil {
				return err
			}
			continue
		}

		if err := w.pack(resolved, rel, info); err != nil {
			return err
		}
	}
	return nil
}

// enter walks the directory at resolved, packing its entries under prefix, with
// link the innermost directory link on the descent path to it.
//
// A loop is caught by the descent path rather than by a set of every directory
// visited, because two links to one directory from different places (en/assets
// and ja/assets) must both pack. Directories are told apart by resolved path
// alone. Where one directory can show under two spellings (a case-insensitive
// filesystem) a repeat can go unnoticed for a while, and maxLinkedDirs ends
// the walk whatever the spelling.
func (w *walker) enter(resolved, prefix string, onPath map[string]bool, link string) error {
	// A descent through plain directories only ever makes longer paths, so a
	// repeat needs a link and link names one here.
	if onPath[resolved] {
		return errors.New(i18n.Tf("deploy.err_symlink_loop", link))
	}
	if link != "" {
		w.linkedDirs++
		if w.linkedDirs > maxLinkedDirs {
			return errors.New(i18n.Tf("deploy.err_symlink_fanout", link, groupThousands(maxLinkedDirs)))
		}
	}

	onPath[resolved] = true
	defer delete(onPath, resolved)
	return w.walk(resolved, prefix, onPath, link)
}

// pack writes the file at resolved, which info describes, as a regular entry
// at rel. Both caps are checked first, so a file the limits refuse is never
// opened, and the stats count only what was written. The header carries the
// size checked and tar.Writer refuses a longer copy, so a file that grows in
// the meantime cannot add more than was checked.
//
// The walk's checks resolve the file by name, so a component swapped for a
// link between those checks and the open could otherwise send the read
// outside the deploy directory. The open therefore goes through the root,
// which refuses any path that leaves absDir, and names the file by its
// resolved path, since the root also refuses absolute links even when they
// point inside it. The identity check then refuses a file replaced inside the
// directory in that window. On Windows, os.SameFile against info may reopen
// the path when it compares, so the identity check is weaker there; the root
// still confines the read. A regular file swapped for a named pipe in that
// window still blocks at the open: that takes a hostile writer inside the
// deploy directory, and Ctrl-C ends it.
//
// The root opens each directory on the way down for reading, where a plain
// open would need only search permission. So a file reached through a link
// into a dot-directory cannot be opened when that dot-directory, or any
// directory inside it on the way to the file, grants search but not read. The
// walk itself lists every directory outside dot-directories, which needs read
// permission anyway.
func (w *walker) pack(resolved, rel string, info os.FileInfo) error {
	if w.stats.FileCount+1 > MaxFileCount {
		return ErrTooManyFiles
	}
	if w.stats.TotalSize+info.Size() > w.maxTotalSize {
		return ErrTooLarge
	}

	beforeOpen(resolved)
	f, err := w.open(resolved, info)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_open_file", rel), err)
	}
	defer f.Close()

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_tar_header", rel), err)
	}
	header.Name = filepath.ToSlash(rel)

	if err := w.tw.WriteHeader(header); err != nil {
		return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_write_header", rel), err)
	}
	if _, err := io.Copy(w.tw, f); err != nil {
		return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_write_file", rel), err)
	}

	w.stats.FileCount++
	w.stats.TotalSize += info.Size()
	return nil
}

// open opens the file at resolved, a path under absDir, through the root, and
// returns it only if it is still the file info describes.
func (w *walker) open(resolved string, info os.FileInfo) (*os.File, error) {
	name, err := filepath.Rel(w.absDir, resolved)
	if err != nil {
		return nil, err
	}
	f, err := w.root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !os.SameFile(info, opened) {
		f.Close()
		return nil, errFileChanged
	}
	return f, nil
}

// groupThousands writes n with a comma between each group of three digits, the
// form the catalog copy uses for a limit. It is for non-negative counts.
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
