package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: the frame on every error this package wraps
// renders from the message catalog. Load rather than Setup: nothing here
// reports which language is in force, only renders in it.
//
// EnvAPIKey is cleared for a second reason: it outranks the credentials file,
// so on a machine that exports it every test here that loads credentials would
// read that key instead of the one it just wrote. There is no *testing.T here,
// hence os.Unsetenv rather than t.Setenv.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Unsetenv(EnvAPIKey)
	os.Exit(m.Run())
}

func TestSaveAndLoadCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	err := SaveCredentials("kk_live_testkey123", "test@example.com")
	if err != nil {
		t.Fatalf("SaveCredentials() error = %v", err)
	}

	creds, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials() error = %v", err)
	}
	if creds.Version != 1 {
		t.Errorf("version = %d, want 1", creds.Version)
	}
	if creds.APIKey != "kk_live_testkey123" {
		t.Errorf("api_key = %q, want kk_live_testkey123", creds.APIKey)
	}
	if creds.Email != "test@example.com" {
		t.Errorf("email = %q, want test@example.com", creds.Email)
	}
}

func TestLoadCredentialsMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	creds, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials() error = %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil credentials, got %+v", creds)
	}
}

// A credentials file that parses but holds no key is no credential: the load
// returns nil the way a missing file does, so every consumer that already
// treats nil as not logged in refuses over such a file rather than sending a
// request with no key in it. The fixtures are written by hand rather than
// through SaveCredentials, which is how a correct file is produced and never
// writes one of these. Each is a raw-string literal so that the \n in the two
// fixtures that carry one reaches the file as the two-character JSON escape; a
// real newline inside a JSON string is a parse error, which would fail those
// subtests for a reason that has nothing to do with the rule.
func TestLoadCredentialsFileKeyFollowsTheBlankRule(t *testing.T) {
	t.Run("an empty key", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"version":1,"api_key":"","email":"file@example.com"}`
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		if creds != nil {
			t.Errorf("expected nil credentials, got %+v", creds)
		}
	})

	t.Run("a whitespace-only key", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"version":1,"api_key":"  \n","email":"file@example.com"}`
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		if creds != nil {
			t.Errorf("expected nil credentials, got %+v", creds)
		}
	})

	t.Run("no api_key field at all", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"version":1,"email":"file@example.com"}`
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		if creds != nil {
			t.Errorf("expected nil credentials, got %+v", creds)
		}
	})

	t.Run("surrounding whitespace is trimmed off the file key", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"version":1,"api_key":"  kk_live_fromfile\n","email":"file@example.com"}`
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		wantCredentials(t, creds, "kk_live_fromfile", "file@example.com", SourceFile)
	})
}

func TestSaveCredentialsFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveCredentials("kk_live_testkey123", "test@example.com")

	path := filepath.Join(tmpDir, "kamakiri", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestSaveCredentialsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	SaveCredentials("kk_live_abc", "abc@example.com")

	path := filepath.Join(tmpDir, "kamakiri", "credentials.json")
	data, _ := os.ReadFile(path)

	var raw map[string]any
	json.Unmarshal(data, &raw)

	if raw["version"] != float64(1) {
		t.Errorf("version = %v, want 1", raw["version"])
	}
	if raw["api_key"] != "kk_live_abc" {
		t.Errorf("api_key = %v, want kk_live_abc", raw["api_key"])
	}
	if raw["email"] != "abc@example.com" {
		t.Errorf("email = %v, want abc@example.com", raw["email"])
	}
	// Source is stamped at load time, so nothing in the file has any business
	// carrying it: the field is tagged out of the JSON, and this is the
	// assertion that holds the tag to that. Counting the keys rather than
	// looking for Source alone also catches a field added later without a tag.
	if len(raw) != 3 {
		t.Errorf("credentials.json holds keys %v, want version, api_key and email only", raw)
	}
}

func TestCredentialsPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")

	path, err := CredentialsPath()
	if err != nil {
		t.Fatalf("CredentialsPath() error = %v", err)
	}
	want := "/tmp/test-config/kamakiri/credentials.json"
	if path != want {
		t.Errorf("CredentialsPath() = %q, want %q", path, want)
	}
}

// wantCredentials asserts the whole credential, since the environment branch
// differs from the file branch in more than the key: it carries no email, and
// it stamps a source of its own.
func wantCredentials(t *testing.T, creds *Credentials, key, email string, source Source) {
	t.Helper()

	if creds == nil {
		t.Fatal("LoadCredentials() = nil, want credentials")
	}
	if creds.Version != 1 {
		t.Errorf("version = %d, want 1", creds.Version)
	}
	if creds.APIKey != key {
		t.Errorf("api_key = %q, want %q", creds.APIKey, key)
	}
	if creds.Email != email {
		t.Errorf("email = %q, want %q", creds.Email, email)
	}
	if creds.Source != source {
		t.Errorf("source = %q, want %q", creds.Source, source)
	}
}

// KAMAKIRI_API_KEY resolves ahead of the file, so what a load returns is
// decided by two things at once: what the variable holds, and whether a file is
// there to fall back to. Every combination of the two is below.
func TestLoadCredentialsResolvesTheEnvironmentFirst(t *testing.T) {
	t.Run("a key in the variable, no file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(EnvAPIKey, "kk_live_fromenv")

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		wantCredentials(t, creds, "kk_live_fromenv", "", SourceEnvironment)
	})

	t.Run("surrounding whitespace is trimmed off the key", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(EnvAPIKey, "  kk_live_fromenv\n")

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		wantCredentials(t, creds, "kk_live_fromenv", "", SourceEnvironment)
	})

	t.Run("the variable outranks a file that holds another key", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if err := SaveCredentials("kk_live_fromfile", "file@example.com"); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvAPIKey, "kk_live_fromenv")

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		wantCredentials(t, creds, "kk_live_fromenv", "", SourceEnvironment)
	})

	t.Run("a blank variable falls through to the file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if err := SaveCredentials("kk_live_fromfile", "file@example.com"); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvAPIKey, "  \n")

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		wantCredentials(t, creds, "kk_live_fromfile", "file@example.com", SourceFile)
	})

	t.Run("an empty variable and no file is still no credential", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(EnvAPIKey, "")

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		if creds != nil {
			t.Errorf("expected nil credentials, got %+v", creds)
		}
	})

	// The source is stamped by the load, never read from the file, which is
	// what keeps a file that names the environment from being believed.
	t.Run("a file claiming the environment still loads as the file", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", tmpDir)
		t.Setenv(EnvAPIKey, "")

		dir := filepath.Join(tmpDir, "kamakiri")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"version":1,"api_key":"kk_live_fromfile","email":"file@example.com","Source":"environment"}`
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}

		creds, err := LoadCredentials()
		if err != nil {
			t.Fatalf("LoadCredentials() error = %v", err)
		}
		wantCredentials(t, creds, "kk_live_fromfile", "file@example.com", SourceFile)
	})
}

// The trim-and-non-blank rule has one owner, so it is pinned on that owner
// rather than through a caller, over the shapes a CI secret arrives in.
func TestAPIKeyFromEnvironment(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		want   string
		wantOK bool
	}{
		{"a key", "kk_live_fromenv", "kk_live_fromenv", true},
		{"a key a secret store handed back with a newline", "  kk_live_fromenv\n", "kk_live_fromenv", true},
		{"whitespace only", "  \n", "", false},
		{"empty", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvAPIKey, c.value)

			key, ok := APIKeyFromEnvironment()
			if key != c.want || ok != c.wantOK {
				t.Errorf("APIKeyFromEnvironment() = (%q, %t), want (%q, %t)", key, ok, c.want, c.wantOK)
			}
		})
	}
}
