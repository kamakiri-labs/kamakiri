// Package redirects lints a deploy-root _redirects file before upload, applying
// the same rules the server does so the user gets local, line-numbered feedback.
//
// The lint is UX only. The CLI is untrusted, so the server parses and validates
// the same file and stays the single authority; a lint that disagrees with the
// server is a UX bug, not a security hole.
package redirects

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The caps match the server's, and stay under Netlify's ~2000 and Cloudflare's
// ~2100 rule ceilings so a file we accept still ports to either.
const (
	maxBytes     = 65536
	maxRules     = 1000
	maxLineBytes = 2048
)

// splat is the only placeholder accepted, as the destination's trailing segment.
const splat = ":splat"

// namedPlaceholder matches a Netlify-style ":name" capture at a path-segment
// start (the start of the string or just after a "/"). A mid-segment colon
// (`/a:b`) is a legitimate literal path and is deliberately not matched.
var namedPlaceholder = regexp.MustCompile(`(^|/):[A-Za-z_]`)

// controlChars matches C0 controls and DEL, none of which may reach a Location
// header.
var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

var allowedStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// A raw backslash is forbidden everywhere, since browsers normalize it to "/"
// and `/\evil.com` then leaves the origin. Braces are forbidden so that nothing
// in a path can forge a placeholder the edge would expand.
var forbiddenChars = []string{"\x00", "\n", "\r", "\t", " ", "{", "}", "\\"}

// Lint checks <dir>/_redirects when it is present and a regular file, returning
// a line-numbered error for a malformed one. A file it cannot read at all is not
// an error: the lint must never block a deploy the server would have accepted.
func Lint(dir string) error {
	path := filepath.Join(dir, "_redirects")

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > maxBytes {
		return errors.New(i18n.Tf("redirects.err_too_large", maxBytes))
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return Parse(content)
}

// Parse validates _redirects content, returning a line-numbered error on the
// first problem or nil if every rule is accepted.
func Parse(content []byte) error {
	if len(content) > maxBytes {
		return errors.New(i18n.Tf("redirects.err_too_large", maxBytes))
	}
	if !utf8.Valid(content) {
		return errors.New(i18n.T("redirects.err_not_utf8"))
	}

	text := normalizeNewlines(stripBOM(content))

	count := 0
	for i, line := range strings.Split(text, "\n") {
		lineno := i + 1
		if len(line) > maxLineBytes {
			// The line frame keeps its err_ class here, where the _headers
			// equivalent is the class-neutral line_prefix: this one only ever
			// fronts an error, while _headers puts the same frame in front of
			// the drop warnings its lint returns alongside them.
			return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), i18n.Tf("redirects.err_line_too_long", maxLineBytes))
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if err := parseRule(trimmed, lineno); err != nil {
			return err
		}

		count++
		if count > maxRules {
			return errors.New(i18n.Tf("redirects.err_too_many_rules", maxRules))
		}
	}

	return nil
}

func stripBOM(content []byte) string {
	return strings.TrimPrefix(string(content), "\ufeff")
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func parseRule(trimmed string, lineno int) error {
	fields := strings.FieldsFunc(trimmed, func(r rune) bool { return r == ' ' || r == '\t' })

	var from, to string
	status := 301

	switch len(fields) {
	case 1:
		return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), i18n.T("redirects.err_missing_destination"))
	case 2:
		from, to = fields[0], fields[1]
	case 3:
		from, to = fields[0], fields[1]
		s, err := parseStatus(fields[2])
		if err != nil {
			return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), err)
		}
		status = s
	default:
		return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), i18n.T("redirects.err_too_many_fields"))
	}

	for _, field := range []string{from, to} {
		if controlChars.MatchString(field) {
			return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), i18n.T("redirects.err_control_character"))
		}
	}

	if err := checkSource(from); err != nil {
		return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), err)
	}

	to, err := parseTarget(to)
	if err != nil {
		return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), err)
	}

	if err := validateRule(from, to, status); err != nil {
		return fmt.Errorf("%s: %s", i18n.Tf("redirects.err_line", lineno), err)
	}

	return nil
}

func parseStatus(raw string) (int, error) {
	if strings.HasSuffix(raw, "!") {
		return 0, errors.New(i18n.T("redirects.err_forced_redirect"))
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(i18n.T("redirects.err_invalid_status_code"))
	}
	return n, nil
}

