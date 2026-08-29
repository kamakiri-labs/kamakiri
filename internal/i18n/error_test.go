package i18n

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewErrorLooksUpAtCallTime(t *testing.T) {
	restoreSelection(t)

	err := NewError("login.prompt_email")

	Load("en")
	if got := err.Error(); got != "Email: " {
		t.Errorf("Error() = %q, want %q", got, "Email: ")
	}
	Load("ja")
	if got := err.Error(); got != "メールアドレス: " {
		t.Errorf("Error() after Load(ja) = %q, want %q", got, "メールアドレス: ")
	}
}

func TestNewErrorMatchesItselfWithErrorsIs(t *testing.T) {
	sentinel := NewError("login.prompt_email")

	if !errors.Is(sentinel, sentinel) {
		t.Error("errors.Is(sentinel, sentinel) = false, want true")
	}
}

// Identity is the whole contract: two sentinels built from one key are two
// distinct errors, so a package that adopts a key another one already uses does
// not silently start matching its errors.
func TestNewErrorDoesNotMatchATwinOnTheSameKey(t *testing.T) {
	first := NewError("login.prompt_email")
	second := NewError("login.prompt_email")

	if errors.Is(first, second) {
		t.Error("errors.Is(first, second) = true for two separate sentinels on one key, want false")
	}
}

func TestNewErrorMatchesThroughAWrap(t *testing.T) {
	sentinel := NewError("login.prompt_email")

	wrapped := fmt.Errorf("saving the file: %w", sentinel)

	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is(wrapped, sentinel) = false, want true")
	}
}

func TestNewErrorOnAMissingKeyPrintsTheKey(t *testing.T) {
	restoreSelection(t)
	Load("en")

	if got := NewError("nonexistent_key").Error(); got != "nonexistent_key" {
		t.Errorf("Error() = %q, want %q", got, "nonexistent_key")
	}
}
