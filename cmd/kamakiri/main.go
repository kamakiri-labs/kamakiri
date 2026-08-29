package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/cdn"
	"github.com/kamakiri-labs/kamakiri/internal/cliflags"
	"github.com/kamakiri-labs/kamakiri/internal/config"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/deploy"
	"github.com/kamakiri-labs/kamakiri/internal/deploys"
	"github.com/kamakiri-labs/kamakiri/internal/domain"
	"github.com/kamakiri-labs/kamakiri/internal/freshness"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	initcmd "github.com/kamakiri-labs/kamakiri/internal/init"
	"github.com/kamakiri-labs/kamakiri/internal/language"
	"github.com/kamakiri-labs/kamakiri/internal/login"
	"github.com/kamakiri-labs/kamakiri/internal/rollback"
	"github.com/kamakiri-labs/kamakiri/internal/status"
	"github.com/kamakiri-labs/kamakiri/internal/streamui"
	"github.com/kamakiri-labs/kamakiri/internal/subdomain"
	"github.com/kamakiri-labs/kamakiri/internal/teardown"
	"github.com/kamakiri-labs/kamakiri/internal/upgrade"
	versioncmd "github.com/kamakiri-labs/kamakiri/internal/version"
)

var version = "(dev)"

// nudgeTerminal reports whether the update nudge has a terminal to write to. It
// is a variable rather than a direct call so a test in this package can force it
// on and drive the dispatch end to end; production binds it to the real check
// and nothing else ever reassigns it. Left alone it is also what keeps every
// test that drives the dispatch with buffers silent, since a buffer is not a
// terminal.
var nudgeTerminal = streamui.IsTerminalWriter

const defaultBaseURL = "https://api.kamakiri-labs.jp/cloud"

// app holds the process surface the dispatch reads and writes: the argument
// tail, with the program name already stripped, and the three streams. Taking
// them from a value rather than from the os package is what lets a test drive
// the dispatch with arguments and streams of its own.
type app struct {
	args           []string
	stdin          io.Reader
	stdout, stderr io.Writer
}

// exitRequest is the panic value a.exit raises, carrying the process exit code
// out to run's recover.
type exitRequest int

