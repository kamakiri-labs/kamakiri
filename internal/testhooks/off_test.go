//go:build !testhooks

package testhooks

import "testing"

func TestHooksOffWithoutTag(t *testing.T) {
	if Enabled {
		t.Error("Enabled = true in a build without the testhooks tag")
	}
	t.Setenv("KAMAKIRI_CDN_SKIP_CERT_CHECK", "1")
	t.Setenv("KAMAKIRI_FRESHNESS_POLL_MS", "200")
	for _, name := range []string{"KAMAKIRI_CDN_SKIP_CERT_CHECK", "KAMAKIRI_FRESHNESS_POLL_MS"} {
		if got := Getenv(name); got != "" {
			t.Errorf("Getenv(%s) = %q, want \"\" while the variable is set", name, got)
		}
	}
}
