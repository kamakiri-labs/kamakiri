package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
)

// usageBlock is every byte `kamakiri` with no command prints, one Go line per
// output line. The `cdn` line lists `cleanup` because dispatch accepts it and
// the unknown-verb error names it, in that same order.
const usageBlock = "Usage: kamakiri <command>\n" +
	"\n" +
	"Commands:\n" +
	"  login       Log in with your email\n" +
	"  logout      Log out and revoke this machine's API key\n" +
	"  key         List, rename and revoke your API keys, and create and rotate deploy keys\n" +
	"  status      Show current status (--verbose shows DNS records inline; --recheck asks the server to check DNS now)\n" +
	"  init        Create a new site\n" +
	"  deploy      Deploy a directory or archive\n" +
	"  deploys     List recent deploys\n" +
	"  rollback    Roll back to a previous deploy\n" +
	"  teardown    Delete the linked site\n" +
	"  cdn         Manage CDN (cloudflare/webaccel/none/cleanup/verify/credentials/status/purge)\n" +
	"  domain      Manage custom domains (register/unregister/verify/set/unset/add/remove/list)\n" +
	"  subdomain   Get or set the site subdomain\n" +
	"  version     Print the CLI version (--version, -v)\n" +
	"  language    Show or set the CLI language (en/ja)\n" +
	"  upgrade     Update the CLI to the latest release\n" +
	"\n" +
	"Environment variables:\n" +
	"  KAMAKIRI_API_KEY\n" +
	"    The API key every command that needs a credential uses, for CI runners\n" +
	"    with no credentials file. While it is set, the credentials file is not\n" +
	"    read. Create a deploy key for it with `kamakiri key create`.\n" +
	"\n" +
	"  KAMAKIRI_CDN_TOKEN, KAMAKIRI_CDN_SECRET\n" +
	"    WebAccel API credentials for `kamakiri cdn webaccel`. Prefer these\n" +
	"    over --token/--secret flags in CI: command-line arguments are visible\n" +
	"    in `ps`, while env vars are not.\n"

// The usage blocks that run to several lines, one Go line per output line. Each
// is a single catalog value, so a golden quoting only its first line would
// leave the rest of it free to change underneath.
const (
	usageDomainAdd = "Usage: kamakiri domain add <domain> [--redirect [--status N] | --alias] [--no-wait]\n" +
		"  Blocks until the redirect/alias is live, streaming a go-live\n" +
		"  view (waiting → detected → live). The server keeps checking;\n" +
		"  resume with `kamakiri domain verify`.\n" +
		"  --no-wait  print the record + queue and return promptly\n" +
		"             (also the automatic behavior in non-TTY / CI).\n"

	usageDomainRegister = "Usage: kamakiri domain register <domain> [--no-wait]\n" +
		"  Registers account-level ownership of a domain (no project required).\n" +
		"  Prints the TXT to publish, then blocks until verified, streaming the\n" +
		"  view; the server keeps checking.\n" +
		"  --no-wait  print the record and return promptly (also the automatic\n" +
		"             behavior in non-TTY / CI).\n"

	usageDomainRemove = "Usage: kamakiri domain remove <domain> [--no-wait]\n" +
		"  Blocks until the teardown is in sync on our side, non-TTY / CI included.\n" +
		"  --no-wait  queue and return promptly.\n"

	usageDomainSet = "Usage: kamakiri domain set <domain> [--no-wait]\n" +
		"  Blocks until the domain is live, streaming a go-live view\n" +
		"  (waiting → detected → live). The server keeps checking;\n" +
		"  resume with `kamakiri domain verify`.\n" +
		"  --no-wait  print the record + queue and return promptly\n" +
		"             (also the automatic behavior in non-TTY / CI).\n"

	usageDomainUnregister = "Usage: kamakiri domain unregister <domain>\n" +
		"  Removes account-level registration of a domain (no project required).\n" +
		"  Synchronous; fails if a site still uses a host under it (detach first).\n"

	usageDomainUnset = "Usage: kamakiri domain unset [--no-wait]\n" +
		"  Blocks until the teardown is in sync on our side, non-TTY / CI included.\n" +
		"  --no-wait  queue and return promptly.\n"

	usageDomainVerify = "Usage: kamakiri domain verify <domain> [--no-wait]\n" +
		"  Re-checks your DNS now and blocks until the domain is live,\n" +
		"  streaming the view; the server keeps checking.\n" +
		"  --no-wait  open the check and print fresh status promptly\n" +
		"             (also the automatic behavior in non-TTY / CI).\n"

	// The one usage line the mode verbs share, so a mistyped flag on any of
	// them shows the whole mode surface rather than the one mode that was
	// typed.
	usageCDNMode = "Usage: kamakiri cdn <cloudflare|webaccel|none> [--no-wait]\n"
)

// golden is one dispatch invocation and the whole of what it produces. Whole
// blocks rather than phrases, because a fragment assertion leaves every lookup
// it does not quote free to change under it, so
// the swap that matters is always the one just outside the fragment. Comparing
// both streams and the exit code together is what makes these reach the wiring
// rather than the catalog values: they run the real switch, with real
// arguments, through the same seam the process uses.
//
// What has no golden here: the commands whose first act is an API call, namely
// `login`, `deploys`, `domain list`, `subdomain get` and the bare `cdn` view
// with its `cdn status` alias. Their dispatch is pinned up to the hand-off
// only, and for those there is nothing before the hand-off to print. Nothing
// here takes t.Parallel either, since run calls i18n.Setup, which swaps an
// unsynchronized catalog map.
type golden struct {
	name   string
	args   []string
	stdout string
	stderr string
	code   int
}

// pinDispatchEnvironment gives one test an environment that reads the same on
// any machine. XDG_CONFIG_HOME points at an empty directory because run
// resolves the language from the saved settings and loadAPIKey reads the
// credentials file: without it a test would render in whatever language the
// developer saved. KAMAKIRI_API_KEY is cleared alongside it, since it outranks
// that file: an empty config home alone would still hand a developer's real key
// to whatever the test dispatched into. KAMAKIRI_API_URL is cleared too, so a
// test that sets no server dispatches against the default base URL rather than
// a host the developer exported; an empty value reads as unset. The four locale
// variables are cleared for the first of those reasons one step earlier: run
// re-resolves the language on every call, so the catalog TestMain pins does not
// survive it.
func pinDispatchEnvironment(t *testing.T) {
	t.Helper()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KAMAKIRI_API_KEY", "")
	t.Setenv("KAMAKIRI_API_URL", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "")
	t.Setenv("KAMAKIRI_LANG", "")
}

// Nothing else in the package fails when the KAMAKIRI_API_KEY clear above is
// dropped, so the clear is pinned here rather than through a dispatch. What it
// costs is invisible on a machine that does not export the variable, and on one
// that does it is a test handing a real key to the server it drives.
func TestPinDispatchEnvironmentClearsTheAPIKey(t *testing.T) {
	pinDispatchEnvironment(t)

	if _, ok := core.APIKeyFromEnvironment(); ok {
		t.Error("APIKeyFromEnvironment() reports a key after pinDispatchEnvironment, want none")
	}
}

// run drives the dispatch with the environment pinned, so a golden reads the
// same on any machine.
func (g golden) run(t *testing.T) {
	t.Helper()

	pinDispatchEnvironment(t)

	var stdout, stderr bytes.Buffer
	code := run(g.args, strings.NewReader(""), &stdout, &stderr)

	invocation := strings.TrimRight("kamakiri "+strings.Join(g.args, " "), " ")
	if got := stderr.String(); got != g.stderr {
		t.Errorf("`%s` wrote to stderr:\n%s\nwant:\n%s", invocation, got, g.stderr)
	}
	if got := stdout.String(); got != g.stdout {
		t.Errorf("`%s` wrote to stdout:\n%s\nwant:\n%s", invocation, got, g.stdout)
	}
	if code != g.code {
		t.Errorf("`%s` exited %d, want %d", invocation, code, g.code)
	}
}

func runGoldens(t *testing.T, cases []golden) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, c.run)
	}
}

// The help block is a run of separate lookups whose order, blank lines and
// two-column layout only exist as a whole, so it is pinned as one string. It
// goes through the dispatch rather than through printUsage directly because the
// exit code is half of what a bare `kamakiri` contracts, and because reaching
// the page at all is the part a call site can get wrong.
func TestNoCommandPrintsTheUsagePage(t *testing.T) {
	golden{stderr: usageBlock, code: 1}.run(t)
}

// The version fast path is the one command that answers before anything else is
// resolved, so it is the only golden whose output lands on stdout and exits 0.
// The value is the unstamped build's: a release build's comes from the linker,
// which no test build runs.
func TestVersionFastPathGoldens(t *testing.T) {
	const printed = "kamakiri (dev)\n"

	runGoldens(t, []golden{
		{name: "version", args: []string{"version"}, stdout: printed},
		{name: "--version", args: []string{"--version"}, stdout: printed},
		{name: "-v", args: []string{"-v"}, stdout: printed},
	})
}

func TestUnknownCommandGoldens(t *testing.T) {
	runGoldens(t, []golden{
		{
			name:   "top level",
			args:   []string{"bogus"},
			stderr: "Unknown command: bogus\n",
			code:   1,
		},
		{
			// --help is not a command. What someone who types it sees is the
			// unknown-command line, not the usage page, and that is what is
			// pinned here rather than what one might wish it printed.
			name:   "--help",
			args:   []string{"--help"},
			stderr: "Unknown command: --help\n",
			code:   1,
		},
		{
			// The one pair printed tail first: an unknown verb is the news, and
			// the surface it was measured against follows.
			name: "cdn verb",
			args: []string{"cdn", "bogus"},
			stderr: "Unknown cdn command: bogus\n" +
				"Usage: kamakiri cdn <cloudflare|webaccel|none|cleanup|verify|credentials|status|purge>\n",
			code: 1,
		},
		{
			name:   "domain verb",
			args:   []string{"domain", "bogus"},
			stderr: "Unknown domain command: bogus\n",
			code:   1,
		},
		{
			name:   "subdomain verb",
			args:   []string{"subdomain", "bogus"},
			stderr: "Unknown subdomain command: bogus\n",
			code:   1,
		},
		{
			name:   "key verb",
			args:   []string{"key", "bogus"},
			stderr: "Unknown key command: bogus\n",
			code:   1,
		},
		{
			// A verb that reads like one the arm takes, followed by an ID, still
			// reads as any unknown verb does rather than reaching a route.
			name:   "key delete",
			args:   []string{"key", "delete", "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c"},
			stderr: "Unknown key command: delete\n",
			code:   1,
		},
	})
}

// A mistyped secret can ride on a flag (--token=...), so every line that echoes
// an unparsable argument shows only the flag name when the value was attached
// with an equals sign. The cdn and domain arms read credentials before they
// parse, so these runs load a key first; each also asserts the redacted name
// is printed, so a case cannot pass by never reaching the echo.
func TestParseErrorsNeverEchoAFlagValue(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"domain add unknown flag", []string{"domain", "add", "example.com", "--colour=TOPSECRET"}, "Unknown flag: --colour\n"},
		{"cdn verb that is a flag", []string{"cdn", "--token=TOPSECRET", "webaccel"}, "Unknown cdn command: --token\n"},
		{"top level command that is a flag", []string{"--token=TOPSECRET"}, "Unknown command: --token\n"},
		{"domain verb that is a flag", []string{"domain", "--token=TOPSECRET"}, "Unknown domain command: --token\n"},
		{"subdomain verb that is a flag", []string{"subdomain", "--token=TOPSECRET"}, "Unknown subdomain command: --token\n"},
		{"key verb that is a flag", []string{"key", "--token=TOPSECRET"}, "Unknown key command: --token\n"},
		{"key list extra flag", []string{"key", "list", "--token=TOPSECRET"}, "Unexpected argument: --token\n"},
		{"key create unknown flag", []string{"key", "create", "--token=TOPSECRET"}, "Unexpected argument: --token\n"},
		{"key rotate unknown flag", []string{"key", "rotate", "--token=TOPSECRET"}, "Unexpected argument: --token\n"},
		{"unexpected argument", []string{"upgrade", "--token=TOPSECRET"}, "Unexpected argument: --token\n"},
		{"cdn webaccel unknown flag", []string{"cdn", "webaccel", "--secert=TOPSECRET"}, "--secert"},
		{"cdn credentials unknown flag", []string{"cdn", "credentials", "--secert=TOPSECRET"}, "--secert"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Setenv("KAMAKIRI_API_KEY", "kk_live_test")
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			var stdout, stderr bytes.Buffer
			code := run(c.args, strings.NewReader(""), &stdout, &stderr)

			if code != 1 {
				t.Errorf("`kamakiri %s` exited %d, want 1", strings.Join(c.args, " "), code)
			}
			got := stderr.String() + stdout.String()
			if !strings.Contains(got, c.want) {
				t.Errorf("output:\n%s\nwant it to contain %q", got, c.want)
			}
			if strings.Contains(got, "TOPSECRET") {
				t.Errorf("output echoes the flag value:\n%s", got)
			}
		})
	}
}

