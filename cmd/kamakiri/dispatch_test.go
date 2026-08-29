package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
// resolves the language from the saved settings and setAPIKey reads the
// credentials file: without it a test would render in whatever language the
// developer saved, and hand their real API key to whatever it dispatched into.
// The four locale variables are cleared for the first of those reasons one step
// earlier: run re-resolves the language on every call, so the catalog TestMain
// pins does not survive it.
func pinDispatchEnvironment(t *testing.T) {
	t.Helper()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "")
	t.Setenv("KAMAKIRI_LANG", "")
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
