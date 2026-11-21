//go:build !windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	sigpipeChildEnv = "KAMAKIRI_TEST_SIGPIPE_CHILD"
	sigpipeKeyID    = "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
	sigpipeNewKeyID = "7b2e4d10-9a3c-4f1e-8b20-5c6d7e8f9a01"
	sigpipeSecret   = "kk_deploy_4Wq8ZtR2mN7xK1pL9sB3vC6yH0jD5fGa"
)

// sigpipeCommands are the command lines the child runs, by the value the
// parent puts in sigpipeChildEnv.
var sigpipeCommands = map[string][]string{
	"create": {"key", "create", "--site", "example.com"},
	"rotate": {"key", "rotate", sigpipeKeyID},
}

// The child half of TestKeyCommandsSurviveAClosedStdout. It does nothing
// unless the parent marked it, and then runs the real dispatch on the real
// standard streams, since the runtime raises SIGPIPE only for writes to
// descriptors 1 and 2.
func TestSigpipeChildProcess(t *testing.T) {
	args, ok := sigpipeCommands[os.Getenv(sigpipeChildEnv)]
	if !ok {
		t.Skip("runs only as the child of TestKeyCommandsSurviveAClosedStdout")
	}
	os.Exit(run(args, strings.NewReader(""), os.Stdout, os.Stderr))
}

// When the reader of stdout has already gone, as with `key create | gh secret
// set` after gh failed, the write of the key must come back as an error that
// names the key ID and exits 1. Left to the runtime, the process is killed by
// SIGPIPE inside the write: no line, and a signal status. A rotation is held
// to the same, since its old key is gone by then.
func TestKeyCommandsSurviveAClosedStdout(t *testing.T) {
	const key = `{"id":"%s","name":"ci","kind":"deploy","resource_ids":["s1"],` +
		`"sites":[{"id":"s1","label":"example.com"}],"created_at":"2026-11-17T10:30:00Z"}`
	cases := []struct {
		command    string
		answer     string
		wantPrefix string
		wantSuffix string
	}{
		{
			command:    "create",
			answer:     `{"key":` + fmt.Sprintf(key, sigpipeKeyID) + `,"api_key":"` + sigpipeSecret + `"}`,
			wantPrefix: "the key was created but could not be written to stdout (",
			wantSuffix: `run "kamakiri key rotate ` + sigpipeKeyID + `" to get another`,
		},
		{
			command: "rotate",
			answer: `{"key":` + fmt.Sprintf(key, sigpipeNewKeyID) + `,"api_key":"` + sigpipeSecret + `",` +
				`"replaced":{"id":"` + sigpipeKeyID + `","name":"ci"}}`,
			wantPrefix: "the key was rotated but could not be written to stdout (",
			wantSuffix: `run "kamakiri key rotate ` + sigpipeNewKeyID + `" to get another`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(tc.answer))
			}))
			t.Cleanup(server.Close)

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			t.Cleanup(func() { w.Close() })

			var stderr bytes.Buffer
			cmd := exec.Command(os.Args[0], "-test.run=^TestSigpipeChildProcess$")
			cmd.Env = append(os.Environ(),
				sigpipeChildEnv+"="+tc.command,
				"KAMAKIRI_API_URL="+server.URL,
				"KAMAKIRI_API_KEY=kk_live_login",
				"XDG_CONFIG_HOME="+t.TempDir(),
				"KAMAKIRI_LANG=en",
			)
			cmd.Dir = t.TempDir()
			cmd.Stdout = w
			cmd.Stderr = &stderr

			err = cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child error = %v, want exit status 1; stderr:\n%s", err, stderr.String())
			}
			if exitErr.ExitCode() != 1 {
				t.Fatalf("child exit = %v (code %d), want a plain exit 1, not a signal death; stderr:\n%s",
					exitErr, exitErr.ExitCode(), stderr.String())
			}
			got := strings.TrimRight(stderr.String(), "\n")
			lines := strings.Split(got, "\n")
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, tc.wantPrefix) || !strings.HasSuffix(last, tc.wantSuffix) {
				t.Errorf("last stderr line = %q, want the not-written line ending %q", last, tc.wantSuffix)
			}
			if strings.Contains(stderr.String(), sigpipeSecret) {
				t.Errorf("the key reached stderr:\n%s", stderr.String())
			}
		})
	}
}
