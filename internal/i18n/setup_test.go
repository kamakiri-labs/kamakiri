package i18n

import "testing"

// clearLocale removes every variable Setup consults, the KAMAKIRI_LANG override
// included, so a test states the whole environment it depends on rather than
// inheriting the developer's.
func clearLocale(t *testing.T) {
	t.Helper()
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "")
	t.Setenv("KAMAKIRI_LANG", "")
}

// restoreSelection puts the catalog and the recorded selection back to the
// English default a fresh process starts in, so a test that calls Setup does
// not leave its provenance in force for the next one to read through
// Current(). A bare `defer Load("en")` only swaps the catalog back: Load
// changes nothing else, by design (see its doc comment), so it leaves the
// stale selection behind. These tests are in package i18n, so unlike
// language_test.go's restorePin (which has to go through the exported Setup
// and the environment) they can reset the field directly.
func restoreSelection(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		selection = Selection{Language: "en", From: FromDefault}
		Load("en")
	})
}

func TestSetupPersistedWins(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LANG", "ja_JP.UTF-8")

	Setup("en")

	got := Current()
	if got.Language != "en" {
		t.Errorf("language = %q, want en", got.Language)
	}
	if got.From != FromSetting {
		t.Errorf("provenance = %v, want FromSetting", got.From)
	}
	if got.EnvVar != "" {
		t.Errorf("EnvVar = %q, want empty for a saved preference", got.EnvVar)
	}
	if msg := T("login.prompt_email"); msg != "Email: " {
		t.Errorf("catalog not switched to en: T(login.prompt_email) = %q", msg)
	}
}

func TestSetupPersistedJapaneseLoadsCatalog(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)

	Setup("ja")

	if got := Current(); got.Language != "ja" || got.From != FromSetting {
		t.Errorf("Current() = %+v, want ja from FromSetting", got)
	}
	if msg := T("login.prompt_email"); msg != "メールアドレス: " {
		t.Errorf("catalog not switched to ja: T(login.prompt_email) = %q", msg)
	}
}

// An unsupported value is treated as no preference at all, so a hand-edited
// settings file cannot strand the user in a language with no catalog.
func TestSetupUnsupportedPersistedFallsThrough(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LC_MESSAGES", "ja_JP.UTF-8")

	Setup("fr")

	got := Current()
	if got.Language != "ja" {
		t.Errorf("language = %q, want ja", got.Language)
	}
	if got.From != FromLocale {
		t.Errorf("provenance = %v, want FromLocale", got.From)
	}
	if got.EnvVar != "LC_MESSAGES" {
		t.Errorf("EnvVar = %q, want LC_MESSAGES", got.EnvVar)
	}
}

func TestSetupLocaleNamesTheDecidingVariable(t *testing.T) {
	restoreSelection(t)

	for _, envVar := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Run(envVar, func(t *testing.T) {
			clearLocale(t)
			t.Setenv(envVar, "ja_JP.UTF-8")

			Setup("")

			got := Current()
			if got.Language != "ja" || got.From != FromLocale {
				t.Fatalf("Current() = %+v, want ja from FromLocale", got)
			}
			if got.EnvVar != envVar {
				t.Errorf("EnvVar = %q, want %q", got.EnvVar, envVar)
			}
		})
	}
}

// LC_ALL outranks the other two, so the reported variable has to be the one
// that actually decided.
func TestSetupLocalePrecedence(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LC_ALL", "ja_JP.UTF-8")
	t.Setenv("LC_MESSAGES", "ja_JP.UTF-8")
	t.Setenv("LANG", "ja_JP.UTF-8")

	Setup("")

	if got := Current(); got.EnvVar != "LC_ALL" {
		t.Errorf("EnvVar = %q, want LC_ALL", got.EnvVar)
	}
}

// The first variable that is set decides on its own, so an LC_ALL selecting
// English ends the walk and the Japanese LANG behind it is never read.
func TestSetupLocaleFirstSetVariableDecidesAlone(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LANG", "ja_JP.UTF-8")

	Setup("")

	got := Current()
	if got.Language != "en" || got.From != FromLocale {
		t.Fatalf("Current() = %+v, want en from FromLocale", got)
	}
	if got.EnvVar != "LC_ALL" {
		t.Errorf("EnvVar = %q, want LC_ALL", got.EnvVar)
	}
	if msg := T("login.prompt_email"); msg != "Email: " {
		t.Errorf("catalog not switched to en: T(login.prompt_email) = %q", msg)
	}
}

// An empty value is not a set variable, so the walk carries on past it and the
// variable reported is the next one, which is what actually decided.
func TestSetupLocaleWalksPastAnEmptyVariable(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LC_MESSAGES", "ja_JP.UTF-8")
	t.Setenv("LANG", "en_US.UTF-8")

	Setup("")

	got := Current()
	if got.Language != "ja" || got.From != FromLocale {
		t.Fatalf("Current() = %+v, want ja from FromLocale", got)
	}
	if got.EnvVar != "LC_MESSAGES" {
		t.Errorf("EnvVar = %q, want LC_MESSAGES", got.EnvVar)
	}
}

// An English locale is a decision like any other, so the line names the variable
// that made it instead of calling it the default.
func TestSetupEnglishLocaleNamesItsVariable(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LANG", "en_US.UTF-8")

	Setup("")

	got := Current()
	if got.Language != "en" {
		t.Errorf("language = %q, want en", got.Language)
	}
	if got.From != FromLocale {
		t.Errorf("provenance = %v, want FromLocale", got.From)
	}
	if got.EnvVar != "LANG" {
		t.Errorf("EnvVar = %q, want LANG", got.EnvVar)
	}
}

