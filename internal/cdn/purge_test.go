package cdn

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
)

func TestPurgeHappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	var purgedSite string
	client := &mockClient{
		purgeCDNFn: func(siteID string) (*api.CDNPurgeResponse, error) {
			purgedSite = siteID
			return &api.CDNPurgeResponse{SiteID: siteID, SyncAttemptID: 7}, nil
		},
		getSiteFn: func(id string) (*api.Site, error) {
			if id != "site123" {
				t.Errorf("GetSite id = %q, want site123", id)
			}
			return &api.Site{Freshness: &api.Freshness{State: "fresh"}}, nil
		},
	}

	var out bytes.Buffer
	if err := Purge(client, false, &out); err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if purgedSite != "site123" {
		t.Errorf("PurgeCDN called with %q, want site123", purgedSite)
	}
	output := out.String()
	if !strings.Contains(output, "✓ caches flushed") {
		t.Errorf("missing the flushed milestone, output = %q", output)
	}
	if strings.Contains(output, "Purging CDN cache") {
		t.Errorf("the old 'Purging CDN cache... ' prefix must be gone, output = %q", output)
	}
}

func TestPurgeNoWaitSkipsWait(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		purgeCDNFn: func(_ string) (*api.CDNPurgeResponse, error) {
			return &api.CDNPurgeResponse{SiteID: "site123", SyncAttemptID: 7}, nil
		},
		getSiteFn: func(string) (*api.Site, error) {
			t.Error("GetSite should not be called on the --no-wait path")
			return nil, errors.New("flunk")
		},
	}

	var out bytes.Buffer
	if err := Purge(client, true, &out); err != nil {
		t.Fatalf("Purge() error = %v", err)
	}
	if !strings.Contains(out.String(), "CDN purge queued (--no-wait). The flush continues in the background; check kamakiri status.") {
		t.Errorf("expected queued + converging copy, output = %q", out.String())
	}
}

// A blocked flush must exit non-zero: the caches still hold the old site, so
// reporting success would be a lie a visitor could catch.
func TestPurgeBlockedExitsWithEntry(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		purgeCDNFn: func(_ string) (*api.CDNPurgeResponse, error) {
			return &api.CDNPurgeResponse{SiteID: "site123", SyncAttemptID: 7}, nil
		},
		getSiteFn: func(string) (*api.Site, error) {
			return &api.Site{Freshness: &api.Freshness{State: "blocked", Blockers: []api.Blocker{
				{Provider: "cloudflare", Host: "shop.example.com", Reason: "provider_rejected", Detail: "zone is paused"},
			}}}, nil
		},
	}

	var out bytes.Buffer
	err := Purge(client, false, &out)
	if !errors.Is(err, freshness.ErrFlushBlocked) {
		t.Fatalf("err = %v, want ErrFlushBlocked", err)
	}
	output := out.String()
	if !strings.Contains(output, "✗ Caches purged, but the Cloudflare cache was not flushed: Cloudflare rejected the request for shop.example.com (zone is paused).") {
		t.Errorf("output missing the blocked entry with the purge lead: %q", output)
	}
	if strings.Contains(output, "✓ caches flushed") {
		t.Errorf("a blocked purge must not report flushed: %q", output)
	}
	assertCleanCopy(t, output)
}

func TestPurgeNoCDNConfigured(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		purgeCDNFn: func(_ string) (*api.CDNPurgeResponse, error) {
			return nil, &api.ErrorResponse{Code: "no_cdn_configured", Message: "Site has no CDN configured."}
		},
		getSiteFn: func(string) (*api.Site, error) {
			t.Error("GetSite should not be called when PurgeCDN errors")
			return nil, errors.New("flunk")
		},
	}

	var out bytes.Buffer
	err := Purge(client, false, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no CDN configured") {
		t.Errorf("error = %v, want friendly no-CDN message", err)
	}
}

func TestPurgeNoProject(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)

	client := &mockClient{}
	var out bytes.Buffer
	err := Purge(client, false, &out)
	if err == nil {
		t.Fatal("expected error when no project linked")
	}
}