// exit ends the command with the given code from anywhere in the dispatch. None
// of the helpers that call it has an error return, and each of the ones that
// return anything returns a value its caller needs, so carrying a code out
// through a signature would mean reshaping every one of them and every call
// site. A panic carries it instead: the unwind is immediate, no caller can
// carry on past it by mistake, and it stops at run's recover, which turns the
// code into run's result.
func (a *app) exit(code int) {
	panic(exitRequest(code))
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole dispatch and the seam a test drives in place of the process:
// it takes the argument tail and the three streams instead of reading the os
// package, and reports the exit code instead of calling os.Exit. The result is
// named because the deferred recover below assigns it; an unnamed one could not
// be set from a deferred function, and every helper exit would come out as 0.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	a := &app{args: args, stdin: stdin, stdout: stdout, stderr: stderr}

	// The helpers below exit by panicking with an exitRequest, which this turns
	// back into an exit code. Anything else is a genuine runtime panic and is
	// re-raised, so it keeps its stack trace and its exit status. Panic as an
	// exit mechanism is safe on two conditions this file holds to: this recover
	// is the only defer in it, so the unwind skips no cleanup, and none of its
	// functions runs inside another package's frame (no closures over them, no
	// function values of them handed out), so the unwind only ever passes
	// through frames of this file.
	defer func() {
		switch r := recover().(type) {
		case nil:
		case exitRequest:
			code = int(r)
		default:
			panic(r)
		}

		// Only after a command that is exiting 0, and only with a terminal to
		// write to. A command that failed already owns stderr, and a refusal
		// renders upgrade copy of its own; the arm above re-raises before
		// reaching here, so a panicking command never nudges either. Nudge
		// swallows everything, including its own panics, so nothing here can
		// change the code that has just been settled.
		if code == 0 && nudgeTerminal(a.stderr) {
			upgrade.Nudge(a.stderr, version, api.LatestAdvertised())
		}
	}()

	// Ahead of every api.NewClient call below, since each stamps the value
	// current at construction onto the client it returns. A client built before
	// this line states the dev token for its whole life, so a released binary
	// would report itself as an unstamped build for the rest of the command.
	api.SetVersion(version)

	// Ahead of anything that can print, the usage line below included. The
	// saved preference outranks the environment locale the catalog resolved on
	// its own at startup, so a keyed line emitted before this one would render
	// in a language the user has already overridden.
	i18n.Setup(persistedLanguage())

	if len(a.args) < 1 {
		a.printUsage()
		return 1
	}

	baseURL := os.Getenv("KAMAKIRI_API_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	switch a.args[0] {
	case "version", "--version", "-v":
		versioncmd.Run(a.stdout, version)
	case "language":
		if err := language.Run(a.args[1:], a.stdout); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	case "upgrade":
		// Placed with the cases that build no API client: an upgrade resolves
		// and downloads from the releases site alone, so it works with no
		// credentials and against a server that has stopped serving this
		// version.
		a.requireNoUpgradeArgs(a.args[1:])
		execPath, err := resolvedExecutable()
		if err != nil {
			// The only error here is the operating system's, with no localized
			// frame of its own, so it takes the prefix that class of error
			// carries everywhere else in this file.
			fmt.Fprintln(a.stderr, i18n.T("cmd.err_prefix"), err)
			return 1
		}
		if err := upgrade.Run(version, execPath, a.stdout); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	case "login":
		client := api.NewClient(baseURL)
		if err := login.Run(client, a.stdin, a.stdout); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	case "status":
		var verbose bool
		var recheck bool
		for i := 1; i < len(a.args); i++ {
			name, _, hasValue := cliflags.SplitArg(a.args[i])
			switch name {
			case "--verbose", "-v":
				a.requireNoValueOrExit(name, hasValue)
				verbose = true
			case "--recheck":
				a.requireNoValueOrExit(name, hasValue)
				recheck = true
			default:
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_status"))
				fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", a.args[i]))
				return 1
			}
		}
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		var drift bool
		var err error
		if recheck {
			drift, err = status.RunWithRecheck(a.stdout, version, baseURL, client, verbose)
		} else {
			drift, err = status.Run(a.stdout, version, baseURL, client, verbose)
		}
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		if drift {
			return 1
		}
	case "init":
		noWait := a.parseNoWait(a.args[1:], i18n.T("cmd.usage_init"))
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		if err := initcmd.Run(client, noWait, a.stdin, a.stdout); err != nil {
			// A sync timeout has already put its hint on stdout, so echoing
			// the error would double-report it. The exit stays nonzero: the
			// reconcile did not reach a terminal outcome.
			if !errors.Is(err, api.ErrSyncTimeout) {
				fmt.Fprintln(a.stderr, err)
			}
			return 1
		}
	case "teardown":
		teardownNoWait := a.parseNoWait(a.args[1:], i18n.T("cmd.usage_teardown"))
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		if err := teardown.Run(client, a.stdin, a.stdout, teardownNoWait); err != nil {
			// Alone among these commands, a teardown sync timeout exits 0: the
			// teardown is queued, converges server-side, and the local config
			// is already gone, so a slow but healthy one must not fail a
			// script.
			if errors.Is(err, api.ErrSyncTimeout) {
				return 0
			}
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	case "deploy":
		if len(a.args) < 2 {
			fmt.Fprintln(a.stderr, i18n.T("cmd.usage_deploy"))
			return 1
		}
		path := a.args[1]
		deployNoWait := a.parseNoWait(a.args[2:], i18n.T("cmd.usage_deploy"))
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		a.freshnessExit(deploy.Run(client, path, deployNoWait, a.stdout))
	case "deploys":
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		if err := deploys.Run(client, a.stdout); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	case "rollback":
		if len(a.args) < 2 {
			fmt.Fprintln(a.stderr, i18n.T("cmd.usage_rollback"))
			return 1
		}
		deployID := a.args[1]
		rollbackNoWait := a.parseNoWait(a.args[2:], i18n.T("cmd.usage_rollback"))
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		a.freshnessExit(rollback.Run(client, deployID, rollbackNoWait, a.stdout))
	case "cdn":
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		if len(a.args) < 2 {
			if err := cdn.Status(client, a.stdout); err != nil {
				fmt.Fprintln(a.stderr, err)
				return 1
			}
		} else {
			switch a.args[1] {
			case "status":
				// An explicit alias for the bare `kamakiri cdn` view: someone
				// who reaches for `cdn status` should not hit an error.
				a.requireNoExtraCDNArgs(a.args[2:])
				if err := cdn.Status(client, a.stdout); err != nil {
					fmt.Fprintln(a.stderr, err)
					return 1
				}
			case "cloudflare":
				noWait, extras := a.splitCDNNoWait(a.args[2:])
				a.requireNoExtraCDNArgs(extras)
				a.cdnExit(cdn.SetMode(client, "cloudflare", noWait, a.stdout))
			case "none":
				noWait, extras := a.splitCDNNoWait(a.args[2:])
				a.requireNoExtraCDNArgs(extras)
				a.cdnExit(cdn.SetMode(client, "none", noWait, a.stdout))
			case "cleanup":
				noWait, extras := a.splitCDNNoWait(a.args[2:])
				a.requireNoExtraCleanupArgs(extras)
				a.cdnExit(cdn.Cleanup(client, noWait, a.stdout))
			case "webaccel":
				a.runCDNWebAccel(client)
			case "verify":
				a.requireNoExtraVerifyArgs(a.args[2:])
				a.cdnExit(cdn.Verify(client, a.stdout))
			case "credentials":
				a.runCDNCredentials(client)
			case "purge":
				noWait, extras := a.splitCDNNoWait(a.args[2:])
				a.requireNoExtraPurgeArgs(extras)
				a.cdnExit(cdn.Purge(client, noWait, a.stdout))
			default:
				fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unknown_cdn_command", a.args[1]))
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_cdn"))
				return 1
			}
		}
	case "domain":
		if len(a.args) < 2 {
			// Among the site verbs `verify` is listed first because it is the
			// fast path, not because of any alphabetical order.
			fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain"))
			return 1
		}
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		switch a.args[1] {
		case "register":
			// Account-scoped: it registers ownership, so it needs no linked
			// project, unlike the site verbs below.
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_register"))
				return 1
			}
			noWait := a.parseNoWait(a.args[3:], i18n.T("cmd.usage_domain_register_line"))
			err := domain.Register(client, a.args[2], noWait, a.stdout)
			if errors.Is(err, domain.ErrWatchInterrupted) {
				return 130
			}
			if err != nil {
				fmt.Fprintln(a.stderr, i18n.T("cmd.err_prefix"), err)
				return 1
			}
		case "unregister":
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_unregister"))
				return 1
			}
			a.parseDomainUnregisterArgs(a.args[3:])
			if err := domain.Unregister(client, a.args[2], a.stdout); err != nil {
				fmt.Fprintln(a.stderr, i18n.T("cmd.err_prefix"), err)
				return 1
			}
		case "verify":
			// Deliberately not routed through domainExit: verify has no
			// pre-watch sync, so it can yield neither ErrSyncTimeout nor
			// ErrOurSideShown, and its stderr carries the plain `Error:`
			// prefix rather than domainExit's bare echo.
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_verify"))
				return 1
			}
			noWait := a.parseNoWait(a.args[3:], i18n.T("cmd.usage_domain_verify_line"))
			err := domain.Verify(client, a.args[2], noWait, a.stdout)
			if errors.Is(err, domain.ErrWatchInterrupted) {
				return 130
			}
			if err != nil {
				fmt.Fprintln(a.stderr, i18n.T("cmd.err_prefix"), err)
				return 1
			}
		case "set":
			// `domain set` carries no CDN flag: the server provisions the
			// per-domain CDN resources on its own.
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_set"))
				return 1
			}
			noWait := a.parseNoWait(a.args[3:], i18n.T("cmd.usage_domain_set_line"))
			a.domainExit(domain.Set(client, a.args[2], noWait, a.stdout))
		case "unset":
			noWait := a.parseNoWait(a.args[2:], i18n.T("cmd.usage_domain_unset"))
			a.domainExit(domain.Unset(client, noWait, a.stdout))
		case "add":
			a.runDomainAdd(client)
		case "remove":
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_remove"))
				return 1
			}
			noWait := a.parseNoWait(a.args[3:], i18n.T("cmd.usage_domain_remove_line"))
			a.domainExit(domain.Remove(client, a.args[2], noWait, a.stdout))
		case "list":
			// Account-scoped, and about registrations only: the per-site
			// domain view lives in `kamakiri status`.
			if err := domain.ListRegistrations(client, a.stdout); err != nil {
				fmt.Fprintln(a.stderr, err)
				return 1
			}
		default:
			fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unknown_domain_command", a.args[1]))
			return 1
		}
	case "subdomain":
		if len(a.args) < 2 {
			fmt.Fprintln(a.stderr, i18n.T("cmd.usage_subdomain"))
			return 1
		}
		client := api.NewClient(baseURL)
		a.setAPIKey(client)
		switch a.args[1] {
		case "get":
			if err := subdomain.Get(client, a.stdout); err != nil {
				fmt.Fprintln(a.stderr, err)
				return 1
			}
		case "set":
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_subdomain_set"))
				return 1
			}
			var noWait bool
			var positional string
			for i := 2; i < len(a.args); i++ {
				name, _, hasValue := cliflags.SplitArg(a.args[i])
				switch name {
				case "--no-wait":
					a.requireNoValueOrExit(name, hasValue)
					noWait = true
				default:
					if positional != "" {
						fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", a.args[i]))
						return 1
					}
					positional = a.args[i]
				}
			}
			if positional == "" {
				fmt.Fprintln(a.stderr, i18n.T("cmd.usage_subdomain_set"))
				return 1
			}
			if err := subdomain.Set(client, positional, noWait, a.stdout); err != nil {
				// A sync timeout's hint is already on stdout, so it is not
				// echoed here or in the two toggles below; the exit stays
				// nonzero all the same.
				if !errors.Is(err, api.ErrSyncTimeout) {
					fmt.Fprintln(a.stderr, err)
				}
				return 1
			}
		case "disable":
			noWait := a.parseNoWait(a.args[2:], i18n.Tf("cmd.usage_subdomain_toggle", "disable"))
			if err := subdomain.Disable(client, noWait, a.stdout); err != nil {
				if !errors.Is(err, api.ErrSyncTimeout) {
					fmt.Fprintln(a.stderr, err)
				}
				return 1
			}
		case "enable":
			noWait := a.parseNoWait(a.args[2:], i18n.Tf("cmd.usage_subdomain_toggle", "enable"))
			if err := subdomain.Enable(client, noWait, a.stdout); err != nil {
				if !errors.Is(err, api.ErrSyncTimeout) {
					fmt.Fprintln(a.stderr, err)
				}
				return 1
			}
		default:
			fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unknown_subdomain_command", a.args[1]))
			return 1
		}
	default:
		fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unknown_command", a.args[0]))
		return 1
	}

	return 0
}

