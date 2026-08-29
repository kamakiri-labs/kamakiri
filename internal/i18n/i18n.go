// Package i18n holds the message catalogs and resolves which language the CLI
// renders in.
//
// It imports nothing from the rest of the CLI, and must not start to: every
// package it would reach for is itself localized and so calls back into here,
// making any such import an import cycle. The standard library and
// golang.org/x/text are the whole dependency list.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed messages_en.json messages_ja.json
var messagesFS embed.FS

var messages map[string]string

// The environment-only resolution, so a message printed before main has wired
// anything up still renders and still reports a truthful provenance. main calls
// Setup again with the saved preference, which outranks the environment locale.
func init() {
	Setup("")
}

// Load switches the message catalog to lang, falling back to English for any
// language with no catalog of its own. It changes nothing else: the selection
// Current reports is Setup's to record, so a caller that reads the provenance
// wants Setup instead.
func Load(lang string) {
	filename := "messages_en.json"
	if lang == "ja" {
		filename = "messages_ja.json"
	}

	data, err := messagesFS.ReadFile(filename)
	if err != nil {
		panic(fmt.Sprintf("i18n: failed to read %s: %v", filename, err))
	}

	messages = make(map[string]string)
	if err := json.Unmarshal(data, &messages); err != nil {
		panic(fmt.Sprintf("i18n: failed to parse %s: %v", filename, err))
	}
}

// T returns the message for key, or the key itself when the catalog has no
// entry, so a missing translation degrades to something printable.
func T(key string) string {
	if msg, ok := messages[key]; ok {
		return msg
	}
	return key
}

// Tf formats the message for key with args.
func Tf(key string, args ...any) string {
	return fmt.Sprintf(T(key), args...)
}

// detectLang reads the language out of the environment locale under POSIX
// precedence: the first of LC_ALL, LC_MESSAGES and LANG that is set and
// non-empty decides on its own, and the variables after it are not consulted.
// A value with the ja prefix selects Japanese and anything else selects
// English, C and POSIX included, so a user who sets LC_ALL as an override gets
// the override rather than a scan that reads past it. The returned name is the
// deciding variable's, empty only when none of the three carried a value, so
// the caller can tell a locale choice from the default.
func detectLang() (lang, envVar string) {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		val := os.Getenv(env)
		if val == "" {
			continue
		}
		if strings.HasPrefix(val, "ja") {
			return "ja", env
		}
		return "en", env
	}
	return "en", ""
}
