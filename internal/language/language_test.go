package language

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The language is pinned so the assertions below do not depend on the locale the
// developer happens to run under. It goes through Setup rather than Load because
// this package reports the provenance as well as the catalog, and Load sets only
// the catalog; Setup with a supported value settles both. Setup reads
// KAMAKIRI_LANG ahead of the value it is handed, so an exported override would
// otherwise decide this package's catalog and defeat the pin; clearing it takes
// os.Unsetenv, there being no *testing.T here for t.Setenv. Nothing here uses
// t.Parallel: the catalog is an unsynchronized map, and half these tests swap it
// deliberately.
func TestMain(m *testing.M) {
	os.Unsetenv("KAMAKIRI_LANG")
	i18n.Setup("en")
	os.Exit(m.Run())
}

// clearLocale removes every variable the language resolution consults, the
// KAMAKIRI_LANG override included, so a test states the whole environment it
// depends on.
func clearLocale(t *testing.T) {
	t.Helper()
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "")
	t.Setenv("KAMAKIRI_LANG", "")
}

// restorePin puts the catalog and the recorded selection back to the English pin
// TestMain set, so a test that resolves the language for itself does not leave
// its result in force for the next one. Setup reads KAMAKIRI_LANG ahead of the
// value it is handed, so the cleanup clears that variable outright before
// re-pinning: the pin then holds for a test that sets an override no matter
// which order the cleanups run in, and the call does not have to come before the
// test's own t.Setenv calls to be correct. A plain `defer i18n.Setup("en")`
// cannot do that, since it runs while the override is still set and re-resolves
// straight back to it.
func restorePin(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		os.Unsetenv("KAMAKIRI_LANG")
		i18n.Setup("en")
	})
}

