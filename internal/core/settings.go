package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// settingsVersion is the shape LoadSettings understands. A file written by a
// newer CLI carries a higher number and is ignored rather than misread.
const settingsVersion = 1

// Settings holds the user-level CLI preferences, shared across every project on
// the machine. It holds no secret. Language is stored verbatim: which values are
// supported is the localization package's business, not this one's, so the set
// of languages lives in exactly one place.
type Settings struct {
	Version  int    `json:"version"`
	Language string `json:"language,omitempty"`
}

// settingsPath returns the path to the settings file. It stays unexported
// because the file is read and written only here, unlike the credentials file,
// whose path commands print.
func settingsPath() (string, error) {
	dir, err := kamakiriConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// LoadSettings reads the settings file, returning nil when there is no usable
// preference to read. It reports no error by design: an unresolvable config
// directory and a missing, unreadable, malformed or future-version file all mean
// the same thing to every caller, and a preference the user never has to set
// must never be able to fail a command.
// The signature is the contract; do not widen it to return an error.
func LoadSettings() *Settings {
	path, err := settingsPath()
	if err != nil {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}
	if settings.Version != settingsVersion {
		return nil
	}
	return &settings
}

// SaveSettings writes language to the settings file, replacing whatever was
// there. It reports its error: a preference the user asked to persist and that
// did not persist has to be said out loud.
//
// The file holds no secret, so it is 0644, but the directory is still created
// 0700 like the credentials one. MkdirAll never chmods a directory that already
// exists, so creating it 0755 here would leave the credentials file's directory
// world-readable for good on a machine where the language is set before the
// first login.
func SaveSettings(language string) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_create_config_dir"), err)
	}

	settings := Settings{
		Version:  settingsVersion,
		Language: language,
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_marshal_settings"), err)
	}

	if err := atomicWriteFile(path, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_write_settings"), err)
	}
	return nil
}