// A positional extra argument carries no secret, so it is still echoed whole.
func TestUnexpectedPositionalArgumentIsEchoedWhole(t *testing.T) {
	golden{
		args:   []string{"upgrade", "extra=thing"},
		stderr: "Usage: kamakiri upgrade\nUnexpected argument: extra=thing\n",
		code:   1,
	}.run(t)
}

// A command invoked without the argument it needs prints its own usage block
// and nothing else.
func TestMissingArgumentUsageGoldens(t *testing.T) {
	runGoldens(t, []golden{
		{
			name:   "deploy",
			args:   []string{"deploy"},
			stderr: "Usage: kamakiri deploy <path> [--no-wait]\n",
			code:   1,
		},
		{
			name:   "rollback",
			args:   []string{"rollback"},
			stderr: "Usage: kamakiri rollback <deploy_id> [--no-wait]\n",
			code:   1,
		},
		{
			name:   "domain",
			args:   []string{"domain"},
			stderr: "Usage: kamakiri domain <register|unregister|verify|set|unset|add|remove|list>\n",
			code:   1,
		},
		{
			name:   "domain register",
			args:   []string{"domain", "register"},
			stderr: usageDomainRegister,
			code:   1,
		},
		{
			name:   "domain unregister",
			args:   []string{"domain", "unregister"},
			stderr: usageDomainUnregister,
			code:   1,
		},
		{
			name:   "domain verify",
			args:   []string{"domain", "verify"},
			stderr: usageDomainVerify,
			code:   1,
		},
		{
			name:   "domain set",
			args:   []string{"domain", "set"},
			stderr: usageDomainSet,
			code:   1,
		},
		{
			name:   "domain add",
			args:   []string{"domain", "add"},
			stderr: usageDomainAdd,
			code:   1,
		},
		{
			name:   "domain remove",
			args:   []string{"domain", "remove"},
			stderr: usageDomainRemove,
			code:   1,
		},
		{
			name:   "subdomain",
			args:   []string{"subdomain"},
			stderr: "Usage: kamakiri subdomain <get|set|enable|disable> [subdomain]\n",
			code:   1,
		},
		{
			name:   "subdomain set",
			args:   []string{"subdomain", "set"},
			stderr: "Usage: kamakiri subdomain set <subdomain> [--no-wait]\n",
			code:   1,
		},
		{
			// A flag but no subdomain reaches the same block from the other
			// side of the flag loop, so both branches are pinned.
			name:   "subdomain set with only a flag",
			args:   []string{"subdomain", "set", "--no-wait"},
			stderr: "Usage: kamakiri subdomain set <subdomain> [--no-wait]\n",
			code:   1,
		},
		{
			name:   "key",
			args:   []string{"key"},
			stderr: "Usage: kamakiri key <list|create|rename|revoke|rotate>\n",
			code:   1,
		},
		{
			name:   "key rotate",
			args:   []string{"key", "rotate"},
			stderr: "Usage: kamakiri key rotate <id>\n",
			code:   1,
		},
		{
			name:   "key rename",
			args:   []string{"key", "rename"},
			stderr: "Usage: kamakiri key rename <id> <name>\n",
			code:   1,
		},
		{
			name:   "key rename without a name",
			args:   []string{"key", "rename", "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c"},
			stderr: "Usage: kamakiri key rename <id> <name>\n",
			code:   1,
		},
	})
}

// The usage-line-plus-tail shape: an argument the command has no place for
// prints the usage line and then names the argument, as two lookups the user
// reads as one block.
func TestUnexpectedArgumentGoldens(t *testing.T) {
	runGoldens(t, []golden{
		{
			name:   "status",
			args:   []string{"status", "x"},
			stderr: "Usage: kamakiri status [--verbose] [--recheck]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "init",
			args:   []string{"init", "x"},
			stderr: "Usage: kamakiri init [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "teardown",
			args:   []string{"teardown", "x"},
			stderr: "Usage: kamakiri teardown [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			// The path is taken as the positional, so the extra argument is the
			// third one; `deploy x` alone would be a deploy of ./x.
			name:   "deploy",
			args:   []string{"deploy", "./site", "x"},
			stderr: "Usage: kamakiri deploy <path> [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "rollback",
			args:   []string{"rollback", "dep_abc", "x"},
			stderr: "Usage: kamakiri rollback <deploy_id> [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "domain register",
			args:   []string{"domain", "register", "example.com", "--bogus"},
			stderr: "Usage: kamakiri domain register <domain> [--no-wait]\nUnexpected argument: --bogus\n",
			code:   1,
		},
		{
			name:   "domain verify",
			args:   []string{"domain", "verify", "example.com", "--bogus"},
			stderr: "Usage: kamakiri domain verify <domain> [--no-wait]\nUnexpected argument: --bogus\n",
			code:   1,
		},
		{
			name:   "domain set",
			args:   []string{"domain", "set", "example.com", "x"},
			stderr: "Usage: kamakiri domain set <domain> [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "domain unregister",
			args:   []string{"domain", "unregister", "example.com", "x"},
			stderr: "Usage: kamakiri domain unregister <domain>\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "domain remove",
			args:   []string{"domain", "remove", "example.com", "x"},
			stderr: "Usage: kamakiri domain remove <domain> [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			// The one site whose usage line is the whole multi-line block.
			name:   "domain unset",
			args:   []string{"domain", "unset", "x"},
			stderr: usageDomainUnset + "Unexpected argument: x\n",
			code:   1,
		},
		{
			name:   "subdomain enable",
			args:   []string{"subdomain", "enable", "x"},
			stderr: "Usage: kamakiri subdomain enable [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			// enable and disable share one parser, and the verb it is given is
			// the only thing that differs, so both are pinned.
			name:   "subdomain disable",
			args:   []string{"subdomain", "disable", "x"},
			stderr: "Usage: kamakiri subdomain disable [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			// A second positional, the one site of this shape that prints no
			// usage line above the tail.
			name:   "subdomain set second positional",
			args:   []string{"subdomain", "set", "example", "x"},
			stderr: "Unexpected argument: x\n",
			code:   1,
		},
		{
			// requireNoExtraCDNArgs panics with the exit sentinel, and run's
			// deferred recover turns it back into the exit code.
			name:   "cdn cloudflare",
			args:   []string{"cdn", "cloudflare", "--bogus"},
			stderr: usageCDNMode + "Unexpected argument: --bogus\n",
			code:   1,
		},
		{
			name:   "cdn none",
			args:   []string{"cdn", "none", "x"},
			stderr: usageCDNMode + "Unexpected argument: x\n",
			code:   1,
		},
		{
			// `cdn status` is an alias for the bare view, and its extra
			// arguments are rejected before the view is fetched, which is why
			// this one needs no server.
			name:   "cdn status",
			args:   []string{"cdn", "status", "x"},
			stderr: usageCDNMode + "Unexpected argument: x\n",
			code:   1,
		},
		{
			name:   "cdn verify",
			args:   []string{"cdn", "verify", "x"},
			stderr: "Usage: kamakiri cdn verify\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "cdn purge",
			args:   []string{"cdn", "purge", "x"},
			stderr: "Usage: kamakiri cdn purge [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "cdn cleanup",
			args:   []string{"cdn", "cleanup", "x"},
			stderr: "Usage: kamakiri cdn cleanup [--no-wait]\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name:   "key list",
			args:   []string{"key", "list", "extra"},
			stderr: "Usage: kamakiri key list\nUnexpected argument: extra\n",
			code:   1,
		},
		{
			name:   "key create positional",
			args:   []string{"key", "create", "example.com"},
			stderr: "Usage: kamakiri key create [--name <name>] [--site <site>]...\nUnexpected argument: example.com\n",
			code:   1,
		},
		{
			name:   "key create unknown flag",
			args:   []string{"key", "create", "--site", "example.com", "--yes"},
			stderr: "Usage: kamakiri key create [--name <name>] [--site <site>]...\nUnexpected argument: --yes\n",
			code:   1,
		},
		{
			name:   "key rename third positional",
			args:   []string{"key", "rename", "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c", "ci-box", "extra"},
			stderr: "Usage: kamakiri key rename <id> <name>\nUnexpected argument: extra\n",
			code:   1,
		},
		{
			// Refused before the email prompt, which is why stdout stays empty.
			name:   "login flag",
			args:   []string{"login", "--bogus"},
			stderr: "Usage: kamakiri login [--name <name>] [--revoke-others]\nUnexpected argument: --bogus\n",
			code:   1,
		},
		{
			name:   "login positional",
			args:   []string{"login", "foo"},
			stderr: "Usage: kamakiri login [--name <name>] [--revoke-others]\nUnexpected argument: foo\n",
			code:   1,
		},
		{
			name:   "logout positional",
			args:   []string{"logout", "extra"},
			stderr: "Usage: kamakiri logout\nUnexpected argument: extra\n",
			code:   1,
		},
		{
			name:   "logout flag with a value",
			args:   []string{"logout", "--token=TOPSECRET"},
			stderr: "Usage: kamakiri logout\nUnexpected argument: --token\n",
			code:   1,
		},
	})
}

