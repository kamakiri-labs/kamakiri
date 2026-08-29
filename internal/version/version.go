// Package version implements the `kamakiri version` / `--version` command.
package version

import (
	"fmt"
	"io"
)

// Run prints the version it is given. A release build stamps that value in at
// link time; a local build leaves the caller's default, "(dev)".
func Run(out io.Writer, version string) {
	fmt.Fprintf(out, "kamakiri %s\n", version)
}
