package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// LoginSecret holds the in-progress login secret for channel binding.
type LoginSecret struct {
	Email  string `json:"email"`
	Secret struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"secret"`
}

// IsValid returns true if the secret is for the given email and not expired.
func (ls *LoginSecret) IsValid(email string) bool {
	return ls.Email == email && time.Now().Before(ls.Secret.ExpiresAt)
}

// LoginSecretPath returns the path to the login secret file.
func LoginSecretPath() (string, error) {
	dir, err := kamakiriConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".login_secret"), nil
}

// LoadLoginSecret reads the login secret file.
// Returns nil, nil if the file does not exist.
func LoadLoginSecret() (*LoginSecret, error) {
	path, err := LoginSecretPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", i18n.T("core.err_read_login_secret"), err)
	}

	var ls LoginSecret
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil, fmt.Errorf("%s: %w", i18n.T("core.err_parse_login_secret"), err)
	}
	return &ls, nil
}

// SaveLoginSecret writes the login secret to disk. The token binds the pending
// email-code exchange, so the file is written 0600 and the directory created
// 0700.
func SaveLoginSecret(email, token string, expiresAt time.Time) error {
	path, err := LoginSecretPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_create_config_dir"), err)
	}

	ls := LoginSecret{Email: email}
	ls.Secret.Token = token
	ls.Secret.ExpiresAt = expiresAt

	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_marshal_login_secret"), err)
	}

	if err := atomicWriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("core.err_write_login_secret"), err)
	}
	return nil
}

// DeleteLoginSecret removes the login secret file.
func DeleteLoginSecret() error {
	path, err := LoginSecretPath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", i18n.T("core.err_delete_login_secret"), err)
	}
	return nil
}
