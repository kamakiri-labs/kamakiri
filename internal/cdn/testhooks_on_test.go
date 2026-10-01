//go:build testhooks

package cdn

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
)

// In a testhooks build the variable turns the gate off, and the production
// entry point still reaches live, just without the certificate milestone. The
// probe is replaced by one that records calls, so a hook that stopped working
// fails here instead of sending a real HTTPS request.
func TestWatchToLiveSkipCertCheckEnv(t *testing.T) {
	t.Setenv(EnvSkipCertCheck, "1")
	var probed []string
	setLiveCertCheck(t, func(_ context.Context, host, _ string) error {
		probed = append(probed, host)
		return nil
	})
	client := &scriptClient{frames: []*api.CDNStatusResponse{cf(CdnStateActive)}}
	var out bytes.Buffer
	if err := WatchToLive(client, "s1", "cloudflare", &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "✓ live via Cloudflare") {
		t.Errorf("missing live line: %q", got)
	}
	if len(probed) != 0 {
		t.Errorf("cert gate must be skipped when %s is set, but it probed %q", EnvSkipCertCheck, probed)
	}
	if strings.Contains(got, "SSL certificate") {
		t.Errorf("cert gate must be skipped when %s is set: %q", EnvSkipCertCheck, got)
	}
}
