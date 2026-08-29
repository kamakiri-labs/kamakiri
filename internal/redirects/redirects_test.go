package redirects

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: every lint message renders from the message
// catalog. Load rather than Setup: nothing here reports which language is
// in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

func TestParseAccepted(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"exact default 301", "/old /new"},
		{"explicit 302", "/a /b 302"},
		{"explicit 303", "/a /b 303"},
		{"explicit 307", "/a /b 307"},
		{"explicit 308", "/a /b 308"},
		{"wildcard splat", "/blog/* /articles/:splat"},
		{"splat to root", "/old/* /:splat"},
		{"multi-segment splat", "/a/b/* /x/y/:splat 308"},
		{"comments and blanks", "# comment\n/a /b\n\n# another\n/c /d 302"},
		{"comments-only", "# only a comment\n\n"},
		{"empty", ""},
		{"CRLF and lone CR", "/a /b\r\n/c /d\r"},
		{"leading BOM", "\ufeff/a /b"},
		{"multi space and tab separation", "/a \t  /b\t301"},
		{"non-scheme colon target", "/old /a:b"},
		{"root target", "/old /"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Parse([]byte(tt.content)); err != nil {
				t.Errorf("Parse(%q) = %v, want nil", tt.content, err)
			}
		})
	}
}

func TestParseRejected(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string // substring expected in the error
	}{
		{"status 200", "/a /b 200", "line 1: invalid status 200"},
		{"non-redirect 3xx", "/a /b 304", "line 1: invalid status 304"},
		{"external http", "/a http://evil.com", "line 1: to must start with /"},
		{"protocol-relative", "/a //evil.com", "site-absolute"},
		{"backslash authority", "/a /\\evil.com", "line 1:"},
		{"encoded slash", "/a /%2f%2fevil.com", "encoded slash or backslash"},
		{"encoded backslash", "/a /%5cevil.com", "encoded slash or backslash"},
		{"named placeholder", "/movies/:id /media/:id", "named placeholders"},
		{"literal star in source", "/a*b /c", "line 1:"},
		{"literal star in destination", "/a/* /b/*", "use :splat (not *)"},
		{"splat not at suffix", "/a/* /b/:splat/c", "line 1:"},
		{"splat in source", "/a/:splat /b", "line 1: :splat is only allowed in the destination"},
		{"wildcard source without splat", "/a/* /b", "line 1: from ends with *"},
		{"forced rule", "/a /b 301!", "forced"},
		{"too many fields", "/a /b 302 Country=au", "line 1: too many fields"},
		{"missing destination", "/only-one-field", "line 1: missing destination"},
		{"control character", "/a\f /b", "control character"},
		{"brace in source", "/a{b /c", "line 1: from contains forbidden characters"},
		{"line number reported", "/a /b\n/c /d\n/e http://evil", "line 3:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Parse([]byte(tt.content))
			if err == nil {
				t.Fatalf("Parse(%q) = nil, want error", tt.content)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) error = %q, want to contain %q", tt.content, err.Error(), tt.want)
			}
		})
	}
}

// A rejection the user reads as one line is two catalog lookups in the source:
// the "line N" frame and the message it fronts. A substring check on the
// message alone still passes when the frame is looked up from the wrong key, so
// these cases compare the whole error string. Each rule sits on line 2, behind a
// comment, so the frame's number is pinned as well as its wording, one case per
// rejection shape covered here.
func TestParseRejectionsRenderTheWholeLine(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want string
	}{
		{"line too long", "/old " + strings.Repeat("/b", maxLineBytes),
			fmt.Sprintf("line 2: line is too long (limit %d bytes)", maxLineBytes)},
		{"status is not a number", "/a /b 404x",
			"line 2: invalid status code; expected 301, 302, 303, 307, or 308"},
		{"forced redirect", "/a /b 301!",
			"line 2: forced redirects (a '!' suffix) are not supported"},
		{"control character in a path", "/a\x01 /b",
			"line 2: path contains a control character"},
		{"splat before the last segment of a splat destination", "/a/* /x/:splat/y/:splat",
			"line 2: :splat is only allowed as the destination's trailing path segment"},
		{"named placeholder before a trailing splat", "/a/* /x/:name/:splat",
			"line 2: named placeholders (:name) are not supported"},
		{"splat not trailing in the destination", "/a/* /x/:splat/y",
			"line 2: :splat is only allowed as the destination's trailing path segment"},
		{"named placeholder in the destination", "/a /b/:other",
			"line 2: named placeholders (:name) are not supported"},
		{"encoded CRLF in the source", "/a%0d /b",
			"line 2: from contains encoded CRLF characters"},
		{"wildcard away from the end", "/a*b /c",
			"line 2: from: wildcard * is only allowed as the last character"},
		{"destination wildcard without a source one", "/a /b/:splat",
			"line 2: to ends with * but from does not"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := "# a comment\n" + tt.rule
			err := Parse([]byte(content))
			if err == nil {
				t.Fatalf("Parse(%q) = nil, want error", tt.rule)
			}
			if err.Error() != tt.want {
				t.Errorf("Parse(%q) error =\n  %q\nwant:\n  %q", tt.rule, err.Error(), tt.want)
			}
		})
	}
}

