// Package key implements the `kamakiri key` commands, which act on the API keys
// of the account the run's own key belongs to. They are account-scoped: no
// linked site is needed, so they run from any directory. `key create` reads the
// linked site only when no site is named on the command line.
package key

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/keyname"
)

// APIClient is the API surface the key commands need.
type APIClient interface {
	ListKeys() ([]api.Key, error)
	RenameKey(id, name string) (*api.Key, error)
	RevokeKey(id string) (*api.KeyInfo, error)
	CreateKey(name string, sites []string) (*api.CreatedKey, error)
	RotateKey(id string) (*api.RotatedKey, error)
}

// columnGap separates two columns of the listing, after the marker column. The
// table is laid out by hand rather than through text/tabwriter, which measures
// a cell in runes: a key name or a Japanese header holding wide characters
// would be read as half the columns a terminal gives it, and every column after
// it would be pushed out of line.
const columnGap = "  "

// currentMarker flags the row of the key the command ran with. It is a glyph
// rather than a word, so it reads the same in every language and the marker
// column stays one character wide.
const currentMarker = "*"

// loginKind is the kind of a key that acts on the whole account, the kind
// `kamakiri login` mints.
const loginKind = "login"

// List implements `kamakiri key list`: every key of the account, oldest first
// as the server orders them, with the key the command ran with marked. Ages are
// measured from now, which the caller passes so a test can pin it.
func List(client APIClient, out io.Writer, now time.Time) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}

	keys, err := client.ListKeys()
	if err != nil {
		return api.MapError(err)
	}

	rows := make([][]string, 0, len(keys)+1)
	markers := make([]string, 0, len(keys)+1)
	rows = append(rows, []string{
		i18n.T("key.header_id"),
		i18n.T("key.header_name"),
		i18n.T("key.header_kind"),
		i18n.T("key.header_sites"),
		i18n.T("key.header_created"),
		i18n.T("key.header_last_used"),
	})
	markers = append(markers, " ")
	marked := false
	for _, k := range keys {
		marker := " "
		if k.Current {
			marker = currentMarker
			marked = true
		}
		markers = append(markers, marker)
		rows = append(rows, []string{
			k.ID,
			keyname.Escape(k.Name),
			k.KindText(),
			sites(k),
			created(k.CreatedAt),
			lastUsed(k.LastUsedAt, now),
		})
	}

	// Each column is as wide as its widest cell, the header included. The last
	// column is never padded, so no line ends in whitespace.
	last := len(rows[0]) - 1
	widths := make([]int, last)
	for _, row := range rows {
		for i := range widths {
			widths[i] = max(widths[i], i18n.Width(row[i]))
		}
	}
	for r, row := range rows {
		cells := make([]string, len(row))
		for i := range widths {
			cells[i] = i18n.Pad(row[i], widths[i])
		}
		cells[last] = row[last]
		fmt.Fprintln(out, markers[r]+" "+strings.Join(cells, columnGap))
	}

	if marked {
		fmt.Fprintln(out)
		fmt.Fprintln(out, i18n.T("key.legend_current"))
	}
	return nil
}

// sites renders what a key may act on. A login key acts on the whole account;
// any other kind is limited to the sites it names, shown by the labels the
// server gave them, or by the resource IDs it names when the server gave no
// usable label.
func sites(k api.Key) string {
	if k.Kind == loginKind {
		return i18n.T("key.sites_all")
	}
	return k.SitesText()
}

// created renders the issue time in UTC to the minute. A value that does not
// parse is shown as the server sent it rather than replaced with a made-up
// time.
func created(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// lastUsed renders the last-use time as an age. A value that does not parse is
// shown as the server sent it, as created does.
func lastUsed(iso *string, now time.Time) string {
	if iso == nil {
		return i18n.T("key.last_used_never")
	}
	t, err := time.Parse(time.RFC3339, *iso)
	if err != nil {
		return *iso
	}
	return age(t, now)
}

// age renders how long before now t was, in whole units of the largest that
// fits: minutes under an hour, hours under a day, days under 30 days, months of
// 30 days under 365 days, and years of 365 days from there. Months stop at 11,
// so the last days of a year read as 11 months rather than 12. Under a minute
// reads as just now, and so does a t after now, which a difference between
// this machine's clock and the server's can produce.
func age(t, now time.Time) string {
	const day = 24 * time.Hour
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return i18n.T("key.age_just_now")
	case d < time.Hour:
		if n := int(d / time.Minute); n != 1 {
			return i18n.Tf("key.age_minutes", n)
		}
		return i18n.T("key.age_minute")
	case d < day:
		if n := int(d / time.Hour); n != 1 {
			return i18n.Tf("key.age_hours", n)
		}
		return i18n.T("key.age_hour")
	}
	days := int(d / day)
	switch {
	case days < 30:
		if days != 1 {
			return i18n.Tf("key.age_days", days)
		}
		return i18n.T("key.age_day")
	case days < 365:
		if n := min(days/30, 11); n != 1 {
			return i18n.Tf("key.age_months", n)
		}
		return i18n.T("key.age_month")
	default:
		if n := days / 365; n != 1 {
			return i18n.Tf("key.age_years", n)
		}
		return i18n.T("key.age_year")
	}
}

