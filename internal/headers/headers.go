// Package headers lints a deploy-root _headers file before upload, applying the
// same rules the server does so the user gets local, line-numbered feedback.
// Framing and platform-managed names are dropped rather than rejected, and each
// drop comes back as a warning so it is visible rather than silent.
//
// The lint is UX only. The CLI is untrusted, so the server parses and validates
// the same file and stays the single authority; a lint that disagrees with the
// server is a UX bug, not a security hole.
package headers

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The caps match the server's. The 100-block ceiling is Cloudflare's documented
// limit, so a file we accept still ports to it.
const (
	maxBytes            = 65536
	maxRules            = 100
	maxHeadersPerRule   = 64
	maxTotalHeaderBytes = 32768
	maxLineBytes        = 2048
)

// splat is the _redirects capture token; reflecting it into a header value has
// no binding in the fixed-headers emission, so it is refused rather than emitted
// as the literal text.
const splat = ":splat"

// tokenRegex is the RFC 7230 token charset, anchored end to end (the anchors
// reject "Bad Name" by refusing the partial match). The backtick is a token
// char, so it is spliced in rather than written inside the raw string.
var tokenRegex = regexp.MustCompile(`\A[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+\z`)

// valueRegex is printable ASCII only: it subsumes CR/LF/NUL/C0/DEL, the high
// bytes, and tab. Braces are inside the range and are forbidden separately.
var valueRegex = regexp.MustCompile(`\A[\x20-\x7e]+\z`)

// namedPlaceholder matches a ":name" capture at a path-segment start. A
// mid-segment colon (`/a:b`) is a legitimate literal path and is not matched.
var namedPlaceholder = regexp.MustCompile(`(^|/):[A-Za-z_]`)

var pathForbiddenChars = []string{"\x00", "\n", "\r", "\t", " ", "{", "}", "\\"}

// The edge expands braces in a header value as a placeholder at response time,
// so they are forbidden here exactly as the path forbids them.
var valueForbiddenChars = []string{"{", "}"}

// droppedNames are the framing, hop-by-hop and platform-managed names, folded to
// lower case. They are dropped rather than rejected so a file ported from
// another host still deploys; the server drops the same set independently.
var droppedNames = map[string]bool{
	"connection":        true,
	"proxy-connection":  true,
	"keep-alive":        true,
	"transfer-encoding": true,
	"te":                true,
	"trailer":           true,
	"upgrade":           true,
	"content-length":    true,
	"content-type":      true,
	"content-encoding":  true,
	"content-range":     true,
	"accept-ranges":     true,
	"host":              true,
	"date":              true,
	"server":            true,
	"age":               true,
	"alt-svc":           true,
	"allow":             true,
	"location":          true,
}