func (a *app) runDomainAdd(client *api.Client) {
	if len(a.args) < 3 {
		fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_add"))
		a.exit(1)
	}
	domainName := a.args[2]
	role := "redirect"
	redirectStatus := 301
	noWait := false

	for i := 3; i < len(a.args); i++ {
		name, value, hasValue := cliflags.SplitArg(a.args[i])
		switch name {
		case "--alias":
			a.requireNoValueOrExit(name, hasValue)
			role = "alias"
			redirectStatus = 0
		case "--redirect":
			a.requireNoValueOrExit(name, hasValue)
			role = "redirect"
		case "--status":
			v, next := a.flagValueOrExit(a.args, i, hasValue, value, "--status", i18n.T("cmd.hint_redirect_status"))
			i = next
			redirectStatus = a.parseRedirectStatus(v)
		case "--no-wait":
			a.requireNoValueOrExit(name, hasValue)
			noWait = true
		default:
			fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unknown_flag", a.args[i]))
			a.exit(1)
		}
	}

	if role == "alias" && redirectStatus != 0 {
		fmt.Fprintln(a.stderr, i18n.T("cmd.err_alias_status_exclusive"))
		a.exit(1)
	}

	a.domainExit(domain.Add(client, domainName, role, redirectStatus, noWait, a.stdout))
}