// The flag-error surface: an unknown flag, a value attached to a boolean flag,
// a value-taking flag with nothing to take, and the two rules `domain add`
// enforces on --status. Each of these exits through a.exit.
func TestFlagErrorGoldens(t *testing.T) {
	runGoldens(t, []golden{
		{
			name:   "boolean flag given a value",
			args:   []string{"status", "--verbose=1"},
			stderr: "Error: --verbose takes no value\n",
			code:   1,
		},
		{
			name:   "unknown flag for domain add",
			args:   []string{"domain", "add", "example.com", "--bogus"},
			stderr: "Unknown flag: --bogus\n",
			code:   1,
		},
		{
			name:   "unknown flag for cdn webaccel",
			args:   []string{"cdn", "webaccel", "--bogus"},
			stderr: "unknown flag for cdn webaccel: --bogus\n",
			code:   1,
		},
		{
			name:   "--yes is unknown for cdn webaccel",
			args:   []string{"cdn", "webaccel", "--yes"},
			stderr: "unknown flag for cdn webaccel: --yes\n",
			code:   1,
		},
		{
			name:   "unknown flag for cdn credentials",
			args:   []string{"cdn", "credentials", "--bogus"},
			stderr: "unknown flag for cdn credentials: --bogus\n",
			code:   1,
		},
		{
			// The space form with nothing after it, and the equals form with
			// nothing after the sign, are separate errors and both carry the
			// hint naming the statuses that would be accepted.
			name:   "--status with no value",
			args:   []string{"domain", "add", "example.com", "--status"},
			stderr: "Error: --status requires a value (301, 302, 307, or 308)\n",
			code:   1,
		},
		{
			name:   "--status= with nothing after the sign",
			args:   []string{"domain", "add", "example.com", "--status="},
			stderr: "Error: --status= requires a value after the equals sign (301, 302, 307, or 308)\n",
			code:   1,
		},
		{
			name:   "--status outside the accepted set",
			args:   []string{"domain", "add", "example.com", "--status=999"},
			stderr: "Error: invalid status \"999\", must be 301, 302, 307, or 308\n",
			code:   1,
		},
		{
			name:   "--alias with --status",
			args:   []string{"domain", "add", "example.com", "--alias", "--status=302"},
			stderr: "Error: --alias and --status are mutually exclusive\n",
			code:   1,
		},
		{
			name:   "--status before --alias",
			args:   []string{"domain", "add", "example.com", "--status=302", "--alias"},
			stderr: "Error: --alias and --status are mutually exclusive\n",
			code:   1,
		},
		{
			name:   "--status matching the default before --alias",
			args:   []string{"domain", "add", "example.com", "--status=301", "--alias"},
			stderr: "Error: --alias and --status are mutually exclusive\n",
			code:   1,
		},
		{
			name:   "--alias with --redirect",
			args:   []string{"domain", "add", "example.com", "--alias", "--redirect"},
			stderr: "Error: --alias and --redirect are mutually exclusive\n",
			code:   1,
		},
		{
			name:   "--redirect before --alias",
			args:   []string{"domain", "add", "example.com", "--redirect", "--alias"},
			stderr: "Error: --alias and --redirect are mutually exclusive\n",
			code:   1,
		},
		{
			// No usage line: a valued flag with nothing to take is reported the
			// same way on every command.
			name:   "login --name with no value",
			args:   []string{"login", "--name"},
			stderr: "Error: --name requires a value\n",
			code:   1,
		},
		{
			// A forgotten value must not swallow the next flag: taken as the
			// name, --revoke-others would mint a key called "--revoke-others"
			// and revoke nothing.
			name:   "login --name followed by another flag",
			args:   []string{"login", "--name", "--revoke-others"},
			stderr: "Error: --name requires a value; write a value that starts with \"--\" as --name=<value>\n",
			code:   1,
		},
		{
			// The cdn parsers return the line for main to print, with no
			// "Error:" prefix, and the refused argument is never echoed.
			name:   "cdn credentials --token followed by another flag",
			args:   []string{"cdn", "credentials", "--token", "--secret", "x"},
			stderr: "--token requires a value; write a value that starts with \"--\" as --token=<value>\n",
			code:   1,
		},
		{
			name:   "login --name= with nothing after the sign",
			args:   []string{"login", "--name="},
			stderr: "Error: --name= requires a value after the equals sign\n",
			code:   1,
		},
		{
			name:   "key create --name with no value",
			args:   []string{"key", "create", "--site", "example.com", "--name"},
			stderr: "Error: --name requires a value\n",
			code:   1,
		},
		{
			// A forgotten name must not swallow the flag after it: taken as the
			// name, --site would mint a key called "--site" bound to the linked
			// site rather than the one typed.
			name:   "key create --name followed by another flag",
			args:   []string{"key", "create", "--name", "--site", "example.com"},
			stderr: "Error: --name requires a value; write a value that starts with \"--\" as --name=<value>\n",
			code:   1,
		},
		{
			name:   "key create --site with no value",
			args:   []string{"key", "create", "--site"},
			stderr: "Error: --site requires a value\n",
			code:   1,
		},
		{
			name:   "key create --site followed by another flag",
			args:   []string{"key", "create", "--site", "--name", "ci"},
			stderr: "Error: --site requires a value; write a value that starts with \"--\" as --site=<value>\n",
			code:   1,
		},
		{
			name:   "key create --site= with nothing after the sign",
			args:   []string{"key", "create", "--site="},
			stderr: "Error: --site= requires a value after the equals sign\n",
			code:   1,
		},
		{
			name:   "login --revoke-others with a value",
			args:   []string{"login", "--revoke-others=x"},
			stderr: "Error: --revoke-others takes no value\n",
			code:   1,
		},
	})
}

// `upgrade` builds no API client either, and the two branches reachable from
// here are the ones that answer before any request goes out. A golden binary is
// unstamped, so a bare `upgrade` always stops at the guard against upgrading a
// build that came from no release, which is also what keeps these goldens off
// the network. The empty stdout field is the pin on that: the guard's copy
// travels in the returned error, and the phases ahead of it print nothing.
func TestUpgradeCommandGoldens(t *testing.T) {
	runGoldens(t, []golden{
		{
			name:   "unexpected argument",
			args:   []string{"upgrade", "x"},
			stderr: "Usage: kamakiri upgrade\nUnexpected argument: x\n",
			code:   1,
		},
		{
			name: "a build that came from no release",
			args: []string{"upgrade"},
			stderr: "this build did not come from a release, so it cannot update itself. " +
				"Install a release from https://github.com/kamakiri-labs/kamakiri/releases\n",
			code: 1,
		},
	})
}

// `language` is the one command besides `version` that answers without an API
// call, so its dispatch is pinned end to end. The bare form reports the
// provenance as `default` because the goldens clear every variable the
// resolution consults and give it an empty config home.
func TestLanguageCommandGoldens(t *testing.T) {
	runGoldens(t, []golden{
		{
			name: "bare",
			args: []string{"language"},
			stdout: "Language: English (default)\n" +
				"Change it with `kamakiri language en` or `kamakiri language ja`.\n",
		},
		{
			name:   "unsupported language",
			args:   []string{"language", "bogus"},
			stderr: "unsupported language \"bogus\"; use `en` or `ja`\n",
			code:   1,
		},
		{
			name:   "too many arguments",
			args:   []string{"language", "en", "ja"},
			stderr: "usage: kamakiri language [en|ja]\n",
			code:   1,
		},
	})
}

// forceNudgeTerminal makes the update nudge believe it has a terminal to write
// to for the length of one test. Every other test in this package drives the
// dispatch with buffers, which is not a terminal, so they stay silent without
// arranging anything.
func forceNudgeTerminal(t *testing.T) {
	t.Helper()

	prev := nudgeTerminal
	t.Cleanup(func() { nudgeTerminal = prev })
	nudgeTerminal = func(io.Writer) bool { return true }
}

// stampVersion gives the process a released build's version for the length of
// one test. Without it the nudge would take the `(dev)` branch and prove
// nothing.
func stampVersion(t *testing.T, v string) {
	t.Helper()

	prev := version
	t.Cleanup(func() { version = prev })
	version = v
}

// advertisingAPI stands in for the API, answering the registration listing with
// the given status and naming a newer release in the header the nudge reads.
func advertisingAPI(t *testing.T, status int, body string) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Kamakiri-Cli-Latest-Version", "0.2.0")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// The nudge rides an ordinary command's own API traffic, so it is pinned where
// it actually happens: through the dispatch, against a server that names a
// release. This is what holds the exit-code gate and both arguments the call
// site passes, neither of which any test of the nudge alone can reach.
//
// Each subtest gets a config home of its own. Sharing one would let the first
// run's saved state throttle the second, and the second assertion would then
// hold for the wrong reason.
func TestUpdateNudgeRidesTheDispatch(t *testing.T) {
	const nudge = "kamakiri v0.2.0 is available; you are on v0.1.1. Run `kamakiri upgrade` to update.\n"

	t.Run("after a command that succeeded", func(t *testing.T) {
		pinDispatchEnvironment(t)
		// The command needs a credential to reach the server at all, and the
		// variable is the cheapest way to give it one.
		t.Setenv("KAMAKIRI_API_KEY", "kk_live_test")
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		forceNudgeTerminal(t)
		stampVersion(t, "v0.1.1")
		t.Setenv("KAMAKIRI_API_URL", advertisingAPI(t, 200, `{"registrations":[]}`))

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		if code != 0 {
			t.Fatalf("`kamakiri domain list` exited %d, want 0", code)
		}
		if got, want := stdout.String(), "No domains registered. Run `kamakiri domain register <domain>` to register one.\n"; got != want {
			t.Errorf("stdout:\n%q\nwant:\n%q", got, want)
		}
		if got := stderr.String(); got != nudge {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, nudge)
		}
	})

	t.Run("not after a command that failed", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Setenv("KAMAKIRI_API_KEY", "kk_live_test")
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		forceNudgeTerminal(t)
		stampVersion(t, "v0.1.1")
		t.Setenv("KAMAKIRI_API_URL", advertisingAPI(t, 503, "<html>503 Service Unavailable</html>"))

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri domain list` exited %d, want 1", code)
		}
		if got, want := stderr.String(), "server returned 503\n"; got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
	})

	// The same run that nudges above, with the seam left alone. It is the only
	// difference between the two, so it is the only thing this can be reporting.
	t.Run("not when stderr is no terminal", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Setenv("KAMAKIRI_API_KEY", "kk_live_test")
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		stampVersion(t, "v0.1.1")
		t.Setenv("KAMAKIRI_API_URL", advertisingAPI(t, 200, `{"registrations":[]}`))

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		if code != 0 {
			t.Fatalf("`kamakiri domain list` exited %d, want 0", code)
		}
		if got := stderr.String(); got != "" {
			t.Errorf("stderr:\n%q\nwant nothing", got)
		}
	})
}

// A trailing slash on KAMAKIRI_API_URL is ignored: the request path is the
// one the bare URL builds, never one with a doubled slash. The empty-value
// fallback to the default URL cannot be asserted here without reaching the
// real API.
func TestAPIURLTrailingSlashesAreIgnored(t *testing.T) {
	for _, tc := range []struct{ name, suffix string }{
		{"one trailing slash", "/"},
		{"several trailing slashes", "///"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)

			var path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				io.WriteString(w, `{"registrations":[]}`)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_KEY", "kk_live_slash")
			t.Setenv("KAMAKIRI_API_URL", server.URL+tc.suffix)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			var stdout, stderr bytes.Buffer
			code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

			// Close orders the handler's write of path before the read below.
			server.Close()

			if code != 0 {
				t.Fatalf("`kamakiri domain list` exited %d, want 0; stderr: %q", code, stderr.String())
			}
			if got, want := path, "/v1/pages/registrations"; got != want {
				t.Errorf("request path = %q, want %q", got, want)
			}
		})
	}
}

// The variable is read in one place, deep under every command, so what proves
// it reaches the wire is a real dispatch: an ordinary command, no credentials
// file anywhere, and a server that records what arrived. `domain list` is the
// command that needs no linked project, `deploys` the one whose refusal crosses
// the credential check, and `login` the one command that builds a client
// without loading a credential.
//
// A dispatch that loads credentials leaves the package-level stamp in `api`
// behind it, and nothing else in the suite resets it. Today every dispatch that
// can render the unauthorized copy re-stamps first, so a stale value decides
// nothing; the subtests that stamp it true restore it anyway, so a render site
// added later without that re-stamp cannot inherit a value from another test.
func TestAPIKeyFromEnvironmentRidesTheDispatch(t *testing.T) {
	t.Run("the bearer is the key the variable holds", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

		var authorization string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization = r.Header.Get("Authorization")
			io.WriteString(w, `{"registrations":[]}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		// The handler records from the server's own goroutine, and Close
		// returns only once that goroutine has finished, which is what orders
		// its write against the read below.
		server.Close()

		if code != 0 {
			t.Fatalf("`kamakiri domain list` exited %d, want 0; stderr: %q", code, stderr.String())
		}
		if got, want := authorization, "Bearer kk_live_fromenv"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
	})

	// The stamp the credential load leaves is what the copy reads, so the pin
	// has to be a real dispatch too. The assertion holds only the half that
	// names the variable: the `invalid API key` prefix both arms share is what
	// every other assertion on this copy matches, so nothing else would notice
	// the stamp going missing.
	t.Run("a rejected key from the variable says which variable", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri domain list` exited %d, want 1", code)
		}
		got := stderr.String()
		if !strings.Contains(got, "The key came from KAMAKIRI_API_KEY; check the value that variable holds") {
			t.Errorf("stderr:\n%q\nwant the copy naming the variable", got)
		}
		if strings.Contains(got, "kamakiri login") {
			t.Errorf("stderr:\n%q\nwant no mention of login", got)
		}
	})

	// The stamp's other arm. A stamp that recorded any credential at all rather
	// than an environment one would send a file-credentialed user to look at a
	// variable they never set, and nothing else in the suite would notice: every
	// other assertion on this copy matches the shared `invalid API key` prefix.
	t.Run("a rejected key from the file says to log in again", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

		if err := core.SaveCredentials("kk_live_fromfile", "user@example.com"); err != nil {
			t.Fatalf("SaveCredentials: %v", err)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri domain list` exited %d, want 1", code)
		}
		got := stderr.String()
		if !strings.Contains(got, `Run "kamakiri login" to re-authenticate`) {
			t.Errorf("stderr:\n%q\nwant the copy pointing at login", got)
		}
		if strings.Contains(got, "KAMAKIRI_API_KEY") {
			t.Errorf("stderr:\n%q\nwant no mention of the variable", got)
		}
	})

	// The `domain list` cases above reach the copy through api.MapError, while
	// `init` and `teardown` build it themselves, so only a real dispatch through
	// one of them proves that a direct render site consults the stamp.
	t.Run("a rejected key on a direct render site says which variable", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		// The project file is written relative to the working directory, so the
		// temp directory is what keeps it out of the developer's tree.
		t.Chdir(t.TempDir())

		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"teardown"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri teardown` exited %d, want 1", code)
		}
		got := stderr.String()
		if !strings.Contains(got, "The key came from KAMAKIRI_API_KEY; check the value that variable holds") {
			t.Errorf("stderr:\n%q\nwant the copy naming the variable", got)
		}
		if strings.Contains(got, "kamakiri login") {
			t.Errorf("stderr:\n%q\nwant no mention of login", got)
		}
	})

	// `login` mints a key rather than using one, so it is the one command that
	// builds its client without setAPIKey and must send no bearer even while the
	// variable holds a key. Nothing short of a real dispatch reaches that: the
	// login package drives a client of function fields, where no request exists
	// to inspect.
	t.Run("login sends no bearer under the variable", func(t *testing.T) {
		pinDispatchEnvironment(t)

		var registerAuth, verifyAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/auth/register":
				registerAuth = r.Header.Get("Authorization")
				io.WriteString(w, `{"message":"Confirmation code sent."}`)
			case "/v1/auth/verify":
				verifyAuth = r.Header.Get("Authorization")
				io.WriteString(w, `{"api_key":"kk_live_minted"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"login"}, strings.NewReader("user@example.com\nY\nABC123\n"), &stdout, &stderr)

		// The handler records from the server's own goroutines, and Close returns
		// only once those have finished, which is what orders both writes against
		// the reads below.
		server.Close()

		if code != 0 {
			t.Fatalf("`kamakiri login` exited %d, want 0; stderr: %q", code, stderr.String())
		}
		if registerAuth != "" || verifyAuth != "" {
			t.Errorf("login sent an Authorization header (register %q, verify %q), want none on either", registerAuth, verifyAuth)
		}
		if got := stdout.String(); !strings.Contains(got, "KAMAKIRI_API_KEY is set") {
			t.Errorf("stdout:\n%q\nwant the note saying the variable still wins", got)
		}
	})

	// The whole line, rather than the bare `not logged in` prefix every
	// neighbouring assertion matches on: this is the only test that holds the
	// wording, and the blank value carries the trim rule end to end.
	t.Run("a blank value is no credential", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Setenv("KAMAKIRI_API_KEY", " ")

		var stdout, stderr bytes.Buffer
		code := run([]string{"deploys"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri deploys` exited %d, want 1", code)
		}
		want := "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
	})
}

// The flag reaches the verify body in both forms, and when it is repeated the
// last value is the one sent, as with every other valued flag. An empty value is
// refused by the name check, after parsing and before any request: the flag
// was given, so it is not read as a request for the default name. A separated
// value starting with "--" is refused as a missing value, so `--name
// --revoke-others` sends nothing rather than naming a key after the flag.
func TestLoginNameRidesTheDispatch(t *testing.T) {
	cases := []struct {
		name string
		args []string
		sent string
	}{
		{"space form", []string{"login", "--name", "ci-box"}, "ci-box"},
		{"equals form", []string{"login", "--name=ci-box"}, "ci-box"},
		{"repeated", []string{"login", "--name", "first", "--name=ci-box"}, "ci-box"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)

			var verifyName any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/auth/register":
					io.WriteString(w, `{"message":"Confirmation code sent."}`)
				case "/v1/auth/verify":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decoding the verify body: %v", err)
					}
					verifyName = body["name"]
					io.WriteString(w, `{"api_key":"kk_live_minted","key":{"id":"0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59","name":"ci-box"}}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader("user@example.com\nY\nABC123\n"), &stdout, &stderr)

			// Close waits for the handler's goroutines, which orders the write to
			// verifyName against the read below.
			server.Close()

			if code != 0 {
				t.Fatalf("`kamakiri %s` exited %d, want 0; stderr: %q", strings.Join(tc.args, " "), code, stderr.String())
			}
			if verifyName != tc.sent {
				t.Errorf("verify name = %#v, want %q", verifyName, tc.sent)
			}
			if got := stdout.String(); !strings.Contains(got, "\nKey name: ci-box\n") {
				t.Errorf("stdout:\n%q\nwant the key name line", got)
			}
		})
	}

	t.Run("empty value", func(t *testing.T) {
		pinDispatchEnvironment(t)

		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"login", "--name", ""}, strings.NewReader("user@example.com\nY\nABC123\n"), &stdout, &stderr)
		server.Close()

		if code != 1 {
			t.Errorf("`kamakiri login --name \"\"` exited %d, want 1", code)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("sent %d requests, want none", n)
		}
		if got, want := stdout.String(), "Error: --name must be 1 to 64 characters with no control characters.\n"; got != want {
			t.Errorf("stdout:\n%q\nwant:\n%q", got, want)
		}
		if got, want := stderr.String(), "invalid key name\n"; got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("followed by another flag", func(t *testing.T) {
		pinDispatchEnvironment(t)

		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"login", "--name", "--revoke-others"}, strings.NewReader("user@example.com\nY\nABC123\n"), &stdout, &stderr)
		server.Close()

		if code != 1 {
			t.Errorf("`kamakiri login --name --revoke-others` exited %d, want 1", code)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("sent %d requests, want none", n)
		}
		if got := stdout.String(); got != "" {
			t.Errorf("stdout:\n%q\nwant nothing", got)
		}
		if got, want := stderr.String(), "Error: --name requires a value; write a value that starts with \"--\" as --name=<value>\n"; got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
	})
}

