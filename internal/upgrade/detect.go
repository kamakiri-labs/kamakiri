package upgrade

import (
	"path"
	"runtime"
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/core"
)

// Method is how the running binary is upgraded. These three values are the
// whole set.
type Method string

const (
	// MethodSelf means the binary is replaced in place by the CLI itself.
	MethodSelf Method = "self"
	// MethodHomebrew means Homebrew owns the install and upgrades it.
	MethodHomebrew Method = "homebrew"
	// MethodNpm means npm owns the install and upgrades it.
	MethodNpm Method = "npm"
)

// markerMethods maps the install methods a marker can record to the upgrade
// method they imply. Two places hold the set of known methods, and a new
// channel needs an entry in both: the marker loader in core drops a method it
// does not know, so a marker naming one never reaches this map by that route,
// and this map is what decides for a marker a caller hands over directly. A
// method missing from either place says no more than an absent marker does,
// which is why a channel that must not be self-replaced needs both entries, or
// a path heuristic below, and never a marker alone.
var markerMethods = map[string]Method{
	core.InstallMethodScript: MethodSelf,
}

// Detect reports how the binary at execPath is upgraded, given the install
// marker (nil when there is none).
//
// The caller passes the running executable's path resolved through
// os.Executable, then filepath.Abs, then filepath.EvalSymlinks, and both
// halves of the decision depend on it having done so. A relative path defeats
// the marker match, which compares against an absolute path recorded at
// install time, and os.Executable does not promise an absolute path: on darwin
// it can echo a relative exec path. An unresolved
// symlink defeats the Homebrew heuristic, since a Homebrew install is reached
// through a symlink in the prefix bin directory, and only following it lands
// on the Cellar path the heuristic looks for.
//
// The marker's recorded path carries the same requirement, and it falls on
// whoever writes the marker: an installer records the path it installed to with
// symlinks already resolved. The two paths are compared as text, so a marker
// recording a path through a symlinked directory names something other than the
// resolved path the caller passes, and does not apply to it.
//
// Path heuristics on that resolved path outrank the marker: they describe
// where the binary actually is, while the marker only records what an
// installer once believed. The marker decides when no heuristic fires and it
// names this exact binary, so a marker left behind by an install that has
// since been replaced by another channel cannot speak for the new one.
func Detect(execPath string, marker *core.InstallMarker) Method {
	return detect(execPath, marker, runtime.GOOS)
}

// detect takes the GOOS as a parameter so that the platform rules, which are
// the easiest part of this to get wrong, are drivable from a test on any
// platform.
func detect(execPath string, marker *core.InstallMarker, goos string) Method {
	resolved := normalizePath(execPath, goos)

	if hasSegment(resolved, "Cellar", goos) || hasSegment(resolved, "Caskroom", goos) {
		return MethodHomebrew
	}
	if hasSegment(resolved, "node_modules", goos) {
		return MethodNpm
	}
	if markerApplies(marker, resolved, goos) {
		if method, ok := markerMethods[marker.Method]; ok {
			return method
		}
	}
	return MethodSelf
}

// normalizePath puts a path into the single cleaned, slash-separated form the
// comparisons below work on. Windows accepts both separators, so a backslash
// becomes a slash there and nowhere else: off Windows a backslash is an
// ordinary filename character, and folding it would split one directory into
// several. Cleaning is path.Clean on that slash form rather than
// filepath.Clean, which cleans for the platform the CLI is running on and so
// would leave a Windows path untouched everywhere else; on Unix the two agree.
func normalizePath(p, goos string) string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	return path.Clean(p)
}

// hasSegment reports whether a normalized path contains segment as a whole
// path segment. Matching whole segments is what keeps a directory named
// myCellar or node_modules_old from being read as an install channel.
func hasSegment(normalized, segment, goos string) bool {
	for _, part := range strings.Split(normalized, "/") {
		if pathEqual(part, segment, goos) {
			return true
		}
	}
	return false
}

// markerApplies reports whether the marker speaks for the binary at the given
// normalized path. It is a separate predicate because every method a marker
// can carry today implies self-replacement, which is also the fallback, so the
// path rules it holds are asserted directly on it rather than through the
// answer Detect returns.
func markerApplies(marker *core.InstallMarker, normalizedExecPath, goos string) bool {
	// A marker that records no path names no binary, and the guard is what keeps
	// that case away from the comparison below: cleaning turns an empty path into
	// ".", so an empty recorded path would otherwise match an empty exec path.
	if marker == nil || marker.Path == "" {
		return false
	}
	return pathEqual(normalizePath(marker.Path, goos), normalizedExecPath, goos)
}

// pathEqual compares two normalized paths, or two path segments, under the
// platform's case rule: Windows paths are case-insensitive, and everywhere
// else two names differing in case are two different names.
func pathEqual(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
