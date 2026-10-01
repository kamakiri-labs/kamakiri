//go:build !testhooks

package freshness

import "testing"

// A build without the testhooks tag, which is what a user runs, keeps the
// production schedule even when the environment carries a valid override.
func TestPollScheduleFromEnvIgnoredWithoutTag(t *testing.T) {
	t.Setenv(EnvFreshnessPollMS, "200")
	sched := pollScheduleFromEnv()
	for _, p := range pollProbes {
		if got := sched(p.elapsed); got != p.def {
			t.Errorf("at elapsed=%s: got %s, want default %s", p.elapsed, got, p.def)
		}
	}
}
