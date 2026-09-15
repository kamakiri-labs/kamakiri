package login

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

const secretTTL = 10 * time.Minute

// APIClient is the API surface the login flow needs.
type APIClient interface {
	Register(email string, tosAccepted bool, secretHash string) (*api.RegisterResponse, error)
	Verify(email, code, secret string) (*api.VerifyResponse, error)
}

// Run executes the login flow, reading from in and writing to out.
func Run(client APIClient, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)

	fmt.Fprint(out, i18n.T("login.prompt_email"))
	email, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	email = strings.TrimSpace(email)

	if _, parseErr := mail.ParseAddress(email); parseErr != nil {
		fmt.Fprintln(out, i18n.T("login.err_invalid_email"))
		return errors.New(i18n.T("login.err_aborted_invalid_email"))
	}

	fmt.Fprint(out, i18n.T("login.prompt_tos"))
	tosInput, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	tosInput = strings.TrimSpace(strings.ToLower(tosInput))
	if tosInput != "y" && tosInput != "" {
		fmt.Fprintln(out, i18n.T("login.err_tos_required"))
		return errors.New(i18n.T("login.err_aborted_tos_rejected"))
	}

	// The channel-binding secret ties this client to the verify step: the
	// server stores its hash at register and checks it at verify.
	secret, err := getOrCreateSecret(email)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("login.err_login_secret"), err)
	}

	secretHash := hashSecret(secret)

	_, err = client.Register(email, true, secretHash)
	if err != nil {
		apiErr, ok := err.(*api.ErrorResponse)
		if ok && apiErr.Code == "rate_limited" {
			// A valid code already exists, so skip ahead to the code prompt.
			fmt.Fprintln(out, i18n.T("login.code_already_sent"))
		} else {
			return handleAPIError(out, err)
		}
	} else {
		fmt.Fprintln(out, i18n.Tf("login.confirmation_sent", email))
	}

	for {
		fmt.Fprint(out, i18n.T("login.prompt_code"))
		code, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		code = strings.TrimSpace(code)

		resp, err := client.Verify(email, code, secret)
		if err != nil {
			apiErr, ok := err.(*api.ErrorResponse)
			if ok && apiErr.Code == "invalid_code" {
				fmt.Fprintln(out, i18n.T("login.err_invalid_code_retry"))
				continue
			}
			return handleAPIError(out, err)
		}

		// Delete the local secret before saving credentials: the server has
		// already consumed it on verify, so it serves no further purpose.
		_ = core.DeleteLoginSecret()

		if err := core.SaveCredentials(resp.APIKey, email); err != nil {
			return fmt.Errorf("%s: %w", i18n.T("login.err_save_credentials"), err)
		}

		credPath, _ := core.CredentialsPath()
		fmt.Fprintln(out, i18n.Tf("login.api_key_saved", credPath))
		// The key was minted and saved, and yet nothing about the next command
		// changes while the environment holds a key of its own, so say so rather
		// than leave the user to work out why the account did not switch.
		if _, fromEnvironment := core.APIKeyFromEnvironment(); fromEnvironment {
			fmt.Fprintln(out, i18n.T("login.env_override_note"))
		}
		return nil
	}
}

func getOrCreateSecret(email string) (string, error) {
	ls, err := core.LoadLoginSecret()
	if err == nil && ls != nil && ls.IsValid(email) {
		return ls.Secret.Token, nil
	}

	secret, err := generateSecret()
	if err != nil {
		return "", err
	}

	expiresAt := time.Now().Add(secretTTL)
	if err := core.SaveLoginSecret(email, secret, expiresAt); err != nil {
		return "", err
	}

	return secret, nil
}

func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("%s: %w", i18n.T("login.err_generate_secret"), err)
	}
	return hex.EncodeToString(b), nil
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func handleAPIError(out io.Writer, err error) error {
	// Ahead of the type assertion below: a version refusal is a sentinel rather
	// than a coded error response, so it would otherwise fall through to the
	// network copy and tell the user something false.
	if errors.Is(err, api.ErrUpgradeRequired) {
		fmt.Fprintln(out, api.ErrUpgradeRequired)
		return err
	}

	apiErr, ok := err.(*api.ErrorResponse)
	if !ok {
		fmt.Fprintln(out, i18n.T("login.err_network"))
		return err
	}

	switch apiErr.Code {
	case "invalid_email":
		fmt.Fprintln(out, i18n.T("login.err_invalid_email"))
	case "tos_required":
		fmt.Fprintln(out, i18n.T("login.err_tos_required"))
	case "invite_required":
		// The flow aborts here without ever prompting for a code, so the copy
		// must not send the user looking for a mail that was never sent.
		fmt.Fprintln(out, i18n.T("login.err_invite_required"))
	case "invalid_code":
		fmt.Fprintln(out, i18n.T("login.err_invalid_code"))
	case "too_many_attempts":
		fmt.Fprintln(out, i18n.T("login.err_too_many_attempts"))
	case "rate_limited":
		fmt.Fprintln(out, i18n.T("login.err_rate_limited"))
	case "auth_rate_limited":
		// Kept apart from `rate_limited`, which means a code is already in the
		// user's inbox: rendering a throttled request that way would send them
		// hunting for a mail that does not exist.
		fmt.Fprintln(out, i18n.T("login.err_auth_rate_limited"))
	default:
		fmt.Fprintln(out, i18n.T("login.err_server"))
	}

	return err
}