// domainExit maps a domain-verb result to a process exit code. It is shared by
// `domain set/add/unset/remove`, so their matrices are identical by
// construction:
//
//	nil                 → 0   live, or the --no-wait async submit, where
//	                          DNS not yet live is the expected state
//	ErrWatchInterrupted → 130 Ctrl-C; the detach copy is already on stdout
//	ErrSyncTimeout      → 1   pre-watch sync timeout, interactive path only;
//	                          the ⧗ resume hint is already on stdout
//	ErrOurSideShown     → 1   pre-watch sync failure; the ✗ reason is already
//	                          on stdout
//	other               → 1   transport or API error; echo to stderr
func (a *app) domainExit(err error) {
	code, echo := domainExitCode(err)
	if echo {
		fmt.Fprintln(a.stderr, err)
	}
	a.exit(code)
}

// domainExitCode is the pure core of domainExit, returning the exit code and
// whether to echo err to stderr.
func domainExitCode(err error) (code int, echoStderr bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, domain.ErrWatchInterrupted):
		return 130, false
	case errors.Is(err, api.ErrSyncTimeout), errors.Is(err, domain.ErrOurSideShown):
		// The reason is already on stdout, so nonzero without an echo: CI
		// still sees the failure, the user does not read it twice.
		return 1, false
	default:
		return 1, true
	}
}

