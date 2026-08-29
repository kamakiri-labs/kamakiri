package deploy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// ErrTooManyFiles reports a directory over MaxFileCount.
var ErrTooManyFiles = errors.New("exceeds file count limit")

// ErrTooLarge reports a directory over MaxTotalSize.
var ErrTooLarge = errors.New("exceeds size limit")

// FileStats counts the files packed and their total size, uncompressed.
type FileStats struct {
	FileCount int
	TotalSize int64
}

// MaxTotalSize is the largest total size a deploy may carry, uncompressed. It
// and MaxFileCount are both enforced server-side too; failing here only saves
// the upload.
const MaxTotalSize = 500 * 1024 * 1024

// MaxFileCount is the largest number of files a deploy may carry.
const MaxFileCount = 50000

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

// Create packs dir into a gzipped tar and returns it for reading. An entry
// whose own name starts with a dot is skipped, and a symlink leaving dir is an
// error. The reader is seekable, so a caller can measure the archive. The
// caller must close it, which is also what removes the temp file from disk.
func Create(dir string) (io.ReadCloser, FileStats, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_resolve_directory"), err)
	}
	absDir, err = filepath.EvalSymlinks(absDir)
	if err != nil {
		return nil, FileStats{}, fmt.Errorf("%s: %w", i18n.T("deploy.err_resolve_directory"), err)
	}

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

	var stats FileStats

	err = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		// Dotfiles are never published: .git, .env and the like would put
		// history and secrets on a public site.
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			return nil
		}

		// Symlinks are followed, so the target gets both guards: it must stay
		// under the source directory, and the dotfile rule is re-checked on the
		// resolved file's own name, since a plain-looking link can point at .env.
		// Only that last component is re-checked, so a link into a dot-directory
		// (.git/config) still packs.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_resolve_symlink", rel), err)
		}
		if !strings.HasPrefix(resolved, absDir+string(filepath.Separator)) && resolved != absDir {
			return errors.New(i18n.Tf("deploy.err_symlink_outside", rel, resolved))
		}

		resolvedBase := filepath.Base(resolved)
		if strings.HasPrefix(resolvedBase, ".") {
			return nil
		}

		resolvedInfo, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_stat_file", rel), err)
		}

		header, err := tar.FileInfoHeader(resolvedInfo, "")
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_tar_header", rel), err)
		}
		header.Name = filepath.ToSlash(rel)

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_write_header", rel), err)
		}

		f, err := os.Open(resolved)
		if err != nil {
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_open_file", rel), err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", i18n.Tf("deploy.err_write_file", rel), err)
		}
		f.Close()

		stats.FileCount++
		stats.TotalSize += resolvedInfo.Size()

		if stats.FileCount > MaxFileCount {
			return ErrTooManyFiles
		}
		if stats.TotalSize > MaxTotalSize {
			return ErrTooLarge
		}

		return nil
	})

	if err != nil {
		archive.Close()
		return nil, stats, err
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

	return archive, stats, nil
}
