// Package upgrade implements `kamakiri upgrade`, which brings the running
// binary up to the latest published release, together with the install-method
// detection that decides whether it may do that at all.
//
// The command writes into the user's install directory, and its order follows
// from that: it proves the directory is writable before it fetches a byte, it
// holds the download to the checksum the release publishes before anything is
// moved into place, and every way it can return short of a completed
// replacement leaves the directory exactly as it found it. Returning is the
// qualifier: a Ctrl-C kills the process before any deferred work runs, so an
// interrupted download leaves its partial .tmp- file behind.
package upgrade

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/semver"
)

// releasesBaseURL is the site releases are resolved and downloaded from.
const releasesBaseURL = "https://github.com/kamakiri-labs/kamakiri"

// devVersion is the stamp a build that came from no release carries. It is
// compared before any normalization, since it is not a version and nothing may
// try to read it as one.
const devVersion = "(dev)"

// checksumsName is the file a release publishes its sha256 sums in, under the
// same tag as the binaries.
const checksumsName = "checksums.txt"

// maxTagBytes is the longest release tag that is read as a version at all. A
// tag is a version string and the ones published here run to under ten
// characters, so this is generous by an order of magnitude. The bound is a
// security rule rather than tidiness: the tag is chosen by whatever answered
// the request, the transport allows megabytes of headers, the parser allocates
// in proportion to what it is handed, and an accepted tag is then printed to
// the terminal and put into the URLs the download comes from. The header itself
// has already been read by the time the bound runs, so what it keeps the value
// out of is the parser, whose allocation runs to many times its input, along
// with the terminal and those URLs.
const maxTagBytes = 64

// The caps on what is read from the network. They refuse rather than truncate,
// because the bytes land in the user's install directory and a body that keeps
// coming is a failure, not a short file. The asset cap is a wide multiple of
// what a release actually weighs, so it only ever catches something that has
// already gone wrong.
const (
	maxAssetBytes     = 200 << 20
	maxChecksumsBytes = 1 << 20
)

// sha256HexLen is how many characters a sha256 digest spells out in hex.
const sha256HexLen = sha256.Size * 2

// probeTimeout bounds the two short requests, the redirect that names the
// release and the checksums file. Each answers in a few hundred bytes, so one
// still open after this is not going to finish.
const probeTimeout = 10 * time.Second

// responseHeaderTimeout bounds how long the asset download waits for the server
// to start answering. The transfer itself is left unbounded on purpose; see
// downloadClient.
const responseHeaderTimeout = 30 * time.Second

// errTooLarge marks a body that ran past its cap, so the caller can say which
// file it was reading.
var errTooLarge = errors.New("the body ran past its cap")

// writeError marks a failure that came from the file being written rather than
// from the body being read. A copy reports one error for both sides, and the
// two need different copy: a disk with no room left is not a connection that
// dropped.
type writeError struct{ err error }

func (e writeError) Error() string { return e.err.Error() }

func (e writeError) Unwrap() error { return e.err }

// markedWriter reports whatever the writer beneath it fails with as a write
// failure, which is the only thing that keeps the two sides of a copy apart
// once one error value stands for both.
type markedWriter struct{ dst io.Writer }

func (m markedWriter) Write(p []byte) (int, error) {
	n, err := m.dst.Write(p)
	if err != nil {
		return n, writeError{err}
	}
	return n, nil
}

// Run brings the binary at execPath up to the latest release, writing a
// milestone line to out for each phase that has something to report and
// returning a fully worded, single-line error for every failure.
//
// version is the raw build stamp, "(dev)" and a leading v included. execPath is
// the running executable's path already resolved through os.Executable, then
// filepath.Abs, then filepath.EvalSymlinks: the install-method rules depend on
// that, and so does replacing the file the user is actually running.
func Run(version, execPath string, out io.Writer) error {
	return run(version, execPath, releasesBaseURL, runtime.GOOS, runtime.GOARCH, maxAssetBytes, out)
}