// parseDomainUnregisterArgs rejects every flag and extra positional. Alone
// among the state-changing verbs, `domain unregister` is synchronous: it drops
// a registration record and tears down no DNS or certificate, so there is
// nothing to wait on.
func (a *app) parseDomainUnregisterArgs(args []string) {
	if len(args) > 0 {
		fmt.Fprintln(a.stderr, i18n.T("cmd.usage_domain_unregister_line"))
		fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", args[0]))
		a.exit(1)
	}
}

func (a *app) flagValueOrExit(args []string, i int, hasValue bool, embedded, name, hint string) (string, int) {
	v, next, err := cliflags.Value(args, i, hasValue, embedded, name, hint)
	if err != nil {
		fmt.Fprintln(a.stderr, i18n.T("cmd.err_prefix"), err)
		a.exit(1)
	}
	return v, next
}

// requireNoValueOrExit rejects a value attached to a boolean flag, as in
// `--alias=true`, and exits rather than returning the error.
func (a *app) requireNoValueOrExit(name string, hasValue bool) {
	if err := cliflags.RequireNoValue(name, hasValue); err != nil {
		fmt.Fprintln(a.stderr, i18n.T("cmd.err_prefix"), err)
		a.exit(1)
	}
}

// parseNoWait parses an argument tail whose only accepted flag is `--no-wait`,
// the whole flag surface of most of the state-changing verbs, and reports
// whether it was given. The caller slices off its own positionals first, so the
// knowledge of where they end stays next to where they are read, and passes the
// verb's usage already rendered, since the catalog coverage gate requires a
// literal key at every lookup. Anything other than `--no-wait` ends the command
// rather than returning: the usage and the offending argument go to stderr and
// the process exits 1.
func (a *app) parseNoWait(args []string, usage string) bool {
	var noWait bool
	for i := 0; i < len(args); i++ {
		name, _, hasValue := cliflags.SplitArg(args[i])
		switch name {
		case "--no-wait":
			a.requireNoValueOrExit(name, hasValue)
			noWait = true
		default:
			fmt.Fprintln(a.stderr, usage)
			fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", args[i]))
			a.exit(1)
		}
	}
	return noWait
}

func (a *app) parseRedirectStatus(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || !config.ValidStatus(n) {
		fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_invalid_status", raw))
		a.exit(1)
	}
	return n
}