// The flag reaches the verify body as a JSON true and the run lists what the
// server revoked; without it the body carries no such field. A value on the
// flag is refused before anything is sent, since a revoke the user did not
// spell plainly must not go out.
func TestLoginRevokeOthersRidesTheDispatch(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		present bool
	}{
		{"given", []string{"login", "--revoke-others"}, true},
		{"after --name", []string{"login", "--name", "ci-box", "--revoke-others"}, true},
		{"absent", []string{"login"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)

			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/auth/register":
					io.WriteString(w, `{"message":"Confirmation code sent."}`)
				case "/v1/auth/verify":
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decoding the verify body: %v", err)
					}
					io.WriteString(w, `{"api_key":"kk_live_minted","key":{"id":"0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59","name":"ci-box"},`+
						`"revoked":[{"id":"5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65","name":"old-laptop"}]}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader("user@example.com\nY\nABC123\n"), &stdout, &stderr)

			// Close waits for the handler's goroutines, which orders the write to
			// body against the read below.
			server.Close()

			if code != 0 {
				t.Fatalf("`kamakiri %s` exited %d, want 0; stderr: %q", strings.Join(tc.args, " "), code, stderr.String())
			}
			value, present := body["revoke_others"]
			if present != tc.present || (present && value != true) {
				t.Errorf("verify body %v: revoke_others = %#v (present %t), want present %t and true", body, value, present, tc.present)
			}
			block := "\nRevoked 1 other key:\n  old-laptop (5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65)\n"
			if got := stdout.String(); strings.Contains(got, block) != tc.present {
				t.Errorf("stdout:\n%q\nwant the revoked block %t", got, tc.present)
			}
		})
	}

	t.Run("with a value", func(t *testing.T) {
		pinDispatchEnvironment(t)

		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"login", "--revoke-others=false"}, strings.NewReader("user@example.com\nY\nABC123\n"), &stdout, &stderr)
		server.Close()

		if code != 1 {
			t.Errorf("`kamakiri login --revoke-others=false` exited %d, want 1", code)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("sent %d requests, want none", n)
		}
		if got := stdout.String(); got != "" {
			t.Errorf("stdout:\n%q\nwant nothing", got)
		}
		if got, want := stderr.String(), "Error: --revoke-others takes no value\n"; got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
	})
}

// A credentials file that holds no key refuses every command that needs a
// credential, with the login advice, rather than sending a request the server
// could only reject. `status` is the exception that reports the same file on
// its page instead. `deploys` is the command driven because its refusal
// crosses the credential check with no other check ahead of it, so the line it
// prints can only have come from that check: the keyless file leaves the load
// with no error and the client's key empty, and the credential check is what
// refuses.
// No project is linked, so nothing is sent whichever way the check goes.
func TestAKeylessCredentialsFileRefusesAsNotLoggedIn(t *testing.T) {
	pinDispatchEnvironment(t)
	t.Chdir(t.TempDir())

	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"api_key":"   "}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"deploys"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("`kamakiri deploys` exited %d, want 1", code)
	}
	want := "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
	}
}

// A project file that cannot be parsed is the load error only when nothing
// ranks ahead of it: the dispatch prints the credential failure, or a held
// key-description failure, in its place, as every command needing both does.
func TestStatusRanksAMalformedProjectFileBehindTheKeyThroughTheDispatch(t *testing.T) {
	writeMalformedProject := func(t *testing.T) {
		t.Helper()
		path := core.ProjectPath()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	t.Run("no credential", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())
		writeMalformedProject(t)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		want := "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		if got := stdout.String(); strings.Contains(got, "parse project config") {
			t.Errorf("stdout:\n%q\nwant no project load error on the page", got)
		}
	})

	t.Run("a held key-description error", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())
		writeMalformedProject(t)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/keys/current" {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"code":"internal_error","message":"Internal server error."}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"not_found","message":"not found"}`)
		}))
		t.Cleanup(server.Close)
		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		if got, want := stderr.String(), "Internal server error.\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

// statusKeyID is the key the local servers below describe as the one in use.
const statusKeyID = "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c"

// statusNoSiteLinked is the line the English `status` page ends with in a
// directory that links no site.
const statusNoSiteLinked = "No site is linked to this directory. Run \"kamakiri init\" to create one.\n"

// statusKey is the answer to GET /v1/keys/current those servers give. With a
// key in hand, `status` describes it before anything else, so every server a
// dispatched `status` reaches with a key has to answer it.
func statusKey() any {
	return map[string]api.Key{"key": {ID: statusKeyID, Name: "github-ci", Kind: "login", Current: true}}
}

// currentKeyAPI stands in for the API with only the route that describes the
// key in use, for a `status` run that links no site and so reads nothing else.
func currentKeyAPI(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/keys/current" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"not_found","message":"not found"}`)
			return
		}
		payload, err := json.Marshal(statusKey())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(payload)
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// An unreadable credentials file is a finding `status` reports rather than a
// reason to refuse: it renders the whole page with the failure on its own line,
// prints the same failure on stderr after it, and exits 1, so a check has the
// page, the reason and a code.
func TestStatusReportsAnUnreadableCredentialsFile(t *testing.T) {
	t.Run("the page carries the error and the exit is 1", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		// No project is linked here, so the page ends with the sentence saying
		// so, before any request, and needs no server.
		t.Chdir(t.TempDir())
		dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("not json"), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		if line := stderr.String(); !strings.HasPrefix(line, "parse credentials:") ||
			strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("stderr:\n%q\nwant one line starting with %q", line, "parse credentials:")
		}
		// The Credentials line names a throwaway config home, so the two lines
		// that carry no path are what is pinned.
		got := stdout.String()
		if !strings.Contains(got, "Logged in:    (error: parse credentials:") {
			t.Errorf("stdout:\n%q\nwant the logged-in line carrying the load error", got)
		}
		if !strings.HasSuffix(got, statusNoSiteLinked) {
			t.Errorf("stdout:\n%q\nwant the page rendered to its last line, %q", got, statusNoSiteLinked)
		}
	})

	// The variable outranks the file and the file is then left unread, so
	// nothing it holds reaches the page or the exit code. The Credentials line
	// pins the precedence, since the page asks APIKeyFromEnvironment directly,
	// and the exit 0 over a file that cannot be parsed pins that the file
	// decides nothing. The variable is a key in hand, so the page describes it,
	// and the local server is what that request reaches.
	t.Run("the variable outranks the file for the exit code too", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())
		dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("not json"), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", currentKeyAPI(t))

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 0 {
			t.Fatalf("`kamakiri status` exited %d, want 0; stderr: %q", code, stderr.String())
		}
		if got := stderr.String(); got != "" {
			t.Errorf("stderr:\n%q\nwant nothing", got)
		}
		got := stdout.String()
		for _, want := range []string{
			"Credentials:  KAMAKIRI_API_KEY (environment)",
			"Logged in:    yes",
			"Key:          github-ci (login, " + statusKeyID + ")",
			statusNoSiteLinked,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("stdout:\n%q\nwant a line %q", got, want)
			}
		}
	})

	// With a site linked and no key, the page ends at the API server row and
	// nothing is sent: a keyless lookup could only come back unauthorized.
	t.Run("a linked site is not looked up without a key", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}

		dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("not json"), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		if line := stderr.String(); !strings.HasPrefix(line, "parse credentials:") ||
			strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("stderr:\n%q\nwant one line starting with %q", line, "parse credentials:")
		}
		got := stdout.String()
		if !strings.Contains(got, "Logged in:    (error: parse credentials:") {
			t.Errorf("stdout:\n%q\nwant the logged-in line carrying the load error", got)
		}
		if !strings.HasSuffix(got, "API server:   "+server.URL+"\n") {
			t.Errorf("stdout:\n%q\nwant the page ending at the API server row", got)
		}
		if requests != 0 {
			t.Error("a request went out, but a failed load leaves no key to send")
		}
	})
}

