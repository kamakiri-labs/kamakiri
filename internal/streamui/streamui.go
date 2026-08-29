// Package streamui holds the terminal-rendering primitives every streaming wait
// shares. Such a wait prints two kinds of line: transient status lines, redrawn
// in place on a TTY, and permanent milestone lines that scrollback keeps.
package streamui

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// Renderer streams a wait's output to out, keeping a single in-place transient
// block distinct from the permanent lines above it. Every write goes through the
// methods below, all of which clear first: a write that skipped the clear would
// leave the transient counter wrong and corrupt the next rewind.
type Renderer struct {
	out       io.Writer
	tty       bool
	transient int
}

// New returns a Renderer writing to out. tty gates the ANSI cursor rewind;
// IsTerminalWriter answers it for a real writer.
func New(out io.Writer, tty bool) *Renderer {
	return &Renderer{out: out, tty: tty}
}

// Clear rewinds the current transient block so the next write starts over it. It
// does nothing off a terminal, where every line written is a line kept.
func (r *Renderer) Clear() {
	if r.transient > 0 && r.tty {
		fmt.Fprintf(r.out, "\033[%dA\033[J", r.transient)
	}
	r.transient = 0
}

// Commit clears the transient block and writes lines permanently: scrollback
// keeps them and no later rewind touches them.
func (r *Renderer) Commit(lines ...string) {
	r.Clear()
	for _, l := range lines {
		fmt.Fprintln(r.out, l)
	}
}

// Transient clears the previous transient block and redraws lines as the new
// one, for the next write to rewind. Off a terminal nothing is rewound, so a
// caller redrawing on every poll has to throttle itself or flood the output.
func (r *Renderer) Transient(lines ...string) {
	r.Clear()
	for _, l := range lines {
		fmt.Fprintln(r.out, l)
	}
	r.transient = len(lines)
}

// IsTerminalWriter reports whether out is a real terminal, the condition for
// cursor control and glyph rendering.
func IsTerminalWriter(out io.Writer) bool {
	if file, ok := out.(*os.File); ok {
		return term.IsTerminal(int(file.Fd()))
	}
	return false
}

// HumanizeElapsed renders elapsed time for a progress tick. Under a minute it
// says so rather than inventing a precise small number, then it counts whole
// minutes. Milestones use HumanizeSpan instead; the two are not interchangeable.
func HumanizeElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.T("streamui.elapsed_under_a_minute")
	case d < time.Hour:
		return i18n.Tf("streamui.elapsed_minutes", int(d.Minutes()))
	default:
		return i18n.Tf("streamui.elapsed_hours", int(d.Hours()), int(d.Minutes())%60)
	}
}

// HumanizeSpan renders a completed phase span for a `✓ … (~Ns)` milestone. It
// shows real sub-minute seconds and floors nothing, so a caller that must not
// report a small duration has to gate the annotation itself.
func HumanizeSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.Tf("streamui.span_seconds", int(d.Seconds()))
	case d < time.Hour:
		return i18n.Tf("streamui.span_minutes", int(d.Minutes()))
	default:
		return i18n.Tf("streamui.span_hours", int(d.Hours()), int(d.Minutes())%60)
	}
}
