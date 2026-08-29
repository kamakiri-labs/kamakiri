package cdn

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
)

// A site halfway through leaving its CDN also reads as none-mode here, which
// is why the pointer must name the command that does resume its climb.
func TestVerifyNoCDN(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			return &api.CDNStatusResponse{CDNMode: "none"}, nil
		},
	}
	var out bytes.Buffer
	if err := Verify(client, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "No CDN configured") {
		t.Errorf("missing the no-CDN pointer: %q", got)
	}
	if !strings.Contains(got, "kamakiri status") {
		t.Errorf("a mid-exit site must be pointed at `kamakiri status` to resume: %q", got)
	}
	assertCleanCopy(t, got)
}

func TestVerifyResumesWatch(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")
	t.Setenv(EnvSkipCertCheck, "1")

	statusCalls := 0
	client := &mockClient{
		// The first frame has to be terminal. This path runs the production
		// watch, with the real poll schedule, so anything else would add seconds
		// of real sleeping to the test. A mid-flight resume is exercised
		// deterministically elsewhere.
		cdnStatusFn: func(string) (*api.CDNStatusResponse, error) {
			statusCalls++
			return cf(CdnStateActive), nil
		},
		// The CDN mutators have no function set, so any of them would panic
		// here. That is the assertion: verify only reads.
	}
	var out bytes.Buffer
	if err := Verify(client, &out); err != nil {
		t.Fatal(err)
	}
	if statusCalls < 1 {
		t.Fatal("verify must poll CDNStatus")
	}
	if !strings.Contains(out.String(), "✓ live via Cloudflare") {
		t.Errorf("got %q", out.String())
	}
}
