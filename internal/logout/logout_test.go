package logout

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/core"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// testLang is the catalog every test here renders against. A test that switches
// away restores this rather than a literal of its own.
const testLang = "en"

// The catalog is pinned so the assertions below hold whatever locale the suite
// runs under; none of the tests is parallel, since the catalog is an
// unsynchronized map. KAMAKIRI_API_KEY is cleared because it outranks the
// credentials file: on a machine that exports it, a test that loads the key it
// just saved would get the developer's instead. There is no *testing.T here,
// hence os.Unsetenv rather than t.Setenv.
func TestMain(m *testing.M) {
	i18n.Load(testLang)
	os.Unsetenv(core.EnvAPIKey)
	os.Exit(m.Run())
}

// mayStillBeValid is the line every failure that leaves the key's fate unknown
// ends with.
const mayStillBeValid = `the key may still be valid and is still saved; run "kamakiri logout" again`

// fakeClient answers the revoke from fixed values. It counts the calls and
// records whether the credentials file was still on disk when the revoke was
// asked for, which is what holds the order of the revoke and the delete.
type fakeClient struct {
	revoked *api.KeyInfo
	err     error

	calls        int
	fileAtRevoke bool
}

func (f *fakeClient) RevokeCurrentKey() (*api.KeyInfo, error) {
	f.calls++
	f.fileAtRevoke = credentialsFileExists()
	return f.revoked, f.err
}