// run takes the releases site, the GOOS, the GOARCH and the asset cap as
// parameters so a test can drive the whole flow against a local server, both
// platforms' replace sequences, and a body that runs past its cap, from any
// machine. Run binds those four, and that binding is pinned by a test of its
// own, because an unbound runtime.GOOS read survives being replaced with a
// literal and a cap nothing drives survives being removed.
func run(version, execPath, baseURL, goos, goarch string, maxAsset int64, out io.Writer) error {
	// First, and on every invocation rather than only on one that upgrades: a
	// file a previous Windows upgrade could not delete is cleared the next time
	// the user runs this command at all, whatever that run goes on to do.
	removeStaleAside(execPath, goos)

	if version == devVersion {
		return errors.New(i18n.Tf("upgrade.err_dev_build", releasesPage(baseURL)))
	}

	fmt.Fprintln(out, i18n.T("upgrade.checking"))
	tag, err := resolveLatest(baseURL)
	if err != nil {
		return err
	}

	latest, latestErr := semver.Parse(tag)
	current, currentErr := semver.Parse(version)
	// Neither side decides anything once one of them is unreadable. An unparsed
	// version orders below every parsed one, so leaning on the ordering here
	// would read this build as older than any release and fetch on the strength
	// of a string this command could not make sense of. Which side could not be
	// read is what the two lines differ on: the releases page is not at fault
	// for a stamp this build carries, and a build whose stamp is real but
	// unreadable is a live case rather than a hypothetical one.
	switch {
	case latestErr != nil:
		return errors.New(i18n.Tf("upgrade.err_no_release", releasesPage(baseURL)))
	case currentErr != nil:
		return errors.New(i18n.Tf("upgrade.err_unreadable_version", version, releasesPage(baseURL)))
	}
	if semver.Compare(latest, current) <= 0 {
		fmt.Fprintln(out, i18n.Tf("upgrade.already_current", version))
		return nil
	}
	fmt.Fprintln(out, i18n.Tf("upgrade.available", tag, version))

	// A channel that owns the install is the one that upgrades it, so the CLI
	// only replaces a binary nothing else manages. Deferring is this command
	// working as designed and ends it without an error. The two lines are two
	// literal keys rather than one built from the method: the check that every
	// key has a call site and every call site a key reads the source, and a
	// computed key is invisible to it on both counts. The unexported form of the
	// detection is called so its path rules read the same platform the rest of
	// this flow was handed rather than the one this binary is running on.
	switch detect(execPath, core.LoadInstallMarker(), goos) {
	case MethodHomebrew:
		fmt.Fprintln(out, i18n.T("upgrade.defer_homebrew"))
		return nil
	case MethodNpm:
		fmt.Fprintln(out, i18n.Tf("upgrade.defer_npm", execPath))
		return nil
	}

	asset := assetName(goos, goarch)
	dir := filepath.Dir(execPath)

	// The temp file is created before a byte is fetched, and before the line
	// that says a download is under way, so an install directory this user
	// cannot write to fails in a moment rather than after a download that then
	// has nowhere to land, and without having claimed to be fetching anything.
	// It is created beside the binary it will replace, which is also what keeps
	// the rename below inside one filesystem.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return errors.New(i18n.Tf("upgrade.err_create_temp", dir))
	}
	tmpName := tmp.Name()
	replaced := false
	// Armed the moment the file exists and disarmed only by a completed
	// replacement, so no failure between here and there can leave a temp file
	// sitting in the user's install directory.
	defer func() {
		if !replaced {
			os.Remove(tmpName)
		}
	}()
	if err := os.Chmod(tmpName, 0o755); err != nil {
		tmp.Close()
		return errors.New(i18n.Tf("upgrade.err_create_temp", dir))
	}

	fmt.Fprintln(out, i18n.Tf("upgrade.downloading", asset, dir))
	sum, err := download(tmp, dir, baseURL, tag, asset, maxAsset)
	if err != nil {
		return err
	}

	if err := verify(baseURL, tag, asset, sum); err != nil {
		return err
	}
	fmt.Fprintln(out, i18n.T("upgrade.verified"))

	if err := replaceBinary(tmpName, execPath, goos); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("upgrade.err_replace"), err)
	}
	replaced = true

	fmt.Fprintln(out, i18n.Tf("upgrade.done", tag, execPath))
	return nil
}