// runCDNWebAccel parses `kamakiri cdn webaccel` (--token, --secret, --yes,
// --no-wait) and dispatches to the cdn package. A missing token or secret falls
// back to the environment, then to credentials the server already holds, then
// to an interactive prompt.
func (a *app) runCDNWebAccel(client *api.Client) {
	noWait, args := a.splitCDNNoWait(a.args[2:])
	opts, err := cdn.ParseWebAccelArgs(args)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		a.exit(1)
	}

	a.cdnExit(cdn.SetWebAccel(client, opts, noWait, a.stdin, a.stdout))
}

// runCDNCredentials parses `kamakiri cdn credentials` (--token, --secret,
// --no-wait) and dispatches to the cdn package. It rejects --domain-id: this
// command rotates authentication and nothing else. A missing token or secret
// falls back to the environment, then to a hidden interactive prompt.
func (a *app) runCDNCredentials(client *api.Client) {
	noWait, args := a.splitCDNNoWait(a.args[2:])
	opts, err := cdn.ParseCredentialsArgs(args)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		a.exit(1)
	}
	a.cdnExit(cdn.RotateCredentials(client, opts, noWait, a.stdin, a.stdout))
}

// cdnExit maps a CDN-verb result to a process exit code, for the verbs that can
// block on the go-live watch or the pre-watch sync:
//
//	nil                    → 0   live, queued or disabled
//	ErrCdnWatchInterrupted,      Ctrl-C; the detach copy is already on stdout.
//	  domain.ErrWatchInterrupted, Three sentinels because three waits can be
//	  freshness.ErrInterrupted    interrupted: cdn's own go-live watch, the
//	                       → 130  domain-package watches `cdn none` runs, and
//	                              `cdn purge`'s freshness wait.
//	ErrSyncTimeout,        → 1   the reason is already on stdout, whether a
//	  ErrCdnReported,             queued hint, a ✗ breakdown or rejected
//	  freshness.ErrFlushBlocked   credentials
//	other                  → 1   transport or API error; echo to stderr
func (a *app) cdnExit(err error) {
	code, echo := cdnExitCode(err)
	if echo {
		fmt.Fprintln(a.stderr, err)
	}
	a.exit(code)
}

// cdnExitCode is the pure core of cdnExit, returning the exit code and whether
// to echo err to stderr.
func cdnExitCode(err error) (code int, echoStderr bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, cdn.ErrCdnWatchInterrupted), errors.Is(err, domain.ErrWatchInterrupted),
		errors.Is(err, freshness.ErrInterrupted):
		return 130, false
	case errors.Is(err, api.ErrSyncTimeout), errors.Is(err, cdn.ErrCdnReported),
		errors.Is(err, freshness.ErrFlushBlocked):
		return 1, false
	default:
		return 1, true
	}
}

// freshnessExit maps a freshness-wait result, from `deploy` or `rollback`, to a
// process exit code. It stays separate from domainExit and cdnExit rather than
// merging with them, since one table over the union of all three families'
// sentinels would document none of them:
//
//	nil                       → 0   the site is live, or the --no-wait async
//	                                submit returned
//	freshness.ErrInterrupted  → 130 Ctrl-C; the detach copy is already on stdout
//	freshness.ErrFlushBlocked → 1   a terminal flush block; the actionable ✗
//	                                entry is already on stdout
//	other                     → 1   a failure before the wait, such as a
//	                                transport error or a rejected upload;
//	                                echo to stderr
func (a *app) freshnessExit(err error) {
	code, echo := freshnessExitCode(err)
	if echo {
		fmt.Fprintln(a.stderr, err)
	}
	a.exit(code)
}

// freshnessExitCode is the pure core of freshnessExit, returning the exit code
// and whether to echo err to stderr.
func freshnessExitCode(err error) (code int, echoStderr bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, freshness.ErrInterrupted):
		return 130, false
	case errors.Is(err, freshness.ErrFlushBlocked):
		// The actionable reason is already committed to stdout; echoing it would
		// double-report the same failure.
		return 1, false
	default:
		return 1, true
	}
}

