package i18n

import (
	"strings"
	"testing"
)

func TestLoadEnglish(t *testing.T) {
	Load("en")

	if got := T("login.prompt_email"); got != "Email: " {
		t.Errorf("T(login.prompt_email) = %q, want %q", got, "Email: ")
	}
}

func TestLoadJapanese(t *testing.T) {
	Load("ja")

	if got := T("login.prompt_email"); got != "メールアドレス: " {
		t.Errorf("T(login.prompt_email) = %q, want %q", got, "メールアドレス: ")
	}
}

func TestTf(t *testing.T) {
	Load("en")

	got := Tf("login.confirmation_sent", "user@example.com")
	want := "Confirmation code sent to user@example.com."
	if got != want {
		t.Errorf("Tf(login.confirmation_sent) = %q, want %q", got, want)
	}
}

func TestInviteRequiredPresentInBothLocales(t *testing.T) {
	restoreSelection(t)

	got := map[string]string{}
	for _, lang := range []string{"en", "ja"} {
		Load(lang)
		got[lang] = T("login.err_invite_required")

		if got[lang] == "login.err_invite_required" {
			t.Errorf("login.err_invite_required missing in %s locale", lang)
		}
		if !strings.Contains(got[lang], "beta@kamakiri-labs.jp") {
			t.Errorf("login.err_invite_required (%s) missing the beta contact address: %q", lang, got[lang])
		}
	}

	// The JA copy must be a real translation, not a copy of the English string,
	// so a regression that reverts it (while keeping the address) is caught.
	if got["en"] == got["ja"] {
		t.Errorf("login.err_invite_required is not localized: en and ja are identical")
	}
}

func TestMissingKeyReturnsKey(t *testing.T) {
	Load("en")

	if got := T("nonexistent_key"); got != "nonexistent_key" {
		t.Errorf("T(nonexistent_key) = %q, want %q", got, "nonexistent_key")
	}
}

func TestDetectLangJapaneseLocaleNamesItsVariable(t *testing.T) {
	clearLocale(t)
	t.Setenv("LANG", "ja_JP.UTF-8")

	got, envVar := detectLang()
	if got != "ja" {
		t.Errorf("detectLang() = %q, want %q", got, "ja")
	}
	if envVar != "LANG" {
		t.Errorf("detectLang() named %q as the deciding variable, want %q", envVar, "LANG")
	}
}

// An English locale is a decision like any other, so it names the variable that
// made it instead of leaving the name empty for the default to claim.
func TestDetectLangEnglishLocaleNamesItsVariable(t *testing.T) {
	clearLocale(t)
	t.Setenv("LANG", "en_US.UTF-8")

	got, envVar := detectLang()
	if got != "en" {
		t.Errorf("detectLang() = %q, want %q", got, "en")
	}
	if envVar != "LANG" {
		t.Errorf("detectLang() named %q as the deciding variable, want %q", envVar, "LANG")
	}
}