// resolveLatest asks the releases site which release is the latest and returns
// its tag.
//
// The answer is a redirect to that release's own page, and it is read strictly:
// the tag is the one string a server chooses that then goes into a URL this
// command downloads from. A Location that leaves the site, leaves this
// repository's releases path, or does not end in a single segment that parses
// as a version is no answer at all, and neither is any status but a redirect.
func resolveLatest(baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("%s: %w", i18n.T("upgrade.err_request_failed"), err)
	}
	resp, err := resolveClient().Get(baseURL + "/releases/latest")
	if err != nil {
		return "", fmt.Errorf("%s: %w", i18n.T("upgrade.err_request_failed"), err)
	}
	// The redirect policy hands the response back unfollowed, and an unfollowed
	// response still owns a body nobody else will close.
	defer resp.Body.Close()

	tag, ok := tagFromRedirect(base, resp)
	if !ok {
		return "", errors.New(i18n.Tf("upgrade.err_no_release", releasesPage(baseURL)))
	}
	return tag, nil
}

// tagFromRedirect reads the release tag out of the answer to releases/latest,
// and reports whether it is one this command will act on.
func tagFromRedirect(base *url.URL, resp *http.Response) (string, bool) {
	// Anything but a redirect is the site saying it has no latest release: a
	// repository whose releases do not qualify answers a redirect to its
	// releases index instead, and one that does not exist answers 404.
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", false
	}
	// Location is resolved against the request URL, so a relative one is
	// absolute by here and can be held to the site the request went to.
	location, err := resp.Location()
	if err != nil {
		return "", false
	}
	if location.Scheme != base.Scheme || location.Host != base.Host {
		return "", false
	}
	prefix := strings.TrimSuffix(base.Path, "/") + "/releases/tag/"
	if !strings.HasPrefix(location.Path, prefix) {
		return "", false
	}
	// Path is the decoded form, so a segment that arrived as %2F is a slash by
	// now and is refused here rather than reaching the parser as one segment.
	// The length is held here too, and ahead of the parse rather than after it,
	// which is where its value lies; see maxTagBytes.
	tag := strings.TrimPrefix(location.Path, prefix)
	if tag == "" || len(tag) > maxTagBytes || strings.Contains(tag, "/") {
		return "", false
	}
	if _, err := semver.Parse(tag); err != nil {
		return "", false
	}
	return tag, true
}

