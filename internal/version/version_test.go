package version

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsVersion(t *testing.T) {
	var buf bytes.Buffer
	Run(&buf, "v0.1.0")

	got := buf.String()
	if got != "kamakiri v0.1.0\n" {
		t.Errorf("got %q, want %q", got, "kamakiri v0.1.0\n")
	}
}

func TestRunDevDefault(t *testing.T) {
	var buf bytes.Buffer
	Run(&buf, "(dev)")

	if got := buf.String(); !strings.Contains(got, "(dev)") {
		t.Errorf("got %q, want it to contain %q", got, "(dev)")
	}
}