// isolateConfig points the config directory at a temporary one. Without it a
// test run rewrites the developer's own settings file.
func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestRunBareReportsASavedPreference(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	i18n.Setup("en")

	var out bytes.Buffer
	if err := Run(nil, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := "Language: English (saved preference)\n" +
		"Change it with `kamakiri language en` or `kamakiri language ja`.\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRunBareNamesTheDecidingEnvironmentVariable(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	t.Setenv("LC_MESSAGES", "ja_JP.UTF-8")
	i18n.Setup("")

	var out bytes.Buffer
	if err := Run(nil, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := "言語: 日本語（環境変数 LC_MESSAGES による指定）\n" +
		"言語は `kamakiri language en` または `kamakiri language ja` で変更できます。\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// The override decides over a saved preference, so the provenance has to name
// it: a user looking for the setting that explains the language is looking in
// the wrong place until the line tells them so.
func TestRunBareNamesTheOverrideVariable(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	t.Setenv("KAMAKIRI_LANG", "en")
	i18n.Setup("ja")

	var out bytes.Buffer
	if err := Run(nil, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := "Language: English (from the KAMAKIRI_LANG environment variable)\n" +
		"Change it with `kamakiri language en` or `kamakiri language ja`.\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// The same line in Japanese: the override borrows the locale key, whose two
// renderings are separate strings, so naming the variable has to be pinned in
// both.
func TestRunBareNamesTheOverrideVariableInJapanese(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	t.Setenv("KAMAKIRI_LANG", "ja")
	i18n.Setup("en")

	var out bytes.Buffer
	if err := Run(nil, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := "言語: 日本語（環境変数 KAMAKIRI_LANG による指定）\n" +
		"言語は `kamakiri language en` または `kamakiri language ja` で変更できます。\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRunBareReportsTheDefault(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	i18n.Setup("")

	var out bytes.Buffer
	if err := Run(nil, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := "Language: English (default)\n" +
		"Change it with `kamakiri language en` or `kamakiri language ja`.\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// The confirmation renders in the language just chosen, so a user who cannot
// read the old one still sees that the switch took.
func TestRunJapaneseSavesAndConfirmsInJapanese(t *testing.T) {
	restorePin(t)
	dir := isolateConfig(t)
	clearLocale(t)
	i18n.Setup("en")

	var out bytes.Buffer
	if err := Run([]string{"ja"}, &out); err != nil {
		t.Fatalf("Run(ja) error = %v", err)
	}

	if got, want := out.String(), "言語を日本語に設定しました。\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got := i18n.Current(); got.Language != "ja" || got.From != i18n.FromSetting {
		t.Errorf("Current() = %+v, want ja from FromSetting", got)
	}

	data, err := os.ReadFile(filepath.Join(dir, "kamakiri", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if raw["version"] != float64(1) {
		t.Errorf("version = %v, want 1", raw["version"])
	}
	if raw["language"] != "ja" {
		t.Errorf("language = %v, want ja", raw["language"])
	}
}

func TestRunEnglishFromJapaneseConfirmsInEnglish(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	i18n.Setup("ja")

	var out bytes.Buffer
	if err := Run([]string{"en"}, &out); err != nil {
		t.Fatalf("Run(en) error = %v", err)
	}

	if got, want := out.String(), "Language set to English.\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if settings := core.LoadSettings(); settings == nil || settings.Language != "en" {
		t.Errorf("LoadSettings() = %+v, want the saved en preference", settings)
	}
}

// The preference is saved either way, but under an override it is not what is in
// force, so the confirmation renders in the language the override forces and the
// note says the saved value is waiting on the variable being unset.
func TestRunSavesUnderAnOverrideAndNotesIt(t *testing.T) {
	restorePin(t)
	dir := isolateConfig(t)
	clearLocale(t)
	t.Setenv("KAMAKIRI_LANG", "en")
	i18n.Setup("")

	var out bytes.Buffer
	if err := Run([]string{"ja"}, &out); err != nil {
		t.Fatalf("Run(ja) error = %v", err)
	}

	want := "Language set to Japanese.\n" +
		"KAMAKIRI_LANG is set, so it decides the language for now; the saved preference applies once it is unset.\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got := i18n.Current(); got.Language != "en" || got.From != i18n.FromEnvOverride {
		t.Errorf("Current() = %+v, want en from FromEnvOverride", got)
	}

	data, err := os.ReadFile(filepath.Join(dir, "kamakiri", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if raw["language"] != "ja" {
		t.Errorf("language = %v, want ja", raw["language"])
	}
}

// The same pair the other way round, since both lines render in the language the
// override forces rather than in the one just saved.
func TestRunSavesUnderAJapaneseOverrideAndNotesItInJapanese(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	t.Setenv("KAMAKIRI_LANG", "ja")
	i18n.Setup("")

	var out bytes.Buffer
	if err := Run([]string{"en"}, &out); err != nil {
		t.Fatalf("Run(en) error = %v", err)
	}

	want := "言語を英語に設定しました。\n" +
		"環境変数 KAMAKIRI_LANG が設定されているため、現在の言語はこの変数で決まります。KAMAKIRI_LANG を解除すると、保存された設定が適用されます。\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if settings := core.LoadSettings(); settings == nil || settings.Language != "en" {
		t.Errorf("LoadSettings() = %+v, want the saved en preference", settings)
	}
}

// A value the override does not recognize leaves the saved preference in charge,
// so the save is an ordinary one: nothing is waiting on the variable and the
// note stays away. What decides the note is the language actually in force, not
// KAMAKIRI_LANG merely carrying something.
func TestRunSavesUnderAnIgnoredOverrideWithoutTheNote(t *testing.T) {
	restorePin(t)
	isolateConfig(t)
	clearLocale(t)
	t.Setenv("KAMAKIRI_LANG", "EN")
	i18n.Setup("")

	var out bytes.Buffer
	if err := Run([]string{"ja"}, &out); err != nil {
		t.Fatalf("Run(ja) error = %v", err)
	}

	if got, want := out.String(), "言語を日本語に設定しました。\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got := i18n.Current(); got.Language != "ja" || got.From != i18n.FromSetting {
		t.Errorf("Current() = %+v, want ja from FromSetting", got)
	}
}

func TestRunRejectsAnUnsupportedLanguage(t *testing.T) {
	dir := isolateConfig(t)
	if err := core.SaveSettings("ja"); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "kamakiri", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var out bytes.Buffer
	runErr := Run([]string{"fr"}, &out)

	if runErr == nil {
		t.Fatal("Run(fr) = nil, want an error")
	}
	want := "unsupported language \"fr\"; use `en` or `ja`"
	if runErr.Error() != want {
		t.Errorf("error = %q, want %q", runErr.Error(), want)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing on the failure path", out.String())
	}

	after, err := os.ReadFile(filepath.Join(dir, "kamakiri", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("settings file changed to %q, want it left at %q", after, before)
	}
}

func TestRunRejectsExtraArguments(t *testing.T) {
	isolateConfig(t)

	var out bytes.Buffer

	err := Run([]string{"en", "ja"}, &out)

	if err == nil {
		t.Fatal("Run(en ja) = nil, want an error")
	}
	if want := "usage: kamakiri language [en|ja]"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing on the failure path", out.String())
	}
}

// A preference that did not persist has to be reported, in the language still
// in force, with the reason for it passed on as delivered inside our framing.
func TestRunReportsASaveFailure(t *testing.T) {
	restorePin(t)
	dir := isolateConfig(t)
	clearLocale(t)
	i18n.Setup("en")

	// A regular file where the config directory belongs, so the write fails.
	if err := os.WriteFile(filepath.Join(dir, "kamakiri"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var out bytes.Buffer
	err := Run([]string{"ja"}, &out)

	if err == nil {
		t.Fatal("Run(ja) = nil, want an error when the settings cannot be written")
	}
	if !strings.HasPrefix(err.Error(), "could not save the language preference: ") {
		t.Errorf("error = %q, want it framed by the save-failure copy", err.Error())
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want no confirmation when nothing was saved", out.String())
	}
	if got := i18n.Current(); got.Language != "en" {
		t.Errorf("Current() = %+v, want the language left as it was", got)
	}
}

// Under an override the language still in force is the one the override picked,
// so that is the language the failure reports itself in. The note belongs to the
// success path only: nothing was saved, so nothing is waiting on the variable.
func TestRunReportsASaveFailureUnderAnOverride(t *testing.T) {
	restorePin(t)
	dir := isolateConfig(t)
	clearLocale(t)
	t.Setenv("KAMAKIRI_LANG", "ja")
	i18n.Setup("en")

	// A regular file where the config directory belongs, so the write fails.
	if err := os.WriteFile(filepath.Join(dir, "kamakiri"), []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var out bytes.Buffer
	err := Run([]string{"en"}, &out)

	if err == nil {
		t.Fatal("Run(en) = nil, want an error when the settings cannot be written")
	}
	if !strings.HasPrefix(err.Error(), "言語設定を保存できませんでした: ") {
		t.Errorf("error = %q, want it framed by the Japanese save-failure copy", err.Error())
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want neither a confirmation nor the note when nothing was saved", out.String())
	}
	if settings := core.LoadSettings(); settings != nil {
		t.Errorf("LoadSettings() = %+v, want nothing written when the save failed", settings)
	}
}
