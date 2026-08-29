package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// updateCheckVersion is the shape LoadUpdateCheck understands. A file written
// by a newer CLI carries a higher number and is ignored rather than misread.
const updateCheckVersion = 1

// UpdateCheck records when the CLI last told the user about a newer release,
// which is the whole of what keeps that line to once a day. It holds no secret
// and nothing the user asked to keep.
type UpdateCheck struct {
	Version   int       `json:"version"`
	LastNudge time.Time `json:"last_nudge"`
}

// updateCheckPath returns the path to the update-check file. It stays
// unexported because the file is read and written only here.
func updateCheckPath() (string, error) {
	dir, err := kamakiriConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update-check.json"), nil
}

// LoadUpdateCheck reads the update-check file, returning nil when there is
// nothing usable to read. It reports no error by design, following the settings
// file's fail-open rule: an unresolvable config directory and a missing,
// unreadable, malformed or newer-version file all mean the same thing, which is
// that nothing is known about when the user was last told. A timestamp that is
// not a valid RFC 3339 one fails the unmarshal and lands in the same place.
// The signature is the contract; do not widen it to return an error.
func LoadUpdateCheck() *UpdateCheck {
	path, err := updateCheckPath()
	if err != nil {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var state UpdateCheck
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	if state.Version != updateCheckVersion {
		return nil
	}
	return &state
}

// SaveUpdateCheck records lastNudge as the moment the user was told about a
// newer release. It reports nothing at all, which is the one place this file
// departs from SaveSettings: nobody asked for this to be written, so a failure
// has nobody to tell, and the command it decorates must not fail because of it.
// The signature is the contract; do not widen it to return an error.
//
// The file holds no secret, so it is 0644, but the directory is still created
// 0700 like the credentials one, for the reason settings.go records: MkdirAll
// never chmods a directory that already exists, and this can be the first thing
// that ever creates it.
func SaveUpdateCheck(lastNudge time.Time) {
	path, err := updateCheckPath()
	if err != nil {
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}

	data, err := json.MarshalIndent(UpdateCheck{
		Version:   updateCheckVersion,
		LastNudge: lastNudge,
	}, "", "  ")
	if err != nil {
		return
	}

	_ = atomicWriteFile(path, append(data, '\n'), 0644)
}
