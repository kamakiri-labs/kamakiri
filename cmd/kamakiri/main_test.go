package main

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/cdn"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/domain"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under. Load rather than Setup: it swaps the catalog and leaves
// the recorded selection alone, which is what the dispatch goldens need, since
// each of them drives run and run resolves a selection of its own from the
// environment the golden pins.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

// Nothing but this test enforces that `domain add` inherits `set`'s exit-code
// matrix verbatim, since they share it only by calling the same mapper.
func TestDomainExitCode(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantEcho bool
	}{
		{"nil/live or async-submit", nil, 0, false},
		{"Ctrl-C", domain.ErrWatchInterrupted, 130, false},
		{"Ctrl-C wrapped", fmt.Errorf("x: %w", domain.ErrWatchInterrupted), 130, false},
		{"pre-watch sync timeout (hint on stdout)", api.ErrSyncTimeout, 1, false},
		{"our-side failure (✗ already on stdout)", fmt.Errorf("boom: %w", domain.ErrOurSideShown), 1, false},
		{"transport / API error", errors.New("connection refused"), 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, echo := domainExitCode(c.err)
			if code != c.wantCode || echo != c.wantEcho {
				t.Errorf("domainExitCode(%v) = (%d, %t), want (%d, %t)",
					c.err, code, echo, c.wantCode, c.wantEcho)
			}
		})
	}
}

func TestCdnExitCode(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantEcho bool
	}{
		{"nil/live or queued/disabled", nil, 0, false},
		{"Ctrl-C", cdn.ErrCdnWatchInterrupted, 130, false},
		{"Ctrl-C wrapped", fmt.Errorf("x: %w", cdn.ErrCdnWatchInterrupted), 130, false},
		// `cdn none` waits through the domain package, so its Ctrl-C arrives as
		// a domain sentinel and must still map to a clean 130.
		{"Ctrl-C (cdn none, domain watch)", domain.ErrWatchInterrupted, 130, false},
		{"Ctrl-C (cdn none) wrapped", fmt.Errorf("x: %w", domain.ErrWatchInterrupted), 130, false},
		{"pre-watch sync timeout (hint on stdout)", api.ErrSyncTimeout, 1, false},
		{"terminal error (reason already on stdout)", fmt.Errorf("boom: %w", cdn.ErrCdnReported), 1, false},
		// `cdn purge` waits through the freshness package, so its sentinels
		// reach cdnExit too.
		{"Ctrl-C (cdn purge freshness wait)", freshness.ErrInterrupted, 130, false},
		{"terminal flush block (cdn purge, ✗ on stdout)", freshness.ErrFlushBlocked, 1, false},
		{"transport / API error", errors.New("connection refused"), 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, echo := cdnExitCode(c.err)
			if code != c.wantCode || echo != c.wantEcho {
				t.Errorf("cdnExitCode(%v) = (%d, %t), want (%d, %t)",
					c.err, code, echo, c.wantCode, c.wantEcho)
			}
		})
	}
}

// The build's version reaches the wire only through this package's `version`
// var, so this pins the shapes it arrives in: the ldflags default and both tag
// forms, normalized by api.SetVersion and stamped onto a freshly built client.
// It drives that var rather than a literal, so a change to the default fails
// here. The call to SetVersion inside run is not covered, since the test makes
// that call itself.
func TestBuildVersionReachesAClientsStatedVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"an unstamped local build", "(dev)", "dev"},
		{"a release tag as the release script stamps it", "v0.1.1", "0.1.1"},
		{"the same tag with no v", "0.1.1", "0.1.1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			previous := version
			version = c.version
			t.Cleanup(func() {
				version = previous
				api.SetVersion(previous)
			})

			api.SetVersion(version)

			if got := api.NewClient("https://example.test").Version; got != c.want {
				t.Errorf("client version = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFreshnessExitCode(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantEcho bool
	}{
		{"nil/live or --no-wait submit", nil, 0, false},
		{"Ctrl-C", freshness.ErrInterrupted, 130, false},
		{"Ctrl-C wrapped", fmt.Errorf("x: %w", freshness.ErrInterrupted), 130, false},
		{"terminal flush block (✗ already on stdout)", freshness.ErrFlushBlocked, 1, false},
		{"terminal flush block wrapped", fmt.Errorf("x: %w", freshness.ErrFlushBlocked), 1, false},
		{"transport / API / pre-wait error", errors.New("connection refused"), 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, echo := freshnessExitCode(c.err)
			if code != c.wantCode || echo != c.wantEcho {
				t.Errorf("freshnessExitCode(%v) = (%d, %t), want (%d, %t)",
					c.err, code, echo, c.wantCode, c.wantEcho)
			}
		})
	}
}

// The saved preference reaching i18n.Setup is the whole point of reading the
// settings file at startup, and the read is the half of that wiring a test can
// hold: it needs no catalog swap, so it leaves the language alone.
func TestPersistedLanguage(t *testing.T) {
	t.Run("saved preference", func(t *testing.T) {
		// Without the isolation this rewrites the developer's own settings.
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if err := core.SaveSettings("ja"); err != nil {
			t.Fatalf("SaveSettings() error = %v", err)
		}

		if got := persistedLanguage(); got != "ja" {
			t.Errorf("persistedLanguage() = %q, want ja", got)
		}
	})

	t.Run("no settings file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())

		if got := persistedLanguage(); got != "" {
			t.Errorf("persistedLanguage() = %q, want empty when nothing is saved", got)
		}
	})
}