// Lint checks <dir>/_headers when it is present and a regular file, returning one
// warning per dropped line and a line-numbered error for a malformed file. A file
// it cannot read at all is not an error: the lint must never block a deploy the
// server would have accepted.
func Lint(dir string) ([]string, error) {
	path := filepath.Join(dir, "_headers")

	info, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	if info.Size() > maxBytes {
		return nil, errors.New(i18n.Tf("headers.err_too_large", maxBytes))
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	return Parse(content)
}

// Parse validates _headers content, returning the drop warnings and a
// line-numbered error on the first hard reject. Warnings collected before an
// error come back alongside it, and callers discard them there, so a drop is
// reported only for a file that is otherwise valid.
func Parse(content []byte) ([]string, error) {
	if len(content) > maxBytes {
		return nil, errors.New(i18n.Tf("headers.err_too_large", maxBytes))
	}
	// NUL is valid UTF-8, so it is checked at the byte level before the UTF-8 check.
	if bytes.IndexByte(content, 0) != -1 {
		return nil, errors.New(i18n.T("headers.err_nul_byte"))
	}
	if !utf8.Valid(content) {
		return nil, errors.New(i18n.T("headers.err_not_utf8"))
	}

	text := stripBOM(content)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	// A real line-ending CR is gone; any residual CR is a stray one that, if
	// treated as a break, could split a line into two directives.
	if strings.Contains(text, "\r") {
		return nil, errors.New(i18n.T("headers.err_stray_carriage_return"))
	}

	p := &parser{}
	for i, line := range strings.Split(text, "\n") {
		lineno := i + 1
		if len(line) > maxLineBytes {
			return p.warnings, fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.Tf("headers.err_line_too_long", maxLineBytes))
		}

		contentLine := stripIndent(line)
		switch {
		case contentLine == "":
			if err := p.flush(); err != nil {
				return p.warnings, err
			}
		case strings.HasPrefix(contentLine, "#"):
			// A comment does not close the open block.
		case indented(line):
			if err := p.addDirective(contentLine, lineno); err != nil {
				return p.warnings, err
			}
		case strings.HasPrefix(contentLine, "/"):
			if err := p.openBlock(contentLine, lineno); err != nil {
				return p.warnings, err
			}
		default:
			return p.warnings, fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.T("headers.err_expected_path_or_comment"))
		}
	}

	if err := p.flush(); err != nil {
		return p.warnings, err
	}
	if p.totalBytes > maxTotalHeaderBytes {
		return p.warnings, errors.New(i18n.Tf("headers.err_total_bytes", maxTotalHeaderBytes))
	}
	return p.warnings, nil
}

type block struct {
	path       string
	lineno     int
	set        [][2]string
	unset      []string
	directives int
}

type parser struct {
	cur        *block
	ruleCount  int
	totalBytes int
	warnings   []string
}

func (p *parser) openBlock(contentLine string, lineno int) error {
	path := trimST(contentLine)
	if err := validatePath(path); err != nil {
		return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), err)
	}
	if err := p.flush(); err != nil {
		return err
	}
	p.cur = &block{path: path, lineno: lineno}
	return nil
}

func (p *parser) addDirective(contentLine string, lineno int) error {
	if p.cur == nil {
		return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.T("headers.err_header_without_path"))
	}

	kind, name, value, err := parseDirective(contentLine, lineno)
	if err != nil {
		return err
	}

	p.cur.directives++
	if p.cur.directives > maxHeadersPerRule {
		return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.Tf("headers.err_too_many_headers", maxHeadersPerRule))
	}

	// The drop is charged against the per-block cap above but otherwise removes
	// the line before the value gate, so a ported framing header still deploys.
	if droppedNames[strings.ToLower(name)] {
		p.warnings = append(p.warnings,
			fmt.Sprintf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.Tf("headers.warn_dropped", name)))
		return nil
	}

	switch kind {
	case kindSet:
		if strings.Contains(value, splat) {
			return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.T("headers.err_value_interpolation"))
		}
		if err := validateSetName(name); err != nil {
			return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), err)
		}
		if err := validateValue(value); err != nil {
			return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), err)
		}
		p.cur.set = append(p.cur.set, [2]string{name, value})
	case kindUnset:
		if err := validateUnsetName(name); err != nil {
			return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), err)
		}
		p.cur.unset = append(p.cur.unset, name)
	}
	return nil
}

func (p *parser) flush() error {
	if p.cur == nil {
		return nil
	}
	b := p.cur

	if b.directives == 0 {
		return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", b.lineno), i18n.Tf("headers.err_empty_block", b.path))
	}

	p.ruleCount++
	if p.ruleCount > maxRules {
		return errors.New(i18n.Tf("headers.err_too_many_blocks", maxRules))
	}

	if err := checkConflict(b); err != nil {
		return fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", b.lineno), err)
	}

	// The total byte bound is on the kept set only (dropped names never reach it).
	for _, kv := range b.set {
		p.totalBytes += len(kv[0]) + len(kv[1])
	}
	for _, name := range b.unset {
		p.totalBytes += len(name)
	}

	p.cur = nil
	return nil
}

