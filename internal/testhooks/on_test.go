//go:build testhooks

package testhooks

import "testing"

func TestHooksOnWithTag(t *testing.T) {
	if !Enabled {
		t.Error("Enabled = false in a testhooks build")
	}
	t.Setenv("KAMAKIRI_CDN_SKIP_CERT_CHECK", "1")
	t.Setenv("KAMAKIRI_FRESHNESS_POLL_MS", "200")
	if got := Getenv("KAMAKIRI_CDN_SKIP_CERT_CHECK"); got != "1" {
		t.Errorf(`Getenv(KAMAKIRI_CDN_SKIP_CERT_CHECK) = %q, want "1"`, got)
	}
	if got := Getenv("KAMAKIRI_FRESHNESS_POLL_MS"); got != "200" {
		t.Errorf(`Getenv(KAMAKIRI_FRESHNESS_POLL_MS) = %q, want "200"`, got)
	}
}
