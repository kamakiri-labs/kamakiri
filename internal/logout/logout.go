// Package logout implements `kamakiri logout`, which revokes the key this
// machine is logged in with and then removes the saved copy of it.
//
// The invariant it keeps: the saved copy of a key that may still work is never
// removed. The file goes only once the server has revoked the key or has
// refused it as invalid; on any other outcome it stays, so the user still holds
// the key, and a second run can settle what the first could not.
package logout

import (
	"errors"
	"fmt"
	"io"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
	"github.com/kamakiri-labs/kamakiri/internal/keyname"
)

// APIClient is the API surface logout needs.
type APIClient interface {
	RevokeCurrentKey() (*api.KeyInfo, error)
}

// Run implements `kamakiri logout` for the credential the caller loaded, nil
// when there is none. A failed load is the caller's to report: Run is handed
// only what loaded.
//
// A key from KAMAKIRI_API_KEY is refused with nothing sent, since the variable
// is not this command's to change and the file beside it is not the key in use.
// With no credential there is nothing to log out of, which is not an error.
//
// Only the API's own unauthorized refusal counts as the key being invalid. A
// 401 whose body is not the API's reaches here as a bare status line and proves
// nothing about the key, since anything between this machine and the server can
// answer one; it goes, with a transport failure, a 5xx, a success whose body
// does not parse, a redirect the client did not follow and an answer that names
// no key (an empty object), to the branch that keeps the file, because the
// revoke may or may not have happened. A version refusal is returned
// unchanged, without the line pointing at a second run, since that run would be
// refused the same way.
//
// A deploy key in the credentials file is refused the revoke as it is every key
// route but the one that describes it, so it cannot revoke itself and stays
// valid: the file is kept, by the invariant above, and the error names the key
// and says to revoke it with a login key, again with no line pointing at a
// second run.
func Run(client APIClient, creds *core.Credentials, out io.Writer) error {
	if creds != nil && creds.Source == core.SourceEnvironment {
		return errors.New(i18n.T("logout.err_key_from_environment"))
	}
	if creds == nil {
		fmt.Fprintln(out, i18n.T("logout.not_logged_in"))
		return nil
	}

	revoked, err := client.RevokeCurrentKey()
	if err != nil {
		var apiErr *api.ErrorResponse
		switch {
		case errors.Is(err, api.ErrUpgradeRequired):
			return err
		case errors.As(err, &apiErr) && apiErr.Code == "unauthorized":
			if err := core.DeleteCredentials(); err != nil {
				return fmt.Errorf("%s: %w", i18n.T("logout.err_delete_invalid_key"), err)
			}
			fmt.Fprintln(out, i18n.T("logout.already_invalid"))
			return nil
		case errors.As(err, &apiErr) && apiErr.Code == "deploy_key_refused":
			return deployKeyKept(apiErr.Key)
		default:
			// Two lines: the first is what every command prints for this
			// failure, and the second is what it means for the key.
			return fmt.Errorf("%w\n%s", api.MapError(err), i18n.T("logout.err_may_still_be_valid"))
		}
	}

	name := keyname.Escape(revoked.Name)
	if err := core.DeleteCredentials(); err != nil {
		return fmt.Errorf("%s: %w", i18n.Tf("logout.err_delete_revoked_key", name), err)
	}
	fmt.Fprintln(out, i18n.Tf("logout.logged_out", name))
	return nil
}

// deployKeyKept is the error for a deploy key that could not revoke itself. The
// service always names the key; a body that does not gets the line that names
// no key rather than one with empty slots.
func deployKeyKept(key *api.KeyInfo) error {
	if key == nil || key.Name == "" || key.ID == "" {
		return errors.New(i18n.T("logout.err_deploy_key_generic"))
	}
	return errors.New(i18n.Tf("logout.err_deploy_key", keyname.Escape(key.Name), keyname.Escape(key.ID)))
}
