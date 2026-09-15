package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// EnvAPIKey names the environment variable that carries the API key for a run
// with no credentials file, which is how a CI runner authenticates.
const EnvAPIKey = "KAMAKIRI_API_KEY"

// Source names where a loaded credential came from, so a consumer holding one
// can tell a key that came out of the environment from one read out of the
// file without going back to either.
type Source string

const (
	SourceFile        Source = "file"
	SourceEnvironment Source = "environment"
)

// Credentials holds the API key a run authenticates with, and the account
// email when that key came from the file. Source is stamped by LoadCredentials
// after the unmarshal rather than taken from the file, which is why a crafted
// file cannot claim to have come from the environment, and the tag keeps the
// field out of the file SaveCredentials writes.
type Credentials struct {
	Version int    `json:"version"`
	APIKey  string `json:"api_key"`
	Email   string `json:"email,omitempty"`
	Source  Source `json:"-"`
}

// APIKeyFromEnvironment returns the API key EnvAPIKey holds, and whether it
// holds one at all: a value that is empty or blank counts as unset, the way an
// empty KAMAKIRI_API_URL does. The value is trimmed of surrounding whitespace
// because a secret store hands one back with a trailing newline often enough,
// and Go's HTTP transport refuses a header value carrying one before the request
// leaves the process, so the user would meet an unlocalized transport error
// instead of anything this CLI words. Nothing else about the value is checked;
// the server is the authority on what a key looks like.
func APIKeyFromEnvironment() (string, bool) {
	key := strings.TrimSpace(os.Getenv(EnvAPIKey))
	return key, key != ""
}

// CredentialsPath returns the path to the credentials file.
func CredentialsPath() (string, error) {
	dir, err := kamakiriConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// LoadCredentials resolves the credential this run authenticates with:
// EnvAPIKey first, and the credentials file only when the variable holds no
// key. While the variable is in force the file is not read at all, which is
// what lets a CI runner that has no such file authenticate. Returns nil, nil
// when neither is there. The file counts as holding a key under the same rule
// the variable follows, surrounding whitespace trimmed and a blank or absent
// value counting as none, so a hand-edited file with a trailing newline still
// authenticates and a file with no key in it reads as not logged in rather
// than as a key the server will reject.
func LoadCredentials() (*Credentials, error) {
	if key, ok := APIKeyFromEnvironment(); ok {
		return &Credentials{Version: 1, APIKey: key, Source: SourceEnvironment}, nil
	}

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
	creds.APIKey = strings.TrimSpace(creds.APIKey)
	if creds.APIKey == "" {
		return nil, nil
	}
	creds.Source = SourceFile
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
