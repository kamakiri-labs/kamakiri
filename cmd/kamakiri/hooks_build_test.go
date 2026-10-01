package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testHookNames mirrors cdn.EnvSkipCertCheck (internal/cdn/watch.go) and
// freshness.EnvFreshnessPollMS (internal/freshness/freshness.go). If either
// constant's value changes, the tagged build stops containing the old name
// and the test fails until this list follows it.
var testHookNames = []string{
	"KAMAKIRI_CDN_SKIP_CERT_CHECK",
	"KAMAKIRI_FRESHNESS_POLL_MS",
}

// TestTestHookNamesStayOutOfUntaggedBuild builds the CLI as a user gets it
// and checks that neither test-hook variable name survives into the binary,
// so no code path in a build without the testhooks tag still uses them. The
// tagged build must contain both names, which proves the check can see them
// at all.
//
// The names live in exported constants in untagged files, so they are not
// compiled out by the tag. They drop out of the untagged binary only because
// the off stub of testhooks.Getenv is inlined and its argument eliminated.
// Any code path that keeps a name live, a non-inlined stub or a direct use of
// a constant, puts it back, and that is what this test catches.
//
// Both builds pass -tags and -gcflags=all= on the command line because a
// -gcflags=all=-l inherited through GOFLAGS would disable inlining and put
// the names back, and an inherited -tags=testhooks would compile the readers
// in. Flags on the command line override GOFLAGS wherever it comes from;
// clearing GOFLAGS in the child's environment would not be enough, since go
// then falls back to the value in its go env file.
func TestTestHookNamesStayOutOfUntaggedBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI twice; skipped under -short")
	}
	// go test puts its own toolchain first on PATH, so this is the go that
	// runs the test.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain on PATH to build the CLI with: %v", err)
	}

	untagged := buildKamakiri(t, goBin, "")
	for _, name := range testHookNames {
		if bytes.Contains(untagged, []byte(name)) {
			t.Errorf("untagged build contains %s; a code path outside the testhooks tag keeps it live", name)
		}
	}

	tagged := buildKamakiri(t, goBin, "testhooks")
	for _, name := range testHookNames {
		if !bytes.Contains(tagged, []byte(name)) {
			t.Errorf("testhooks build does not contain %s; the check cannot see the name, so the untagged result proves nothing", name)
		}
	}
}

// buildKamakiri builds ./cmd/kamakiri from the module root with the given
// build tags and returns the binary's bytes.
func buildKamakiri(t *testing.T, goBin, tags string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "kamakiri")
	cmd := exec.Command(goBin, "build", "-tags="+tags, "-gcflags=all=", "-o", out, "./cmd/kamakiri")
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags=%q: %v\n%s", tags, err, output)
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the built binary: %v", err)
	}
	return bin
}
