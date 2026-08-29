package upgrade

import (
	"bytes"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/core"
)

// The whole line in each language. Whole blocks rather than phrases, for the
// reason the command's copy tests give: the catalog gates cannot tell one key
// from another of the same shape, so only an output comparison catches a swap.
func TestNudgeCopy(t *testing.T) {
	cases := []struct {
		lang string
		want string
	}{
		{"en", "kamakiri v0.2.0 is available; you are on v0.1.1. Run `kamakiri upgrade` to update.\n"},
		{"ja", "kamakiri v0.2.0 が利用可能です。現在のバージョンは v0.1.1 です。`kamakiri upgrade` を実行して更新してください。\n"},
	}

	for _, tc := range cases {
		t.Run("in "+tc.lang, func(t *testing.T) {
			isolateConfigDir(t)
			pinLanguage(t, tc.lang)

			var out bytes.Buffer
			Nudge(&out, "v0.1.1", "0.2.0")

			if got := out.String(); got != tc.want {
				t.Errorf("Nudge() wrote:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

// Both sides are spelled with the leading v the rest of the CLI prints, and the
// two arrive spelled the other way round from the copy test above: here the
// build stamp carries no v and the advertised release does. Each side is
// therefore normalized by a test of its own, so dropping the work on either one
// alone fails here or there.
func TestNudgeSpellsBothVersionsTheSameWay(t *testing.T) {
	isolateConfigDir(t)

	const want = "kamakiri v0.2.0 is available; you are on v0.1.1. Run `kamakiri upgrade` to update.\n"

	var out bytes.Buffer
	Nudge(&out, "0.1.1", "v0.2.0")

	if got := out.String(); got != want {
		t.Errorf("Nudge() wrote:\n%q\nwant:\n%q", got, want)
	}
}

// Every reason the line is not the user's business. Two of them are the parse
// gate, which is what keeps a value the CLI does not control off the terminal:
// the branch is on the parse error rather than on the ordering of an unparsed
// version, so it holds whichever side failed.
func TestNudgeStaysSilent(t *testing.T) {
	cases := []struct {
		name    string
		version string
		latest  string
	}{
		{"the advertised release is the running one", "v0.1.1", "0.1.1"},
		{"the advertised release is older", "v0.2.0", "0.1.1"},
		{"the advertised release is an earlier prerelease of the running one", "v0.2.0", "0.2.0-rc.1"},
		{"a build that came from no release", "(dev)", "0.9.0"},
		{"nothing was advertised", "v0.1.1", ""},
		{"the running version does not parse", "v0.1.7.10", "0.9.0"},
		{"the advertised version does not parse", "v0.1.1", "0.2"},
		{"the advertised version carries terminal escapes", "v0.1.1", "0.2.0\x1b[2J"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)

			var out bytes.Buffer
			Nudge(&out, tc.version, tc.latest)

			if got := out.String(); got != "" {
				t.Errorf("Nudge() wrote %q, want nothing", got)
			}
			if state := core.LoadUpdateCheck(); state != nil {
				t.Errorf("a silent nudge saved %+v, want nothing written", state)
			}
		})
	}
}

func TestNudgeThrottle(t *testing.T) {
	cases := []struct {
		name  string
		shown time.Duration
		print bool
	}{
		{"an hour inside the window", -23 * time.Hour, false},
		{"an hour past it", -25 * time.Hour, true},
		// A clock that has been set forward, or a hand-edited file. Staying
		// quiet until it passes costs the user nothing, where reading it as
		// long ago would put the line on every single command.
		{"a moment that has not arrived yet", time.Hour, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)
			shown := time.Now().Add(tc.shown)
			core.SaveUpdateCheck(shown)

			var out bytes.Buffer
			Nudge(&out, "v0.1.1", "0.2.0")

			printed := out.String() != ""
			if printed != tc.print {
				t.Errorf("Nudge() wrote %q, want printed = %v", out.String(), tc.print)
			}

			state := core.LoadUpdateCheck()
			if state == nil {
				t.Fatal("LoadUpdateCheck() = nil, want the seeded state")
			}
			if tc.print {
				if time.Since(state.LastNudge) > time.Minute {
					t.Errorf("last_nudge = %v, want the moment the line was written", state.LastNudge)
				}
				return
			}
			if !state.LastNudge.Truncate(time.Second).Equal(shown.Truncate(time.Second)) {
				t.Errorf("last_nudge = %v, want the seeded %v", state.LastNudge, shown)
			}
		})
	}
}

func TestNudgeRecordsThatItPrinted(t *testing.T) {
	isolateConfigDir(t)

	var out bytes.Buffer
	Nudge(&out, "v0.1.1", "0.2.0")

	state := core.LoadUpdateCheck()
	if state == nil {
		t.Fatal("LoadUpdateCheck() = nil, want the moment of the nudge")
	}
	if time.Since(state.LastNudge) > time.Minute {
		t.Errorf("last_nudge = %v, want the moment the line was written", state.LastNudge)
	}
}

// panickingWriter stands in for a stream that fails in the one way an error
// return cannot express.
type panickingWriter struct{ written bool }

func (w *panickingWriter) Write([]byte) (int, error) {
	w.written = true
	panic("the terminal went away")
}

// The recovery is part of the contract rather than hygiene. A panic raised here
// would unwind through a recover that has already run, and land as a fresh
// panic on a command the user watched succeed. The state assertion pins the
// other half: the field records that a line was written, so a run where nothing
// reached the stream records nothing.
func TestNudgeSurvivesAWriterThatPanics(t *testing.T) {
	isolateConfigDir(t)

	out := &panickingWriter{}
	Nudge(out, "v0.1.1", "0.2.0")

	if !out.written {
		t.Fatal("Nudge() never wrote, so nothing here proves it recovers")
	}
	if state := core.LoadUpdateCheck(); state != nil {
		t.Errorf("a nudge nobody saw saved %+v, want nothing written", state)
	}
}
