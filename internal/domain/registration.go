package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// regColumnGap separates two columns of the registration tables. They are laid
// out by hand rather than through text/tabwriter, which measures a cell in
// runes: it reads a Japanese header or status as half the columns a terminal
// gives it and pads against that, skewing every column after it.
const regColumnGap = 2

// The registration statuses. A registration proves control of a name and claims
// nothing exclusively, so two accounts can hold a pending one for the same name
// and neither spoils the other; only attaching a domain to a site is exclusive.
// Anything else on the wire counts as not yet verified.
const (
	RegStatusAwaiting = "awaiting"
	RegStatusVerified = "verified"
)

// Register implements `kamakiri domain register <name>`. It is account-scoped,
// so it needs no project and works from any directory: it requests the
// registration, prints the TXT record to publish, and by default blocks until
// the registration verifies.
//
// Registration is the prerequisite for attaching a domain to a site, not a
// go-live, so the terminal milestone points at `domain set` rather than a URL.
func Register(client APIClient, name string, noWait bool, out io.Writer) error {
	// Account-scoped, so no project is required, but a credential still is:
	// without the check the request goes out with no key and comes back blaming
	// an invalid one. `verify` reaches register below after its own project
	// check, which is why the check sits in the wrapper.
	if err := core.RequireCredentials(); err != nil {
		return err
	}

	interactive := !noWait && isTerminalWriter(out)
	return register(client, name, interactive, out)
}

// register is the testable core of Register.
func register(client APIClient, name string, interactive bool, out io.Writer) error {
	reg, err := client.RegisterDomain(name)
	if err != nil {
		return api.MapError(err)
	}

	// Already verified, either at this exact name or through an ancestor the
	// server returns in its place. Nothing to publish or wait for.
	if reg.Status == RegStatusVerified {
		// The comparison is against the name as the server normalized it, so
		// mixed-case or fully-qualified input still reads as the exact name
		// rather than as a name covered by an ancestor.
		if strings.EqualFold(strings.TrimRight(name, "."), reg.Name) {
			fmt.Fprintln(out, i18n.Tf("domain.reg_already_registered", reg.Name))
		} else {
			fmt.Fprintln(out, i18n.Tf("domain.reg_covered_by_ancestor", name, reg.Name))
		}
		fmt.Fprintln(out, i18n.Tf("domain.reg_next_set", name))
		return nil
	}

	printRegisterInstructions(out, reg)

	if !interactive {
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.Tf("domain.reg_requested", reg.Name, reg.Name))
		return nil
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.Tf("domain.reg_add_txt_resume", reg.Name))
	fmt.Fprintln(out)

	ctx, cancel := signalCtx()
	defer cancel()
	return watchRegisterFn(ctx, client, reg.Name, reg.TXT.Name, out)
}

// printRegisterInstructions renders the TXT record the user must publish. Its
// name is a sub-label, so this is always the easy kind of DNS change, even for
// a domain whose eventual delivery record is an apex A.
//
// It says to add rather than replace because a registration is not exclusive:
// another account may hold its own pending registration of the same name, and
// many registrar interfaces make replacing the value the easy action. Both
// values have to coexist, or whoever is overwritten never verifies. This is the
// one call site both the interactive and the `--no-wait` paths go through, so
// the warning reaches a scripted run too, which is the one nobody is watching.
func printRegisterInstructions(out io.Writer, reg *api.Registration) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.T("domain.reg_publish_header"))
	for _, line := range regTableLines([][3]string{
		{"  " + i18n.T("domain.reg_header_type"), i18n.T("domain.reg_header_name"), i18n.T("domain.reg_header_value")},
		{"  " + reg.TXT.Type, reg.TXT.Name, reg.TXT.Value},
	}) {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, i18n.T("domain.reg_keep_other_values"))
}

// regTableLines lays out a three-column block: the first two columns are padded
// to their widest cell across the block plus a fixed gap, and the third is left
// bare, so no line ends in whitespace. The measure is display width rather than
// runes, which is what keeps a Japanese header from starting its neighbour's
// column early.
func regTableLines(rows [][3]string) []string {
	first, second := 0, 0
	for _, r := range rows {
		first = max(first, i18n.Width(r[0]))
		second = max(second, i18n.Width(r[1]))
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, i18n.Pad(r[0], first+regColumnGap)+
			i18n.Pad(r[1], second+regColumnGap)+r[2])
	}
	return lines
}

var watchRegisterFn = watchRegisterCtx

func watchRegisterCtx(ctx context.Context, client APIClient, name, txtName string, out io.Writer) error {
	return registerWatch(client, name, txtName, out, watchOpts{
		pollSchedule: defaultPollSchedule,
		now:          time.Now,
		isTTY:        isTerminalWriter(out),
		ctx:          ctx,
	})
}