func checkSource(from string) error {
	if strings.Contains(from, splat) {
		return errors.New(i18n.T("redirects.err_splat_in_source"))
	}
	if namedPlaceholder.MatchString(from) {
		return errors.New(i18n.T("redirects.err_named_placeholder"))
	}
	// A trailing `*` (the wildcard) and its position are checked by validateRule.
	return nil
}

// parseTarget rejects a user-typed `*` in the destination and converts a
// trailing `:splat` to the internal trailing `*`, so the only `*` that can
// reach validateRule originates from a `:splat`.
func parseTarget(to string) (string, error) {
	switch {
	case strings.Contains(to, "*"):
		return "", errors.New(i18n.T("redirects.err_star_destination"))
	case strings.HasSuffix(to, "/"+splat):
		base := to[:len(to)-len(splat)]
		if strings.Contains(base, splat) {
			return "", errors.New(i18n.T("redirects.err_splat_not_trailing"))
		}
		if namedPlaceholder.MatchString(base) {
			return "", errors.New(i18n.T("redirects.err_named_placeholder"))
		}
		return strings.TrimSuffix(to, splat) + "*", nil
	case strings.Contains(to, splat):
		return "", errors.New(i18n.T("redirects.err_splat_not_trailing"))
	case namedPlaceholder.MatchString(to):
		return "", errors.New(i18n.T("redirects.err_named_placeholder"))
	default:
		return to, nil
	}
}

func validateRule(from, to string, status int) error {
	if err := validatePrefix(from, "from"); err != nil {
		return err
	}
	if err := validatePrefix(to, "to"); err != nil {
		return err
	}
	if err := validateSameOrigin(to, "to"); err != nil {
		return err
	}
	if err := validateForbidden(from, "from"); err != nil {
		return err
	}
	if err := validateForbidden(to, "to"); err != nil {
		return err
	}
	if err := validateEncodedCRLF(from, "from"); err != nil {
		return err
	}
	if err := validateEncodedCRLF(to, "to"); err != nil {
		return err
	}
	if err := validateWildcard(from, "from"); err != nil {
		return err
	}
	if err := validateWildcard(to, "to"); err != nil {
		return err
	}
	if err := validateWildcardPair(from, to); err != nil {
		return err
	}
	return validateStatus(status)
}

func validatePrefix(path, field string) error {
	if !strings.HasPrefix(path, "/") {
		return errors.New(i18n.Tf("redirects.err_must_start_with_slash", field))
	}
	return nil
}

// validateSameOrigin is the open-redirect gate on the target: a leading "/" is
// not enough, since `//host` and `/\host` are protocol-relative and the encoded
// slash/backslash reintroduce the authority break after one decode pass.
func validateSameOrigin(path, field string) error {
	lower := strings.ToLower(path)
	switch {
	case strings.HasPrefix(path, "//"), strings.HasPrefix(path, "/\\"):
		return errors.New(i18n.Tf("redirects.err_not_site_absolute", field))
	case strings.Contains(lower, "%2f"), strings.Contains(lower, "%5c"):
		return errors.New(i18n.Tf("redirects.err_encoded_slash", field))
	default:
		return nil
	}
}

func validateForbidden(path, field string) error {
	for _, c := range forbiddenChars {
		if strings.Contains(path, c) {
			return errors.New(i18n.Tf("redirects.err_forbidden_characters", field))
		}
	}
	return nil
}

func validateEncodedCRLF(path, field string) error {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%0d") || strings.Contains(lower, "%0a") {
		return errors.New(i18n.Tf("redirects.err_encoded_crlf", field))
	}
	return nil
}

func validateWildcard(path, field string) error {
	if idx := strings.Index(path, "*"); idx != -1 && idx != len(path)-1 {
		return errors.New(i18n.Tf("redirects.err_wildcard_position", field))
	}
	return nil
}

func validateWildcardPair(from, to string) error {
	fromWild := strings.HasSuffix(from, "*")
	toWild := strings.HasSuffix(to, "*")
	switch {
	case fromWild && !toWild:
		return errors.New(i18n.T("redirects.err_from_wildcard_only"))
	case toWild && !fromWild:
		return errors.New(i18n.T("redirects.err_to_wildcard_only"))
	default:
		return nil
	}
}

func validateStatus(status int) error {
	if !allowedStatuses[status] {
		return errors.New(i18n.Tf("redirects.err_invalid_status", status))
	}
	return nil
}
