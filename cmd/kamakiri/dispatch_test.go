package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	"    read. Take the key from the file `kamakiri login` saves on your machine.\n" +
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
		"  view (waiting → detected → live). Safe to Ctrl-C any time.\n" +
		"  The server keeps checking; resume with `kamakiri domain verify`.\n" +
		"  --no-wait  print the record + queue and return promptly\n" +
		"             (also the automatic behavior in non-TTY / CI).\n"

	usageDomainRegister = "Usage: kamakiri domain register <domain> [--no-wait]\n" +
		"  Registers account-level ownership of a domain (no project required).\n" +
		"  Prints the TXT to publish, then blocks until verified, streaming the\n" +
		"  view. Safe to Ctrl-C any time; the server keeps checking.\n" +
		"  --no-wait  print the record and return promptly (also the automatic\n" +
		"             behavior in non-TTY / CI).\n"

	usageDomainRemove = "Usage: kamakiri domain remove <domain> [--no-wait]\n" +
		"  Blocks until the teardown is in sync on our side.\n" +
		"  Safe to Ctrl-C any time. Teardown continues server-side.\n" +
		"  --no-wait  queue and return promptly (also the automatic\n" +
		"             behavior in non-TTY / CI).\n"

	usageDomainSet = "Usage: kamakiri domain set <domain> [--no-wait]\n" +
		"  Blocks until the domain is live, streaming a go-live view\n" +
		"  (waiting → detected → live). Safe to Ctrl-C any time.\n" +
		"  The server keeps checking; resume with `kamakiri domain verify`.\n" +
		"  --no-wait  print the record + queue and return promptly\n" +
		"             (also the automatic behavior in non-TTY / CI).\n"

	usageDomainUnregister = "Usage: kamakiri domain unregister <domain>\n" +
		"  Removes account-level registration of a domain (no project required).\n" +
		"  Synchronous; fails if a site still uses a host under it (detach first).\n"

	usageDomainUnset = "Usage: kamakiri domain unset [--no-wait]\n" +
		"  --no-wait  exit 0 immediately after queueing.\n" +
		"             Without it, the command blocks for up to ~60s waiting\n" +
		"             for the edge to apply, and exits 1 with `still syncing`\n" +
		"             if the deadline is hit.\n"

	usageDomainVerify = "Usage: kamakiri domain verify <domain> [--no-wait]\n" +
		"  Re-checks your DNS now and blocks until the domain is live,\n" +
		"  streaming the view. Safe to Ctrl-C any time; the server keeps\n" +
		"  checking.\n" +
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
	})
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

// An unreadable credentials file is a finding `status` reports rather than a
// reason to refuse: it renders the whole page with the failure on its own line,
// prints the same failure on stderr after it, and exits 1, so a check has the
// page, the reason and a code.
func TestStatusReportsAnUnreadableCredentialsFile(t *testing.T) {
	t.Run("the page carries the error and the exit is 1", func(t *testing.T) {
		pinDispatchEnvironment(t)
		t.Cleanup(func() { api.SetKeyFromEnvironment(false) })
		// No project is linked here, so the page ends at the site line before
		// any request and needs no server.
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
		if !strings.Contains(got, "Site:         (none)") {
			t.Errorf("stdout:\n%q\nwant the page rendered to its last line", got)
		}
	})

	// The variable outranks the file and the file is then left unread, so
	// nothing it holds reaches the page or the exit code. The Credentials line
	// pins the precedence, since the page asks APIKeyFromEnvironment directly,
	// and the exit 0 over a file that cannot be parsed pins that the file
	// decides nothing.
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
			"Site:         (none)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("stdout:\n%q\nwant a line %q", got, want)
			}
		}
	})

	// With a site linked and no key, the page still ends at the site line, but
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
		if !strings.Contains(got, "Site:         site123") {
			t.Errorf("stdout:\n%q\nwant the site line carrying the site ID", got)
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
		for _, want := range []string{"Logged in:    no", "Site:         (none)"} {
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
		for _, want := range []string{"Logged in:    no", "Site:         site123"} {
			if !strings.Contains(got, want) {
				t.Errorf("stdout:\n%q\nwant a line %q", got, want)
			}
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
		if got := stdout.String(); !strings.Contains(got, "Site:         site123") {
			t.Errorf("stdout:\n%q\nwant the site line carrying the site ID", got)
		}
		if got := stderr.String(); !strings.Contains(got, "The key came from KAMAKIRI_API_KEY; check the value that variable holds") {
			t.Errorf("stderr:\n%q\nwant the copy naming the variable", got)
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
		if got := stdout.String(); !strings.Contains(got, "Site:         site123") {
			t.Errorf("stdout:\n%q\nwant the site line carrying the site ID", got)
		}
		if line := stderr.String(); !strings.HasPrefix(line, "request failed:") ||
			strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("stderr:\n%q\nwant one line starting with %q", line, "request failed:")
		}
	})
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
