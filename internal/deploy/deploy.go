package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/headers"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/redirects"
	"github.com/kamakiri-labs/kamakiri/internal/sizelimit"
)

const progressThreshold = 50 * 1024 * 1024

// APIClient is the API surface the deploy flow needs. Deploy uploads the
// tarball and returns once the server has queued it. GetSite reads the site
// once before packing, for its per-deploy size limit, and then backs the
// freshness wait that follows the upload.
type APIClient interface {
	Deploy(config []byte, tarball io.Reader) (*api.DeployResult, error)
	GetSite(id string) (*api.Site, error)
}

type tarballInfo struct {
	reader    io.ReadCloser
	size      int64  // compressed tarball size
	fileCount int    // >0 for directory, 0 for archive
	fileName  string // non-empty for archive
}

func (t *tarballInfo) label() string {
	if t.fileCount > 0 {
		return i18n.Tf("deploy.file_count", t.fileCount)
	}
	return t.fileName
}

// Run executes the deploy flow. By default it blocks on the streaming freshness
// wait after the upload, so success means every cache a visitor could hit serves
// the new deploy. With noWait it returns as soon as the server has queued the
// upload, which is what a CI script fanning out many deploys wants.
//
// The duration on the upload line covers upload and queue time only; extending
// it over the wait would conflate transport time, which the user already sees a
// progress bar for, with reconcile and flush time.
func Run(client APIClient, path string, noWait bool, out io.Writer) error {
	creds, err := core.LoadCredentials()
	if err != nil {
		return err
	}
	if creds == nil {
		return errors.New(i18n.T("common.err_not_logged_in"))
	}

	config, err := core.LoadProject()
	if err != nil {
		return err
	}
	if config == nil {
		return errors.New(i18n.T("common.err_no_site_linked"))
	}

	// Linting locally buys a fast line-numbered error and a warning for each
	// header line the server will silently drop. An archive is not linted, since
	// reading its embedded files is not cheap; the server parses and validates
	// either shape and stays the single authority.
	if stat, statErr := os.Stat(path); statErr == nil && stat.IsDir() {
		if err := redirects.Lint(path); err != nil {
			return err
		}

		warnings, err := headers.Lint(path)
		if err != nil {
			return err
		}
		for _, w := range warnings {
			fmt.Fprintln(out, w)
		}
	}

	// The site is read for every deploy, archive or directory, before anything
	// is packed. An answer the upload would get as well (see conclusiveRefusal)
	// ends the deploy here with the error the upload would have produced. Any
	// other failed read does not stop it: the server is the authority on every
	// limit, so the local check falls back to MaxTotalSize and the upload goes
	// ahead.
	site, err := client.GetSite(config.ID)
	if err != nil {
		if conclusiveRefusal(err) {
			return api.MapError(err)
		}
		site = nil
	}

	// A desired status of deleted means a teardown was accepted, whether it is
	// still running, failed and left this directory linked, or has finished.
	// The server refuses the upload too, but only after it was packed and sent,
	// so this check stops the deploy before any of that work is done.
	if site != nil && site.Status == "deleted" {
		return errors.New(i18n.T("api.err_site_torn_down"))
	}

	info, err := prepareTarball(path, sizeLimit(site))
	if err != nil {
		return err
	}
	defer info.reader.Close()

	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("deploy.err_marshal_config"), err)
	}

	label := info.label()
	showProgress := info.size >= progressThreshold

	var uploadReader io.Reader = info.reader
	if showProgress {
		uploadReader = &progressReader{
			reader: info.reader,
			total:  info.size,
			out:    out,
			label:  label,
		}
	} else {
		fmt.Fprint(out, i18n.Tf("deploy.uploading", label, formatBytes(info.size)))
	}

	start := time.Now()
	result, err := client.Deploy(configJSON, uploadReader)
	elapsed := time.Since(start)

	if err != nil {
		if showProgress {
			fmt.Fprintln(out)
		}
		return api.MapError(err)
	}

	if showProgress {
		fmt.Fprintf(out, "\r%s%s\n",
			i18n.Tf("deploy.uploading", label, formatBytes(info.size)),
			i18n.Tf("deploy.upload_done", elapsed.Seconds()))
	} else {
		fmt.Fprintln(out, i18n.Tf("deploy.upload_done", elapsed.Seconds()))
	}

	if noWait {
		// Nothing here may claim the site is live: the deploy is only queued,
		// and no cache has been flushed. The URL echo names the site's address,
		// which is true even before it resolves.
		fmt.Fprintln(out, i18n.T("deploy.queued_no_wait"))
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.Tf("deploy.deploy_id", result.DeployID))
		fmt.Fprintf(out, "\u2192 %s\n", result.URL)
		fmt.Fprintln(out, i18n.T("deploy.background_notice"))
		return nil
	}

	// The deploy id prints before the wait so that every exit path, converged or
	// blocked or interrupted, leaves it in scrollback for the user to quote.
	fmt.Fprintln(out, i18n.T("deploy.deploying"))
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.Tf("deploy.deploy_id", result.DeployID))
	return freshness.Wait(client, config.ID, freshness.DeployMsgs(), out)
}

