package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// ProjectConfig is the local link between a directory and a site. A config
// carrying a `redirects` key still loads and silently drops it, which is what we
// want: redirects are authored in a deploy-root `_redirects` file, and stale
// rules copied into the config must not compete with it.
type ProjectConfig struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
}

// RequireCredentials returns nil when this run has a credential, and otherwise
// the error a caller prints as-is: the load error when the file cannot be read,
// the not-logged-in line when neither the variable nor the file holds a key.
func RequireCredentials() error {
	creds, err := LoadCredentials()
	if err != nil {
		return err
	}
	if creds == nil {
		return errors.New(i18n.T("common.err_not_logged_in"))
	}
	return nil
}

// RequireProject returns the linked site's config, or an error the caller can
// print as-is when the user is not logged in or the directory is not linked.
func RequireProject() (*ProjectConfig, error) {
	if err := RequireCredentials(); err != nil {
		return nil, err
	}

	config, err := LoadProject()
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New(i18n.T("common.err_no_site_linked"))
	}

	return config, nil
}

// ProjectPath returns the config path, relative to the current directory.
func ProjectPath() string {
	return filepath.Join(".kamakiri", "config.json")
}

// LoadProject reads and parses the project config file.
// Returns nil, nil if the file does not exist.
func LoadProject() (*ProjectConfig, error) {
	path := ProjectPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", i18n.T("core.err_read_project_config"), err)
	}

	var config ProjectConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%s: %w", i18n.T("core.err_parse_project_config"), err)
	}
	return &config, nil
}

// rejectSymlinks fails if the .kamakiri directory or the config file is a
// symlink, so that a planted link cannot redirect the write to a file outside
// the project tree.
func rejectSymlinks(path string) error {
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", i18n.Tf("core.err_check_path", dir), err)
		}
	} else if info.Mode()&os.ModeSymlink != 0 {
		return errors.New(i18n.Tf("core.err_symlink", dir))
	}
	if info, err := os.Lstat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", i18n.Tf("core.err_check_path", path), err)
		}
	} else if info.Mode()&os.ModeSymlink != 0 {
		return errors.New(i18n.Tf("core.err_symlink", path))
	}
	return nil
}

// SaveProject writes the project config to .kamakiri/config.json.
func SaveProject(config *ProjectConfig) error {
	path := ProjectPath()

	if err := rejectSymlinks(path); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_create_project_dir"), err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_marshal_project_config"), err)
	}

	if err := atomicWriteFile(path, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_write_project_config"), err)
	}
	return nil
}
