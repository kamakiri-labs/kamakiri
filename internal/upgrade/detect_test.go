package upgrade

import (
	"runtime"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/core"
)

func scriptMarker(path string) *core.InstallMarker {
	return &core.InstallMarker{Version: 1, Method: core.InstallMethodScript, Path: path}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name     string
		execPath string
		marker   *core.InstallMarker
		goos     string
		want     Method
	}{
		{
			name:     "homebrew cellar",
			execPath: "/opt/homebrew/Cellar/kamakiri/0.1.1/bin/kamakiri",
			goos:     "darwin",
			want:     MethodHomebrew,
		},
		{
			name:     "homebrew caskroom",
			execPath: "/opt/homebrew/Caskroom/kamakiri/0.1.1/kamakiri",
			goos:     "darwin",
			want:     MethodHomebrew,
		},
		{
			name:     "homebrew cellar on linuxbrew",
			execPath: "/home/linuxbrew/.linuxbrew/Cellar/kamakiri/0.1.1/bin/kamakiri",
			goos:     "linux",
			want:     MethodHomebrew,
		},
		{
			name:     "npm global node_modules",
			execPath: "/usr/local/lib/node_modules/kamakiri/bin/kamakiri",
			goos:     "linux",
			want:     MethodNpm,
		},
		{
			name:     "plain install directory",
			execPath: "/usr/local/bin/kamakiri",
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "directory ending in Cellar is not a Cellar segment",
			execPath: "/home/user/myCellar/bin/kamakiri",
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "directory starting with node_modules is not a node_modules segment",
			execPath: "/home/user/node_modules_old/kamakiri",
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "windows cellar path with backslashes",
			execPath: `C:\opt\Cellar\kamakiri\0.1.1\bin\kamakiri.exe`,
			goos:     "windows",
			want:     MethodHomebrew,
		},
		{
			name:     "windows npm path with backslashes",
			execPath: `C:\Users\alex\AppData\Roaming\npm\node_modules\kamakiri\kamakiri.exe`,
			goos:     "windows",
			want:     MethodNpm,
		},
		{
			name:     "windows cellar path with forward slashes",
			execPath: "C:/opt/Cellar/kamakiri/0.1.1/bin/kamakiri.exe",
			goos:     "windows",
			want:     MethodHomebrew,
		},
		{
			// A backslash is a legal filename character on Unix, so this is one
			// directory named `a\Cellar\b`, not three.
			name:     "backslashes are not separators off windows",
			execPath: `/home/user/a\Cellar\b/kamakiri`,
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "case-insensitive segment match on windows",
			execPath: `C:\Users\alex\AppData\Roaming\npm\NODE_MODULES\kamakiri\kamakiri.exe`,
			goos:     "windows",
			want:     MethodNpm,
		},
		{
			name:     "case-sensitive segment match off windows",
			execPath: "/usr/local/lib/NODE_MODULES/kamakiri/bin/kamakiri",
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "script marker naming this binary",
			execPath: "/home/user/.local/bin/kamakiri",
			marker:   scriptMarker("/home/user/.local/bin/kamakiri"),
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "script marker naming another binary",
			execPath: "/usr/local/bin/kamakiri",
			marker:   scriptMarker("/home/user/.local/bin/kamakiri"),
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "no marker at all",
			execPath: "/usr/local/bin/kamakiri",
			goos:     "linux",
			want:     MethodSelf,
		},
		{
			name:     "homebrew outranks a matching marker",
			execPath: "/opt/homebrew/Cellar/kamakiri/0.1.1/bin/kamakiri",
			marker:   scriptMarker("/opt/homebrew/Cellar/kamakiri/0.1.1/bin/kamakiri"),
			goos:     "darwin",
			want:     MethodHomebrew,
		},
		{
			name:     "npm outranks a matching marker",
			execPath: "/usr/local/lib/node_modules/kamakiri/bin/kamakiri",
			marker:   scriptMarker("/usr/local/lib/node_modules/kamakiri/bin/kamakiri"),
			goos:     "linux",
			want:     MethodNpm,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detect(tt.execPath, tt.marker, tt.goos); got != tt.want {
				t.Errorf("detect(%q, %+v, %q) = %q, want %q", tt.execPath, tt.marker, tt.goos, got, tt.want)
			}
		})
	}
}