// The POSIX locales are set variables that select English, so they decide and
// are named like any other value rather than falling through to the default.
func TestSetupPosixLocaleSelectsEnglishAndIsNamed(t *testing.T) {
	restoreSelection(t)

	for _, val := range []string{"C", "POSIX"} {
		t.Run(val, func(t *testing.T) {
			clearLocale(t)
			t.Setenv("LC_ALL", val)
			t.Setenv("LANG", "ja_JP.UTF-8")

			Setup("")

			got := Current()
			if got.Language != "en" || got.From != FromLocale {
				t.Fatalf("Current() = %+v, want en from FromLocale", got)
			}
			if got.EnvVar != "LC_ALL" {
				t.Errorf("EnvVar = %q, want LC_ALL", got.EnvVar)
			}
		})
	}
}

func TestSetupDefaultsToEnglishWithNoLocaleSet(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)

	Setup("")

	got := Current()
	if got.Language != "en" {
		t.Errorf("language = %q, want en", got.Language)
	}
	if got.From != FromDefault {
		t.Errorf("provenance = %v, want FromDefault", got.From)
	}
	if got.EnvVar != "" {
		t.Errorf("EnvVar = %q, want empty when no locale variable carries a value", got.EnvVar)
	}
}

// The override is per-invocation and the preference is stored config, so the
// override wins whichever way round the two languages are.
func TestSetupOverrideBeatsThePersistedSetting(t *testing.T) {
	restoreSelection(t)

	cases := []struct {
		override  string
		persisted string
		wantEmail string
	}{
		{override: "en", persisted: "ja", wantEmail: "Email: "},
		{override: "ja", persisted: "en", wantEmail: "メールアドレス: "},
	}
	for _, tc := range cases {
		t.Run(tc.override, func(t *testing.T) {
			clearLocale(t)
			t.Setenv("KAMAKIRI_LANG", tc.override)

			Setup(tc.persisted)

			got := Current()
			if got.Language != tc.override || got.From != FromEnvOverride {
				t.Fatalf("Current() = %+v, want %s from FromEnvOverride", got, tc.override)
			}
			if got.EnvVar != "KAMAKIRI_LANG" {
				t.Errorf("EnvVar = %q, want KAMAKIRI_LANG", got.EnvVar)
			}
			if msg := T("login.prompt_email"); msg != tc.wantEmail {
				t.Errorf("catalog not switched to %s: T(login.prompt_email) = %q", tc.override, msg)
			}
		})
	}
}

// With nothing saved the override still outranks the locale.
func TestSetupOverrideBeatsTheEnvironmentLocale(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)
	t.Setenv("LANG", "ja_JP.UTF-8")
	t.Setenv("KAMAKIRI_LANG", "en")

	Setup("")

	got := Current()
	if got.Language != "en" || got.From != FromEnvOverride {
		t.Fatalf("Current() = %+v, want en from FromEnvOverride", got)
	}
	if got.EnvVar != "KAMAKIRI_LANG" {
		t.Errorf("EnvVar = %q, want KAMAKIRI_LANG", got.EnvVar)
	}
	if msg := T("login.prompt_email"); msg != "Email: " {
		t.Errorf("catalog not switched to en: T(login.prompt_email) = %q", msg)
	}
}

// The override is exactly `en` or `ja`, with no case folding and no locale
// suffix parsed off. Anything else fails open the way an unsupported saved
// preference does, so a typo in a shell profile cannot fail a command.
func TestSetupIgnoresAnUnrecognizedOverride(t *testing.T) {
	restoreSelection(t)

	cases := []struct {
		name string
		val  string
	}{
		{name: "wrong case", val: "EN"},
		{name: "language name", val: "japanese"},
		{name: "locale value", val: "ja_JP.UTF-8"},
		{name: "no catalog", val: "fr"},
		{name: "empty", val: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearLocale(t)
			t.Setenv("KAMAKIRI_LANG", tc.val)

			Setup("ja")

			got := Current()
			if got.Language != "ja" || got.From != FromSetting {
				t.Errorf("Current() = %+v, want ja from FromSetting", got)
			}
			if msg := T("login.prompt_email"); msg != "メールアドレス: " {
				t.Errorf("catalog not switched to ja: T(login.prompt_email) = %q", msg)
			}
		})
	}
}

func TestSetupIsRepeatable(t *testing.T) {
	restoreSelection(t)
	clearLocale(t)

	Setup("ja")
	Setup("en")

	if got := Current(); got.Language != "en" || got.From != FromSetting {
		t.Errorf("Current() = %+v, want en from FromSetting", got)
	}
	if msg := T("login.prompt_email"); msg != "Email: " {
		t.Errorf("catalog not switched back to en: T(login.prompt_email) = %q", msg)
	}
}

func TestSupported(t *testing.T) {
	for _, lang := range []string{"en", "ja"} {
		if !Supported(lang) {
			t.Errorf("Supported(%q) = false, want true", lang)
		}
	}
	for _, lang := range []string{"", "fr", "EN", "ja_JP", "ja-JP", " ja", "ja "} {
		if Supported(lang) {
			t.Errorf("Supported(%q) = true, want false", lang)
		}
	}
}
