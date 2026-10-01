//go:build !testhooks

package cdn

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
)

// A build without the testhooks tag, which is what a user runs, must hold the
// live line behind the certificate gate even when the environment asks to skip
// it.
func TestWatchToLiveIgnoresSkipCertCheckEnvWithoutTag(t *testing.T) {
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
	if want := []string{"example.com"}; !slices.Equal(probed, want) {
		t.Errorf("cert gate probed %q, want %q", probed, want)
	}
	mustContainInOrder(t, out.String(), "SSL certificate checked", "✓ live via Cloudflare")
}
