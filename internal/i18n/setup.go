package i18n

import "os"

// overrideEnvVar names the variable that forces the language for one
// invocation.
const overrideEnvVar = "KAMAKIRI_LANG"

// Provenance says how the language in force was chosen.
type Provenance int

const (
	// FromDefault is English, chosen because nothing else pointed anywhere.
	FromDefault Provenance = iota
	// FromLocale is a language read out of the environment locale.
	FromLocale
	// FromSetting is a language the user saved.
	FromSetting
	// FromEnvOverride is a language forced by KAMAKIRI_LANG.
	FromEnvOverride
)

// Selection is the language in force together with how it was reached. EnvVar
// names the environment variable that decided, and is set when From is
// FromLocale or FromEnvOverride.
type Selection struct {
	Language string
	From     Provenance
	EnvVar   string
}

var selection = Selection{Language: "en", From: FromDefault}

// Setup resolves the language and loads the matching catalog. persisted is the
// preference the user saved, empty when there is none; a value no catalog
// covers counts as no preference, so a hand-edited settings file falls through
// to the locale rather than stranding the user. The order is KAMAKIRI_LANG,
// then the saved preference, then the environment locale, then English.
//
// KAMAKIRI_LANG outranks the saved preference because it is the per-invocation
// override, which is the point of having it: one run in the other language
// without rewriting stored config, the way an environment variable beats a
// config file everywhere else. It has to be exactly `en` or `ja`; any other
// value is ignored in silence and resolution carries on down the order, the
// same fail-open an unsupported saved preference gets, so a typo in a shell
// profile cannot fail a command.
//
// Loading the catalog and recording the selection happen here together, so what
// Current reports is what T renders. Load is the narrower door and swaps the
// catalog on its own, leaving the recorded selection where it was, so anything
// that reports the language as well as rendering in it comes through here.
//
// Calling it again is expected: setting the language re-resolves so the
// confirmation prints in the language then in force. It is not safe to call
// concurrently, since it swaps an unsynchronized catalog map.
func Setup(persisted string) {
	if override := os.Getenv(overrideEnvVar); Supported(override) {
		selection = Selection{Language: override, From: FromEnvOverride, EnvVar: overrideEnvVar}
		Load(override)
		return
	}

	if Supported(persisted) {
		selection = Selection{Language: persisted, From: FromSetting}
		Load(persisted)
		return
	}

	if lang, envVar := detectLang(); envVar != "" {
		selection = Selection{Language: lang, From: FromLocale, EnvVar: envVar}
		Load(lang)
		return
	}

	selection = Selection{Language: "en", From: FromDefault}
	Load("en")
}

// Current returns the language in force and how it was chosen.
func Current() Selection {
	return selection
}

// Supported reports whether lang is one the CLI has a catalog for.
func Supported(lang string) bool {
	return lang == "en" || lang == "ja"
}
