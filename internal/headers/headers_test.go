package headers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: every lint message and drop warning renders
// from the message catalog. Load rather than Setup: nothing here reports
// which language is in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

func TestParseAccepted(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"a block with several set lines", "/*\n  X-Custom-Header: value\n  Cache-Control: public, max-age=3600"},
		{"exact path", "/about\n  X: y"},
		{"exact file path", "/assets/style.css\n  X: y"},
		{"site-wide wildcard", "/*\n  X: y"},
		{"prefix wildcard", "/blog/*\n  X: y"},
		{"repeated name multi-value", "/*\n  Link: </a>\n  Link: </b>"},
		{"unset of a default", "/embed/*\n  ! X-Frame-Options"},
		{"unset of a non-default", "/*\n  ! X-Whatever"},
		{"value interior spaces preserved", "/*\n  Content-Security-Policy: default-src 'self'; img-src *"},
		{"comments inside and outside a block", "# outside\n/a\n  # inside\n  X-A: 1\n\n/b\n  X-B: 2"},
		{"two blocks same path", "/*\n  X-A: 1\n/*\n  X-A: 2"},
		{"CRLF line endings", "/a\r\n  X: y\r\n"},
		{"leading BOM", "\ufeff/a\n  X: y"},
		{"tab and multi-space indentation", "/a\n\tX: y\n      Z: w"},
		{"comments-only", "# only a comment\n\n"},
		{"empty", ""},
		{"Na:me parses as name Na", "/a\n  Na:me: v"},
		{"a literal mid-segment colon in a path", "/a:b\n  X: y"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := Parse([]byte(tt.content))
			if err != nil {
				t.Errorf("Parse(%q) = %v, want nil", tt.content, err)
			}
			if len(warnings) != 0 {
				t.Errorf("Parse(%q) warnings = %v, want none", tt.content, warnings)
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
		{"NUL byte", "/a\n  X: y\x00", "NUL"},
		{"mid-line CR", "/a\n  X: a\rb", "carriage return"},
		{"lone CR at EOF", "/a\n  X: y\r", "carriage return"},
		{"empty value", "/a\n  X-Foo:", "_headers line 2: header \"X-Foo\" has an empty value"},
		{"whitespace-only value", "/a\n  X-Foo:   ", "empty value"},
		{"non-token name", "/a\n  Bad Name: y", "_headers line 2: header name \"Bad Name\" is not a valid token"},
		{"empty name", "/a\n  : y", "_headers line 2: header name is empty"},
		{"control char in value", "/a\n  X: a\x01b", "printable ASCII"},
		{"DEL in value", "/a\n  X: a\x7fb", "printable ASCII"},
		{"high byte in value", "/a\n  X: café", "printable ASCII"},
		{"brace in value", "/a\n  X-Leak: {env.SECRET}", "_headers line 2: header value must not contain { or }"},
		{"star mid pattern", "/a*b\n  X: y", "trailing wildcard"},
		{"doubled star", "/a/**\n  X: y", "trailing wildcard"},
		{"placeholder path", "/blog/:slug\n  X: y", "named placeholder"},
		{"brace in path", "/a{b\n  X: y", "_headers line 1: path contains forbidden characters"},
		{"value interpolation", "/a/*\n  X-Foo: /new/:splat", "value interpolation"},
		{"bare directive col 0", "X-Foo: bar", "_headers line 1: expected a path"},
		{"indented no open block", "  X-Foo: bar", "no open block"},
		{"empty block", "/a\n\n/b\n  X: y", "has no headers"},
		{"directive without colon or bang", "/a\n  no-colon-here", "Name: value"},
		{"bang with no name", "/a\n  !   ", "! must be followed by a header name"},
		{"set unset conflict mixed case", "/a\n  X-Foo: v\n  ! x-foo", "both set and unset"},
		{"Set-Cookie", "/a\n  Set-Cookie: a=b", "_headers line 2: Set-Cookie is not supported"},
		{"set-cookie case-folded", "/a\n  set-cookie: a=b", "Set-Cookie is not supported"},
		{"HSTS", "/a\n  Strict-Transport-Security: max-age=1", "Strict-Transport-Security is not supported"},
		{"line number reported", "/a\n  X-A: 1\n/b\n  Bad Name: 2", "_headers line 4:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.content))
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
// the "_headers line N" frame and the message it fronts. A substring check on
// the message alone still passes when the frame is looked up from the wrong
// key, so these cases compare the whole error string, one case per rejection
// shape covered here.
func TestParseRejectionsRenderTheWholeLine(t *testing.T) {
	manyHeaders := "/a"
	for i := 0; i <= maxHeadersPerRule; i++ {
		manyHeaders += fmt.Sprintf("\n  X-H%d: v", i)
	}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"line too long", "/a\n  X-Long: " + strings.Repeat("y", maxLineBytes),
			fmt.Sprintf("_headers line 2: line is too long (limit %d bytes)", maxLineBytes)},
		{"header before any path", "  X-Foo: bar",
			"_headers line 1: a header must follow a path line (indented line with no open block)"},
		{"too many headers in one block", manyHeaders,
			fmt.Sprintf("_headers line %d: too many headers in this block (limit %d)", maxHeadersPerRule+2, maxHeadersPerRule)},
		{"value interpolation", "/a/*\n  X-Foo: /new/:splat",
			"_headers line 2: value interpolation (:splat) is not supported"},
		{"empty block", "/a\n\n/b\n  X: y",
			"_headers line 1: block /a has no headers (an empty block is not allowed)"},
		{"set and unset conflict", "/a\n  X-Foo: v\n  ! x-foo",
			"_headers line 1: header x-foo is both set and unset in the same block"},
		{"unset without a name", "/a\n  !   ",
			"_headers line 2: ! must be followed by a header name"},
		{"unset of a name that is not a token", "/a\n  ! Bad Name",
			`_headers line 2: header name "Bad Name" is not a valid token`},
		{"directive with neither colon nor bang", "/a\n  no-colon-here",
			`_headers line 2: expected "Name: value" or "! Name"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.content))
			if err == nil {
				t.Fatalf("Parse(%q) = nil, want error", tt.content)
			}
			if err.Error() != tt.want {
				t.Errorf("Parse(%q) error =\n  %q\nwant:\n  %q", tt.content, err.Error(), tt.want)
			}
		})
	}
}

func TestParseDropWarnings(t *testing.T) {
	t.Run("a framing name is dropped with a warning, the rest survives", func(t *testing.T) {
		warnings, err := Parse([]byte("/*\n  Connection: keep-alive\n  X-Keep: yes"))
		if err != nil {
			t.Fatalf("Parse = %v, want nil (drop, not reject)", err)
		}
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want exactly 1", warnings)
		}
		if !strings.Contains(warnings[0], "_headers line 2: Connection is managed by the platform and was dropped.") {
			t.Errorf("warning = %q", warnings[0])
		}
	})

	t.Run("each framing / platform-managed name is dropped", func(t *testing.T) {
		for _, name := range []string{
			"Content-Length", "Transfer-Encoding", "Content-Type", "Content-Encoding",
			"Host", "Date", "Server", "Age", "Location", "Accept-Ranges",
		} {
			warnings, err := Parse([]byte("/*\n  " + name + ": v\n  X-Keep: yes"))
			if err != nil {
				t.Errorf("Parse(%s) = %v, want nil", name, err)
			}
			if len(warnings) != 1 {
				t.Errorf("Parse(%s) warnings = %v, want 1", name, warnings)
			}
		}
	})

	t.Run("a mixed-case framing name is dropped", func(t *testing.T) {
		warnings, err := Parse([]byte("/*\n  transfer-encoding: chunked\n  X-Keep: yes"))
		if err != nil || len(warnings) != 1 {
			t.Errorf("Parse = (%v, %v), want (1 warning, nil)", warnings, err)
		}
	})

	t.Run("a block whose every directive is dropped is accepted with warnings", func(t *testing.T) {
		warnings, err := Parse([]byte("/*\n  Connection: keep-alive\n  Content-Length: 5"))
		if err != nil {
			t.Fatalf("Parse = %v, want nil", err)
		}
		if len(warnings) != 2 {
			t.Errorf("warnings = %v, want 2", warnings)
		}
	})
}

func TestParseNonUTF8Rejected(t *testing.T) {
	_, err := Parse([]byte("/a\n  X: \xff\xfe"))
	if err == nil {
		t.Fatal("Parse(non-utf8) = nil, want error")
	}
	if want := "_headers must be valid UTF-8 text"; !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(non-utf8) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverByteCap(t *testing.T) {
	big := strings.Repeat("# padding comment line\n", 5000)
	if len(big) <= maxBytes {
		t.Fatalf("test fixture too small: %d bytes", len(big))
	}
	_, err := Parse([]byte(big))
	if err == nil {
		t.Fatal("Parse(oversized) = nil, want error")
	}
	if want := fmt.Sprintf("_headers is too large (limit %d bytes)", maxBytes); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(oversized) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverBlockCap(t *testing.T) {
	// maxRules+1 blocks (identical paths are allowed), each a single directive.
	var b strings.Builder
	for i := 0; i <= maxRules; i++ {
		b.WriteString("/p\n  X: y\n")
	}
	if b.Len() >= maxBytes {
		t.Fatalf("block-cap fixture exceeds byte cap: %d bytes", b.Len())
	}
	_, err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("Parse(over block cap) = nil, want error")
	}
	if want := fmt.Sprintf("_headers: too many header blocks (limit %d)", maxRules); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(over block cap) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverHeadersPerBlockCap(t *testing.T) {
	// One block with maxHeadersPerRule+1 directives (a repeated name is allowed).
	var b strings.Builder
	b.WriteString("/*\n")
	for i := 0; i <= maxHeadersPerRule; i++ {
		b.WriteString("  X: v\n")
	}
	_, err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("Parse(over per-block cap) = nil, want error")
	}
	if want := fmt.Sprintf("too many headers in this block (limit %d)", maxHeadersPerRule); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(over per-block cap) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverTotalBytesCap(t *testing.T) {
	// 65 single-directive blocks of (1-byte name + 511-byte value) = 65 * 512 =
	// 33280 kept bytes, over the 32768 cap, while each line and the file stay
	// under their own caps and the per-block count is 1.
	value := strings.Repeat("a", 511)
	var b strings.Builder
	for i := 0; i < 65; i++ {
		b.WriteString("/p\n  X: ")
		b.WriteString(value)
		b.WriteString("\n")
	}
	if b.Len() >= maxBytes {
		t.Fatalf("total-bytes fixture exceeds byte cap: %d bytes", b.Len())
	}
	_, err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("Parse(over total-bytes cap) = nil, want error")
	}
	if want := fmt.Sprintf("_headers exceeds the total header byte limit (%d bytes)", maxTotalHeaderBytes); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(over total-bytes cap) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseDropsChargedAgainstPerBlockCap(t *testing.T) {
	// A flood of droppable framing lines must still trip the per-block cap: the
	// count is charged on raw input, so an attacker cannot evade it by padding.
	var b strings.Builder
	b.WriteString("/*\n")
	for i := 0; i <= maxHeadersPerRule; i++ {
		b.WriteString("  Connection: keep-alive\n")
	}
	_, err := Parse([]byte(b.String()))
	if err == nil {
		t.Fatal("Parse(over per-block cap via droppable lines) = nil, want error")
	}
	if want := fmt.Sprintf("too many headers in this block (limit %d)", maxHeadersPerRule); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(over per-block cap via droppable lines) error = %q, want to contain %q", err.Error(), want)
	}
}

func TestParseOverLineCap(t *testing.T) {
	long := "/*\n  X-Foo: " + strings.Repeat("a", maxLineBytes+50)
	_, err := Parse([]byte(long))
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
		warnings, err := Lint(dir)
		if err != nil || warnings != nil {
			t.Errorf("Lint(empty dir) = (%v, %v), want (nil, nil)", warnings, err)
		}
	})

	t.Run("valid file is nil", func(t *testing.T) {
		dir := t.TempDir()
		writeHeaders(t, dir, "/*\n  X-Custom: v\n")
		warnings, err := Lint(dir)
		if err != nil {
			t.Errorf("Lint(valid) = %v, want nil", err)
		}
		if len(warnings) != 0 {
			t.Errorf("Lint(valid) warnings = %v, want none", warnings)
		}
	})

	t.Run("dropped line returns a warning, not an error", func(t *testing.T) {
		dir := t.TempDir()
		writeHeaders(t, dir, "/*\n  Server: leak\n  X-Keep: yes\n")
		warnings, err := Lint(dir)
		if err != nil {
			t.Fatalf("Lint(drop) = %v, want nil", err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "Server is managed by the platform and was dropped.") {
			t.Errorf("Lint(drop) warnings = %v", warnings)
		}
	})

	t.Run("malformed file errors", func(t *testing.T) {
		dir := t.TempDir()
		writeHeaders(t, dir, "/*\n  Set-Cookie: a=b\n")
		_, err := Lint(dir)
		if err == nil {
			t.Fatal("Lint(malformed) = nil, want error")
		}
		if !strings.Contains(err.Error(), "Set-Cookie is not supported") {
			t.Errorf("Lint(malformed) error = %q", err.Error())
		}
	})

	t.Run("directory named _headers is ignored", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "_headers"), 0755); err != nil {
			t.Fatal(err)
		}
		warnings, err := Lint(dir)
		if err != nil || warnings != nil {
			t.Errorf("Lint(dir _headers) = (%v, %v), want (nil, nil)", warnings, err)
		}
	})

	t.Run("oversize file errors", func(t *testing.T) {
		dir := t.TempDir()
		writeHeaders(t, dir, strings.Repeat("# pad\n", 20000))
		_, err := Lint(dir)
		if err == nil {
			t.Fatal("Lint(oversize) = nil, want error")
		}
		if want := fmt.Sprintf("_headers is too large (limit %d bytes)", maxBytes); !strings.Contains(err.Error(), want) {
			t.Errorf("Lint(oversize) error = %q, want to contain %q", err.Error(), want)
		}
	})
}

func writeHeaders(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "_headers"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