// TestSetAPIKeyStillRefusesOnAFailedLoad pins that the wrapper every command
// that authenticates relies on still exits on a failed load; status is the one
// command that renders the failure instead. `domain register` is the command
// driven because the wrapper's line and the verb's own are told apart by their
// shape: the wrapper prints the load error bare, while anything the verb returns
// is printed behind the `Error:` prefix, so a wrapper that stopped exiting shows
// up as a prefixed line. The address it would dial has nothing listening, so
// nothing this test does can reach a real host.
func TestSetAPIKeyStillRefusesOnAFailedLoad(t *testing.T) {
	pinDispatchEnvironment(t)
	t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
	t.Chdir(t.TempDir())
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("not json"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("KAMAKIRI_API_URL", "http://127.0.0.1:1")

	var stdout, stderr bytes.Buffer
	code := run([]string{"domain", "register", "example.com"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("`kamakiri domain register` exited %d, want 1", code)
	}
	if got := stdout.String(); got != "" {
		t.Errorf("stdout:\n%q\nwant nothing", got)
	}
	if line := stderr.String(); !strings.HasPrefix(line, "parse credentials:") ||
		strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("stderr:\n%q\nwant one line starting with %q", line, "parse credentials:")
	}
}

// TestStatusExitsWhenItCannotVouch pins the exit code as a verdict: the page
// renders as far as it got, and status exits 1 with one line on stderr whenever
// it could not vouch for this machine or the linked site.
func TestStatusExitsWhenItCannotVouch(t *testing.T) {
	t.Run("not logged in on a fresh machine", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		got := stdout.String()
		for _, want := range []string{"Logged in:    no", statusNoSiteLinked} {
			if !strings.Contains(got, want) {
				t.Errorf("stdout:\n%q\nwant a line %q", got, want)
			}
		}
		want := "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key\n"
		if line := stderr.String(); line != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", line, want)
		}
	})

	// A file that is there but holds no key is reported as present and not
	// logged in, and nothing is sent over it: a keyless lookup could only come
	// back unauthorized, and its copy would blame a key that was never sent.
	t.Run("a credentials file holding no key is not logged in and sends nothing", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}

		dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"api_key":""}`), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		got := stdout.String()
		if !strings.Contains(got, "Logged in:    no") {
			t.Errorf("stdout:\n%q\nwant a line %q", got, "Logged in:    no")
		}
		if !strings.HasSuffix(got, "API server:   "+server.URL+"\n") {
			t.Errorf("stdout:\n%q\nwant the page ending at the API server row", got)
		}
		if strings.Contains(got, "(not found)") {
			t.Errorf("stdout:\n%q\nwant the credentials line naming the file that is there", got)
		}
		want := "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key\n"
		if line := stderr.String(); line != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", line, want)
		}
		if requests != 0 {
			t.Error("a request went out, but a file holding no key leaves no key to send")
		}
	})

	t.Run("a rejected key", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		if got := stdout.String(); !strings.HasSuffix(got, "API server:   "+server.URL+"\n") {
			t.Errorf("stdout:\n%q\nwant the page ending at the API server row", got)
		}
		if got := stderr.String(); !strings.Contains(got, "The key came from KAMAKIRI_API_KEY; check the value that variable holds") {
			t.Errorf("stderr:\n%q\nwant the copy naming the variable", got)
		}
	})

	// The page's own unit tests hold every branch of a rejected key; what only
	// the dispatch can show is that the error the page returns knows where the
	// key came from, since the arm is what stamps the source. No site is linked,
	// so the key's description is the one request the run makes, and a rejected
	// key still fails the run.
	t.Run("a rejected key from the variable with no linked site", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		wantRows := "Credentials:  KAMAKIRI_API_KEY (environment)\n" +
			"Logged in:    no\n" +
			"Key:          rejected\n"
		if got := stdout.String(); !strings.Contains(got, wantRows) || !strings.HasSuffix(got, statusNoSiteLinked) {
			t.Errorf("stdout:\n%q\nwant the rows:\n%q\nand the page ending with %q", got, wantRows, statusNoSiteLinked)
		}
		want := "invalid API key. The key came from KAMAKIRI_API_KEY; check the value that variable holds\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
		if len(paths) != 1 || paths[0] != "/v1/keys/current" {
			t.Errorf("requests went to %q, want the one describing the key", paths)
		}
	})

	// A 200 that names no key describes nothing, so the Key row carries the
	// error rather than an empty description, and the run cannot vouch for the
	// key.
	t.Run("an answer that names no key with no linked site", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			io.WriteString(w, `{}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)
		server.Close()

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		wantRows := "Credentials:  KAMAKIRI_API_KEY (environment)\n" +
			"Logged in:    yes\n" +
			"Key:          (error: parse response: the response names no key)\n"
		if got := stdout.String(); !strings.Contains(got, wantRows) || !strings.HasSuffix(got, statusNoSiteLinked) {
			t.Errorf("stdout:\n%q\nwant the rows:\n%q\nand the page ending with %q", got, wantRows, statusNoSiteLinked)
		}
		want := "parse response: the response names no key\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
		if len(paths) != 1 || paths[0] != "/v1/keys/current" {
			t.Errorf("requests went to %q, want the one describing the key", paths)
		}
	})

	// Nothing listens on port 1, so the request fails in the transport and the
	// tail of the line is the platform's own dial error, which is why only the
	// catalog prefix is matched.
	t.Run("an unreachable API", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())

		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", "http://127.0.0.1:1")

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		if got := stdout.String(); !strings.HasSuffix(got, "API server:   http://127.0.0.1:1\n") {
			t.Errorf("stdout:\n%q\nwant the page ending at the API server row", got)
		}
		if line := stderr.String(); !strings.HasPrefix(line, "request failed:") ||
			strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("stderr:\n%q\nwant one line starting with %q", line, "request failed:")
		}
	})

	// A project file that is there but does not parse is reported as the load
	// error, the very line `deploy` prints over the same file, and only the key's
	// description is asked for: no site is read over a project the run could
	// not load.
	t.Run("a project file that does not parse", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())
		if err := os.MkdirAll(".kamakiri", 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(core.ProjectPath(), []byte("not json"), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path != "/v1/keys/current" {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"code":"not_found","message":"not found"}`)
				return
			}
			payload, err := json.Marshal(statusKey())
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write(payload)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		line := stderr.String()
		if !strings.HasPrefix(line, "parse project config: ") ||
			strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("stderr:\n%q\nwant one line starting with %q", line, "parse project config: ")
		}
		if got := stdout.String(); !strings.HasSuffix(got, "API server:   "+server.URL+"\n") {
			t.Errorf("stdout:\n%q\nwant the page ending at the API server row", got)
		}
		if len(paths) != 1 || paths[0] != "/v1/keys/current" {
			t.Errorf("requests went to %q, want the one describing the key", paths)
		}

		var deployOut, deployErr bytes.Buffer
		if code := run([]string{"deploy", "."}, strings.NewReader(""), &deployOut, &deployErr); code != 1 {
			t.Fatalf("`kamakiri deploy .` exited %d, want 1", code)
		}
		if got := deployErr.String(); got != line {
			t.Errorf("`kamakiri deploy .` printed %q over the same file, want the line status printed, %q", got, line)
		}
	})
}

// A directory with no linked site is a finding the page states, not a reason
// to fail: the page ends with the sentence naming `kamakiri init`, stderr stays
// empty and the exit is 0.
func TestStatusWithNoLinkedSiteExitsZero(t *testing.T) {
	pinDispatchEnvironment(t)
	t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
	t.Chdir(t.TempDir())

	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
	url := currentKeyAPI(t)
	t.Setenv("KAMAKIRI_API_URL", url)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("`kamakiri status` exited %d, want 0; stderr: %q", code, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Errorf("stderr:\n%q\nwant nothing", got)
	}
	want := "API server:   " + url + "\n" + statusNoSiteLinked
	if got := stdout.String(); !strings.HasSuffix(got, want) {
		t.Errorf("stdout:\n%q\nwant it to end with:\n%q", got, want)
	}
}

// Drift is the one exit 1 the page reaches with no error in hand, so the arm
// prints its line from a key of its own. The fixture is marshalled from the
// wire types rather than spelled out as JSON so it cannot drift from them.
func TestStatusDriftExitsWithALine(t *testing.T) {
	pinDispatchEnvironment(t)
	t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
	t.Chdir(t.TempDir())

	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/pages/sites/"):
			body = api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true}
		case r.URL.Path == "/v1/pages/domains":
			body = api.DomainList{Domains: []api.Domain{
				{
					Domain: "example.com",
					Role:   "canonical",
					State:  "serving",
					// The server-derived verdict says the customer publishes
					// direct apex A records, so the drift check applies.
					DnsMatchedAlternative: "a",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "example.com", Type: "alias_or_aname", Value: "x.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "173.245.48.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "103.21.244.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{
							Name: "example.com", Type: "a",
							// The old cluster IPs, against the post-flip anycast
							// set expected above.
							Values:           []string{"198.51.100.10", "198.51.100.11"},
							AlternativeGroup: "apex_primary",
						},
					},
				},
			}}
		case r.URL.Path == "/v1/pages/cdn/status":
			body = api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}
		case r.URL.Path == "/v1/keys/current":
			body = statusKey()
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"not_found","message":"not found"}`)
			return
		}
		payload, err := json.Marshal(body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(payload)
	}))
	t.Cleanup(server.Close)

	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
	t.Setenv("KAMAKIRI_API_URL", server.URL)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("`kamakiri status` exited %d, want 1; stderr: %q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "✗ A records do not match the current edge for cloudflare mode") {
		t.Errorf("stdout:\n%q\nwant the drift block", got)
	}
	want := "A records do not match the current edge. The kamakiri status page lists the expected and observed values.\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestStatusKeyErrorAndDriftBothReachStderr pins that a held key error does not