func credentialsFileExists() bool {
	path, err := core.CredentialsPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// loggedIn gives the test a config home holding a saved key and a login left
// half-finished, and returns the credential as the dispatch would load it.
func loggedIn(t *testing.T) *core.Credentials {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := core.SaveCredentials("kk_live_test", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := core.SaveLoginSecret("test@example.com", "token", time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	path, err := core.CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if savedCredentialsBytes, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	creds, err := core.LoadCredentials()
	if err != nil || creds == nil {
		t.Fatalf("LoadCredentials() = %+v, %v; want the saved key", creds, err)
	}
	return creds
}

func useLanguage(t *testing.T, lang string) {
	t.Helper()
	i18n.Load(lang)
	t.Cleanup(func() { i18n.Load(testLang) })
}

// savedCredentialsBytes is the credentials file as loggedIn wrote it, which
// assertFileKept compares against.
var savedCredentialsBytes []byte

func assertFileKept(t *testing.T) {
	t.Helper()
	path, err := core.CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("the credentials file is gone, want it kept: %v", err)
		return
	}
	if !bytes.Equal(got, savedCredentialsBytes) {
		t.Errorf("the credentials file = %q, want it unchanged: %q", got, savedCredentialsBytes)
	}
}

func assertFileRemoved(t *testing.T) {
	t.Helper()
	if credentialsFileExists() {
		t.Error("the credentials file is still there, want it removed")
	}
}

// The login secret is another command's state, so logout leaves it whatever
// happens to the key.
func assertLoginSecretKept(t *testing.T) {
	t.Helper()
	secret, err := core.LoadLoginSecret()
	if err != nil || secret == nil {
		t.Errorf("LoadLoginSecret() = %+v, %v; want the half-finished login kept", secret, err)
	}
}

func TestRunRevokesTheKeyThenRemovesTheFile(t *testing.T) {
	creds := loggedIn(t)
	client := &fakeClient{revoked: &api.KeyInfo{ID: "5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65", Name: "alexis-laptop"}}

	var out bytes.Buffer
	if err := Run(client, creds, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := "Logged out. The key alexis-laptop was revoked; any copy of it, in a CI secret for instance, no longer works.\n"
	if got := out.String(); got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
	if client.calls != 1 {
		t.Errorf("RevokeCurrentKey called %d times, want once", client.calls)
	}
	if !client.fileAtRevoke {
		t.Error("the credentials file was already gone when the revoke was sent, want it removed only after")
	}
	assertFileRemoved(t)
	assertLoginSecretKept(t)
}

func TestRunEscapesTheRevokedKeysName(t *testing.T) {
	creds := loggedIn(t)
	client := &fakeClient{revoked: &api.KeyInfo{ID: "5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65", Name: "laptop\x1b[2J‮"}}

	var out bytes.Buffer
	if err := Run(client, creds, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := `Logged out. The key laptop\u{001b}[2J\u{202e} was revoked; any copy of it, in a CI secret for instance, no longer works.` + "\n"
	if got := out.String(); got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
}

// The credential is read for its source, not looked up again: the variable is
// what the loader answered from, and the file next to it is left alone.
func TestRunWithAKeyFromTheEnvironmentChangesNothing(t *testing.T) {
	loggedIn(t)
	t.Setenv(core.EnvAPIKey, "kk_live_fromenv")
	creds, err := core.LoadCredentials()
	if err != nil || creds == nil || creds.Source != core.SourceEnvironment {
		t.Fatalf("LoadCredentials() = %+v, %v; want the environment's key", creds, err)
	}
	client := &fakeClient{revoked: &api.KeyInfo{Name: "github-ci"}}

	var out bytes.Buffer
	err = Run(client, creds, &out)

	want := `KAMAKIRI_API_KEY holds the key in use, so logout changes nothing. Unset it to stop using that key, or revoke it with "kamakiri key revoke" from another key`
	if err == nil || err.Error() != want {
		t.Fatalf("Run() error = %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing", out.String())
	}
	if client.calls != 0 {
		t.Errorf("RevokeCurrentKey called %d times, want never", client.calls)
	}
	assertFileKept(t)
	assertLoginSecretKept(t)
}

func TestRunWithNoCredentialSendsNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	client := &fakeClient{revoked: &api.KeyInfo{Name: "alexis-laptop"}}

	var out bytes.Buffer
	if err := Run(client, nil, &out); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := out.String(), "Not logged in.\n"; got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
	if client.calls != 0 {
		t.Errorf("RevokeCurrentKey called %d times, want never", client.calls)
	}
}

// The server's own refusal of the key proves the key dead, so the copy on disk
// is worthless and goes.
func TestRunRemovesTheFileWhenTheKeyWasAlreadyInvalid(t *testing.T) {
	creds := loggedIn(t)
	client := &fakeClient{err: &api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"}}

	var out bytes.Buffer
	if err := Run(client, creds, &out); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := out.String(), "Logged out. The saved key was already invalid.\n"; got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
	if !client.fileAtRevoke {
		t.Error("the credentials file was already gone when the revoke was sent, want it removed only after")
	}
	assertFileRemoved(t)
	assertLoginSecretKept(t)
}

// Any failure that does not prove the key dead leaves it saved: the revoke may
// or may not have reached the server, so the key may still work, and the copy on
// disk is what lets a second run settle it. The error is what every command
// prints for that failure, then the line that says so.
func TestRunKeepsTheFileWhenTheKeyMayStillBeValid(t *testing.T) {
	refused := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"a transport failure",
			fmt.Errorf("request failed: %w", refused),
			"request failed: dial tcp 127.0.0.1:1: connect: connection refused",
		},
		{
			"a 500 with the API's body",
			&api.ErrorResponse{Code: "internal_error", Message: "Internal server error"},
			"Internal server error",
		},
		{
			"a 500 with any other body",
			errors.New("server returned 500"),
			"server returned 500",
		},
		{
			// A 401 that is not the API's own refusal proves nothing about the
			// key: a proxy in front of the server can answer one.
			"a 401 with any other body",
			errors.New("server returned 401"),
			"server returned 401",
		},
		{
			// The revoke may have happened, so the key is not known to be valid
			// either; a second run then gets the 401 and removes the file.
			"a success whose body does not parse",
			fmt.Errorf("parse response: %w", errors.New("unexpected end of JSON input")),
			"parse response: unexpected end of JSON input",
		},
		{
			"an API refusal under another code",
			&api.ErrorResponse{Code: "key_not_found", Message: "Key not found."},
			"Key not found.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds := loggedIn(t)
			client := &fakeClient{err: tc.err}

			var out bytes.Buffer
			err := Run(client, creds, &out)

			if want := tc.want + "\n" + mayStillBeValid; err == nil || err.Error() != want {
				t.Fatalf("Run() error = %v, want %q", err, want)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("errors.Is(err, the client's error) = false through %q", err)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q, want nothing", out.String())
			}
			if client.calls != 1 {
				t.Errorf("RevokeCurrentKey called %d times, want once", client.calls)
			}
			assertFileKept(t)
			assertLoginSecretKept(t)
		})
	}
}

// A deploy key cannot revoke itself: the service refuses it the route, so the
// key stays valid and its saved copy stays with it. The line names the key and
// says how to end it; it carries no second line pointing at another run, which
// would be refused the same way.
func TestRunKeepsADeployKeyThatCannotRevokeItself(t *testing.T) {
	const keyID = "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
	sites := []api.LabeledSite{{ID: "k3j9x2m4p7q1w8e5", Label: "example.com"}}
	cases := []struct {
		name string
		lang string
		key  *api.KeyInfo
		want string
	}{
		{
			"a named key",
			"en",
			&api.KeyInfo{ID: keyID, Name: "github-actions"},
			`this is a deploy key ("github-actions", 3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40), which cannot revoke itself. The credentials file was kept. Revoke the key with "kamakiri key revoke <id>" using a login key`,
		},
		{
			"a named key, in Japanese",
			"ja",
			&api.KeyInfo{ID: keyID, Name: "github-actions"},
			`このAPIキーはデプロイキー「github-actions」(3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40) で、自分自身を失効させることはできません。認証情報ファイルはそのまま残しています。ログインキーを使って "kamakiri key revoke <id>" で失効させてください`,
		},
		{
			"a name to escape",
			"en",
			&api.KeyInfo{ID: keyID, Name: "ci‮box\x1b[2J"},
			`this is a deploy key ("ci\u{202e}box\u{001b}[2J", 3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40), which cannot revoke itself. The credentials file was kept. Revoke the key with "kamakiri key revoke <id>" using a login key`,
		},
		{
			"a name and an ID to escape, in Japanese",
			"ja",
			&api.KeyInfo{ID: "3f2a\u202e9c1e\x1b[2J", Name: "ci\u202ebox\x1b[2J"},
			`このAPIキーはデプロイキー「ci\u{202e}box\u{001b}[2J」(3f2a\u{202e}9c1e\u{001b}[2J) で、自分自身を失効させることはできません。認証情報ファイルはそのまま残しています。ログインキーを使って "kamakiri key revoke <id>" で失効させてください`,
		},
		{
			// The service always names the key; a body that does not still
			// gets a whole sentence.
			"a body that names no key",
			"en",
			nil,
			`this is a deploy key, which cannot revoke itself. The credentials file was kept. Revoke the key with "kamakiri key revoke <id>" using a login key`,
		},
		{
			"a body that names no key, in Japanese",
			"ja",
			nil,
			`このAPIキーはデプロイキーで、自分自身を失効させることはできません。認証情報ファイルはそのまま残しています。ログインキーを使って "kamakiri key revoke <id>" で失効させてください`,
		},
		{
			"a key with no name",
			"en",
			&api.KeyInfo{ID: keyID},
			`this is a deploy key, which cannot revoke itself. The credentials file was kept. Revoke the key with "kamakiri key revoke <id>" using a login key`,
		},
		{
			"a key with no ID",
			"en",
			&api.KeyInfo{Name: "github-actions"},
			`this is a deploy key, which cannot revoke itself. The credentials file was kept. Revoke the key with "kamakiri key revoke <id>" using a login key`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds := loggedIn(t)
			useLanguage(t, tc.lang)
			client := &fakeClient{err: &api.ErrorResponse{Code: "deploy_key_refused", Message: "This is a deploy key.", Key: tc.key, Sites: sites}}

			var out bytes.Buffer
			err := Run(client, creds, &out)

			if err == nil || err.Error() != tc.want {
				t.Fatalf("Run() error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q, want nothing", out.String())
			}
			if client.calls != 1 {
				t.Errorf("RevokeCurrentKey called %d times, want once", client.calls)
			}
			assertFileKept(t)
			assertLoginSecretKept(t)
		})
	}
}

