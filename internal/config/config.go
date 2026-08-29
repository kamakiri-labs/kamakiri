package config

// ValidStatus reports whether status is one a redirect domain may use. It backs
// the `--status` flag on `domain add` only; path redirects are authored in a
// deploy-root `_redirects` file and validated there.
func ValidStatus(status int) bool {
	return status == 301 || status == 302 || status == 307 || status == 308
}