// hide the drift line: the page found drift and could not describe the key, so
// a script log gets the error first and then the line saying the page has a
// drift block.
func TestStatusKeyErrorAndDriftBothReachStderr(t *testing.T) {
	pinDispatchEnvironment(t)
	t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
	t.Chdir(t.TempDir())

	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/pages/sites/"):
			body = api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true}
		case r.URL.Path == "/v1/pages/domains":
			body = api.DomainList{Domains: []api.Domain{
				{
					Domain: "example.com",
					Role:   "canonical",
					State:  "serving",
					// The server-derived verdict says the customer publishes
					// direct apex A records, so the drift check applies.
					DnsMatchedAlternative: "a",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "example.com", Type: "alias_or_aname", Value: "x.kamakiri-pages.site.", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "173.245.48.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
						{Name: "example.com", Type: "a", Value: "103.21.244.0", Purpose: "primary", Required: true, AlternativeGroup: "apex_primary"},
					},
					DNSRecordsObserved: []api.DNSRecordObserved{
						{
							Name: "example.com", Type: "a",
							// The old cluster IPs, against the post-flip anycast
							// set expected above.
							Values:           []string{"198.51.100.10", "198.51.100.11"},
							AlternativeGroup: "apex_primary",
						},
					},
				},
			}}
		case r.URL.Path == "/v1/pages/cdn/status":
			body = api.CDNStatusResponse{CDNMode: "cloudflare", Provider: "cloudflare"}
		case r.URL.Path == "/v1/keys/current":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"code":"internal_error","message":"Internal server error."}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"not_found","message":"not found"}`)
			return
		}
		payload, err := json.Marshal(body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(payload)
	}))
	t.Cleanup(server.Close)

	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
	t.Setenv("KAMAKIRI_API_URL", server.URL)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("`kamakiri status` exited %d, want 1; stderr: %q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "✗ A records do not match the current edge for cloudflare mode") {
		t.Errorf("stdout:\n%q\nwant the drift block", got)
	}
	want := "Internal server error.\n" +
		"A records do not match the current edge. The kamakiri status page lists the expected and observed values.\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestStatusRecheckWarningRidesTheDispatch pins which stream the dispatch hands
// the recheck loop: a recheck the server did not perform is noted on stderr and
// not in the page, and it decides nothing about the exit code, so a run with
// nothing else wrong leaves stderr holding that one line and exits 0.
func TestStatusRecheckWarningRidesTheDispatch(t *testing.T) {
	pinDispatchEnvironment(t)
	t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
	t.Chdir(t.TempDir())

	if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case strings.HasSuffix(r.URL.Path, "/recheck"):
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"code":"internal_error","message":"probe worker unavailable"}`)
			return
		case strings.HasPrefix(r.URL.Path, "/v1/pages/sites/"):
			body = api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true}
		case r.URL.Path == "/v1/pages/domains":
			body = api.DomainList{Domains: []api.Domain{
				{
					Domain: "next.altstack.jp",
					Role:   "canonical",
					// Awaiting DNS, so the loop asks about this row, and the row
					// matched no apex A alternative, so the drift check does not
					// apply and the exit code is the recheck's to decide.
					State:      "awaiting_dns",
					DnsVerdict: "absent",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "next.altstack.jp", Type: "cname", Value: "t.kamakiri-pages.site.", Purpose: "primary", Required: true},
					},
				},
			}}
		case r.URL.Path == "/v1/pages/cdn/status":
			body = api.CDNStatusResponse{CDNMode: "none", Provider: "none"}
		case r.URL.Path == "/v1/keys/current":
			body = statusKey()
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"not_found","message":"not found"}`)
			return
		}
		payload, err := json.Marshal(body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(payload)
	}))
	t.Cleanup(server.Close)

	t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
	t.Setenv("KAMAKIRI_API_URL", server.URL)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--recheck"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("`kamakiri status --recheck` exited %d, want 0; stderr: %q", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "next.altstack.jp") {
		t.Errorf("stdout:\n%q\nwant the row for the domain the recheck was refused for", got)
	}
	if strings.Contains(got, "not performed") {
		t.Errorf("stdout:\n%q\nwant the warning on stderr, not on stdout", got)
	}
	want := "recheck of next.altstack.jp not performed (probe worker unavailable). The server keeps checking on its own.\n"
	if line := stderr.String(); line != want {
		t.Errorf("stderr = %q, want %q", line, want)
	}
}

// A deploy key's `status` runs through the real client against a server that
// answers as one would for such a key: --recheck sends no recheck request and
// prints its one line, and a linked site outside the key's list ends the page at
// the API server row and exits 1 with the refusal's line.
func TestStatusWithADeployKeyRidesTheDispatch(t *testing.T) {
	const (
		deployKeyJSON = `{"key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"github-actions","kind":"deploy",` +
			`"resource_ids":["k3j9x2m4p7q1w8e5"],"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}],` +
			`"created_at":"2026-11-13T02:05:59Z","last_used_at":null,"current":true}}`
		notInKey = `{"code":"site_not_in_key","message":"This deploy key cannot act on this site.",` +
			`"key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"github-actions"},` +
			`"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}],"site":{"id":"site123","label":"blog.example.org"}}`
	)

	// serve answers the reads a deploy key's status makes, refusing the site
	// read when refuseSite is set, and counts every recheck request that arrives.
	serve := func(t *testing.T, refuseSite bool) *atomic.Int32 {
		t.Helper()
		var rechecks atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body any
			switch {
			case strings.HasSuffix(r.URL.Path, "/recheck"):
				rechecks.Add(1)
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"code":"deploy_key_refused","message":"This is a deploy key."}`)
				return
			case r.URL.Path == "/v1/keys/current":
				io.WriteString(w, deployKeyJSON)
				return
			case strings.HasPrefix(r.URL.Path, "/v1/pages/sites/"):
				if refuseSite {
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, notInKey)
					return
				}
				body = api.Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true}
			case r.URL.Path == "/v1/pages/domains":
				body = api.DomainList{Domains: []api.Domain{{
					Domain: "next.altstack.jp", Role: "canonical", State: "awaiting_dns", DnsVerdict: "absent",
					DNSRecordsExpected: []api.DNSRecord{
						{Name: "next.altstack.jp", Type: "cname", Value: "t.kamakiri-pages.site.", Purpose: "primary", Required: true},
					},
				}}}
			case r.URL.Path == "/v1/pages/cdn/status":
				body = api.CDNStatusResponse{CDNMode: "none", Provider: "none"}
			default:
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"code":"not_found","message":"not found"}`)
				return
			}
			payload, err := json.Marshal(body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write(payload)
		}))
		t.Cleanup(server.Close)
		t.Setenv("KAMAKIRI_API_URL", server.URL)
		t.Setenv("KAMAKIRI_API_KEY", "kk_deploy_fromenv")
		return &rechecks
	}

	t.Run("--recheck sends no recheck", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())
		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}
		rechecks := serve(t, false)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status", "--recheck"}, strings.NewReader(""), &stdout, &stderr)

		if code != 0 {
			t.Fatalf("`kamakiri status --recheck` exited %d, want 0; stderr: %q", code, stderr.String())
		}
		if n := rechecks.Load(); n != 0 {
			t.Errorf("%d recheck requests arrived, want none", n)
		}
		want := "--recheck needs a login key; this run uses a deploy key, so no domain was rechecked.\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		got := stdout.String()
		for _, line := range []string{"Sites:        example.com\n", "next.altstack.jp"} {
			if !strings.Contains(got, line) {
				t.Errorf("stdout:\n%s\nwant it to hold %q", got, line)
			}
		}
	})

	t.Run("a linked site outside the key's list", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		t.Chdir(t.TempDir())
		if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
			t.Fatalf("SaveProject: %v", err)
		}
		serve(t, true)

		var stdout, stderr bytes.Buffer
		code := run([]string{"status"}, strings.NewReader(""), &stdout, &stderr)

		if code != 1 {
			t.Fatalf("`kamakiri status` exited %d, want 1", code)
		}
		if got, want := stdout.String(), "API server:   "+os.Getenv("KAMAKIRI_API_URL")+"\n"; !strings.HasSuffix(got, want) {
			t.Errorf("stdout:\n%s\nwant the page to end at the API server row, %q", got, want)
		}
		want := `this deploy key ("github-actions") cannot act on blog.example.org: it can deploy to example.com and nothing else` + "\n"
		if got := stderr.String(); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

// TestAccountScopedDomainVerbsRefuseBeforeSending pins that the domain verbs
// that need no project still need a credential, and say so before anything is
// sent: a keyless request could only come back unauthorized, blaming a key that
// was never there.
func TestAccountScopedDomainVerbsRefuseBeforeSending(t *testing.T) {
	const notLoggedIn = "not logged in. Run \"kamakiri login\", or set KAMAKIRI_API_KEY to an API key\n"

	t.Run("domain list refuses before it sends", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "list"}, strings.NewReader(""), &stdout, &stderr)

		// The handler records from the server's own goroutine, and Close
		// returns only once that goroutine has finished, which is what orders
		// its write against the read below.
		server.Close()

		if code != 1 {
			t.Fatalf("`kamakiri domain list` exited %d, want 1", code)
		}
		if got := stderr.String(); got != notLoggedIn {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, notLoggedIn)
		}
		if requests != 0 {
			t.Error("a request went out, but there is no credential to send")
		}
	})

	t.Run("domain register says so behind its prefix", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(server.Close)

		t.Setenv("KAMAKIRI_API_URL", server.URL)

		var stdout, stderr bytes.Buffer
		code := run([]string{"domain", "register", "example.com"}, strings.NewReader(""), &stdout, &stderr)

		server.Close()

		if code != 1 {
			t.Fatalf("`kamakiri domain register` exited %d, want 1", code)
		}
		if got, want := stderr.String(), "Error: "+notLoggedIn; got != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", got, want)
		}
		if requests != 0 {
			t.Error("a request went out, but there is no credential to send")
		}
	})
}