// download streams the release asset into dst, closes it whatever the transfer
// did, and returns the sha256 of what was written as lowercase hex. dir is where
// dst lives, and is named by the copy a failure to write carries.
//
// The close is part of this function rather than the caller's to make, for two
// reasons. It is the last thing that can fail on the way to disk, and the digest
// is already computed by then: a filesystem that only reports a full disk at
// close would otherwise leave a short file whose digest, taken from the bytes
// handed over, matches the published one, and that file is what would be renamed
// over the user's binary. And folding it in means no call site can forget the
// close, a check on the path rather than one sitting beside it. dst is an
// io.WriteCloser rather than the file itself so a test can drive a close that
// fails.
func download(dst io.WriteCloser, dir, baseURL, tag, asset string, maxAsset int64) (sum string, err error) {
	// A failure the transfer already reported outranks one from the close: it
	// names what actually went wrong, and closing a file whose download failed
	// tells the user nothing further.
	defer func() {
		closeErr := dst.Close()
		if err == nil && closeErr != nil {
			sum = ""
			err = fmt.Errorf("%s: %w", i18n.Tf("upgrade.err_write", dir), closeErr)
		}
	}()

	resp, err := downloadClient().Get(assetURL(baseURL, tag, asset))
	if err != nil {
		return "", fmt.Errorf("%s: %w", i18n.T("upgrade.err_request_failed"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(i18n.Tf("upgrade.err_download", asset, resp.StatusCode))
	}

	digest := sha256.New()
	// The destination is marked so that the one error a copy reports can still
	// be traced to the side it came from: a full disk is a local problem, and
	// naming the network for it would send the user looking in the wrong place.
	err = copyCapped(io.MultiWriter(markedWriter{dst}, digest), resp.Body, maxAsset)
	var write writeError
	switch {
	case errors.Is(err, errTooLarge):
		return "", errors.New(i18n.Tf("upgrade.err_too_large", asset, maxAsset>>20))
	case errors.As(err, &write):
		return "", fmt.Errorf("%s: %w", i18n.Tf("upgrade.err_write", dir), write.err)
	case err != nil:
		return "", fmt.Errorf("%s: %w", i18n.T("upgrade.err_request_failed"), err)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// verify fetches the release's checksums file and holds the download's sum to
// the line naming this asset.
func verify(baseURL, tag, asset, sum string) error {
	resp, err := shortClient().Get(assetURL(baseURL, tag, checksumsName))
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("upgrade.err_request_failed"), err)
	}
	defer resp.Body.Close()
	// Checked before a byte of the body is read, so the short page a site
	// serves for a missing file can never be parsed as a list of sums and
	// reported as a checksum that does not match.
	if resp.StatusCode != http.StatusOK {
		return errors.New(i18n.Tf("upgrade.err_checksums", resp.StatusCode))
	}

	var body bytes.Buffer
	if err := copyCapped(&body, resp.Body, maxChecksumsBytes); err != nil {
		if errors.Is(err, errTooLarge) {
			return errors.New(i18n.Tf("upgrade.err_too_large", checksumsName, maxChecksumsBytes>>20))
		}
		return fmt.Errorf("%s: %w", i18n.T("upgrade.err_request_failed"), err)
	}

	published, named := publishedSum(body.String(), asset)
	if published == "" {
		// A line that names the asset and carries nothing usable is its own
		// answer: the file was published for this platform and the sum in it
		// cannot be read, which is a different thing to fix than a release that
		// never listed this asset.
		if named {
			return errors.New(i18n.Tf("upgrade.err_checksum_malformed", asset))
		}
		return errors.New(i18n.Tf("upgrade.err_checksum_missing", asset))
	}
	// A published checksum is not a secret, so an ordinary comparison is the
	// right one: there is nothing here a timing attack could learn. The case is
	// ignored because two spellings of a hex digest are one digest.
	if !strings.EqualFold(published, sum) {
		return errors.New(i18n.Tf("upgrade.err_checksum_mismatch", asset, published, sum))
	}
	return nil
}

// publishedSum returns the checksum the file records for asset, and whether any
// line names asset at all. The format is sha256sum's own, the hex digest then
// two spaces then the file name, and the name is compared in full so one asset's
// line can never be read as another's. A line naming asset whose first field is
// not a digest does not end the scan, so the first usable line wins wherever it
// sits in the file; an empty sum with named set is a file that names asset and
// never carries a digest for it.
func publishedSum(checksums, asset string) (sum string, named bool) {
	for _, line := range strings.Split(checksums, "\n") {
		field, name, ok := strings.Cut(line, "  ")
		if !ok || name != asset {
			continue
		}
		named = true
		if isHexDigest(field) {
			return field, true
		}
	}
	return "", named
}

// isHexDigest reports whether field is a sha256 digest spelled in hex and
// nothing else. Holding the field to that shape before it is read as a sum is
// what keeps a server's choice of bytes off the terminal: the mismatch line
// prints the published sum back to the user, so a field carrying escape
// sequences, or a megabyte of them, would print as it stands.
func isHexDigest(field string) bool {
	if len(field) != sha256HexLen {
		return false
	}
	for _, char := range field {
		switch {
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'f':
		case char >= 'A' && char <= 'F':
		default:
			return false
		}
	}
	return true
}

// copyCapped copies src into dst, refusing a body that runs past limit rather
// than truncating it. One byte more than the cap allows is read, which is what
// tells a body sitting exactly at the cap from one that goes on.
func copyCapped(dst io.Writer, src io.Reader, limit int64) error {
	read, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return err
	}
	if read > limit {
		return errTooLarge
	}
	return nil
}

// assetName is the name a release publishes this platform's binary under.
func assetName(goos, goarch string) string {
	name := "kamakiri-" + goos + "-" + goarch
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// assetURL builds the download URL for one file published under a tag.
//
// The tag goes in exactly as the release named it, and both halves of that
// matter. Nothing but digits, dots, hyphens and ASCII letters gets past the
// version parser, so an accepted tag carries no character that could mean
// something else in a URL. And a release is published under the tag as written,
// its leading v included, so a tag normalized to 0.1.1 would ask for a tag that
// does not exist and the 404 would read as a release with no assets.
func assetURL(baseURL, tag, name string) string {
	return baseURL + "/releases/download/" + tag + "/" + name
}

// releasesPage is the page the copy points at when there is nothing to upgrade
// to: whatever the site does have is listed there.
func releasesPage(baseURL string) string {
	return baseURL + "/releases"
}

// resolveClient probes for the redirect that names the latest release without
// following it, the redirect being the answer rather than a step towards one.
func resolveClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// shortClient fetches the checksums file, a few hundred bytes, under the same
// budget as the probe above. It follows redirects, unlike the probe.
func shortClient() *http.Client {
	return &http.Client{Timeout: probeTimeout}
}

// downloadClient fetches the release asset. It bounds how long the server may
// take to start answering but puts no deadline on the transfer: a connection
// that stalls fails quickly, while a slow but live download over a thin link is
// allowed to finish rather than being cut off partway by a clock, and Ctrl-C is
// the user's way out of one they no longer want. Redirects are followed, since
// an asset URL answers with one to the storage host serving the bytes. The
// default transport is cloned rather than built from nothing, which is what
// keeps a machine behind a proxy working.
func downloadClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: transport}
}

