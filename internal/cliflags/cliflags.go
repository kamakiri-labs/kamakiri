// Package cliflags holds the pure helpers behind the kamakiri command's
// hand-rolled flag parsing. They return errors for the caller to report, and
// cover both flag forms uniformly:
//
//   - space form:  `--name value`
//   - equals form: `--name=value`
//
// A boolean flag rejects the equals form through RequireNoValue, so `--alias=true`
// cannot quietly set `alias` whatever value it embeds.
package cliflags

import (
	"errors"
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// SplitArg separates "--name=value" into ("--name", "value", true), or "--name"
// into ("--name", "", false). Only the first `=` splits, so a value that itself
// contains one keeps it: `--domain-id=foo=1001` yields "foo=1001".
func SplitArg(arg string) (name, value string, hasValue bool) {
	if idx := strings.Index(arg, "="); idx > 0 {
		return arg[:idx], arg[idx+1:], true
	}
	return arg, "", false
}

// Value returns the value for the flag at args[i] and the loop index to carry
// on from: the same index for the equals form, i+1 once the space form has
// consumed the next argument. A non-empty hint is appended to a missing-value
// error to show the format expected.
func Value(args []string, i int, hasValue bool, embedded, name, hint string) (string, int, error) {
	if hasValue {
		if embedded == "" {
			return "", i, errors.New(i18n.Tf("cliflags.err_equals_requires_value", name, FormatHint(hint)))
		}
		return embedded, i, nil
	}
	if i+1 >= len(args) {
		return "", i, errors.New(i18n.Tf("cliflags.err_requires_value", name, FormatHint(hint)))
	}
	return args[i+1], i + 1, nil
}

// RequireNoValue rejects the equals form on a boolean flag. The embedded value
// would otherwise be dropped without a word, letting `--no-wait=false` turn the
// wait off.
func RequireNoValue(name string, hasValue bool) error {
	if hasValue {
		return errors.New(i18n.Tf("cliflags.err_takes_no_value", name))
	}
	return nil
}

// FormatHint wraps hint in parentheses, ready to append to a missing-value
// error, and returns "" for an empty hint.
func FormatHint(hint string) string {
	if hint == "" {
		return ""
	}
	return i18n.Tf("cliflags.hint_wrapper", hint)
}