// The key arm hands the client it armed to the key package and prints what the
// package returns on stderr. Each run is driven against a server, so the
// wiring is held as well as the parse: the route reached, the bearer sent, and
// both streams. The listing's last-used column reads `never` so the page does
// not depend on the clock the arm passes.
func TestKeyCommandsRideTheDispatch(t *testing.T) {
	const keyID = "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c"
	cases := []struct {
		name   string
		args   []string
		status int
		answer string
		route  string
		stdout string
		stderr string
		code   int
	}{
		{
			name:   "key list",
			args:   []string{"key", "list"},
			status: http.StatusOK,
			answer: `{"keys":[{"id":"` + keyID + `","name":"github-ci","kind":"login","resource_ids":[],"created_at":"2026-11-13T02:05:00Z","last_used_at":null,"current":true}]}`,
			route:  "GET /v1/keys",
			stdout: "  ID                                    Name       Kind   Sites  Created           Last used\n" +
				"* c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c  github-ci  login  all    2026-11-13 02:05  never\n" +
				"\n" +
				"* the key this command ran with\n",
		},
		{
			name:   "key rename",
			args:   []string{"key", "rename", keyID, "ci-box"},
			status: http.StatusOK,
			answer: `{"key":{"id":"` + keyID + `","name":"ci-box","kind":"login","resource_ids":[],"created_at":"2026-11-13T02:05:00Z","last_used_at":null,"current":false}}`,
			route:  "PATCH /v1/keys/" + keyID,
			stdout: "Renamed key " + keyID + " to ci-box.\n",
		},
		{
			name:   "key revoke",
			args:   []string{"key", "revoke", keyID},
			status: http.StatusOK,
			answer: `{"key":{"id":"` + keyID + `","name":"github-ci"}}`,
			route:  "DELETE /v1/keys/" + keyID,
			stdout: "Revoked key github-ci (" + keyID + "). It no longer works anywhere it was used.\n",
		},
		{
			// The key comes from the variable here, so the refusal says to
			// revoke it with another key rather than to log out.
			name:   "key revoke of the key the command runs with",
			args:   []string{"key", "revoke", keyID},
			status: http.StatusConflict,
			answer: `{"code":"current_key","message":"This is the key the request was made with."}`,
			route:  "DELETE /v1/keys/" + keyID,
			stderr: "this is the key in KAMAKIRI_API_KEY, which this command runs with. Revoke it using another key, such as the one on your own machine\n",
			code:   1,
		},
		{
			// Every command but logout meets a deploy key's refusal through
			// the line every refusal is worded with, and nothing else.
			name:   "key list with a deploy key",
			args:   []string{"key", "list"},
			status: http.StatusForbidden,
			answer: `{"code":"deploy_key_refused","message":"This is a deploy key.","key":{"id":"` + keyID + `","name":"github-actions"},"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}]}`,
			route:  "GET /v1/keys",
			stderr: "this is a deploy key (\"github-actions\"): it can deploy to example.com and nothing else. Use a login key for this command\n",
			code:   1,
		},
		{
			name:   "key rename of no key of the account",
			args:   []string{"key", "rename", keyID, "ci-box"},
			status: http.StatusNotFound,
			answer: `{"code":"key_not_found","message":"Key not found."}`,
			route:  "PATCH /v1/keys/" + keyID,
			stderr: "no key with ID " + keyID + " in this account\n",
			code:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			var route, authorization string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				route = r.Method + " " + r.URL.Path
				authorization = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.answer)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr)

			// The handler records from the server's own goroutine, and Close
			// returns only once that goroutine has finished, which is what
			// orders its writes against the reads below.
			server.Close()

			if code != tc.code {
				t.Errorf("exited %d, want %d", code, tc.code)
			}
			if got := stdout.String(); got != tc.stdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", got, tc.stdout)
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if route != tc.route {
				t.Errorf("request = %q, want %q", route, tc.route)
			}
			if got, want := authorization, "Bearer kk_live_fromenv"; got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

// `key create` through the real switch and client: the flags reach the request
// body (no name field at all without --name, the sites in the order typed, the
// linked site's ID without --site), and stdout carries the key alone on
// success and nothing on any failure, so whatever main prints is covered too.
func TestKeyCreateRidesTheDispatch(t *testing.T) {
	const (
		keyID      = "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
		apiKey     = "kk_deploy_4Wq8ZtR2mN7xK1pL9sB3vC6yH0jD5fGa"
		linkedSite = "k3j9x2m4p7q1w8e5"
		created    = `{"key":{"id":"` + keyID + `","name":"github-actions","kind":"deploy","resource_ids":["` + linkedSite + `","a1b2c3d4e5f6g7h8"],` +
			`"sites":[{"id":"` + linkedSite + `","label":"example.com"},{"id":"a1b2c3d4e5f6g7h8","label":"docs.kamakiri-pages.jp"}],` +
			`"created_at":"2026-11-17T10:30:00Z","last_used_at":null,"current":false},"api_key":"` + apiKey + `"}`
		lines = "Created deploy key \"github-actions\" (" + keyID + ").\n" +
			"Sites: example.com, docs.kamakiri-pages.jp\n" +
			"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n" +
			"A notice is being sent to the account's email address.\n"
	)
	cases := []struct {
		name   string
		args   []string
		linked bool
		status int
		answer string
		body   map[string]any
		stdout string
		stderr string
		code   int
	}{
		{
			name:   "--site twice, no --name",
			args:   []string{"key", "create", "--site", "example.com", "--site=docs"},
			status: http.StatusCreated,
			answer: created,
			body:   map[string]any{"sites": []any{"example.com", "docs"}},
			stdout: apiKey + "\n",
			stderr: lines,
		},
		{
			name:   "--name, in either form",
			args:   []string{"key", "create", "--name=first", "--site", "example.com", "--name", "github-actions"},
			status: http.StatusCreated,
			answer: created,
			body:   map[string]any{"name": "github-actions", "sites": []any{"example.com"}},
			stdout: apiKey + "\n",
			stderr: lines,
		},
		{
			name:   "the linked site without --site",
			args:   []string{"key", "create"},
			linked: true,
			status: http.StatusCreated,
			answer: created,
			body:   map[string]any{"sites": []any{linkedSite}},
			stdout: apiKey + "\n",
			stderr: lines,
		},
		{
			name:   "a site that matches nothing",
			args:   []string{"key", "create", "--site", "Docs"},
			status: http.StatusUnprocessableEntity,
			answer: `{"code":"key_site_not_found","message":"No site of this account matches docs.kamakiri-pages.jp.","reference":"docs.kamakiri-pages.jp"}`,
			body:   map[string]any{"sites": []any{"Docs"}},
			stderr: "no site of this account matches docs.kamakiri-pages.jp. Name a site by its subdomain, one of its domains, or its site ID\n",
			code:   1,
		},
		{
			name:   "the linked site matching nothing",
			args:   []string{"key", "create"},
			linked: true,
			status: http.StatusUnprocessableEntity,
			answer: `{"code":"key_site_not_found","message":"No site of this account matches ` + linkedSite + `.kamakiri-pages.jp.","reference":"` + linkedSite + `.kamakiri-pages.jp"}`,
			body:   map[string]any{"sites": []any{linkedSite}},
			stderr: "the site linked here is not an active site of this account. Name a site with --site\n",
			code:   1,
		},
		{
			name:   "run with a deploy key",
			args:   []string{"key", "create", "--site", "example.com"},
			status: http.StatusForbidden,
			answer: `{"code":"deploy_key_refused","message":"This is a deploy key.","key":{"id":"` + keyID + `","name":"github-actions"},"sites":[{"id":"` + linkedSite + `","label":"example.com"}]}`,
			body:   map[string]any{"sites": []any{"example.com"}},
			stderr: "this is a deploy key (\"github-actions\"): it can deploy to example.com and nothing else. Use a login key for this command\n",
			code:   1,
		},
		{
			name:   "no --site and no linked site",
			args:   []string{"key", "create", "--name", "github-actions"},
			stderr: "no site linked here. Run this in a linked directory, or name a site with --site\n",
			code:   1,
		},
		{
			name:   "a name the rule refuses",
			args:   []string{"key", "create", "--name", "", "--site", "example.com"},
			stderr: "invalid key name: it must be 1 to 64 characters with no control characters\n",
			code:   1,
		},
		{
			name:   "--name followed by --site",
			args:   []string{"key", "create", "--name", "--site", "example.com"},
			stderr: "Error: --name requires a value; write a value that starts with \"--\" as --name=<value>\n",
			code:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
			t.Chdir(t.TempDir())
			if tc.linked {
				if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: linkedSite}); err != nil {
					t.Fatal(err)
				}
			}

			var requests atomic.Int32
			var route, authorization string
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				route = r.Method + " " + r.URL.Path
				authorization = r.Header.Get("Authorization")
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding the request body: %v", err)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.answer)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr)

			// Close returns only once the handler's goroutine has finished,
			// which orders its writes against the reads below.
			server.Close()

			if code != tc.code {
				t.Errorf("exited %d, want %d", code, tc.code)
			}
			if got := stdout.String(); got != tc.stdout {
				t.Errorf("stdout = %q, want %q", got, tc.stdout)
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if tc.body == nil {
				if n := requests.Load(); n != 0 {
					t.Errorf("sent %d requests, want none", n)
				}
				return
			}
			if n := requests.Load(); n != 1 {
				t.Fatalf("sent %d requests, want 1", n)
			}
			if route != "POST /v1/keys" {
				t.Errorf("request = %q, want %q", route, "POST /v1/keys")
			}
			if got, want := authorization, "Bearer kk_live_fromenv"; got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
			if !reflect.DeepEqual(body, tc.body) {
				t.Errorf("request body = %#v, want %#v", body, tc.body)
			}
		})
	}
}

// A command line that does not name exactly one key ID is refused before
// anything is sent, with a credential in hand so that a refusal cannot pass by
// the run stopping for want of one. The command takes no flag, so any argument
// starting with a hyphen is one it does not take, and the first such argument,
// or a second ID, is named, redacted, after the usage line.
func TestKeyRevokeRefusesABadCommandLineBeforeSending(t *testing.T) {
	const (
		keyID = "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c"
		usage = "Usage: kamakiri key revoke <id>\n"

		invalidID = "invalid key ID. A key ID looks like 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65; run \"kamakiri key list\" to see yours\n"
	)
	cases := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"no argument", []string{"key", "revoke"}, usage},
		{"--all-others", []string{"key", "revoke", "--all-others"}, usage + "Unexpected argument: --all-others\n"},
		{"--all-others with --yes", []string{"key", "revoke", "--all-others", "--yes"}, usage + "Unexpected argument: --all-others\n"},
		{"--all-others with a value", []string{"key", "revoke", "--all-others=x"}, usage + "Unexpected argument: --all-others\n"},
		{"an ID with --all-others", []string{"key", "revoke", keyID, "--all-others"}, usage + "Unexpected argument: --all-others\n"},
		{"--yes alone", []string{"key", "revoke", "--yes"}, usage + "Unexpected argument: --yes\n"},
		{"an ID with --yes", []string{"key", "revoke", keyID, "--yes"}, usage + "Unexpected argument: --yes\n"},
		{"a second ID", []string{"key", "revoke", keyID, "extra"}, usage + "Unexpected argument: extra\n"},
		{"an unknown flag with a value", []string{"key", "revoke", keyID, "--token=TOPSECRET"}, usage + "Unexpected argument: --token\n"},
		{"a short flag", []string{"key", "revoke", "-y", keyID}, usage + "Unexpected argument: -y\n"},
		{"an empty ID", []string{"key", "revoke", ""}, invalidID},
		{"an empty ID and a second ID", []string{"key", "revoke", "", keyID}, usage + "Unexpected argument: " + keyID + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader("y\n"), &stdout, &stderr)
			server.Close()

			if code != 1 {
				t.Errorf("exited %d, want 1", code)
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if got := stdout.String(); got != "" {
				t.Errorf("stdout:\n%q\nwant nothing", got)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("sent %d requests, want none", n)
			}
		})
	}
}

// A rotation command line that does not name exactly one key ID is refused
// before anything is sent, as a revoke's is: the command takes no flag, so any
// argument starting with a hyphen is one it does not take, and the first such
// argument, or a second ID, is named, redacted, after the usage line.
func TestKeyRotateRefusesABadCommandLineBeforeSending(t *testing.T) {
	const (
		keyID = "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
		usage = "Usage: kamakiri key rotate <id>\n"

		invalidID = "invalid key ID. A key ID looks like 5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65; run \"kamakiri key list\" to see yours\n"
	)
	cases := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"no argument", []string{"key", "rotate"}, usage},
		{"a flag alone", []string{"key", "rotate", "--name"}, usage + "Unexpected argument: --name\n"},
		{"an ID with --site", []string{"key", "rotate", keyID, "--site", "example.com"}, usage + "Unexpected argument: --site\n"},
		{"a second ID", []string{"key", "rotate", keyID, "extra"}, usage + "Unexpected argument: extra\n"},
		{"an unknown flag with a value", []string{"key", "rotate", keyID, "--token=TOPSECRET"}, usage + "Unexpected argument: --token\n"},
		{"a short flag", []string{"key", "rotate", "-y", keyID}, usage + "Unexpected argument: -y\n"},
		{"an empty ID", []string{"key", "rotate", ""}, invalidID},
		{"an empty ID and a second ID", []string{"key", "rotate", "", keyID}, usage + "Unexpected argument: " + keyID + "\n"},
		{"current", []string{"key", "rotate", "current"}, invalidID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr)
			server.Close()

			if code != 1 {
				t.Errorf("exited %d, want 1", code)
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if got := stdout.String(); got != "" {
				t.Errorf("stdout:\n%q\nwant nothing", got)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("sent %d requests, want none", n)
			}
		})
	}
}

// `key rotate` through the real switch and client: the request reaches the
// key's rotate route with no body, stdout carries the new key alone on success
// and nothing on any failure, and a failure that leaves the outcome unknown,
// an error answer or one that is not the API's, is followed by the line saying
// the key may have been rotated, printed by main as part of the error.
func TestKeyRotateRidesTheDispatch(t *testing.T) {
	const (
		oldID   = "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
		newID   = "7b2e4d10-9a3c-4f1e-8b20-5c6d7e8f9a01"
		apiKey  = "kk_deploy_9Hc2Lm5Qx8Tn1Vb4Zr7Kp0Wd3Fs6Gj"
		rotated = `{"key":{"id":"` + newID + `","name":"github-actions","kind":"deploy","resource_ids":["k3j9x2m4p7q1w8e5","a1b2c3d4e5f6g7h8"],` +
			`"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"},{"id":"a1b2c3d4e5f6g7h8","label":"docs.kamakiri-pages.jp"}],` +
			`"created_at":"2026-11-18T09:00:00Z","last_used_at":null,"current":false},"api_key":"` + apiKey + `",` +
			`"replaced":{"id":"` + oldID + `","name":"github-actions"}}`

		maybeRotated = "The key may have been rotated. Run \"kamakiri key list\" to see whether it was replaced.\n"
	)
	cases := []struct {
		name   string
		status int
		answer string
		stdout string
		stderr string
		code   int
	}{
		{
			name:   "a deploy key",
			status: http.StatusCreated,
			answer: rotated,
			stdout: apiKey + "\n",
			stderr: "Rotated deploy key \"github-actions\": " + newID + " replaces " + oldID + ", which no longer works.\n" +
				"Sites: example.com, docs.kamakiri-pages.jp\n" +
				"This key is shown once. Store it as KAMAKIRI_API_KEY in your CI secrets.\n" +
				"A notice is being sent to the account's email address.\n",
		},
		{
			name:   "a login key",
			status: http.StatusConflict,
			answer: `{"code":"key_not_rotatable","message":"This is a login key, which cannot be rotated. Log in again on the machine that uses it, then revoke this one."}`,
			stderr: "this is a login key, which cannot be rotated. Run \"kamakiri login\" on the machine that uses it, then revoke this one with \"kamakiri key revoke " + oldID + "\"\n",
			code:   1,
		},
		{
			name:   "no key of the account",
			status: http.StatusNotFound,
			answer: `{"code":"key_not_found","message":"Key not found."}`,
			stderr: "no key with ID " + oldID + " in this account\n",
			code:   1,
		},
		{
			name:   "a server error",
			status: http.StatusInternalServerError,
			answer: `{"code":"server_error","message":"Failed to rotate the key."}`,
			stderr: "Failed to rotate the key.\n" + maybeRotated,
			code:   1,
		},
		{
			name:   "an answer that is not JSON",
			status: http.StatusBadGateway,
			answer: "<html>Bad Gateway</html>",
			stderr: "server returned 502\n" + maybeRotated,
			code:   1,
		},
		{
			name:   "a success that carries no key",
			status: http.StatusCreated,
			answer: `{}`,
			stderr: "parse response: the response names no key\n" + maybeRotated,
			code:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			var requests atomic.Int32
			var route, authorization string
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				route = r.Method + " " + r.URL.Path
				authorization = r.Header.Get("Authorization")
				body, _ = io.ReadAll(r.Body)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.answer)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run([]string{"key", "rotate", oldID}, strings.NewReader(""), &stdout, &stderr)

			// Close returns only once the handler's goroutine has finished,
			// which orders its writes against the reads below.
			server.Close()

			if code != tc.code {
				t.Errorf("exited %d, want %d", code, tc.code)
			}
			if got := stdout.String(); got != tc.stdout {
				t.Errorf("stdout = %q, want %q", got, tc.stdout)
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if n := requests.Load(); n != 1 {
				t.Fatalf("sent %d requests, want 1", n)
			}
			if want := "POST /v1/keys/" + oldID + "/rotate"; route != want {
				t.Errorf("request = %q, want %q", route, want)
			}
			if got, want := authorization, "Bearer kk_live_fromenv"; got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
			if len(body) != 0 {
				t.Errorf("request body = %q, want none", body)
			}
		})
	}
}