// A version refusal is returned as it is, with no tail: running logout again
// would be refused the same way, so pointing at it would send the user round in
// a circle.
func TestRunReturnsAVersionRefusalUnchanged(t *testing.T) {
	creds := loggedIn(t)
	client := &fakeClient{err: api.ErrUpgradeRequired}

	var out bytes.Buffer
	err := Run(client, creds, &out)

	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("Run() error = %v, want errors.Is(err, api.ErrUpgradeRequired)", err)
	}
	if got, want := err.Error(), api.ErrUpgradeRequired.Error(); got != want {
		t.Errorf("Run() error = %q, want the upgrade line alone, %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing", out.String())
	}
	assertFileKept(t)
}

// Once the key is revoked or known dead, the file holds a key that no longer
// works, so failing to remove it is an error: the user is told what happened to
// the key and why the copy is still there.
func TestRunReportsAFileItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a directory whatever its mode says, so no removal here would fail")
	}
	cases := []struct {
		name   string
		lang   string
		client *fakeClient
		want   string
	}{
		{
			"after a revoke",
			"en",
			&fakeClient{revoked: &api.KeyInfo{Name: "alexis-laptop"}},
			"the key alexis-laptop was revoked, but the saved copy could not be removed: delete credentials: ",
		},
		{
			"after the server refused the key",
			"en",
			&fakeClient{err: &api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"}},
			"the saved key was already invalid, but it could not be removed: delete credentials: ",
		},
		{
			"after a revoke, in Japanese",
			"ja",
			&fakeClient{revoked: &api.KeyInfo{Name: "alexis-laptop"}},
			"キー「alexis-laptop」は失効させましたが、保存されていたコピーを削除できませんでした: 認証情報の削除: ",
		},
		{
			"after the server refused the key, in Japanese",
			"ja",
			&fakeClient{err: &api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"}},
			"保存されていたキーはすでに無効でしたが、削除できませんでした: 認証情報の削除: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds := loggedIn(t)
			useLanguage(t, tc.lang)
			dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kamakiri")
			if err := os.Chmod(dir, 0500); err != nil {
				t.Fatalf("sealing the config directory: %v", err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0700) })

			var out bytes.Buffer
			err := Run(tc.client, creds, &out)

			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Run() error = %v, want one starting %q", err, tc.want)
			}
			if !errors.Is(err, fs.ErrPermission) {
				t.Errorf("errors.Is(err, fs.ErrPermission) = false through %q", err)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q, want nothing", out.String())
			}
			if tc.client.calls != 1 {
				t.Errorf("RevokeCurrentKey called %d times, want once", tc.client.calls)
			}
			assertFileKept(t)
		})
	}
}

