package login

import (
	"os"

	"golang.org/x/sys/unix"
)

// flushInput discards what is queued for reading on the terminal f. TIOCFLUSH
// takes the queue to drop as a flag, and on this line discipline TCIFLUSH has
// the value of the read-queue flag. Errors are ignored: the flush is a courtesy,
// and without it the run behaves as it would on a platform that has no flush at
// all.
func flushInput(f *os.File) {
	_ = unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, unix.TCIFLUSH)
}
