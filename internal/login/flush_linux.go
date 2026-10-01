package login

import (
	"os"

	"golang.org/x/sys/unix"
)

// flushInput discards what is queued for reading on the terminal f. Errors are
// ignored: the flush is a courtesy, and without it the run behaves as it would
// on a platform that has no flush at all.
func flushInput(f *os.File) {
	_ = unix.IoctlSetInt(int(f.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}