// Whether the marker applies is asserted on its own predicate, because every
// method a marker can carry today implies self-replacement, which is also what
// an unusable marker falls back to: the two are indistinguishable through
// Detect's result alone.
func TestMarkerApplies(t *testing.T) {
	tests := []struct {
		name     string
		marker   *core.InstallMarker
		execPath string
		goos     string
		want     bool
	}{
		{
			name:     "nil marker",
			execPath: "/usr/local/bin/kamakiri",
			goos:     "linux",
			want:     false,
		},
		{
			name:     "same path",
			marker:   scriptMarker("/home/user/.local/bin/kamakiri"),
			execPath: "/home/user/.local/bin/kamakiri",
			goos:     "linux",
			want:     true,
		},
		{
			name:     "different path",
			marker:   scriptMarker("/home/user/.local/bin/kamakiri"),
			execPath: "/usr/local/bin/kamakiri",
			goos:     "linux",
			want:     false,
		},
		{
			// Cleaning an empty path yields ".", on the recorded side as much as
			// on the exec side, so a marker with no path must be rejected before
			// the two are compared.
			name:     "empty recorded path",
			marker:   scriptMarker(""),
			execPath: "",
			goos:     "linux",
			want:     false,
		},
		{
			name:     "relative recorded path",
			marker:   scriptMarker("bin/kamakiri"),
			execPath: "/usr/local/bin/kamakiri",
			goos:     "linux",
			want:     false,
		},
		{
			name:     "different path under the same directory",
			marker:   scriptMarker("/usr/local/bin/kamakiri"),
			execPath: "/usr/local/bin/kamakiri-old",
			goos:     "linux",
			want:     false,
		},
		{
			// Both sides are cleaned, so a marker written with a redundant
			// path still names the binary it names.
			name:     "uncleaned marker path",
			marker:   scriptMarker("/usr/local/bin/../bin//kamakiri"),
			execPath: "/usr/local/bin/kamakiri",
			goos:     "linux",
			want:     true,
		},
		{
			name:     "marker separators are normalized on windows",
			marker:   scriptMarker(`C:\Program Files\kamakiri\kamakiri.exe`),
			execPath: "C:/Program Files/kamakiri/kamakiri.exe",
			goos:     "windows",
			want:     true,
		},
		{
			name:     "case-insensitive path match on windows",
			marker:   scriptMarker(`c:\program files\kamakiri\kamakiri.exe`),
			execPath: `C:\Program Files\Kamakiri\kamakiri.exe`,
			goos:     "windows",
			want:     true,
		},
		{
			name:     "case-sensitive path match off windows",
			marker:   scriptMarker("/home/user/bin/kamakiri"),
			execPath: "/home/user/bin/Kamakiri",
			goos:     "linux",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markerApplies(tt.marker, normalizePath(tt.execPath, tt.goos), tt.goos)
			if got != tt.want {
				t.Errorf("markerApplies(%+v, %q, %q) = %v, want %v", tt.marker, tt.execPath, tt.goos, got, tt.want)
			}
		})
	}
}

// An install method this CLI does not map is worth no more than no marker at
// all, so an install recorded by a future channel is upgraded in place rather
// than by a method whose meaning this build cannot know. Detect takes its
// marker from the caller, so this is the caller-supplied case rather than one
// the loader can hand over: the loader drops an unknown method itself, and
// this is the second gate behind it.
func TestDetectUnknownMarkerMethod(t *testing.T) {
	marker := &core.InstallMarker{Version: 1, Method: "deb", Path: "/usr/bin/kamakiri"}

	if got := detect("/usr/bin/kamakiri", marker, "linux"); got != MethodSelf {
		t.Errorf("detect() = %q, want %q", got, MethodSelf)
	}
}

// Every method a marker can carry today implies self-replacement, which is also
// what detect falls back to, so the marker branch decides nothing that shows in
// the result and would go unnoticed if it were removed. Registering a method
// that implies something else for the length of this test is what makes the
// branch observable, and pins that detect consults the marker, that it does so
// only for the binary the marker names, and that a path heuristic still wins.
// Registering it mutates package state, so no test in this package may call
// t.Parallel while this one exists.
func TestDetectConsultsTheMarker(t *testing.T) {
	const method = "test-deferring-channel"
	markerMethods[method] = MethodNpm
	t.Cleanup(func() { delete(markerMethods, method) })

	const installed = "/home/user/.local/bin/kamakiri"
	marker := &core.InstallMarker{Version: 1, Method: method, Path: installed}

	if got := detect(installed, marker, "linux"); got != MethodNpm {
		t.Errorf("detect(%q, marker naming it, %q) = %q, want %q", installed, "linux", got, MethodNpm)
	}

	const elsewhere = "/usr/local/bin/kamakiri"
	if got := detect(elsewhere, marker, "linux"); got != MethodSelf {
		t.Errorf("detect(%q, marker naming another binary, %q) = %q, want %q", elsewhere, "linux", got, MethodSelf)
	}

	const cellar = "/opt/homebrew/Cellar/kamakiri/0.1.1/bin/kamakiri"
	atCellar := &core.InstallMarker{Version: 1, Method: method, Path: cellar}
	if got := detect(cellar, atCellar, "darwin"); got != MethodHomebrew {
		t.Errorf("detect(%q, marker naming it, %q) = %q, want %q", cellar, "darwin", got, MethodHomebrew)
	}
}

// Detect is the production entry point, and its only job beyond detect is to
// bind the platform rules to the platform it is running on. The second path
// below is the one that asserts that binding: it is an npm install on Windows,
// where segments match without regard to case, and an ordinary directory
// everywhere else, so its expectation has to be derived from the platform the
// test is running on. A path that answers the same everywhere would pass
// against any GOOS the binding happened to name.
func TestDetectUsesTheRunningPlatform(t *testing.T) {
	if got := Detect("/usr/local/lib/node_modules/kamakiri/bin/kamakiri", nil); got != MethodNpm {
		t.Errorf("Detect() = %q, want %q", got, MethodNpm)
	}

	const uppercase = "/usr/local/lib/NODE_MODULES/kamakiri"
	want := MethodSelf
	if runtime.GOOS == "windows" {
		want = MethodNpm
	}
	if got := Detect(uppercase, nil); got != want {
		t.Errorf("Detect(%q, nil) = %q, want %q on %s", uppercase, got, want, runtime.GOOS)
	}
}