func TestParseDoubleEncodedSlashAccepted(t *testing.T) {
	// %252f decodes once, in the browser, to a literal %2f that stays in the
	// path, so it never breaks the origin and must be accepted.
	if err := Parse([]byte("/old /x%252fy")); err != nil {
		t.Errorf("Parse double-encoded = %v, want nil", err)
	}
}

func TestParseNonUTF8Rejected(t *testing.T) {
	err := Parse([]byte("/a /b\xff\xfe"))
	if err == nil {
		t.Fatal("Parse(non-utf8) = nil, want error")
	}
	if want := "_redirects must be valid UTF-8 text"; !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(non-utf8) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverByteCap(t *testing.T) {
	big := strings.Repeat("# padding comment line\n", 5000)
	if len(big) <= maxBytes {
		t.Fatalf("test fixture too small: %d bytes", len(big))
	}
	err := Parse([]byte(big))
	if err == nil {
		t.Fatal("Parse(oversized) = nil, want error")
	}
	if want := fmt.Sprintf("_redirects is too large (limit %d bytes)", maxBytes); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(oversized) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverRuleCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxRules+1; i++ {
		b.WriteString("/a /b\n")
	}
	if b.Len() >= maxBytes {
		t.Fatalf("rule-cap fixture exceeds byte cap: %d bytes", b.Len())
	}
	err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("Parse(over rule cap) = nil, want error")
	}
	if want := fmt.Sprintf("too many redirect rules (limit %d)", maxRules); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(over rule cap) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverLineCap(t *testing.T) {
	long := "/a" + strings.Repeat("b", maxLineBytes+50) + " /c"
	err := Parse([]byte(long))
	if err == nil {
		t.Fatal("Parse(over line cap) = nil, want error")
	}
	if want := fmt.Sprintf("line is too long (limit %d bytes)", maxLineBytes); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(over line cap) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestLint(t *testing.T) {
	t.Run("absent file is nil", func(t *testing.T) {
		dir := t.TempDir()
		if err := Lint(dir); err != nil {
			t.Errorf("Lint(empty dir) = %v, want nil", err)
		}
	})

	t.Run("valid file is nil", func(t *testing.T) {
		dir := t.TempDir()
		writeRedirects(t, dir, "/old /new 301\n")
		if err := Lint(dir); err != nil {
			t.Errorf("Lint(valid) = %v, want nil", err)
		}
	})

	t.Run("malformed file errors", func(t *testing.T) {
		dir := t.TempDir()
		writeRedirects(t, dir, "/old no-slash-destination\n")
		err := Lint(dir)
		if err == nil {
			t.Fatal("Lint(malformed) = nil, want error")
		}
		if !strings.Contains(err.Error(), "to must start with /") {
			t.Errorf("Lint(malformed) error = %q", err.Error())
		}
	})

	t.Run("directory named _redirects is ignored", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "_redirects"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := Lint(dir); err != nil {
			t.Errorf("Lint(dir _redirects) = %v, want nil", err)
		}
	})

	t.Run("oversize file errors", func(t *testing.T) {
		dir := t.TempDir()
		writeRedirects(t, dir, strings.Repeat("# pad\n", 20000))
		err := Lint(dir)
		if err == nil {
			t.Fatal("Lint(oversize) = nil, want error")
		}
		if want := fmt.Sprintf("_redirects is too large (limit %d bytes)", maxBytes); !strings.Contains(err.Error(), want) {
			t.Errorf("Lint(oversize) error = %q, want to contain %q", err.Error(), want)
		}
	})
}

func writeRedirects(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "_redirects"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