// CreateOptions is what the command line says about a `key create` run.
// NameGiven records that --name appeared at all, apart from its value, so an
// empty value reaches the name check and is refused there rather than read as
// no name. Sites holds each --site value in the order typed.
type CreateOptions struct {
	Name      string
	NameGiven bool
	Sites     []string
}

// Create implements `kamakiri key create`: it creates a deploy key bound to the
// sites named, or to the linked site when none is named, and hands the key over.
//
// The key alone goes to stdout, so the command can be piped straight into a
// secret store; the lines a person reads go to stderr. Nothing else ever
// reaches stdout: every failure is returned for the caller to print on stderr,
// so a pipe never receives an error line in place of a key.
//
// Without --name no name is sent, and the server names the key; the default is
// kept there alone. The name check and the site lookup come before anything is
// sent, so a refused name or a directory with nothing to bind costs no request.
func Create(client APIClient, opts CreateOptions, stdout, stderr io.Writer) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}
	var name string
	if opts.NameGiven {
		name = strings.TrimSpace(opts.Name)
		if !keyname.Valid(name) {
			return errors.New(i18n.T("key.err_invalid_name"))
		}
	}

	sites := opts.Sites
	linked := len(sites) == 0
	if linked {
		config, err := core.LoadProject()
		if err != nil {
			return err
		}
		if config == nil || config.ID == "" {
			return errors.New(i18n.T("key.err_create_no_site"))
		}
		sites = []string{config.ID}
	}

	created, err := client.CreateKey(name, sites)
	if err != nil {
		return createError(err, linked)
	}

	fmt.Fprintln(stderr, i18n.Tf("key.created_key", keyname.Escape(created.Key.Name), keyname.Escape(created.Key.ID)))
	fmt.Fprintln(stderr, i18n.Tf("key.created_sites", created.Key.SitesText()))
	fmt.Fprintln(stderr, i18n.T("key.created_shown_once"))
	fmt.Fprintln(stderr, i18n.T("key.created_notice"))
	// The key exists on the server and cannot be shown again, so a failed write
	// (a full disk behind a redirect, a closed pipe, which arrives here as an
	// error) must not read as success.
	// The error names the key ID, never the key, so it can be revoked.
	if _, err := fmt.Fprintln(stdout, created.APIKey); err != nil {
		return errors.New(i18n.Tf("key.err_create_key_not_written", err, keyname.Escape(created.Key.ID)))
	}
	return nil
}

// createError words a refused creation. When the key was to be bound to the
// linked site, an unmatched reference is that site's ID, which the server
// echoes expanded into a host the user never typed, so the line names the
// linked site instead of repeating the echo. Anything else is worded as every
// command words it.
func createError(err error, linked bool) error {
	var apiErr *api.ErrorResponse
	if linked && errors.As(err, &apiErr) && apiErr.Code == "key_site_not_found" {
		return errors.New(i18n.T("key.err_create_linked_site_not_found"))
	}
	return api.MapError(err)
}

// Rename implements `kamakiri key rename <id> <name>`. The name is trimmed and
// held to the rule the server applies, so a name it would refuse costs no
// request. On success it prints the ID and name the server stored.
func Rename(client APIClient, id, name string, out io.Writer) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}
	if err := checkID(id); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if !keyname.Valid(name) {
		return errors.New(i18n.T("key.err_invalid_name"))
	}

	renamed, err := client.RenameKey(id, name)
	if err != nil {
		return keyError(err, id)
	}
	fmt.Fprintln(out, i18n.Tf("key.renamed", renamed.ID, keyname.Escape(renamed.Name)))
	return nil
}

// Revoke implements `kamakiri key revoke <id>`. It asks nothing: the ID is
// typed in full, which is confirmation enough for one key. On success it prints
// the ID and name the server answered with.
func Revoke(client APIClient, id string, out io.Writer) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}
	if err := checkID(id); err != nil {
		return err
	}

	revoked, err := client.RevokeKey(id)
	if err != nil {
		return keyError(err, id)
	}
	fmt.Fprintln(out, i18n.Tf("key.revoked", keyname.Escape(revoked.Name), revoked.ID))
	return nil
}

