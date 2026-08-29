package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// kamakiriConfigDir returns $XDG_CONFIG_HOME/kamakiri, or ~/.config/kamakiri
// when that variable is unset.
func kamakiriConfigDir() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%s: %w", i18n.T("core.err_determine_config_dir"), err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "kamakiri"), nil
}

// atomicWriteFile writes through a temp file in the same directory and renames
// it into place, so an interrupted write cannot leave a half-written file where
// credentials or config are expected. Permissions are set before the rename, so
// the file is never briefly readable by anyone else.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
