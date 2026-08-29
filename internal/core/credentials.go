package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// Credentials holds the stored API key and account email.
type Credentials struct {
	Version int    `json:"version"`
	APIKey  string `json:"api_key"`
	Email   string `json:"email,omitempty"`
}

// CredentialsPath returns the path to the credentials file.
func CredentialsPath() (string, error) {
	dir, err := kamakiriConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// LoadCredentials reads and parses the credentials file.
// Returns nil, nil if the file does not exist.
func LoadCredentials() (*Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", i18n.T("core.err_read_credentials"), err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("%s: %w", i18n.T("core.err_parse_credentials"), err)
	}
	return &creds, nil
}

// SaveCredentials writes the API key and email to the credentials file. The key
// is a bearer token, so the file is written 0600 and the directory created 0700.
func SaveCredentials(apiKey, email string) error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_create_config_dir"), err)
	}

	creds := Credentials{
		Version: 1,
		APIKey:  apiKey,
		Email:   email,
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_marshal_credentials"), err)
	}

	if err := atomicWriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_write_credentials"), err)
	}
	return nil
}