// asideName is where a Windows upgrade parks the binary it is replacing. The
// replacement and the cleanup at the start of the next run both derive it here,
// so the two can never look for different files.
func asideName(target string) string {
	return target + ".old"
}

// removeStaleAside clears what a previous Windows upgrade parked beside the
// binary. That upgrade's own delete fails for as long as the upgrading process
// is alive, so the file waits here for the next run. Finding nothing to remove
// is the ordinary case, and neither it nor a failure is reported.
func removeStaleAside(execPath, goos string) {
	if goos != "windows" {
		return
	}
	os.Remove(asideName(execPath))
}

// replaceBinary moves the verified download onto the running binary.
//
// Off Windows that is one rename inside the install directory, so it crosses no
// filesystem and leaves no moment where the binary is missing.
//
// Windows holds a running image in a way that allows renaming it but not
// overwriting it, so there the binary is moved aside first and the download
// renamed into its place. Deleting the aside then fails for as long as this
// process lives; that failure is the expected outcome and is never reported,
// the file being cleared at the start of the next run instead. If the second
// rename fails the aside is moved back, so the user is left with a working
// binary rather than none.
//
// The platform is a parameter rather than a build tag, which is what keeps both
// sequences compiled, vetted and driven by tests on every machine. What no test
// here reaches is the interaction with a genuinely running image, since that
// needs Windows itself.
func replaceBinary(tmpName, target, goos string) error {
	if goos != "windows" {
		return os.Rename(tmpName, target)
	}

	aside := asideName(target)
	if err := os.Rename(target, aside); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Rename(aside, target)
		return err
	}
	os.Remove(aside)
	return nil
}