// requireNoUpgradeArgs rejects every argument to `upgrade`. The command takes
// the one release the site names as latest and nothing else, so it has no flag
// and no positional to parse.
func (a *app) requireNoUpgradeArgs(extras []string) {
	if len(extras) == 0 {
		return
	}
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_upgrade"))
	fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", extras[0]))
	a.exit(1)
}

// resolvedExecutable returns the path of the running binary, absolute and with
// every symlink followed. Both are the upgrade path's contract rather than
// tidiness: os.Executable does not promise an absolute path, and only following
// the symlinks lands on the real file, which is what decides both how the
// install is recognized and which file is replaced. It is resolved here rather
// than inside the upgrade package so a test can drive that package at a binary
// of its own choosing instead of at the test binary.
func resolvedExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// requireNoExtraVerifyArgs rejects every argument to `cdn verify`. The command
// re-attaches to a watch and changes nothing, so it takes no flags at all, not
// even --no-wait.
func (a *app) requireNoExtraVerifyArgs(extras []string) {
	if len(extras) == 0 {
		return
	}
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_cdn_verify"))
	fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", extras[0]))
	a.exit(1)
}

// splitCDNNoWait pulls the optional `--no-wait` out of the `cdn` argument tail
// and returns it with the remaining arguments. The split exists because
// ParseWebAccelArgs knows only webaccel's own flags; every other mode requires
// an empty remainder and treats anything left as a usage error.
func (a *app) splitCDNNoWait(args []string) (bool, []string) {
	remainder := make([]string, 0, len(args))
	var noWait bool
	for i := 0; i < len(args); i++ {
		name, _, hasValue := cliflags.SplitArg(args[i])
		if name == "--no-wait" {
			a.requireNoValueOrExit(name, hasValue)
			noWait = true
			continue
		}
		remainder = append(remainder, args[i])
	}
	return noWait, remainder
}

// requireNoExtraCDNArgs exits with the shared cdn usage line when anything is
// left after the `--no-wait` split. The line covers the whole mode surface
// rather than the one mode that was typed, so a mistyped flag on any of them
// shows all three.
func (a *app) requireNoExtraCDNArgs(extras []string) {
	if len(extras) == 0 {
		return
	}
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_cdn_mode"))
	fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", extras[0]))
	a.exit(1)
}

// requireNoExtraPurgeArgs is requireNoExtraCDNArgs for `cdn purge`, whose usage
// hint names its own verb rather than the mode-set line.
func (a *app) requireNoExtraPurgeArgs(extras []string) {
	if len(extras) == 0 {
		return
	}
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_cdn_purge"))
	fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", extras[0]))
	a.exit(1)
}

// requireNoExtraCleanupArgs is requireNoExtraCDNArgs for `cdn cleanup`, which
// takes only `--no-wait`.
func (a *app) requireNoExtraCleanupArgs(extras []string) {
	if len(extras) == 0 {
		return
	}
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_cdn_cleanup"))
	fmt.Fprintln(a.stderr, i18n.Tf("cmd.err_unexpected_argument", extras[0]))
	a.exit(1)
}

// persistedLanguage returns the language preference saved on this machine, or
// an empty string when there is none to read. Unlike setAPIKey below it cannot
// fail the command: an unreadable settings file means no preference, and the
// language then falls back to the environment locale.
func persistedLanguage() string {
	if settings := core.LoadSettings(); settings != nil {
		return settings.Language
	}
	return ""
}

func (a *app) setAPIKey(client *api.Client) {
	creds, err := core.LoadCredentials()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		a.exit(1)
	}
	if creds != nil {
		client.APIKey = creds.APIKey
	}
}

func (a *app) printUsage() {
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_synopsis"))
	fmt.Fprintln(a.stderr, "")
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_commands_header"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_login"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_status"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_init"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_deploy"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_deploys"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_rollback"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_teardown"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_cdn"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_domain"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_subdomain"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_version"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_language"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_command_upgrade"))
	fmt.Fprintln(a.stderr, "")
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_env_header"))
	fmt.Fprintln(a.stderr, i18n.T("cmd.usage_env_cdn"))
}
