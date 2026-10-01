package login

import (
	"os"

	"golang.org/x/sys/windows"
)

// flushInput discards the input records queued on the console f. Errors are
// ignored: the flush is a courtesy, and without it the run behaves as it would
// on a platform that has no flush at all.
func flushInput(f *os.File) {
	_ = windows.FlushConsoleInputBuffer(windows.Handle(f.Fd()))
}