// stubAnswer is one canned response of a dispatch test's stub server.
type stubAnswer struct {
	status int
	body   string
}

// `init` and `teardown` render a deploy key's refusals through the same mapping
// as every other command, so a key name the server controls reaches the
// terminal escaped and in the catalog's wording, with nothing on stdout but the
// command's own prompt.
func TestInitAndTeardownRenderDeployKeyRefusals(t *testing.T) {
	const siteRow = `"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}]`
	const keyRow = `"key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"ci\u202ebox"}`
	cases := []struct {
		name string
		args []string
		// stdin answers the prompt of the command, when it has one.
		stdin string
		// answers maps "METHOD path" to the status and body the stub returns;
		// requests lists the requests the command is expected to make, in order.
		answers  map[string]stubAnswer
		requests []string
		stdout   string
		stderr   string
	}{
		{
			name:   "init refused as a deploy key",
			args:   []string{"init"},
			stdin:  "blog\n",
			stdout: "Subdomain: ",
			answers: map[string]stubAnswer{
				"POST /v1/pages/sites": {http.StatusForbidden, `{"code":"deploy_key_refused","message":"This is a deploy key.",` + keyRow + `,` + siteRow + `}`},
			},
			requests: []string{"POST /v1/pages/sites"},
			stderr:   "this is a deploy key (\"ci\\u{202e}box\"): it can deploy to example.com and nothing else. Use a login key for this command\n",
		},
		{
			// A deploy key may read its own site, so the refusal comes from the
			// DELETE, after the confirmation.
			name:  "teardown refused as a deploy key",
			args:  []string{"teardown"},
			stdin: "y\n",
			answers: map[string]stubAnswer{
				"GET /v1/pages/sites/site123":    {http.StatusOK, `{"id":"site123","subdomain":"example"}`},
				"DELETE /v1/pages/sites/site123": {http.StatusForbidden, `{"code":"deploy_key_refused","message":"This is a deploy key.",` + keyRow + `,` + siteRow + `}`},
			},
			requests: []string{"GET /v1/pages/sites/site123", "DELETE /v1/pages/sites/site123"},
			stdout:   "This will permanently delete \"example\" and remove it from kamakiri-pages.jp.\nAre you sure? (y/N) ",
			stderr:   "this is a deploy key (\"ci\\u{202e}box\"): it can deploy to example.com and nothing else. Use a login key for this command\n",
		},
		{
			name:  "teardown of a site off the key's list",
			args:  []string{"teardown"},
			stdin: "y\n",
			answers: map[string]stubAnswer{
				"GET /v1/pages/sites/site123": {http.StatusForbidden, `{"code":"site_not_in_key","message":"Not in key.",` + keyRow + `,` + siteRow + `,"site":{"id":"z9y8x7w6v5u4t3s2","label":"blog.example.org"}}`},
			},
			requests: []string{"GET /v1/pages/sites/site123"},
			stderr:   "this deploy key (\"ci\\u{202e}box\") cannot act on blog.example.org: it can deploy to example.com and nothing else\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
			t.Chdir(t.TempDir())
			// init asks for a subdomain only when no site is linked.
			if tc.args[0] != "init" {
				if err := core.SaveProject(&core.ProjectConfig{Version: 1, Kind: "pages", ID: "site123"}); err != nil {
					t.Fatalf("SaveProject: %v", err)
				}
			}

			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				route := r.Method + " " + r.URL.Path
				requests = append(requests, route)
				answer, ok := tc.answers[route]
				if !ok {
					answer = stubAnswer{http.StatusNotFound, `{}`}
				}
				w.WriteHeader(answer.status)
				io.WriteString(w, answer.body)
			}))
			t.Cleanup(server.Close)
			t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			t.Setenv("KAMAKIRI_API_URL", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			server.Close()

			if code != 1 {
				t.Errorf("exited %d, want 1", code)
			}
			if got := stdout.String(); got != tc.stdout {
				t.Errorf("stdout:\n%q\nwant:\n%q", got, tc.stdout)
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if !slices.Equal(requests, tc.requests) {
				t.Errorf("requests = %q, want %q", requests, tc.requests)
			}
			if tc.args[0] == "teardown" {
				if _, err := os.Stat(core.ProjectPath()); err != nil {
					t.Errorf("project config not kept: %v", err)
				}
			}
		})
	}
}

// The logout arm loads the credential itself and hands it to the logout
// package, which decides from it whether anything is sent. Each run is driven
// against a local server that counts what reaches it, so a run that must send
// nothing is held to that, and the bearer of the one that sends is the key the
// file held.
func TestLogoutRidesTheDispatch(t *testing.T) {
	const (
		notLoggedIn = "Not logged in.\n"
		environment = `KAMAKIRI_API_KEY holds the key in use, so logout changes nothing. Unset it to stop using that key, or revoke it with "kamakiri key revoke" from another key` + "\n"
	)
	cases := []struct {
		name string
		args []string
		// file is what the credentials file holds before the run, or nothing
		// when it is empty.
		file        string
		environment bool
		status      int
		answer      string
		requests    int32
		stdout      string
		stderr      string
		// stderrPrefix stands in for stderr where the line carries a path or
		// an error from the operating system: one line starting with it.
		stderrPrefix string
		code         int
		fileKept     bool
	}{
		{
			name:     "a revoke",
			args:     []string{"logout"},
			file:     `{"version":1,"api_key":"kk_live_fromfile","email":"a@example.com"}`,
			status:   http.StatusOK,
			answer:   `{"key":{"id":"5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65","name":"alexis-laptop"}}`,
			requests: 1,
			stdout:   "Logged out. The key alexis-laptop was revoked; any copy of it, in a CI secret for instance, no longer works.\n",
		},
		{
			name:     "a key the server already refuses",
			args:     []string{"logout"},
			file:     `{"version":1,"api_key":"kk_live_fromfile","email":"a@example.com"}`,
			status:   http.StatusUnauthorized,
			answer:   `{"code":"unauthorized","message":"invalid API key"}`,
			requests: 1,
			stdout:   "Logged out. The saved key was already invalid.\n",
		},
		{
			name:     "a server failure",
			args:     []string{"logout"},
			file:     `{"version":1,"api_key":"kk_live_fromfile","email":"a@example.com"}`,
			status:   http.StatusInternalServerError,
			answer:   "<html>500 Internal Server Error</html>",
			requests: 1,
			stderr:   "server returned 500\nthe key may still be valid and is still saved; run \"kamakiri logout\" again\n",
			code:     1,
			fileKept: true,
		},
		{
			// A deploy key cannot revoke itself: the credentials file stays,
			// nothing prints on stdout, and the one line on stderr names the
			// key with every slot escaped.
			name:     "a deploy key",
			args:     []string{"logout"},
			file:     `{"version":1,"api_key":"kk_live_fromfile","email":"a@example.com"}`,
			status:   http.StatusForbidden,
			answer:   `{"code":"deploy_key_refused","message":"This is a deploy key.","key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"ci\u202ebox"},"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}]}`,
			requests: 1,
			stderr:   "this is a deploy key (\"ci\\u{202e}box\", 3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40), which cannot revoke itself. The credentials file was kept. Revoke the key with \"kamakiri key revoke <id>\" using a login key\n",
			code:     1,
			fileKept: true,
		},
		{
			name:   "no credential",
			args:   []string{"logout"},
			stdout: notLoggedIn,
		},
		{
			// A file with no key in it is no credential, as for every command,
			// and logout leaves it where it is.
			name:     "a credentials file with no key",
			args:     []string{"logout"},
			file:     `{"version":1,"api_key":"   "}`,
			stdout:   notLoggedIn,
			fileKept: true,
		},
		{
			name:        "a key from the environment",
			args:        []string{"logout"},
			file:        `{"version":1,"api_key":"kk_live_fromfile","email":"a@example.com"}`,
			environment: true,
			stderr:      environment,
			code:        1,
			fileKept:    true,
		},
		{
			name:         "a credentials file that cannot be read",
			args:         []string{"logout"},
			file:         "not json",
			stderrPrefix: "parse credentials:",
			code:         1,
			fileKept:     true,
		},
		{
			name:     "an extra argument",
			args:     []string{"logout", "extra"},
			file:     `{"version":1,"api_key":"kk_live_fromfile","email":"a@example.com"}`,
			stderr:   "Usage: kamakiri logout\nUnexpected argument: extra\n",
			code:     1,
			fileKept: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinDispatchEnvironment(t)
			t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

			path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri", "credentials.json")
			if tc.file != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(path, []byte(tc.file), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}

			var requests atomic.Int32
			var route, authorization string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				route = r.Method + " " + r.URL.Path
				authorization = r.Header.Get("Authorization")
				if tc.requests == 0 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.answer)
			}))
			t.Cleanup(server.Close)

			t.Setenv("KAMAKIRI_API_URL", server.URL)
			if tc.environment {
				t.Setenv("KAMAKIRI_API_KEY", "kk_live_fromenv")
			}

			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr)
			// Close returns only once the handler's goroutine has finished,
			// which orders its writes against the reads below.
			server.Close()

			if code != tc.code {
				t.Errorf("exited %d, want %d", code, tc.code)
			}
			if got := stdout.String(); got != tc.stdout {
				t.Errorf("stdout:\n%q\nwant:\n%q", got, tc.stdout)
			}
			if tc.stderrPrefix != "" {
				if line := stderr.String(); !strings.HasPrefix(line, tc.stderrPrefix) ||
					strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
					t.Errorf("stderr:\n%q\nwant one line starting with %q", line, tc.stderrPrefix)
				}
			} else if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", got, tc.stderr)
			}
			if n := requests.Load(); n != tc.requests {
				t.Errorf("sent %d requests, want %d", n, tc.requests)
			}
			if tc.requests > 0 {
				if route != "DELETE /v1/keys/current" {
					t.Errorf("request = %q, want %q", route, "DELETE /v1/keys/current")
				}
				if got, want := authorization, "Bearer kk_live_fromfile"; got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
			}
			_, err := os.Stat(path)
			switch {
			case tc.file == "":
			case tc.fileKept && err != nil:
				t.Errorf("credentials.json: Stat() error = %v, want the file kept", err)
			case !tc.fileKept && !errors.Is(err, os.ErrNotExist):
				t.Errorf("credentials.json: Stat() error = %v, want the file removed", err)
			}
		})
	}
}

// A revoke that went through while the file could not be removed exits 1: the
// file now holds a dead key, and the line says so. The config directory is made
// read-only, which root ignores.
func TestLogoutExitsWhenItCannotRemoveTheRevokedKey(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a directory whatever its mode says, so no removal here would fail")
	}
	pinDispatchEnvironment(t)
	t.Cleanup(func() { api.SetKeyFromEnvironment(false) })

	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"api_key":"kk_live_fromfile"}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("sealing the config directory: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"key":{"id":"5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65","name":"alexis-laptop"}}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("KAMAKIRI_API_URL", server.URL)

	var stdout, stderr bytes.Buffer
	code := run([]string{"logout"}, strings.NewReader(""), &stdout, &stderr)

	if code != 1 {
		t.Errorf("exited %d, want 1", code)
	}
	if got := stdout.String(); got != "" {
		t.Errorf("stdout:\n%q\nwant nothing", got)
	}
	const want = "the key alexis-laptop was revoked, but the saved copy could not be removed: delete credentials: "
	if line := stderr.String(); !strings.HasPrefix(line, want) ||
		strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("stderr:\n%q\nwant one line starting with %q", line, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("credentials.json: Stat() error = %v, want the file still there", err)
	}
}
