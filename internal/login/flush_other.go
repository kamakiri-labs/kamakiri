//go:build !linux && !darwin && !windows

package login

import "os"

// flushInput does nothing where no terminal input flush is wired up, so what is
// typed during a countdown is read after it.
func flushInput(*os.File) {}
