// Package language implements the `kamakiri language` command: it reports the
// language the CLI renders in and how that was decided, and saves the
// preference.
package language

import (
	"errors"
	"fmt"
	"io"

	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// Run prints the current language with no argument, and saves `en` or `ja` as
// the preference with one. Anything else returns an error and prints nothing.
func Run(args []string, out io.Writer) error {
	switch {
	case len(args) == 0:
		report(out)
		return nil
	case len(args) == 1 && i18n.Supported(args[0]):
		return save(args[0], out)
	case len(args) == 1:
		return errors.New(i18n.Tf("language.err_unsupported", args[0]))
	default:
		return errors.New(i18n.T("language.err_usage"))
	}
}

func report(out io.Writer) {
	current := i18n.Current()
	fmt.Fprintln(out, i18n.Tf("language.current", displayName(current.Language), sourceLabel(current)))
	fmt.Fprintln(out, i18n.T("language.hint_change"))
}

// save writes the preference before re-resolving, so a failed write leaves
// nothing changed and reports itself in the language still in force. The
// re-resolution is honest rather than a shortcut: the confirmation renders in
// whatever is actually in force afterwards, which is the language just saved,
// proving the switch took, unless KAMAKIRI_LANG is overriding it, in which case
// the note says so.
func save(lang string, out io.Writer) error {
	if err := core.SaveSettings(lang); err != nil {
		// The frame is ours and localizes; the reason, whatever produced it, is
		// passed on as delivered.
		return fmt.Errorf("%s: %w", i18n.T("language.err_save"), err)
	}

	i18n.Setup(lang)
	fmt.Fprintln(out, i18n.Tf("language.set_confirm", displayName(lang)))
	if i18n.Current().From == i18n.FromEnvOverride {
		fmt.Fprintln(out, i18n.T("language.env_override_note"))
	}
	return nil
}

// displayName returns the name of lang in the language in force. The key comes
// from a switch over literals rather than from assembling one: the check that
// every key has a call site and every call site a key reads the source, so a key
// built at run time is invisible to it on both counts.
func displayName(lang string) string {
	switch lang {
	case "ja":
		return i18n.T("language.name_ja")
	default:
		return i18n.T("language.name_en")
	}
}

// sourceLabel says what decided the language in force, naming the environment
// variable when one did. Its keys are literals for the same reason displayName's
// are.
func sourceLabel(current i18n.Selection) string {
	switch current.From {
	case i18n.FromSetting:
		return i18n.T("language.source_setting")
	// The KAMAKIRI_LANG override renders through this key too, despite the name:
	// what the line reports is which environment variable decided, which reads
	// the same whether that was a locale variable or the override.
	case i18n.FromLocale, i18n.FromEnvOverride:
		return i18n.Tf("language.source_locale", current.EnvVar)
	default:
		return i18n.T("language.source_default")
	}
}
