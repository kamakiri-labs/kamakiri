package upgrade

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/semver"
)

// nudgeInterval is how long the line stays quiet once it has been shown. It is
// a decoration on somebody else's command, so once a day is as often as it can
// be worth reading.
const nudgeInterval = 24 * time.Hour

// Nudge writes one line to stderr saying that latest is out and how to move to
// it, and writes nothing at all in every other case. version is the raw build
// stamp, its leading v and the "(dev)" token included; latest is the release
// the server named, or an empty string when it named none.
//
// It reports nothing and can fail nothing. The command it decorates has already
// done what the user asked, so a state file that cannot be read, a version that
// cannot be parsed and a stream that cannot be written to all leave the line
// unsaid and the command's own result untouched.
//
// The caller decides whether there is a terminal to write to and whether the
// command it follows succeeded; everything else is decided here.
func Nudge(stderr io.Writer, version, latest string) {
	// Armed before anything else, and part of the contract rather than
	// hygiene. This runs inside a deferred recover that has already consumed
	// its own recover(), so a panic raised here would not be caught there: it
	// would leave the process as a fresh panic on a command the user watched
	// succeed.
	defer func() { _ = recover() }()

	// Read as a raw token before anything tries to parse it, the way the
	// command's own guard reads it: a build that came from no release has no
	// version to compare and nothing to update itself to.
	if version == devVersion {
		return
	}

	// Both sides have to parse before anything is printed. The gate is on the
	// parse error rather than on the ordering of an unparsed version, and that
	// is what keeps a value that is not a version off the user's terminal, the
	// advertised side being chosen by whatever answered the request. It must
	// survive any rewrite of the comparison package.
	current, err := semver.Parse(version)
	if err != nil {
		return
	}
	newest, err := semver.Parse(latest)
	if err != nil {
		return
	}
	if semver.Compare(newest, current) <= 0 {
		return
	}

	// A timestamp ahead of the clock, from a machine whose time was set forward
	// or a file somebody edited, silences the line until it passes rather than
	// firing it on every command. The two failure modes are not symmetric: a
	// line nobody sees costs nothing, and a line on every single command is
	// exactly the noise the throttle exists to prevent.
	if state := core.LoadUpdateCheck(); state != nil && time.Since(state.LastNudge) < nudgeInterval {
		return
	}

	// Written before it is recorded, because the field records that the user
	// was told: a line that never reached the stream is not a nudge that
	// happened.
	fmt.Fprintln(stderr, i18n.Tf("upgrade.nudge", nudgeVersion(latest), nudgeVersion(version)))
	core.SaveUpdateCheck(time.Now())
}

// nudgeVersion spells a version with exactly one leading v, for display only:
// the comparison above tolerates either spelling. The two sides arrive spelled
// differently, the advertised release having had its v dropped where it was
// recorded and the build stamp keeping its own, so without this the one line
// would disagree with itself. The v is the spelling the rest of the CLI prints:
// `kamakiri version` echoes the stamp as it stands, and releases are published
// under tags that carry it. Nothing unvalidated gains a prefix here: both sides
// have already parsed as versions by the time the line is written.
func nudgeVersion(v string) string {
	return "v" + strings.TrimPrefix(v, "v")
}
