//go:build testhooks

package freshness

import (
	"testing"
	"time"
)

// In a testhooks build a positive value replaces the whole schedule with that
// fixed interval.
func TestPollScheduleFromEnvOverride(t *testing.T) {
	t.Setenv(EnvFreshnessPollMS, "200")
	sched := pollScheduleFromEnv()
	for _, p := range pollProbes {
		if got := sched(p.elapsed); got != 200*time.Millisecond {
			t.Errorf("at elapsed=%s: got %s, want 200ms", p.elapsed, got)
		}
	}
}