func TestRunInJapanese(t *testing.T) {
	cases := []struct {
		name        string
		environment bool
		loggedIn    bool
		client      *fakeClient
		stdout      string
		err         string
	}{
		{
			name:     "a revoke",
			loggedIn: true,
			client:   &fakeClient{revoked: &api.KeyInfo{Name: "alexis-laptop"}},
			stdout:   "ログアウトしました。キー「alexis-laptop」を失効させました。CIのシークレットに保存したものなど、このキーのコピーはどれも使えなくなりました。\n",
		},
		{
			name:     "a key already invalid",
			loggedIn: true,
			client:   &fakeClient{err: &api.ErrorResponse{Code: "unauthorized", Message: "invalid API key"}},
			stdout:   "ログアウトしました。保存されていたキーはすでに無効でした。\n",
		},
		{
			name:   "no credential",
			client: &fakeClient{},
			stdout: "ログインしていません。\n",
		},
		{
			name:        "a key from the environment",
			environment: true,
			client:      &fakeClient{},
			err:         `使用中のキーは環境変数 KAMAKIRI_API_KEY のものなので、logout では何も変更しません。このキーの使用をやめるには KAMAKIRI_API_KEY を解除するか、別のキーから "kamakiri key revoke" で失効させてください`,
		},
		{
			name:     "a failure that leaves the key's fate unknown",
			loggedIn: true,
			client:   &fakeClient{err: &api.ErrorResponse{Code: "internal_error", Message: "Internal server error"}},
			err:      "Internal server error\nキーはまだ有効な可能性があり、保存されたままです。\"kamakiri logout\" を再実行してください",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var creds *core.Credentials
			switch {
			case tc.loggedIn:
				creds = loggedIn(t)
			case tc.environment:
				creds = &core.Credentials{Version: 1, APIKey: "kk_live_fromenv", Source: core.SourceEnvironment}
			}
			useLanguage(t, "ja")

			var out bytes.Buffer
			err := Run(tc.client, creds, &out)

			if tc.err == "" && err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if tc.err != "" && (err == nil || err.Error() != tc.err) {
				t.Fatalf("Run() error = %v, want %q", err, tc.err)
			}
			if got := out.String(); got != tc.stdout {
				t.Errorf("printed %q, want %q", got, tc.stdout)
			}
		})
	}
}