// registerWatch polls the account's registrations until `name` verifies, the
// context is cancelled, a version refusal aborts it, or a transport error
// exhausts the budget. It never gives up on a timer, and it stays a pure read:
// the priority window the initial registration request opened is what keeps the
// server probing.
//
// It returns nil once verified, ErrWatchInterrupted on Ctrl-C, and an error if
// the registration vanishes or the reads keep failing.
func registerWatch(client APIClient, name, txtName string, out io.Writer, opts watchOpts) error {
	start := opts.now()
	transientLines := 0

	const statusFetchBudget = 10
	consecutiveFailures := 0

	clear := func() {
		if transientLines > 0 && opts.isTTY {
			fmt.Fprintf(out, "\033[%dA\033[J", transientLines)
		}
		transientLines = 0
	}
	showTransient := func(lines ...string) {
		clear()
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
		transientLines = len(lines)
	}

	for {
		list, err := client.ListRegistrations()
		now := opts.now()
		interval := opts.pollSchedule(now.Sub(start))

		if err != nil {
			// A refused version is refused on every poll, and the give-up copy
			// points at `kamakiri domain list`, which the same floor refuses. The
			// frame is cleared so the upgrade message lands on a clean terminal.
			if errors.Is(err, api.ErrUpgradeRequired) {
				clear()
				return err
			}
			consecutiveFailures++
			clear()
			fmt.Fprintln(out, i18n.Tf("domain.status_fetch_failed", api.MapError(err)))
			if consecutiveFailures >= statusFetchBudget {
				return fmt.Errorf("%s: %w",
					i18n.Tf("domain.err_reg_fetch_give_up", consecutiveFailures),
					api.MapError(err))
			}
		} else {
			consecutiveFailures = 0
			reg := findRegistration(list, name)
			switch {
			case reg == nil:
				// The registration expired or was removed out of band. The reason
				// is returned rather than printed, so the command layer renders it
				// once on stderr.
				clear()
				return errors.New(i18n.Tf("domain.err_reg_gone", name, name))

			case reg.Status == RegStatusVerified:
				clear()
				fmt.Fprintln(out, i18n.Tf("domain.reg_verified", name))
				fmt.Fprintln(out, i18n.Tf("domain.reg_next_set", name))
				return nil

			default: // awaiting, or a status this CLI does not know
				showTransient(registerWaitingTick(txtName, humanizeElapsed(now.Sub(start))))
			}
		}

		select {
		case <-opts.ctx.Done():
			clear()
			printRegisterDetachCopy(out, name)
			return ErrWatchInterrupted
		case <-time.After(interval):
		}
	}
}

func registerWaitingTick(txtName, elapsed string) string {
	return i18n.Tf("domain.reg_tick_waiting", txtName, elapsed)
}

// printRegisterDetachCopy prints the interrupt block for the registration
// watch. It never points at the site-scoped `kamakiri status`, where a pending
// registration attached to no site is invisible.
func printRegisterDetachCopy(out io.Writer, name string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, i18n.Tf("domain.reg_detach_headline", name))
	fmt.Fprintln(out, i18n.Tf("domain.reg_detach_body", name, name))
}

func isErrorCode(err error, code string) bool {
	if apiErr, ok := err.(*api.ErrorResponse); ok {
		return apiErr.Code == code
	}
	return false
}

// hasPendingRegistration reports whether the account holds a registration at
// the exact name that has not verified. A transport failure reads as no pending
// registration, so `verify` falls back to its normal not-found handling rather
// than reporting the wrong problem.
func hasPendingRegistration(client APIClient, name string) bool {
	list, err := client.ListRegistrations()
	if err != nil {
		return false
	}
	reg := findRegistration(list, name)
	return reg != nil && reg.Status != RegStatusVerified
}

func findRegistration(list *api.RegistrationList, name string) *api.Registration {
	for i := range list.Registrations {
		if list.Registrations[i].Name == name {
			return &list.Registrations[i]
		}
	}
	return nil
}

// Unregister implements `kamakiri domain unregister <name>`. Nothing is torn
// down in DNS or on the edge, so unlike every other state-changing verb it is
// synchronous and takes no wait flag. An unknown name succeeds as a no-op, and
// a registration a site still attaches a host under fails instead.
func Unregister(client APIClient, name string, out io.Writer) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}

	if err := client.UnregisterDomain(name); err != nil {
		return api.MapError(err)
	}
	fmt.Fprintln(out, i18n.Tf("domain.unregister_done", name))
	return nil
}

// ListRegistrations implements `kamakiri domain list`. It is account-scoped and
// needs no project: every registration the account owns, with its status and the
// hosts attached under it. The per-site view lives in `kamakiri status`.
func ListRegistrations(client APIClient, out io.Writer) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}

	list, err := client.ListRegistrations()
	if err != nil {
		return api.MapError(err)
	}

	if len(list.Registrations) == 0 {
		fmt.Fprintln(out, i18n.T("domain.reg_list_empty"))
		return nil
	}

	rows := make([][3]string, 0, len(list.Registrations)+1)
	rows = append(rows, [3]string{
		i18n.T("domain.reg_col_domain"),
		i18n.T("domain.reg_col_status"),
		i18n.T("domain.reg_col_sites"),
	})
	for _, r := range list.Registrations {
		rows = append(rows, [3]string{
			r.Name,
			registrationStatusLabel(r.Status),
			formatAttachments(r.Attachments),
		})
	}
	for _, line := range regTableLines(rows) {
		fmt.Fprintln(out, line)
	}
	return nil
}

// registrationStatusLabel renders a status for the list view. One this CLI does
// not know is shown as the server sent it rather than hidden.
func registrationStatusLabel(status string) string {
	switch status {
	case RegStatusVerified:
		return i18n.T("domain.reg_status_verified")
	case RegStatusAwaiting:
		return i18n.T("domain.reg_status_awaiting")
	default:
		return status
	}
}

// formatAttachments renders the attachment column: each attached host with its
// role, and the status code where a redirect makes that meaningful. The roles
// are wire values the CLI echoes back, the "redirect" in "%s (redirect %d)"
// included, so the column stays in ASCII in every language.
func formatAttachments(attachments []api.RegistrationAttachment) string {
	if len(attachments) == 0 {
		return "-"
	}
	parts := make([]string, len(attachments))
	for i, a := range attachments {
		if a.Role == "redirect" && a.RedirectStatus > 0 {
			parts[i] = fmt.Sprintf("%s (redirect %d)", a.Host, a.RedirectStatus)
		} else {
			parts[i] = fmt.Sprintf("%s (%s)", a.Host, a.Role)
		}
	}
	return strings.Join(parts, ", ")
}