const (
	kindSet   = "set"
	kindUnset = "unset"
)

// parseDirective splits an indented directive into a set (`Name: value`) or an
// unset (`! Name`). Since `!` is itself a token character, it is the whitespace
// after it that marks an unset, which keeps `!Foo: bar` parsing as a set of the
// header named `!Foo`.
func parseDirective(contentLine string, lineno int) (kind, name, value string, err error) {
	if len(contentLine) >= 2 && contentLine[0] == '!' && (contentLine[1] == ' ' || contentLine[1] == '\t') {
		n := trimST(contentLine[1:])
		if n == "" {
			return "", "", "", fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.T("headers.err_unset_needs_name"))
		}
		return kindUnset, n, "", nil
	}

	idx := strings.Index(contentLine, ":")
	if idx == -1 {
		return "", "", "", fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.T("headers.err_expected_directive"))
	}

	name = contentLine[:idx]
	value = trimST(contentLine[idx+1:])
	switch {
	case name == "":
		return "", "", "", fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.T("headers.err_empty_name"))
	case value == "":
		return "", "", "", fmt.Errorf("%s: %s", i18n.Tf("headers.line_prefix", lineno), i18n.Tf("headers.err_empty_value", name))
	}
	return kindSet, name, value, nil
}

func validatePath(path string) error {
	switch {
	case !strings.HasPrefix(path, "/"):
		return errors.New(i18n.T("headers.err_path_must_start_with_slash"))
	case namedPlaceholder.MatchString(path):
		return errors.New(i18n.T("headers.err_path_named_placeholder"))
	case !singleTrailingStar(path):
		return errors.New(i18n.T("headers.err_path_wildcard"))
	}
	for _, c := range pathForbiddenChars {
		if strings.Contains(path, c) {
			return errors.New(i18n.T("headers.err_path_forbidden_characters"))
		}
	}
	return nil
}

func singleTrailingStar(path string) bool {
	switch strings.Count(path, "*") {
	case 0:
		return true
	case 1:
		return strings.HasSuffix(path, "*")
	default:
		return false
	}
}

func validateSetName(name string) error {
	if !tokenRegex.MatchString(name) {
		return errors.New(i18n.Tf("headers.err_invalid_token", name))
	}
	switch folded := strings.ToLower(name); {
	case folded == "set-cookie":
		return errors.New(i18n.T("headers.err_set_cookie"))
	case folded == "strict-transport-security":
		return errors.New(i18n.T("headers.err_hsts"))
	case droppedNames[folded]:
		return errors.New(i18n.Tf("headers.err_managed_name", name))
	}
	return nil
}

func validateValue(value string) error {
	if !valueRegex.MatchString(value) {
		return errors.New(i18n.T("headers.err_value_charset"))
	}
	for _, c := range valueForbiddenChars {
		if strings.Contains(value, c) {
			return errors.New(i18n.T("headers.err_value_braces"))
		}
	}
	return nil
}

func validateUnsetName(name string) error {
	if !tokenRegex.MatchString(name) {
		return errors.New(i18n.Tf("headers.err_invalid_token", name))
	}
	return nil
}

func checkConflict(b *block) error {
	setNames := make(map[string]bool, len(b.set))
	for _, kv := range b.set {
		setNames[strings.ToLower(kv[0])] = true
	}
	for _, name := range b.unset {
		if setNames[strings.ToLower(name)] {
			return errors.New(i18n.Tf("headers.err_set_and_unset", strings.ToLower(name)))
		}
	}
	return nil
}

func stripBOM(content []byte) string {
	return strings.TrimPrefix(string(content), "\ufeff")
}

func stripIndent(line string) string {
	return strings.TrimLeft(line, " \t")
}

func indented(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

func trimST(s string) string {
	return strings.Trim(s, " \t")
}