// Rotate implements `kamakiri key rotate <id>`: it replaces the deploy key id
// with a new one of the same name and sites, which the server does in the same
// step that revokes the old one, and hands the new key over.
//
// The streams are used as Create uses them: the new key alone goes to stdout,
// so it can be piped into the secret store the old key sat in, and every
// failure is returned for the caller to print on stderr. The ID is checked
// before anything is sent.
func Rotate(client APIClient, id string, stdout, stderr io.Writer) error {
	if err := core.RequireCredentials(); err != nil {
		return err
	}
	if err := checkID(id); err != nil {
		return err
	}

	rotated, err := client.RotateKey(id)
	if err != nil {
		return rotateError(err, id)
	}

	// The typed ID has passed checkID, so it can stand in for an answer that
	// leaves the replaced key's ID out rather than print a line with a hole.
	replaced := rotated.Replaced.ID
	if replaced == "" {
		replaced = id
	}
	fmt.Fprintln(stderr, i18n.Tf("key.rotated_key", keyname.Escape(rotated.Key.Name), keyname.Escape(rotated.Key.ID), keyname.Escape(replaced)))
	fmt.Fprintln(stderr, i18n.Tf("key.created_sites", rotated.Key.SitesText()))
	fmt.Fprintln(stderr, i18n.T("key.created_shown_once"))
	fmt.Fprintln(stderr, i18n.T("key.created_notice"))
	// The new key cannot be shown again and the old one is already revoked, so
	// a failed write must not read as success: the line names the new key's ID,
	// never the key, so the user can rotate again by it.
	if _, err := fmt.Fprintln(stdout, rotated.APIKey); err != nil {
		return errors.New(i18n.Tf("key.err_rotate_key_not_written", err, keyname.Escape(rotated.Key.ID)))
	}
	return nil
}

// rotateError words a refused or failed rotation. A login key gets its own
// line, naming the typed ID, which has passed checkID and so is safe to repeat.
//
// A failure can leave it unknown whether the rotation happened: the server may
// have rotated the key and its answer then been lost, come back unreadable, or
// been replaced by one that is not the API's. The old key may then be dead with
// no new one in hand, so the line is followed by one saying the key may have
// been rotated and how to find out. Only the refusals named here are known:
// the server makes each of them before it changes anything. Every other
// failure is unknown, a coded answer with any other code included. The second
// line travels inside the returned error so the caller prints both together.
func rotateError(err error, id string) error {
	if errors.Is(err, api.ErrUpgradeRequired) {
		return err
	}
	var apiErr *api.ErrorResponse
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "key_not_found":
			return errors.New(i18n.Tf("key.err_not_found", id))
		case "key_not_rotatable":
			return errors.New(i18n.Tf("key.err_not_rotatable", id))
		case "unauthorized", "deploy_key_refused", "key_create_rate_limited":
			return api.MapError(err)
		}
	}
	return errors.Join(api.MapError(err), errors.New(i18n.T("key.rotate_maybe_done")))
}

// checkID refuses an argument that is not shaped like a key ID, 8-4-4-4-12 hex
// digits in either case, before it is put in a request path. That keeps a word
// such as "current" from reaching a route of its own instead of being read as an
// ID. The refusal never repeats the argument: the likeliest wrong argument is an
// API key itself, which would then land in a terminal's scrollback or a CI log.
func checkID(id string) error {
	if !isKeyID(id) {
		return errors.New(i18n.T("key.err_invalid_id"))
	}
	return nil
}

func isKeyID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// keyError words a refused key request. The server answers a key of another
// account exactly as one that does not exist, so key_not_found says only that
// this account holds no key with the ID typed, which has passed checkID and so
// is safe to repeat. current_key refuses a revoke of the key the request came
// with, and the way to end that key depends on where it came from: a key from
// the credentials file is ended by logging out, while one from KAMAKIRI_API_KEY
// has to be revoked with another key, since logging out on the machine running
// the command touches nothing of the variable's. The environment is asked
// directly, by the rule the credential loader applies, so the answer is where
// the run's key came from. Anything else is worded as every command words it.
func keyError(err error, id string) error {
	var apiErr *api.ErrorResponse
	if !errors.As(err, &apiErr) {
		return api.MapError(err)
	}
	switch apiErr.Code {
	case "key_not_found":
		return errors.New(i18n.Tf("key.err_not_found", id))
	case "current_key":
		if _, fromEnvironment := core.APIKeyFromEnvironment(); fromEnvironment {
			return errors.New(i18n.T("key.err_current_key_env"))
		}
		return errors.New(i18n.T("key.err_current_key"))
	}
	return api.MapError(err)
}