// The branches above are driven with errors shaped like the client's. These
// drive the real client against a local server, so what decides the branch is
// what the client makes of each answer on the wire: only the API's own 401 body
// removes the file, while a 401 or a success whose body is not the API's keeps
// it. The handler checks the file as the request arrives, which holds the order
// over the wire as the fake does above.
func TestRunAgainstTheRealClient(t *testing.T) {
	const (
		removed = iota
		kept
		upgrade
	)
	cases := []struct {
		name   string
		status int
		body   string
		file   int
		stdout string
		err    string
	}{
		{
			name:   "a revoke",
			status: http.StatusOK,
			body:   `{"key":{"id":"5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65","name":"alexis-laptop"}}`,
			file:   removed,
			stdout: "Logged out. The key alexis-laptop was revoked; any copy of it, in a CI secret for instance, no longer works.\n",
		},
		{
			name:   "the API's 401",
			status: http.StatusUnauthorized,
			body:   `{"code":"unauthorized","message":"invalid API key"}`,
			file:   removed,
			stdout: "Logged out. The saved key was already invalid.\n",
		},
		{
			name:   "a deploy key's refusal",
			status: http.StatusForbidden,
			body:   `{"code":"deploy_key_refused","message":"This is a deploy key.","key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"github-actions"},"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}]}`,
			file:   kept,
			err:    `this is a deploy key ("github-actions", 3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40), which cannot revoke itself. The credentials file was kept. Revoke the key with "kamakiri key revoke <id>" using a login key`,
		},
		{
			name:   "a 401 that is not the API's",
			status: http.StatusUnauthorized,
			body:   "<html>401 Authorization Required</html>",
			file:   kept,
			err:    "server returned 401\n" + mayStillBeValid,
		},
		{
			name:   "a success whose body does not parse",
			status: http.StatusOK,
			body:   "<html>OK</html>",
			file:   kept,
			err:    "parse response: invalid character '<' looking for beginning of value\n" + mayStillBeValid,
		},
		{
			name:   "a redirect Go does not follow",
			status: http.StatusMovedPermanently,
			body:   `{"moved":true}`,
			file:   kept,
			err:    "server returned 301\n" + mayStillBeValid,
		},
		{
			name:   "a success that names no key",
			status: http.StatusOK,
			body:   `{}`,
			file:   kept,
			err:    "parse response: the response names no key\n" + mayStillBeValid,
		},
		{
			name:   "a 500",
			status: http.StatusInternalServerError,
			body:   "<html>500 Internal Server Error</html>",
			file:   kept,
			err:    "server returned 500\n" + mayStillBeValid,
		},
		{
			name:   "a version refusal",
			status: http.StatusUpgradeRequired,
			file:   upgrade,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds := loggedIn(t)

			var route, authorization string
			var fileAtRequest bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				route = r.Method + " " + r.URL.Path
				authorization = r.Header.Get("Authorization")
				fileAtRequest = credentialsFileExists()
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client := api.NewClient(server.URL)
			client.APIKey = creds.APIKey

			var out bytes.Buffer
			err := Run(client, creds, &out)
			// Close returns only once the handler's goroutine has finished,
			// which orders its writes against the reads below.
			server.Close()

			switch tc.file {
			case removed:
				if err != nil {
					t.Fatalf("Run() error = %v, want nil", err)
				}
				assertFileRemoved(t)
			case kept:
				if err == nil || err.Error() != tc.err {
					t.Fatalf("Run() error = %v, want %q", err, tc.err)
				}
				assertFileKept(t)
			case upgrade:
				if !errors.Is(err, api.ErrUpgradeRequired) || err.Error() != api.ErrUpgradeRequired.Error() {
					t.Fatalf("Run() error = %v, want the upgrade line alone", err)
				}
				assertFileKept(t)
			}
			if got := out.String(); got != tc.stdout {
				t.Errorf("printed %q, want %q", got, tc.stdout)
			}
			if route != "DELETE /v1/keys/current" {
				t.Errorf("request = %q, want %q", route, "DELETE /v1/keys/current")
			}
			if got, want := authorization, "Bearer kk_live_test"; got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
			if !fileAtRequest {
				t.Error("the credentials file was already gone when the revoke arrived, want it removed only after")
			}
		})
	}

	// Nothing listens at this address, so the request fails before it leaves
	// the machine and nothing this test does can reach a real host.
	t.Run("a transport failure", func(t *testing.T) {
		creds := loggedIn(t)
		client := api.NewClient("http://127.0.0.1:1")
		client.APIKey = creds.APIKey

		var out bytes.Buffer
		err := Run(client, creds, &out)

		if err == nil || !strings.HasPrefix(err.Error(), "request failed: ") || !strings.HasSuffix(err.Error(), "\n"+mayStillBeValid) {
			t.Fatalf("Run() error = %v, want the transport failure then %q", err, mayStillBeValid)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q, want nothing", out.String())
		}
		assertFileKept(t)
	})

	// A 302 would make the HTTP client re-send the DELETE as a GET, whose 200
	// answer would read as a revoke.
	t.Run("a redirect that turns the DELETE into a GET", func(t *testing.T) {
		creds := loggedIn(t)
		var mu sync.Mutex
		var methods []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			methods = append(methods, r.Method)
			mu.Unlock()
			if r.Method == "DELETE" {
				w.Header().Set("Location", "/v1/keys/current")
				w.WriteHeader(http.StatusFound)
				return
			}
			io.WriteString(w, `{"key":{"id":"5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65","name":"alexis-laptop"}}`)
		}))
		t.Cleanup(server.Close)
		client := api.NewClient(server.URL)
		client.APIKey = creds.APIKey

		var out bytes.Buffer
		err := Run(client, creds, &out)
		server.Close()

		if err == nil || !strings.HasSuffix(err.Error(), "\n"+mayStillBeValid) {
			t.Fatalf("Run() error = %v, want the failure then %q", err, mayStillBeValid)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q, want nothing", out.String())
		}
		if !slices.Equal(methods, []string{"DELETE"}) {
			t.Errorf("methods seen = %v, want only the DELETE", methods)
		}
		assertFileKept(t)
	})

	// Go drops Authorization when a redirect leaves the host and keeps the
	// DELETE, so the other host's 401 to the keyless request would read as an
	// invalid key.
	t.Run("a redirect to another host", func(t *testing.T) {
		creds := loggedIn(t)
		var mu sync.Mutex
		var seenByB []string
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seenByB = append(seenByB, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
		}))
		t.Cleanup(b.Close)
		portB := b.URL[strings.LastIndex(b.URL, ":")+1:]
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://localhost:"+portB+"/v1/keys/current")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		t.Cleanup(a.Close)
		client := api.NewClient(a.URL)
		client.APIKey = creds.APIKey

		var out bytes.Buffer
		err := Run(client, creds, &out)
		a.Close()
		b.Close()

		if err == nil || !strings.HasSuffix(err.Error(), "\n"+mayStillBeValid) {
			t.Fatalf("Run() error = %v, want the failure then %q", err, mayStillBeValid)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q, want nothing", out.String())
		}
		if len(seenByB) != 0 {
			t.Errorf("the other host saw %v, want no request", seenByB)
		}
		assertFileKept(t)
	})
}