type progressReader struct {
	reader    io.Reader
	total     int64
	read      int64
	lastPrint int64
	out       io.Writer
	label     string
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.read += int64(n)
	if p.read-p.lastPrint >= 1024*1024 || err == io.EOF {
		fmt.Fprint(p.out, i18n.Tf("deploy.uploading_progress", p.label, formatBytes(p.read), formatBytes(p.total)))
		p.lastPrint = p.read
	}
	return n, err
}

// prepareTarball opens what path names for upload. maxTotalSize bounds a
// directory only: an archive is sent as given, since measuring what it holds
// would mean decompressing it, and the server checks it on arrival.
func prepareTarball(path string, maxTotalSize int64) (*tarballInfo, error) {
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New(i18n.Tf("deploy.err_path_not_found", path))
		}
		return nil, err
	}

	if stat.IsDir() {
		return prepareDirTarball(path, maxTotalSize)
	}

	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		return prepareArchive(path, stat)
	}

	return nil, errors.New(i18n.T("deploy.err_unsupported_archive"))
}

func prepareDirTarball(dir string, maxTotalSize int64) (*tarballInfo, error) {
	reader, stats, err := Create(dir, maxTotalSize)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, errors.New(i18n.Tf("deploy.err_too_large", sizelimit.MB(maxTotalSize)))
		}
		if errors.Is(err, ErrTooManyFiles) {
			return nil, errors.New(i18n.T("deploy.err_too_many_files"))
		}
		return nil, err
	}

	if stats.FileCount == 0 {
		reader.Close()
		return nil, errors.New(i18n.Tf("deploy.err_empty_directory", dir))
	}

	seeker, ok := reader.(io.ReadSeeker)
	if !ok {
		reader.Close()
		return nil, errors.New(i18n.T("deploy.err_reader_not_seekable"))
	}
	size, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		reader.Close()
		return nil, fmt.Errorf("%s: %w", i18n.T("deploy.err_measure_tarball"), err)
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		reader.Close()
		return nil, fmt.Errorf("%s: %w", i18n.T("deploy.err_reset_tarball"), err)
	}

	return &tarballInfo{
		reader:    reader,
		size:      size,
		fileCount: stats.FileCount,
	}, nil
}

func prepareArchive(path string, info os.FileInfo) (*tarballInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	return &tarballInfo{
		reader:   f,
		size:     info.Size(),
		fileName: filepath.Base(path),
	}, nil
}

// sizeLimit is the uncompressed size a directory deploy may carry: the plan's
// per-deploy limit from the site read, or MaxTotalSize when there is no site or
// it carries no usable limit (an older server sends no quota block). A limit of
// zero or less counts as absent: a quota block without the field decodes to
// zero, which cannot be told apart from no limit sent, and the server applies
// its own limit to the upload either way.
func sizeLimit(site *api.Site) int64 {
	if site != nil && site.Quota != nil && site.Quota.MaxSnapshotBytes > 0 {
		return site.Quota.MaxSnapshotBytes
	}
	return MaxTotalSize
}

// conclusiveRefusal reports whether a failed site read is an answer the upload
// would get too: a rejected key, a CLI version the server refuses, or a site
// it does not know. A transport error, a server error or any other refusal
// could be transient or specific to the read, so the deploy carries on.
func conclusiveRefusal(err error) bool {
	if errors.Is(err, api.ErrUpgradeRequired) {
		return true
	}
	var apiErr *api.ErrorResponse
	if errors.As(err, &apiErr) {
		return apiErr.Code == "unauthorized" || apiErr.Code == "site_not_found"
	}
	return false
}

func formatBytes(b int64) string {
	const mb = 1024 * 1024
	const kb = 1024
	if b >= mb {
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	}
	return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
}
