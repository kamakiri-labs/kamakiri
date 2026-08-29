package core

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// installMarkerVersion is the shape LoadInstallMarker understands. A file
// written by a newer installer carries a higher number and is ignored rather
// than misread.
const installMarkerVersion = 1

// InstallMethodScript is the marker method recorded by the install script. It
// is the only method this CLI knows, and a marker naming any other one is
// ignored.
const InstallMethodScript = "script"

// InstallMarker records how the CLI was installed and where it was installed
// to. Nothing in the CLI writes it: the install script writes it at install
// time, and the upgrade path reads it to decide whether it may replace the
// binary itself. It holds no secret.
type InstallMarker struct {
	Version int    `json:"version"`
	Method  string `json:"method"`
	Path    string `json:"path"`
}

// installMarkerPath returns the path to the install marker file. It stays
// unexported because the file is only ever read here, unlike the credentials
// file, whose path commands print.
func installMarkerPath() (string, error) {
	dir, err := kamakiriConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "install.json"), nil
}

// LoadInstallMarker reads the install marker, returning nil when there is no
// usable marker to read. It reports no error by design, following the settings
// file's fail-open rule: an unresolvable config directory, a missing,
// unreadable, malformed or newer-version file, and a file naming an install
// method this CLI does not know all mean the same thing to every caller, which
// is that nothing is known about how this binary was installed.
// The signature is the contract; do not widen it to return an error.
func LoadInstallMarker() *InstallMarker {
	path, err := installMarkerPath()
	if err != nil {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var marker InstallMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil
	}
	if marker.Version != installMarkerVersion {
		return nil
	}
	if marker.Method != InstallMethodScript {
		return nil
	}
	return &marker
}
