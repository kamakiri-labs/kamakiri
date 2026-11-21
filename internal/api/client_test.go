package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever locale
// the suite runs under. Load rather than Setup: nothing here reports which
// language is in force, only renders in it. Pinning here rather than per test is
// what keeps it clear of the parallel tests: the catalog is an unsynchronized
// map, and swapping it while one of them reads would race.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

func TestRegisterSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/auth/register" {
			t.Errorf("path = %q, want /v1/auth/register", r.URL.Path)
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)

		if body["email"] != "user@example.com" {
			t.Errorf("email = %q, want user@example.com", body["email"])
		}
		if body["tos_accepted"] != true {
			t.Errorf("tos_accepted = %v, want true", body["tos_accepted"])
		}
		if body["secret_hash"] != "abc123hash" {
			t.Errorf("secret_hash = %q, want abc123hash", body["secret_hash"])
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"message": "Confirmation code sent."})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	resp, err := client.Register("user@example.com", true, "abc123hash", false)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if resp.Message != "Confirmation code sent." {
		t.Errorf("message = %q, want %q", resp.Message, "Confirmation code sent.")
	}
}

func TestRegisterInvalidEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "invalid_email",
			"message": "Invalid email address.",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.Register("bad", true, "somehash", false)
	if err == nil {
		t.Fatal("Register() expected error, got nil")
	}

	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "invalid_email" {
		t.Errorf("code = %q, want invalid_email", apiErr.Code)
	}
}

func TestVerifySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/verify" {
			t.Errorf("path = %q, want /v1/auth/verify", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)

		if body["email"] != "user@example.com" {
			t.Errorf("email = %q", body["email"])
		}
		if body["code"] != "ABC123" {
			t.Errorf("code = %q", body["code"])
		}
		if body["secret"] != "my_secret" {
			t.Errorf("secret = %q", body["secret"])
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"api_key": "kk_live_testkey123"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	resp, err := client.Verify("user@example.com", "ABC123", "my_secret", "", false)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if resp.APIKey != "kk_live_testkey123" {
		t.Errorf("api_key = %q, want kk_live_testkey123", resp.APIKey)
	}
}

func TestVerifyInvalidCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "invalid_code",
			"message": "Invalid or expired code.",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.Verify("user@example.com", "WRONG1", "some_secret", "", false)
	if err == nil {
		t.Fatal("Verify() expected error, got nil")
	}

	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "invalid_code" {
		t.Errorf("code = %q, want invalid_code", apiErr.Code)
	}
}

func TestVerifyTooManyAttempts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "too_many_attempts",
			"message": "Too many attempts. Request a new code.",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.Verify("user@example.com", "ABC123", "some_secret", "", false)

	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "too_many_attempts" {
		t.Errorf("code = %q, want too_many_attempts", apiErr.Code)
	}
}

// The server tells a first request from a request for a new code only by the
// resend field, and refuses a resend that is not a JSON boolean, so the body
// has to carry it as one on every request, false included.
func TestRegisterSendsResendAsABoolean(t *testing.T) {
	for _, resend := range []bool{false, true} {
		t.Run(fmt.Sprintf("resend=%t", resend), func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding the request body: %v", err)
				}
				io.WriteString(w, `{"message":"Confirmation code sent.","status":"sent","expires_in":600}`)
			}))
			defer server.Close()

			if _, err := NewClient(server.URL).Register("user@example.com", true, "abc123hash", resend); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			server.Close()

			value, present := body["resend"]
			if !present {
				t.Fatalf("request body %v carries no resend", body)
			}
			got, isBool := value.(bool)
			if !isBool {
				t.Fatalf("resend = %#v (%T), want a JSON boolean", value, value)
			}
			if got != resend {
				t.Errorf("resend = %t, want %t", got, resend)
			}
		})
	}
}

// The field names below are the server's; a register answer decoded under any
// other name would read as an older server's and silently lose the resume.
func TestRegisterDecodesStatusAndExpiry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"message":"A confirmation code is already pending.","status":"pending","expires_in":420}`)
	}))
	defer server.Close()

	resp, err := NewClient(server.URL).Register("user@example.com", true, "abc123hash", false)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	want := RegisterResponse{Message: "A confirmation code is already pending.", Status: "pending", ExpiresIn: 420}
	if *resp != want {
		t.Errorf("Register() = %+v, want %+v", *resp, want)
	}
}

func TestRegisterCooldownDecodesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":"rate_limited","message":"Please wait.","retry_after":37}`)
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Register("user@example.com", true, "abc123hash", true)
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("Register() error = %v (%T), want *ErrorResponse", err, err)
	}
	if apiErr.Code != "rate_limited" || apiErr.RetryAfter != 37 {
		t.Errorf("Register() error = {Code: %q, RetryAfter: %d}, want {rate_limited, 37}", apiErr.Code, apiErr.RetryAfter)
	}
}

// attempts_left is decoded into a pointer because 0 is an answer of its own
// (no attempts are left for this code), distinct from the field being absent
// (the server did not say).
func TestVerifyDecodesAttemptsLeft(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *int
	}{
		{"zero", `{"code":"invalid_code","message":"Invalid or expired code.","attempts_left":0}`, intPtr(0)},
		{"three", `{"code":"invalid_code","message":"Invalid or expired code.","attempts_left":3}`, intPtr(3)},
		{"absent", `{"code":"invalid_code","message":"Invalid or expired code."}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()

			_, err := NewClient(server.URL).Verify("user@example.com", "WRONG1", "some_secret", "", false)
			apiErr, ok := err.(*ErrorResponse)
			if !ok {
				t.Fatalf("Verify() error = %v (%T), want *ErrorResponse", err, err)
			}
			switch {
			case tc.want == nil && apiErr.AttemptsLeft != nil:
				t.Errorf("AttemptsLeft = %d, want absent", *apiErr.AttemptsLeft)
			case tc.want != nil && apiErr.AttemptsLeft == nil:
				t.Errorf("AttemptsLeft absent, want %d", *tc.want)
			case tc.want != nil && *apiErr.AttemptsLeft != *tc.want:
				t.Errorf("AttemptsLeft = %d, want %d", *apiErr.AttemptsLeft, *tc.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

// An empty name is left out of the body, so the server names the key itself
// rather than refusing an empty one; any other name is sent as given. The key
// object of the answer is decoded under the server's field names.
func TestVerifySendsTheNameAndDecodesTheKey(t *testing.T) {
	cases := []struct {
		name    string
		present bool
	}{
		{"", false},
		{"ci-box", true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("name=%q", tc.name), func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding the request body: %v", err)
				}
				io.WriteString(w, `{"api_key":"kk_live_testkey123","key":{"id":"0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59","name":"ci-box"}}`)
			}))
			defer server.Close()

			resp, err := NewClient(server.URL).Verify("user@example.com", "ABC123", "my_secret", tc.name, false)
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			server.Close()

			value, present := body["name"]
			if present != tc.present {
				t.Fatalf("request body %v: name present = %t, want %t", body, present, tc.present)
			}
			if present && value != tc.name {
				t.Errorf("name = %#v, want %q", value, tc.name)
			}
			want := KeyInfo{ID: "0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59", Name: "ci-box"}
			if resp.Key == nil || *resp.Key != want {
				t.Errorf("Key = %+v, want %+v", resp.Key, want)
			}
		})
	}
}

// revoke_others goes out as a JSON true only when asked for, so a plain login
// sends exactly the request it always did; the revoked keys the answer lists
// are decoded in the order the server gave them.
func TestVerifySendsRevokeOthersOnlyWhenAskedAndDecodesTheRevokedKeys(t *testing.T) {
	answer := `{"api_key":"kk_live_testkey123","key":{"id":"0b8f3a52-6a3e-4c55-9d0e-2f1d8c7b6a59","name":"laptop"},` +
		`"revoked":[{"id":"5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65","name":"old-laptop"},{"id":"c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c","name":"github-ci"}]}`
	for _, revokeOthers := range []bool{true, false} {
		t.Run(fmt.Sprintf("revokeOthers=%t", revokeOthers), func(t *testing.T) {
			var raw []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ = io.ReadAll(r.Body)
				io.WriteString(w, answer)
			}))
			defer server.Close()

			resp, err := NewClient(server.URL).Verify("user@example.com", "ABC123", "my_secret", "laptop", revokeOthers)
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			server.Close()

			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decoding the request body %q: %v", raw, err)
			}
			value, present := body["revoke_others"]
			if revokeOthers && value != true {
				t.Errorf("request body %s: revoke_others = %#v, want true", raw, value)
			}
			if !revokeOthers && present {
				t.Errorf("request body %s: revoke_others present, want it left out", raw)
			}

			want := []KeyInfo{
				{ID: "5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65", Name: "old-laptop"},
				{ID: "c3e94b70-1a2d-4e5f-b6c7-8d9e0f1a2b3c", Name: "github-ci"},
			}
			if !reflect.DeepEqual(resp.Revoked, want) {
				t.Errorf("Revoked = %+v, want %+v", resp.Revoked, want)
			}
		})
	}
}

// An older server answers with the key alone, and the response says so with a
// nil Key rather than an empty one.
func TestVerifyWithoutAKeyObjectLeavesKeyNil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"api_key":"kk_live_testkey123"}`)
	}))
	defer server.Close()

	resp, err := NewClient(server.URL).Verify("user@example.com", "ABC123", "my_secret", "ci-box", false)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if resp.Key != nil {
		t.Errorf("Key = %+v, want nil", resp.Key)
	}
}

func TestCreateSiteSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/sites" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer kk_live_test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["subdomain"] != "my-site" {
			t.Errorf("subdomain = %q", body["subdomain"])
		}

		w.WriteHeader(201)
		json.NewEncoder(w).Encode(Site{ID: "abc123", Subdomain: "my-site", URL: "https://my-site.kamakiri-pages.jp"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	site, err := client.CreateSite("my-site")
	if err != nil {
		t.Fatalf("CreateSite() error = %v", err)
	}
	if site.ID != "abc123" {
		t.Errorf("id = %q, want abc123", site.ID)
	}
	if site.Subdomain != "my-site" {
		t.Errorf("subdomain = %q", site.Subdomain)
	}
}

func TestCreateSiteSubdomainTaken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "subdomain_taken", Message: "Subdomain is already taken."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.CreateSite("taken")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "subdomain_taken" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestCreateSiteSubdomainTakenOwnAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "subdomain_taken_own_account",
			"message": "You already own this subdomain.",
			"site_id": "existing123",
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.CreateSite("my-existing")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "subdomain_taken_own_account" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestGetSiteSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/pages/sites/abc123" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer kk_live_test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}

		json.NewEncoder(w).Encode(Site{ID: "abc123", Subdomain: "my-site", URL: "https://my-site.kamakiri-pages.jp"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	site, err := client.GetSite("abc123")
	if err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}
	if site.Subdomain != "my-site" {
		t.Errorf("subdomain = %q", site.Subdomain)
	}
}

func TestGetSiteNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "site_not_found", Message: "Site not found."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.GetSite("nonexistent")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "site_not_found" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestGetSiteForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "forbidden", Message: "You do not own this site."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.GetSite("notmine")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "forbidden" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func ptr[T any](v T) *T { return &v }

func TestSitePointerFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	s := Site{
		ID:                       "x",
		Subdomain:                "a",
		SubdomainObserved:        ptr("b"),
		SubdomainEnabledObserved: ptr(true),
	}

	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Site
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.SubdomainObserved == nil || *back.SubdomainObserved != "b" {
		t.Errorf("SubdomainObserved roundtrip failed: %+v", back.SubdomainObserved)
	}
	if back.SubdomainEnabledObserved == nil || *back.SubdomainEnabledObserved != true {
		t.Errorf("SubdomainEnabledObserved roundtrip failed: %+v", back.SubdomainEnabledObserved)
	}
}

func TestUpdateSubdomainSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("method = %q, want PUT", r.Method)
		}
		if r.URL.Path != "/v1/pages/sites/subdomain" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "abc123" {
			t.Errorf("site_id = %q", body["site_id"])
		}
		if body["subdomain"] != "new-name" {
			t.Errorf("subdomain = %q", body["subdomain"])
		}

		json.NewEncoder(w).Encode(Site{ID: "abc123", Subdomain: "new-name", URL: "https://new-name.kamakiri-pages.jp"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	site, err := client.UpdateSubdomain("abc123", "new-name")
	if err != nil {
		t.Fatalf("UpdateSubdomain() error = %v", err)
	}
	if site.Subdomain != "new-name" {
		t.Errorf("subdomain = %q", site.Subdomain)
	}
}

func TestUpdateSubdomainTaken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "subdomain_taken", Message: "Subdomain is already taken."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.UpdateSubdomain("abc123", "taken")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "subdomain_taken" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestDeploySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/deploy" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer kk_live_test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}

		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("content-type = %q, want multipart/form-data", r.Header.Get("Content-Type"))
		}

		r.ParseMultipartForm(10 << 20)
		configVal := r.FormValue("config")
		if configVal == "" {
			t.Error("missing config field")
		}

		file, _, err := r.FormFile("tarball")
		if err != nil {
			t.Fatalf("missing tarball: %v", err)
		}
		defer file.Close()
		tarballBytes, _ := io.ReadAll(file)
		if string(tarballBytes) != "fake-tarball-data" {
			t.Errorf("tarball content = %q", string(tarballBytes))
		}

		json.NewEncoder(w).Encode(DeployResult{URL: "https://my-site.kamakiri-pages.jp"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	config := []byte(`{"version":1,"kind":"pages","id":"abc123"}`)
	result, err := client.Deploy(config, strings.NewReader("fake-tarball-data"))
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if result.URL != "https://my-site.kamakiri-pages.jp" {
		t.Errorf("url = %q", result.URL)
	}
}

func TestDeployInvalidTarball(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "tarball_invalid", Message: "Failed to extract tarball."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.Deploy([]byte(`{}`), strings.NewReader("bad"))
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "tarball_invalid" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestDeployForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "forbidden", Message: "You do not own this site."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.Deploy([]byte(`{"id":"notmine"}`), strings.NewReader("data"))
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "forbidden" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestSetCanonicalSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("method = %q, want PUT", r.Method)
		}
		if r.URL.Path != "/v1/pages/domains/canonical" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "abc123" {
			t.Errorf("site_id = %q", body["site_id"])
		}
		if body["domain"] != "example.com" {
			t.Errorf("domain = %q", body["domain"])
		}

		json.NewEncoder(w).Encode(Domain{Domain: "example.com", Role: "canonical", DnsTarget: "site123.c1.kamakiri-pages.site"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	domain, err := client.SetCanonical("abc123", "example.com")
	if err != nil {
		t.Fatalf("SetCanonical() error = %v", err)
	}
	if domain.Domain != "example.com" {
		t.Errorf("domain = %q", domain.Domain)
	}
	if domain.DnsTarget != "site123.c1.kamakiri-pages.site" {
		t.Errorf("dns_target = %q", domain.DnsTarget)
	}
}

func TestSetCanonicalDomainTaken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "domain_taken", Message: "Domain is already attached to another site."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.SetCanonical("abc123", "taken.com")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "domain_taken" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestUnsetCanonicalSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/pages/domains/canonical" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "abc123" {
			t.Errorf("site_id = %q", body["site_id"])
		}

		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]int64{"sync_attempt_id": 7})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.UnsetCanonical("abc123")
	if err != nil {
		t.Fatalf("UnsetCanonical() error = %v", err)
	}
	if result.SyncAttemptID != 7 {
		t.Errorf("sync_attempt_id = %d, want 7", result.SyncAttemptID)
	}
}

func TestUnsetCanonicalNoCanonical(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "no_canonical", Message: "Site has no canonical domain."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.UnsetCanonical("abc123")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "no_canonical" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

func TestAddDomainRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/domains" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "abc123" {
			t.Errorf("site_id = %v", body["site_id"])
		}
		if body["domain"] != "www.example.com" {
			t.Errorf("domain = %v", body["domain"])
		}
		if body["role"] != "redirect" {
			t.Errorf("role = %v", body["role"])
		}
		if body["redirect_status"].(float64) != 301 {
			t.Errorf("redirect_status = %v", body["redirect_status"])
		}

		w.WriteHeader(201)
		json.NewEncoder(w).Encode(Domain{
			Domain:         "www.example.com",
			Role:           "redirect",
			RedirectStatus: 301,
			DnsTarget:      "site123.c1.kamakiri-pages.site",
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	domain, err := client.AddDomain("abc123", "www.example.com", "redirect", 301)
	if err != nil {
		t.Fatalf("AddDomain() error = %v", err)
	}
	if domain.Role != "redirect" {
		t.Errorf("role = %q", domain.Role)
	}
	if domain.RedirectStatus != 301 {
		t.Errorf("redirect_status = %d", domain.RedirectStatus)
	}
}

func TestAddDomainAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["redirect_status"]; ok {
			t.Error("redirect_status should not be sent for alias")
		}

		w.WriteHeader(201)
		json.NewEncoder(w).Encode(Domain{Domain: "alias.example.com", Role: "alias"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	domain, err := client.AddDomain("abc123", "alias.example.com", "alias", 0)
	if err != nil {
		t.Fatalf("AddDomain() error = %v", err)
	}
	if domain.Role != "alias" {
		t.Errorf("role = %q", domain.Role)
	}
}

func TestRemoveDomainSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/pages/domains/www.example.com" {
			t.Errorf("path = %q", r.URL.Path)
		}

		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{
			"site_id":         "site123",
			"sync_attempt_id": 99,
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.RemoveDomain("site123", "www.example.com")
	if err != nil {
		t.Fatalf("RemoveDomain() error = %v", err)
	}
	if result.SiteID != "site123" {
		t.Errorf("SiteID = %q, want site123", result.SiteID)
	}
	if result.SyncAttemptID != 99 {
		t.Errorf("SyncAttemptID = %d, want 99", result.SyncAttemptID)
	}
}

func TestRemoveDomainNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "domain_not_found", Message: "Domain not found."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.RemoveDomain("site123", "nonexistent.com")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "domain_not_found" {
		t.Errorf("code = %q", apiErr.Code)
	}
}

// The by-name domain calls carry the linked site's id as their only query
// value, after the escaped host, so the server can refuse a host attached to
// another of the account's sites.
func TestDomainByNameCallsSendTheSiteID(t *testing.T) {
	cases := []struct {
		name      string
		siteID    string
		call      func(c *Client, siteID string) error
		method    string
		path      string
		wantQuery string
	}{
		{
			name:   "remove",
			siteID: "site123",
			call: func(c *Client, siteID string) error {
				_, err := c.RemoveDomain(siteID, "www.example.com")
				return err
			},
			method:    "DELETE",
			path:      "/v1/pages/domains/www.example.com",
			wantQuery: "site_id=site123",
		},
		{
			name:   "recheck",
			siteID: "site123",
			call: func(c *Client, siteID string) error {
				_, err := c.RecheckDomain(siteID, "www.example.com")
				return err
			},
			method:    "POST",
			path:      "/v1/pages/domains/www.example.com/recheck",
			wantQuery: "site_id=site123",
		},
		{
			name:   "remove encodes the id as a query value",
			siteID: "a&b=c",
			call: func(c *Client, siteID string) error {
				_, err := c.RemoveDomain(siteID, "www.example.com")
				return err
			},
			method:    "DELETE",
			path:      "/v1/pages/domains/www.example.com",
			wantQuery: "site_id=a%26b%3Dc",
		},
		{
			name:   "remove sends an empty id as an empty value",
			siteID: "",
			call: func(c *Client, siteID string) error {
				_, err := c.RemoveDomain(siteID, "www.example.com")
				return err
			},
			method:    "DELETE",
			path:      "/v1/pages/domains/www.example.com",
			wantQuery: "site_id=",
		},
		{
			name:   "recheck escapes the host in the path",
			siteID: "site123",
			call: func(c *Client, siteID string) error {
				_, err := c.RecheckDomain(siteID, "a?b.example.com")
				return err
			},
			method:    "POST",
			path:      "/v1/pages/domains/a%3Fb.example.com/recheck",
			wantQuery: "site_id=site123",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method {
					t.Errorf("method = %q, want %q", r.Method, tc.method)
				}
				if r.URL.EscapedPath() != tc.path {
					t.Errorf("path = %q, want %q", r.URL.EscapedPath(), tc.path)
				}
				if r.URL.RawQuery != tc.wantQuery {
					t.Errorf("query = %q, want %q", r.URL.RawQuery, tc.wantQuery)
				}
				if got := r.URL.Query()["site_id"]; len(got) != 1 || got[0] != tc.siteID {
					t.Errorf("site_id values = %q, want exactly [%q]", got, tc.siteID)
				}
				w.WriteHeader(200)
				io.WriteString(w, `{"site_id":"site123","sync_attempt_id":1}`)
			}))
			defer server.Close()

			client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
			if err := tc.call(client, tc.siteID); err != nil {
				t.Fatalf("call error = %v", err)
			}
		})
	}
}

// A domain_on_other_site refusal names the site the host is attached to in
// its subdomain field, which the domain commands render.
func TestErrorResponseCarriesSubdomain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		io.WriteString(w, `{"code":"domain_on_other_site","message":"www.example.com is attached to another of your sites (blog).","subdomain":"blog"}`)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.RemoveDomain("site123", "www.example.com")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T (%v)", err, err)
	}
	if apiErr.Code != "domain_on_other_site" {
		t.Errorf("code = %q", apiErr.Code)
	}
	if apiErr.Subdomain != "blog" {
		t.Errorf("subdomain = %q, want blog", apiErr.Subdomain)
	}
	if apiErr.Message != "www.example.com is attached to another of your sites (blog)." {
		t.Errorf("message = %q", apiErr.Message)
	}
}

func TestListDomainsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/pages/domains" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("site_id") != "abc123" {
			t.Errorf("site_id = %q", r.URL.Query().Get("site_id"))
		}

		json.NewEncoder(w).Encode(DomainList{
			Domains: []Domain{
				{Domain: "example.com", Role: "canonical"},
				{Domain: "www.example.com", Role: "redirect", RedirectStatus: 301},
			},
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.ListDomains("abc123")
	if err != nil {
		t.Fatalf("ListDomains() error = %v", err)
	}
	if len(result.Domains) != 2 {
		t.Fatalf("domains count = %d, want 2", len(result.Domains))
	}
	if result.Domains[0].Role != "canonical" {
		t.Errorf("first domain role = %q", result.Domains[0].Role)
	}
}

func TestDisableSubdomainSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/sites/subdomain/disable" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "abc123" {
			t.Errorf("site_id = %q", body["site_id"])
		}

		json.NewEncoder(w).Encode(Site{ID: "abc123", Subdomain: "my-site", SubdomainEnabled: false})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	site, err := client.DisableSubdomain("abc123")
	if err != nil {
		t.Fatalf("DisableSubdomain() error = %v", err)
	}
	if site.SubdomainEnabled {
		t.Error("subdomain_enabled should be false")
	}
}

func TestEnableSubdomainSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/sites/subdomain/enable" {
			t.Errorf("path = %q", r.URL.Path)
		}

		json.NewEncoder(w).Encode(Site{ID: "abc123", Subdomain: "my-site", SubdomainEnabled: true})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	site, err := client.EnableSubdomain("abc123")
	if err != nil {
		t.Fatalf("EnableSubdomain() error = %v", err)
	}
	if !site.SubdomainEnabled {
		t.Error("subdomain_enabled should be true")
	}
}

func TestMapError(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"site_not_found", "site not found"},
		{"unauthorized", "invalid API key"},
		{"invalid_subdomain", "invalid subdomain"},
		{"subdomain_reserved", "that subdomain is reserved. Choose another"},
		{"subdomain_taken", "subdomain is already taken"},
		{"domain_taken", "already attached to another site. One host serves one site"},
		{"registration_limit", "too many pending registrations"},
		{"domain_reserved", "that domain is part of Kamakiri's own namespace, so it can't be used as a custom domain"},
		{"canonical_exists", "another domain change on this site landed at the same moment. Run the command again"},
		{"no_canonical", "set a canonical domain first"},
		{"has_redirect_domains", "remove redirect domains"},
		{"is_canonical", "kamakiri domain unset"},
		{"domain_not_found", "domain not found"},
		{"invalid_domain", "invalid domain name"},
		{"site_deleted", "this site is being torn down or has been torn down. Run \"kamakiri teardown\" to retry or finish the teardown"},
		{"no_live_deploy", "no published content to purge. Run \"kamakiri deploy <path>\" first"},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := MapError(&ErrorResponse{Code: tt.code, Message: "raw"})
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("MapError(%q) = %q, want to contain %q", tt.code, err.Error(), tt.want)
			}
		})
	}
}

// The unauthorized copy is the one arm whose wording depends on where the key
// came from, so both arms are pinned here and through MapError, which is what
// every command that meets a 401 actually goes through. The value is package
// state: this test flips it and must stay serial, and it restores it because
// every other case that renders this copy expects the file arm, whatever order
// the suite runs in.
func TestUnauthorizedErrorNamesWhereTheKeyCameFrom(t *testing.T) {
	t.Cleanup(func() { SetKeyFromEnvironment(false) })

	SetKeyFromEnvironment(false)
	for name, got := range map[string]string{
		"UnauthorizedError": UnauthorizedError().Error(),
		"MapError":          MapError(&ErrorResponse{Code: "unauthorized", Message: "raw"}).Error(),
	} {
		if !strings.Contains(got, `Run "kamakiri login" to re-authenticate`) {
			t.Errorf("%s with a key from the file = %q, want the login copy", name, got)
		}
		if strings.Contains(got, "KAMAKIRI_API_KEY") {
			t.Errorf("%s with a key from the file = %q, want no mention of the variable", name, got)
		}
	}

	SetKeyFromEnvironment(true)
	for name, got := range map[string]string{
		"UnauthorizedError": UnauthorizedError().Error(),
		"MapError":          MapError(&ErrorResponse{Code: "unauthorized", Message: "raw"}).Error(),
	} {
		if !strings.Contains(got, "The key came from KAMAKIRI_API_KEY; check the value that variable holds") {
			t.Errorf("%s with a key from the environment = %q, want the variable copy", name, got)
		}
		if strings.Contains(got, "kamakiri login") {
			t.Errorf("%s with a key from the environment = %q, want no mention of login", name, got)
		}
	}
}

func TestMapErrorPassthroughNonAPI(t *testing.T) {
	orig := errors.New("network error")
	err := MapError(orig)
	if err != orig {
		t.Errorf("MapError should pass through non-API errors, got %v", err)
	}
}

func TestMapErrorUnknownCode(t *testing.T) {
	apiErr := &ErrorResponse{Code: "unknown_code", Message: "Something went wrong."}
	err := MapError(apiErr)
	if err != apiErr {
		t.Errorf("MapError should return original ErrorResponse for unknown codes, got %v", err)
	}
}

// A wrapped *ErrorResponse is still an API refusal: a known code is mapped and
// an unknown one comes back as the wrapper itself, not the unwrapped response.
func TestMapErrorWrapped(t *testing.T) {
	known := fmt.Errorf("rollback: %w", &ErrorResponse{Code: "site_deleted", Message: "raw"})
	if got := MapError(known).Error(); !strings.Contains(got, "this site is being torn down or has been torn down") {
		t.Errorf("a wrapped known code must be mapped, got %q", got)
	}

	unknown := fmt.Errorf("rollback: %w", &ErrorResponse{Code: "unknown_code", Message: "raw"})
	got := MapError(unknown)
	if got != unknown {
		t.Errorf("a wrapped unknown code must come back as the same wrapped error, got %v", got)
	}
	if !errors.Is(got, unknown) {
		t.Errorf("errors.Is on the wrapper must hold, got %v", got)
	}
}

func TestMapErrorTarballInvalid(t *testing.T) {
	apiErr := &ErrorResponse{Code: "tarball_invalid", Message: "Custom tarball message."}
	err := MapError(apiErr)
	if err.Error() != "Custom tarball message." {
		t.Errorf("MapError(tarball_invalid) = %q, want original message", err.Error())
	}
}

// The quota rejection codes carry a server message naming the actual size/count
// and the limit, so MapError must surface it verbatim rather than a generic copy.
func TestMapErrorQuotaCodesEchoServerMessage(t *testing.T) {
	cases := map[string]string{
		"deploy_too_large":      "Deploy is 142 MB; your account's limit is 100 MB per deploy.",
		"deploy_too_many_files": "Deploy has 25000 files; the limit is 20000 files per deploy.",
		"config_too_large":      "Deploy config is 512 KB; the limit is 256 KB.",
		"site_limit_reached":    "This account is at its limit of 5 sites. Delete a site you no longer need, or contact us to raise the limit.",
	}

	for code, msg := range cases {
		got := MapError(&ErrorResponse{Code: code, Message: msg}).Error()
		if got != msg {
			t.Errorf("MapError(%s) = %q, want the server message %q", code, got, msg)
		}
	}
}

func TestMapErrorDeployRateLimited(t *testing.T) {
	got := MapError(&ErrorResponse{Code: "deploy_rate_limited", Message: "raw"}).Error()
	if !strings.Contains(got, "deploying very frequently") {
		t.Errorf("MapError(deploy_rate_limited) = %q, want the deploy copy", got)
	}
	// It must NOT inherit the recheck `rate_limited` copy: a distinct code is
	// the whole point so a deploy flood is never framed as a recheck.
	if strings.Contains(got, "Automatic checks continue") || strings.Contains(got, "checked very recently") {
		t.Errorf("MapError(deploy_rate_limited) leaked the recheck copy: %q", got)
	}
}

func TestMapErrorRecheckRateLimitedUnchanged(t *testing.T) {
	got := MapError(&ErrorResponse{Code: "rate_limited", Message: "raw"}).Error()
	if !strings.Contains(got, "Automatic checks continue in the background") {
		t.Errorf("MapError(rate_limited) = %q, want the gentle recheck copy unchanged", got)
	}
}

func TestMapErrorRegisterRateLimited(t *testing.T) {
	got := MapError(&ErrorResponse{Code: "register_rate_limited", Message: "raw"}).Error()
	if !strings.Contains(got, "registering domains very frequently") {
		t.Errorf("MapError(register_rate_limited) = %q, want the register copy", got)
	}
	// A distinct code from `rate_limited` / `deploy_rate_limited`, so a register
	// flood must inherit neither the recheck copy nor the deploy copy.
	if strings.Contains(got, "Automatic checks continue") || strings.Contains(got, "checked very recently") {
		t.Errorf("MapError(register_rate_limited) leaked the recheck copy: %q", got)
	}
	if strings.Contains(got, "deploying very frequently") {
		t.Errorf("MapError(register_rate_limited) leaked the deploy copy: %q", got)
	}
}

func TestMapErrorRollbackRateLimited(t *testing.T) {
	const serverMsg = "You're rolling back very frequently. Wait a moment and try again."
	got := MapError(&ErrorResponse{Code: "rollback_rate_limited", Message: serverMsg}).Error()
	want := "you're rolling back very frequently. Wait a moment and try again"
	if got != want {
		t.Errorf("MapError(rollback_rate_limited) = %q, want the rollback copy %q", got, want)
	}
	if got == serverMsg {
		t.Errorf("MapError(rollback_rate_limited) passed the server message through: %q", got)
	}
	// A distinct code again, so a rollback flood inherits none of the other
	// limiters' copy.
	if strings.Contains(got, "Automatic checks continue") || strings.Contains(got, "checked very recently") {
		t.Errorf("MapError(rollback_rate_limited) leaked the recheck copy: %q", got)
	}
	if strings.Contains(got, "deploying very frequently") {
		t.Errorf("MapError(rollback_rate_limited) leaked the deploy copy: %q", got)
	}
	if strings.Contains(got, "registering domains very frequently") {
		t.Errorf("MapError(rollback_rate_limited) leaked the register copy: %q", got)
	}
}

// renderIn switches the catalog to lang for one test and puts English back. It
// must not be called from a parallel test: the catalog is an unsynchronized map,
// and the parallel tests of this package run only once the serial ones are done.
func renderIn(t *testing.T, lang string) {
	t.Helper()
	i18n.Load(lang)
	t.Cleanup(func() { i18n.Load("en") })
}

// The fields of a deploy key's refusals, as the server sends them for a key
// named github-actions bound to two sites.
var (
	deployKey   = &KeyInfo{ID: "3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40", Name: "github-actions"}
	deploySites = []LabeledSite{
		{ID: "k3j9x2m4p7q1w8e5", Label: "example.com"},
		{ID: "a1b2c3d4e5f6g7h8", Label: "docs.kamakiri-pages.jp"},
	}
	refusedSite = &LabeledSite{ID: "z9y8x7w6v5u4t3s2", Label: "blog.example.org"}
)

// Each deploy-key refusal is worded from its body's fields, in both languages,
// and the whole line is compared. The server's message is never what prints.
func TestMapErrorRendersTheDeployKeyRefusals(t *testing.T) {
	cases := []struct {
		name string
		lang string
		err  *ErrorResponse
		want string
	}{
		{
			"a route closed to deploy keys",
			"en",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey, Sites: deploySites},
			`this is a deploy key ("github-actions"): it can deploy to example.com, docs.kamakiri-pages.jp and nothing else. Use a login key for this command`,
		},
		{
			"a route closed to deploy keys, in Japanese",
			"ja",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey, Sites: deploySites},
			`このAPIキーはデプロイキー「github-actions」です。デプロイできるのは example.com、docs.kamakiri-pages.jp だけです。このコマンドにはログインキーを使ってください`,
		},
		{
			"a key bound to one site",
			"en",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey, Sites: deploySites[:1]},
			`this is a deploy key ("github-actions"): it can deploy to example.com and nothing else. Use a login key for this command`,
		},
		{
			"a site not on the key's list",
			"en",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites, Site: refusedSite},
			`this deploy key ("github-actions") cannot act on blog.example.org: it can deploy to example.com, docs.kamakiri-pages.jp and nothing else`,
		},
		{
			"a site not on the key's list, in Japanese",
			"ja",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites, Site: refusedSite},
			`デプロイキー「github-actions」は blog.example.org を操作できません。デプロイできるのは example.com、docs.kamakiri-pages.jp だけです`,
		},
		{
			// The server labels a site that is no longer active with its ID.
			"a torn-down site, labelled with its ID",
			"en",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites, Site: &LabeledSite{ID: "z9y8x7w6v5u4t3s2", Label: "z9y8x7w6v5u4t3s2"}},
			`this deploy key ("github-actions") cannot act on z9y8x7w6v5u4t3s2: it can deploy to example.com, docs.kamakiri-pages.jp and nothing else`,
		},
		{
			"a reference that matches no site",
			"en",
			&ErrorResponse{Code: "key_site_not_found", Message: "raw", Reference: "docs.kamakiri-pages.jp"},
			`no site of this account matches docs.kamakiri-pages.jp. Name a site by its subdomain, one of its domains, or its site ID`,
		},
		{
			"a reference that matches no site, in Japanese",
			"ja",
			&ErrorResponse{Code: "key_site_not_found", Message: "raw", Reference: "docs.kamakiri-pages.jp"},
			`このアカウントに docs.kamakiri-pages.jp に該当するサイトはありません。サイトはサブドメイン、ドメイン、またはサイトIDで指定してください`,
		},
		{
			"the deploy-key limit",
			"en",
			&ErrorResponse{Code: "deploy_key_limit", Message: "raw", Limit: 10},
			`this account already holds 10 deploy keys, the most it can have. Revoke one with "kamakiri key revoke <id>" first`,
		},
		{
			"the deploy-key limit, in Japanese",
			"ja",
			&ErrorResponse{Code: "deploy_key_limit", Message: "raw", Limit: 10},
			`このアカウントのデプロイキーは上限の10件に達しています。先に "kamakiri key revoke <id>" で1件失効させてください`,
		},
		{
			"the creation rate limit",
			"en",
			&ErrorResponse{Code: "key_create_rate_limited", Message: "raw"},
			`too many keys created recently. Try again later`,
		},
		{
			"the creation rate limit, in Japanese",
			"ja",
			&ErrorResponse{Code: "key_create_rate_limited", Message: "raw"},
			`短時間にキーが繰り返し作成されています。しばらく時間をおいてからもう一度お試しください`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renderIn(t, tc.lang)
			if got := MapError(tc.err).Error(); got != tc.want {
				t.Errorf("MapError(%s) = %q, want %q", tc.err.Code, got, tc.want)
			}
		})
	}
}

// A key's name and a site's label come from the server, and the echoed
// reference from what the user typed, so each is escaped before it reaches the
// terminal: a bidi override, a zero-width character, a line separator, ESC and
// BEL are shown as the escapes of their code points, in either language.
func TestMapErrorEscapesWhatTheDeployKeyRefusalsCarry(t *testing.T) {
	hostileKey := &KeyInfo{ID: deployKey.ID, Name: "ci‮box\x1b[2J"}
	hostileSites := []LabeledSite{
		{ID: "k3j9x2m4p7q1w8e5", Label: "ex​ample.com"},
		{ID: "a1b2c3d4e5f6g7h8", Label: "docs .kamakiri-pages.jp"},
	}
	hostileSite := &LabeledSite{ID: "z9y8x7w6v5u4t3s2", Label: "blog\x1b]0;x\x07.example.org"}

	cases := []struct {
		name string
		lang string
		err  *ErrorResponse
		want string
	}{
		{
			"a route closed to deploy keys",
			"en",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: hostileKey, Sites: hostileSites},
			`this is a deploy key ("ci\u{202e}box\u{001b}[2J"): it can deploy to ex\u{200b}ample.com, docs\u{2028}.kamakiri-pages.jp and nothing else. Use a login key for this command`,
		},
		{
			"a site not on the key's list",
			"en",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: hostileKey, Sites: hostileSites, Site: hostileSite},
			`this deploy key ("ci\u{202e}box\u{001b}[2J") cannot act on blog\u{001b}]0;x\u{0007}.example.org: it can deploy to ex\u{200b}ample.com, docs\u{2028}.kamakiri-pages.jp and nothing else`,
		},
		{
			"a site not on the key's list, in Japanese",
			"ja",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: hostileKey, Sites: hostileSites, Site: hostileSite},
			`デプロイキー「ci\u{202e}box\u{001b}[2J」は blog\u{001b}]0;x\u{0007}.example.org を操作できません。デプロイできるのは ex\u{200b}ample.com、docs\u{2028}.kamakiri-pages.jp だけです`,
		},
		{
			"a reference that matches no site",
			"en",
			&ErrorResponse{Code: "key_site_not_found", Message: "raw", Reference: "‮docs ‍.kamakiri-pages.jp"},
			`no site of this account matches \u{202e}docs\u{2028}\u{200d}.kamakiri-pages.jp. Name a site by its subdomain, one of its domains, or its site ID`,
		},
		{
			"a route closed to deploy keys, in Japanese",
			"ja",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: hostileKey, Sites: hostileSites},
			`このAPIキーはデプロイキー「ci\u{202e}box\u{001b}[2J」です。デプロイできるのは ex\u{200b}ample.com、docs\u{2028}.kamakiri-pages.jp だけです。このコマンドにはログインキーを使ってください`,
		},
		{
			"a reference that matches no site, in Japanese",
			"ja",
			&ErrorResponse{Code: "key_site_not_found", Message: "raw", Reference: "\u202edocs\u2028\u200d.kamakiri-pages.jp"},
			`このアカウントに \u{202e}docs\u{2028}\u{200d}.kamakiri-pages.jp に該当するサイトはありません。サイトはサブドメイン、ドメイン、またはサイトIDで指定してください`,
		},
		{
			// A site with no label is shown by its ID, which is as much the
			// server's text as a label is.
			"sites shown by IDs that hold format characters",
			"en",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey,
				Sites: []LabeledSite{{ID: "k3j9\u202ex2m4"}, {ID: "a1b2\x1b[2Jc3d4"}},
				Site:  &LabeledSite{ID: "z9y8\u202ex7w6\x1b[2J"}},
			`this deploy key ("github-actions") cannot act on z9y8\u{202e}x7w6\u{001b}[2J: it can deploy to k3j9\u{202e}x2m4, a1b2\u{001b}[2Jc3d4 and nothing else`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renderIn(t, tc.lang)
			if got := MapError(tc.err).Error(); got != tc.want {
				t.Errorf("MapError(%s) = %q, want %q", tc.err.Code, got, tc.want)
			}
		})
	}
}

// The server always fills the fields these refusals are worded from. A body
// that lacks one still renders a whole sentence: a site with no label is shown
// by its ID, as the server shows a site that is no longer active, and a body
// that leaves a slot with nothing to show gets the line that names no
// particulars, rather than a sentence with a hole in it.
func TestMapErrorWordsADeployKeyRefusalWithFieldsMissing(t *testing.T) {
	const (
		refusedEN  = `this is a deploy key: it can deploy to its own sites and nothing else. Use a login key for this command`
		refusedJA  = `このAPIキーはデプロイキーです。デプロイできるのは、このキーのサイトだけです。このコマンドにはログインキーを使ってください`
		notInKeyEN = `this deploy key cannot act on this site: it can deploy to its own sites and nothing else`
		notInKeyJA = `このデプロイキーはこのサイトを操作できません。デプロイできるのは、このキーのサイトだけです`
		notFoundEN = `no site of this account matches one of the sites named. Name a site by its subdomain, one of its domains, or its site ID`
		notFoundJA = `指定したサイトのうち、このアカウントのどのサイトにも該当しないものがあります。サイトはサブドメイン、ドメイン、またはサイトIDで指定してください`
		limitEN    = `this account already holds the most deploy keys it can have. Revoke one with "kamakiri key revoke <id>" first`
		limitJA    = `このアカウントのデプロイキーは上限に達しています。先に "kamakiri key revoke <id>" で1件失効させてください`
	)
	unlabelled := []LabeledSite{{ID: "k3j9x2m4p7q1w8e5"}, {ID: "a1b2c3d4e5f6g7h8", Label: "docs.kamakiri-pages.jp"}}
	empty := []LabeledSite{{}, {}}

	cases := []struct {
		name string
		lang string
		err  *ErrorResponse
		want string
	}{
		{"deploy_key_refused with no key", "en", &ErrorResponse{Code: "deploy_key_refused", Message: "raw", Sites: deploySites}, refusedEN},
		{"deploy_key_refused with no key, in Japanese", "ja", &ErrorResponse{Code: "deploy_key_refused", Message: "raw", Sites: deploySites}, refusedJA},
		{"deploy_key_refused with an unnamed key", "en", &ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: &KeyInfo{ID: deployKey.ID}, Sites: deploySites}, refusedEN},
		{"deploy_key_refused with no sites", "en", &ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey}, refusedEN},
		{"deploy_key_refused with sites that name nothing", "en", &ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey, Sites: empty}, refusedEN},
		{
			"deploy_key_refused with an unlabelled site",
			"en",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey, Sites: unlabelled},
			`this is a deploy key ("github-actions"): it can deploy to k3j9x2m4p7q1w8e5, docs.kamakiri-pages.jp and nothing else. Use a login key for this command`,
		},
		{
			"deploy_key_refused with one empty site among labelled ones",
			"en",
			&ErrorResponse{Code: "deploy_key_refused", Message: "raw", Key: deployKey, Sites: append([]LabeledSite{{}}, deploySites...)},
			`this is a deploy key ("github-actions"): it can deploy to example.com, docs.kamakiri-pages.jp and nothing else. Use a login key for this command`,
		},
		{"site_not_in_key with no refused site", "en", &ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites}, notInKeyEN},
		{"site_not_in_key with no refused site, in Japanese", "ja", &ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites}, notInKeyJA},
		{"site_not_in_key with an empty refused site", "en", &ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites, Site: &LabeledSite{}}, notInKeyEN},
		{"site_not_in_key with no key", "en", &ErrorResponse{Code: "site_not_in_key", Message: "raw", Sites: deploySites, Site: refusedSite}, notInKeyEN},
		{"site_not_in_key with no sites", "en", &ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Site: refusedSite}, notInKeyEN},
		{
			"site_not_in_key with an unlabelled refused site",
			"en",
			&ErrorResponse{Code: "site_not_in_key", Message: "raw", Key: deployKey, Sites: deploySites, Site: &LabeledSite{ID: "z9y8x7w6v5u4t3s2"}},
			`this deploy key ("github-actions") cannot act on z9y8x7w6v5u4t3s2: it can deploy to example.com, docs.kamakiri-pages.jp and nothing else`,
		},
		{"key_site_not_found with no reference", "en", &ErrorResponse{Code: "key_site_not_found", Message: "raw"}, notFoundEN},
		{"key_site_not_found with no reference, in Japanese", "ja", &ErrorResponse{Code: "key_site_not_found", Message: "raw"}, notFoundJA},
		{"deploy_key_limit with no limit", "en", &ErrorResponse{Code: "deploy_key_limit", Message: "raw"}, limitEN},
		{"deploy_key_limit with no limit, in Japanese", "ja", &ErrorResponse{Code: "deploy_key_limit", Message: "raw"}, limitJA},
		{"deploy_key_limit with a negative limit", "en", &ErrorResponse{Code: "deploy_key_limit", Message: "raw", Limit: -1}, limitEN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renderIn(t, tc.lang)
			if got := MapError(tc.err).Error(); got != tc.want {
				t.Errorf("MapError(%s) = %q, want %q", tc.err.Code, got, tc.want)
			}
		})
	}
}

// The tests above build the refusals as structs. These send each as the server
// does, so what is held is that the client reads every field it words the line
// from off the wire. The 429 carries Retry-After, which no rate-limit line
// reads.
func TestDeployKeyRefusalsAreReadOffTheWire(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			"deploy_key_refused",
			http.StatusForbidden,
			`{"code":"deploy_key_refused","message":"This is a deploy key.","key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"github-actions"},"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"},{"id":"a1b2c3d4e5f6g7h8","label":"docs.kamakiri-pages.jp"}]}`,
			`this is a deploy key ("github-actions"): it can deploy to example.com, docs.kamakiri-pages.jp and nothing else. Use a login key for this command`,
		},
		{
			"site_not_in_key",
			http.StatusForbidden,
			`{"code":"site_not_in_key","message":"This deploy key cannot act on it.","site":{"id":"z9y8x7w6v5u4t3s2","label":"blog.example.org"},"key":{"id":"3f2a9c1e-6b4d-4f0a-9c3e-2d7b5a8e1f40","name":"github-actions"},"sites":[{"id":"k3j9x2m4p7q1w8e5","label":"example.com"}]}`,
			`this deploy key ("github-actions") cannot act on blog.example.org: it can deploy to example.com and nothing else`,
		},
		{
			"key_site_not_found",
			http.StatusUnprocessableEntity,
			`{"code":"key_site_not_found","message":"No site of this account matches docs.kamakiri-pages.jp.","reference":"docs.kamakiri-pages.jp"}`,
			`no site of this account matches docs.kamakiri-pages.jp. Name a site by its subdomain, one of its domains, or its site ID`,
		},
		{
			"deploy_key_limit",
			http.StatusForbidden,
			`{"code":"deploy_key_limit","message":"This account already holds 10 deploy keys, the most it can have.","limit":10}`,
			`this account already holds 10 deploy keys, the most it can have. Revoke one with "kamakiri key revoke <id>" first`,
		},
		{
			"key_create_rate_limited",
			http.StatusTooManyRequests,
			`{"code":"key_create_rate_limited","message":"Too many keys created recently. Try again later."}`,
			`too many keys created recently. Try again later`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1800")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)

			_, err := NewClient(server.URL).ListKeys()
			var apiErr *ErrorResponse
			if !errors.As(err, &apiErr) || apiErr.Code != tc.name {
				t.Fatalf("ListKeys() error = %v, want an ErrorResponse with code %s", err, tc.name)
			}
			if got := MapError(err).Error(); got != tc.want {
				t.Errorf("MapError(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// The creation rate limit is its own code, so a creation flood inherits none of
// the other limiters' copy, and they inherit none of its.
func TestMapErrorKeyCreateRateLimitedIsDistinct(t *testing.T) {
	keyCreate := MapError(&ErrorResponse{Code: "key_create_rate_limited", Message: "raw"}).Error()
	for _, code := range []string{"rate_limited", "deploy_rate_limited", "register_rate_limited", "rollback_rate_limited"} {
		if other := MapError(&ErrorResponse{Code: code, Message: "raw"}).Error(); other == keyCreate {
			t.Errorf("MapError(%s) = %q, the same line as key_create_rate_limited", code, other)
		}
	}
}

// The daily upload budget refusals carry a message naming things only the server
// holds, such as the deploy's charge, what is left of the budget and when it
// resets. A future arm for either code must not replace that message with copy of
// its own.
func TestMapErrorUploadBudgetCodesEchoServerMessage(t *testing.T) {
	cases := map[string]string{
		"upload_budget_exhausted":   "This deploy needs 121 uploads (120 files plus its configuration), and your account has 37 of its 100,000 daily uploads left. The budget resets at 00:00 JST (in about 3h 12m). To ask for a higher budget, email support@kamakiri-labs.jp.",
		"platform_budget_exhausted": "Deploys are paused for every account until 00:00 JST (in about 3h 12m), when the platform's daily upload budget resets. We are notified automatically when this happens. Sites already live keep serving.",
	}

	for code, msg := range cases {
		got := MapError(&ErrorResponse{Code: code, Message: msg}).Error()
		if got != msg {
			t.Errorf("MapError(%s) = %q, want the server message %q", code, got, msg)
		}
	}
}

func TestListDeploysSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/pages/deploys") {
			t.Errorf("path = %q, want /v1/pages/deploys", r.URL.Path)
		}
		if r.URL.Query().Get("site_id") != "site123" {
			t.Errorf("site_id = %q, want site123", r.URL.Query().Get("site_id"))
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"deploys": []map[string]any{
				{"id": "20260401-120000-000", "status": "live", "created_at": "2026-04-01T12:00:00Z"},
				{"id": "20260401-100000-000", "status": nil, "created_at": "2026-04-01T10:00:00Z"},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	deploys, err := client.ListDeploys("site123")
	if err != nil {
		t.Fatalf("ListDeploys() error = %v", err)
	}
	if len(deploys) != 2 {
		t.Fatalf("len(deploys) = %d, want 2", len(deploys))
	}
	if deploys[0].ID != "20260401-120000-000" {
		t.Errorf("deploys[0].ID = %q, want 20260401-120000-000", deploys[0].ID)
	}
	if deploys[0].Status != "live" {
		t.Errorf("deploys[0].Status = %q, want live", deploys[0].Status)
	}
	if deploys[1].Status != "" {
		t.Errorf("deploys[1].Status = %q, want empty (JSON null)", deploys[1].Status)
	}
}

func TestListDeploysEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"deploys": []any{}})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	deploys, err := client.ListDeploys("site123")
	if err != nil {
		t.Fatalf("ListDeploys() error = %v", err)
	}
	if len(deploys) != 0 {
		t.Errorf("len(deploys) = %d, want 0", len(deploys))
	}
}

func TestListDeploysUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized", "message": "Missing API key."})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.ListDeploys("site123")
	if err == nil {
		t.Fatal("ListDeploys() expected error, got nil")
	}
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", apiErr.Code)
	}
}

func TestListDeploysForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]string{"code": "forbidden", "message": "Not your site."})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	_, err := client.ListDeploys("site123")
	if err == nil {
		t.Fatal("ListDeploys() expected error, got nil")
	}
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", apiErr.Code)
	}
}

func TestRollbackSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/rollback" {
			t.Errorf("path = %q, want /v1/pages/rollback", r.URL.Path)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "site123" {
			t.Errorf("site_id = %q, want site123", body["site_id"])
		}
		if body["deploy_id"] != "20260401-100000-000" {
			t.Errorf("deploy_id = %q, want 20260401-100000-000", body["deploy_id"])
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{
			"url":       "https://my-site.kamakiri-pages.jp",
			"deploy_id": "20260401-100000-000",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	result, err := client.Rollback("site123", "20260401-100000-000")
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if result.URL != "https://my-site.kamakiri-pages.jp" {
		t.Errorf("url = %q", result.URL)
	}
	if result.DeployID != "20260401-100000-000" {
		t.Errorf("deploy_id = %q, want 20260401-100000-000", result.DeployID)
	}
}

func TestRollbackDeployNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"code": "deploy_not_found", "message": "Deploy not found."})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	_, err := client.Rollback("site123", "99991231-235959")
	if err == nil {
		t.Fatal("Rollback() expected error, got nil")
	}
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "deploy_not_found" {
		t.Errorf("code = %q, want deploy_not_found", apiErr.Code)
	}
}

func TestRollbackSiteNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"code": "site_not_found", "message": "Site not found."})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	_, err := client.Rollback("nonexistent", "20260401-100000-000")
	if err == nil {
		t.Fatal("Rollback() expected error, got nil")
	}
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "site_not_found" {
		t.Errorf("code = %q, want site_not_found", apiErr.Code)
	}
}

func TestRollbackForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]string{"code": "forbidden", "message": "Not your site."})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	_, err := client.Rollback("site123", "20260401-100000-000")
	if err == nil {
		t.Fatal("Rollback() expected error, got nil")
	}
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", apiErr.Code)
	}
}

func TestMapErrorDeployNotFound(t *testing.T) {
	err := MapError(&ErrorResponse{Code: "deploy_not_found"})
	if err.Error() != "deploy not found" {
		t.Errorf("MapError(deploy_not_found) = %q", err.Error())
	}
}

func TestMapErrorAlreadyLive(t *testing.T) {
	err := MapError(&ErrorResponse{Code: "already_live"})
	if err.Error() != "deploy is already live" {
		t.Errorf("MapError(already_live) = %q", err.Error())
	}
}

// The server message names the colliding deploy id, which is what the user is
// asked to report, so MapError must surface it rather than a copy of its own.
func TestMapErrorDeployIdConflict(t *testing.T) {
	msg := "Deploy id 20260301-143022-384 is one this site has already used, so this deploy was refused."
	err := MapError(&ErrorResponse{Code: "deploy_id_conflict", Message: msg})
	if err.Error() != msg {
		t.Errorf("MapError(deploy_id_conflict) = %q, want the server message %q", err.Error(), msg)
	}
}

func TestMapErrorInvalidMode(t *testing.T) {
	err := MapError(&ErrorResponse{Code: "invalid_mode"})
	if err.Error() != "unknown CDN mode" {
		t.Errorf("MapError(invalid_mode) = %q", err.Error())
	}
}

func TestMapErrorInvalidWebAccelCredentials(t *testing.T) {
	err := MapError(&ErrorResponse{Code: "invalid_webaccel_credentials", Message: "unauthorized"})
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rejected") || !strings.Contains(msg, "apikeys") {
		t.Errorf("MapError(invalid_webaccel_credentials) = %q, want rejection + apikeys hint", msg)
	}
}

func TestSetCDNSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/cdn/set" {
			t.Errorf("path = %q, want /v1/pages/cdn/set", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer kk_live_test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "site123" {
			t.Errorf("site_id = %q", body["site_id"])
		}
		if body["mode"] != "cloudflare" {
			t.Errorf("mode = %q", body["mode"])
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"cdn_mode": "cloudflare"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.SetCDN("site123", "cloudflare")
	if err != nil {
		t.Fatalf("SetCDN() error = %v", err)
	}
	if result.CDNMode != "cloudflare" {
		t.Errorf("CDNMode = %q, want cloudflare", result.CDNMode)
	}
}

func TestCleanupCDNRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/cdn/cleanup" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "site123" {
			t.Errorf("site_id = %q", body["site_id"])
		}
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{
			"site_id": "site123", "sync_attempt_id": 9, "orphan_count": 2, "blocked_count": 1,
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.CleanupCDN("site123")
	if err != nil {
		t.Fatalf("CleanupCDN() error = %v", err)
	}
	if result.OrphanCount != 2 || result.BlockedCount != 1 {
		t.Errorf("got orphan=%d blocked=%d, want 2/1", result.OrphanCount, result.BlockedCount)
	}
}

func TestSetCDNWebAccelRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/cdn/set" {
			t.Errorf("path = %q", r.URL.Path)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["site_id"] != "site123" {
			t.Errorf("site_id = %v", body["site_id"])
		}
		if body["mode"] != "webaccel" {
			t.Errorf("mode = %v", body["mode"])
		}
		if body["token"] != "wa-token" {
			t.Errorf("token = %v", body["token"])
		}
		if body["secret"] != "wa-secret" {
			t.Errorf("secret = %v", body["secret"])
		}
		if _, present := body["domain_ids"]; present {
			t.Errorf("domain_ids must not be sent in managed mode, got %v", body["domain_ids"])
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"cdn_mode": "webaccel"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.SetCDNWebAccel("site123", "wa-token", "wa-secret")
	if err != nil {
		t.Fatalf("SetCDNWebAccel() error = %v", err)
	}
	if result.CDNMode != "webaccel" {
		t.Errorf("CDNMode = %q", result.CDNMode)
	}
}

// Empty token and secret must be absent from the request body, not sent as "":
// that is what makes the server reuse its stored credentials.
func TestSetCDNWebAccelOmitsTokenSecretWhenEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, present := body["token"]; present {
			t.Errorf("token must not be sent when empty, got %v", body["token"])
		}
		if _, present := body["secret"]; present {
			t.Errorf("secret must not be sent when empty, got %v", body["secret"])
		}
		if body["mode"] != "webaccel" {
			t.Errorf("mode = %v, want webaccel", body["mode"])
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"cdn_mode": "webaccel"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.SetCDNWebAccel("site123", "", "")
	if err != nil {
		t.Fatalf("SetCDNWebAccel() error = %v", err)
	}
	if result.CDNMode != "webaccel" {
		t.Errorf("CDNMode = %q", result.CDNMode)
	}
}

func TestSetCDNInvalidMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"code": "invalid_mode", "message": "Invalid CDN mode."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.SetCDN("site123", "bogus")
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "invalid_mode" {
		t.Errorf("code = %q, want invalid_mode", apiErr.Code)
	}
}

func TestCDNStatusSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/pages/cdn/status") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("site_id") != "site123" {
			t.Errorf("site_id = %q", r.URL.Query().Get("site_id"))
		}

		w.WriteHeader(200)
		json.NewEncoder(w).Encode(CDNStatusResponse{
			CDNMode:  "cloudflare",
			Provider: "cloudflare",
			Domains:  []CDNDomainStatus{{Domain: "example.com"}},
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.CDNStatus("site123")
	if err != nil {
		t.Fatalf("CDNStatus() error = %v", err)
	}
	if result.CDNMode != "cloudflare" {
		t.Errorf("cdn_mode = %q, want cloudflare", result.CDNMode)
	}
	if result.Provider != "cloudflare" {
		t.Errorf("provider = %q, want cloudflare", result.Provider)
	}
	if len(result.Domains) != 1 || result.Domains[0].Domain != "example.com" {
		t.Errorf("domains = %v", result.Domains)
	}
}

func TestPurgeCDNSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/pages/cdn/purge" {
			t.Errorf("path = %q, want /v1/pages/cdn/purge", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer kk_live_test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["site_id"] != "site123" {
			t.Errorf("site_id = %q, want site123", body["site_id"])
		}

		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{
			"site_id":         "site123",
			"sync_attempt_id": 42,
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	result, err := client.PurgeCDN("site123")
	if err != nil {
		t.Fatalf("PurgeCDN() error = %v", err)
	}
	if result.SiteID != "site123" {
		t.Errorf("SiteID = %q, want site123", result.SiteID)
	}
	if result.SyncAttemptID != 42 {
		t.Errorf("SyncAttemptID = %d, want 42", result.SyncAttemptID)
	}
}

func TestPurgeCDNNoCDNConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(ErrorResponse{
			Code:    "no_cdn_configured",
			Message: "Site has no CDN configured.",
		})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.PurgeCDN("site123")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "no_cdn_configured" {
		t.Errorf("code = %q, want no_cdn_configured", apiErr.Code)
	}
}

func TestPurgeCDNSiteNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "site_not_found", Message: "Site not found."})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test", HTTPClient: http.DefaultClient}
	_, err := client.PurgeCDN("nonexistent")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "site_not_found" {
		t.Errorf("code = %q, want site_not_found", apiErr.Code)
	}
}

func TestMapErrorNoCdnConfigured(t *testing.T) {
	err := MapError(&ErrorResponse{Code: "no_cdn_configured", Message: "raw"})
	if !strings.Contains(err.Error(), "no CDN configured") {
		t.Errorf("MapError(no_cdn_configured) = %q", err.Error())
	}
	// Pin the quoting style: every other MapError entry quotes commands as
	// "kamakiri ..." (escaped double-quotes), not `kamakiri ...` (backticks).
	// A regression to backticks would render literal backticks to the user.
	if !strings.Contains(err.Error(), `"kamakiri cdn`) {
		t.Errorf("MapError(no_cdn_configured) should quote command as \"kamakiri cdn …\", got %q", err.Error())
	}
}

// userAgentRecorder starts a server that records the User-Agent of the last
// request it saw and answers every call with an empty JSON object, which every
// response struct decodes into.
func userAgentRecorder(t *testing.T) (string, *string) {
	t.Helper()

	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.WriteHeader(200)
		io.WriteString(w, "{}")
	}))
	t.Cleanup(server.Close)

	return server.URL, &seen
}

func wantUserAgent(token string) string {
	return fmt.Sprintf("kamakiri/%s (%s/%s)", token, runtime.GOOS, runtime.GOARCH)
}

func TestUserAgentStatesTheClientVersionAndPlatform(t *testing.T) {
	baseURL, seen := userAgentRecorder(t)

	client := &Client{BaseURL: baseURL, Version: "0.4.2", HTTPClient: http.DefaultClient}
	if _, err := client.GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}

	if *seen != wantUserAgent("0.4.2") {
		t.Errorf("User-Agent = %q, want %q", *seen, wantUserAgent("0.4.2"))
	}
}

func TestUserAgentFallsBackToDevWhenTheClientStatesNoVersion(t *testing.T) {
	// Struct literals all over the tree build a client without a version. The
	// fallback is what keeps them from stating "kamakiri/ (...)", which a server
	// running a version floor refuses.
	baseURL, seen := userAgentRecorder(t)

	client := &Client{BaseURL: baseURL, HTTPClient: http.DefaultClient}
	if _, err := client.GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}

	if *seen != wantUserAgent("dev") {
		t.Errorf("User-Agent = %q, want %q", *seen, wantUserAgent("dev"))
	}
}

func TestNewClientStatesTheDevVersionByDefault(t *testing.T) {
	baseURL, seen := userAgentRecorder(t)

	if _, err := NewClient(baseURL).GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}

	if *seen != wantUserAgent("dev") {
		t.Errorf("User-Agent = %q, want %q", *seen, wantUserAgent("dev"))
	}
}

func TestDeployStatesTheUserAgentToo(t *testing.T) {
	// The multipart upload builds its own request, so it would be the one path
	// to lose the header if it stopped going through doRaw.
	baseURL, seen := userAgentRecorder(t)

	client := &Client{BaseURL: baseURL, Version: "0.4.2", HTTPClient: http.DefaultClient}
	if _, err := client.Deploy([]byte("{}"), strings.NewReader("tarball")); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if *seen != wantUserAgent("0.4.2") {
		t.Errorf("User-Agent = %q, want %q", *seen, wantUserAgent("0.4.2"))
	}
}

func TestSetVersionNormalizes(t *testing.T) {
	prev := version
	t.Cleanup(func() { version = prev })

	cases := []struct {
		in   string
		want string
	}{
		{"v0.1.1", "0.1.1"},
		{"0.1.1", "0.1.1"},
		{"(dev)", "dev"},
	}

	for _, tc := range cases {
		SetVersion(tc.in)
		if version != tc.want {
			t.Errorf("SetVersion(%q) stored %q, want %q", tc.in, version, tc.want)
		}
	}

	SetVersion("0.2.0")
	SetVersion("")
	if version != "0.2.0" {
		t.Errorf(`SetVersion("") stored %q, want the previous value 0.2.0`, version)
	}
}

func TestNewClientStampsTheSetVersion(t *testing.T) {
	prev := version
	t.Cleanup(func() { version = prev })

	SetVersion("v0.1.1")

	baseURL, seen := userAgentRecorder(t)
	client := NewClient(baseURL)
	if client.Version != "0.1.1" {
		t.Errorf("client.Version = %q, want 0.1.1", client.Version)
	}

	if _, err := client.GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}
	if *seen != wantUserAgent("0.1.1") {
		t.Errorf("User-Agent = %q, want %q", *seen, wantUserAgent("0.1.1"))
	}
}

func TestUpgradeRequiredFromA426WithAJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(426)
		json.NewEncoder(w).Encode(ErrorResponse{
			Code:    "upgrade_required",
			Message: "This version of the Kamakiri CLI is no longer supported.",
		})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).GetSite("site123")
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("GetSite() error = %v, want the upgrade sentinel", err)
	}
}

func TestUpgradeRequiredFromA426WithANonJSONBody(t *testing.T) {
	// A refusal from a proxy in front of the API carries no JSON. Without the
	// status check the user would read the bare status-code line and have
	// nothing to act on.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(426)
		io.WriteString(w, "<html>426 Upgrade Required</html>")
	}))
	defer server.Close()

	_, err := NewClient(server.URL).GetSite("site123")
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("GetSite() error = %v, want the upgrade sentinel", err)
	}
	if !strings.Contains(err.Error(), "no longer supported") {
		t.Errorf("error = %q, want the upgrade copy", err.Error())
	}
}

func TestUpgradeRequiredFromTheBodyCodeAtAnotherStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(ErrorResponse{Code: "upgrade_required", Message: "unsupported"})
	}))
	defer server.Close()

	_, err := NewClient(server.URL).GetSite("site123")
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("GetSite() error = %v, want the upgrade sentinel", err)
	}
}

func TestUpgradeRequiredCopyIsPinnedVerbatim(t *testing.T) {
	// Pinned as a literal because this is the whole message the user reads, and
	// nothing else in the tree compares it against one.
	const want = "this version of the CLI is no longer supported. Run \"kamakiri upgrade\" to move to the latest release"

	if got := ErrUpgradeRequired.Error(); got != want {
		t.Errorf("ErrUpgradeRequired.Error() = %q, want %q", got, want)
	}
}

func TestMapErrorPassesTheUpgradeSentinelThrough(t *testing.T) {
	// It is not an *ErrorResponse, so it leaves MapError untouched and every
	// command that prints its error verbatim renders the copy.
	if got := MapError(ErrUpgradeRequired); !errors.Is(got, ErrUpgradeRequired) {
		t.Errorf("MapError() = %v, want the upgrade sentinel", got)
	}
}

func TestAnErrorStatusWithANonJSONBodyNamesTheStatusCode(t *testing.T) {
	// The whole string is compared rather than the number in it, so the frame
	// around the status code is pinned to the key that renders it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, "<html>503 Service Unavailable</html>")
	}))
	defer server.Close()

	const want = "server returned 503"

	_, err := NewClient(server.URL).GetSite("site123")
	if err == nil {
		t.Fatalf("GetSite() error = nil, want %q", want)
	}
	if got := err.Error(); got != want {
		t.Errorf("GetSite() error = %q, want %q", got, want)
	}
}

func TestASuccessWithANonJSONBodyReportsAParseFailure(t *testing.T) {
	// The decoder's own complaint is passed through verbatim, so the whole
	// string pins both the frame's key and the payload it wraps.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "<html>200 OK</html>")
	}))
	defer server.Close()

	const want = "parse response: invalid character '<' looking for beginning of value"

	_, err := NewClient(server.URL).GetSite("site123")
	if err == nil {
		t.Fatalf("GetSite() error = nil, want %q", want)
	}
	if got := err.Error(); got != want {
		t.Errorf("GetSite() error = %q, want %q", got, want)
	}
}

func TestARequestBodyThatCannotMarshalReportsTheMarshalStep(t *testing.T) {
	// A channel has no JSON form, so the encoder refuses before a request is
	// built. The whole string is compared so the frame is pinned to its key.
	const want = "marshal request: json: unsupported type: chan int"

	err := NewClient("http://x").doJSON("POST", "/p", make(chan int), nil)
	if err == nil {
		t.Fatalf("doJSON() error = nil, want %q", want)
	}
	if got := err.Error(); got != want {
		t.Errorf("doJSON() error = %q, want %q", got, want)
	}
}

func TestAMethodTheStandardLibraryRejectsReportsTheCreateStep(t *testing.T) {
	// A space is not a token character, so the request never gets built. The
	// whole string is compared so the frame is pinned to its key.
	const want = `create request: net/http: invalid method "BAD METHOD"`

	err := NewClient("http://x").doJSON("BAD METHOD", "/p", nil, nil)
	if err == nil {
		t.Fatalf("doJSON() error = nil, want %q", want)
	}
	if got := err.Error(); got != want {
		t.Errorf("doJSON() error = %q, want %q", got, want)
	}
}

// failingTransport fails every request with a fixed error, which is how a
// transport-level failure is reached without a real network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("boom")
}

func TestATransportFailureReportsTheSendStep(t *testing.T) {
	// The transport's own error reaches the user through the frame, so the
	// whole string pins both the frame's key and the payload it wraps.
	const want = `request failed: Get "http://x/p": boom`

	client := NewClient("http://x")
	client.HTTPClient = &http.Client{Transport: failingTransport{}}

	err := client.doJSON("GET", "/p", nil, nil)
	if err == nil {
		t.Fatalf("doJSON() error = nil, want %q", want)
	}
	if got := err.Error(); got != want {
		t.Errorf("doJSON() error = %q, want %q", got, want)
	}
}

// advertisingServer answers every request with the given status and, unless
// value is empty, the latest-version header set to it. The body is an empty
// JSON object, which every response struct decodes into.
func advertisingServer(t *testing.T, status int, value string) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if value != "" {
			w.Header().Set(latestVersionHeader, value)
		}
		w.WriteHeader(status)
		io.WriteString(w, "{}")
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// seedLatestAdvertised puts a value in place and restores whatever was there
// afterwards, the way the version tests above do. Seeding rather than clearing
// is what lets a case assert that nothing was recorded, which is a different
// claim from asserting that the value is empty.
func seedLatestAdvertised(t *testing.T, value string) {
	t.Helper()

	prev := latestAdvertised
	t.Cleanup(func() { latestAdvertised = prev })
	latestAdvertised = value
}

func TestLatestAdvertisedRecordsTheHeader(t *testing.T) {
	seedLatestAdvertised(t, "")

	if _, err := NewClient(advertisingServer(t, 200, "0.9.0")).GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}

	if got := LatestAdvertised(); got != "0.9.0" {
		t.Errorf("LatestAdvertised() = %q, want 0.9.0", got)
	}
}

// A command can exit 0 having seen a response that was not a success: a
// best-effort read whose failure changes nothing (the prior-mode read before a
// CDN flip, the nudge inside a watch), a watch poll that is retried, and a
// `kamakiri status --recheck` whose nudge the server did not perform all do, so
// recording only on success would drop the release such a run was told about.
func TestLatestAdvertisedRecordsTheHeaderFromAnErrorResponse(t *testing.T) {
	seedLatestAdvertised(t, "")

	if _, err := NewClient(advertisingServer(t, 503, "0.9.0")).GetSite("site123"); err == nil {
		t.Fatal("GetSite() error = nil, want the server's error")
	}

	if got := LatestAdvertised(); got != "0.9.0" {
		t.Errorf("LatestAdvertised() = %q, want 0.9.0", got)
	}
}

func TestLatestAdvertisedStripsOneLeadingV(t *testing.T) {
	seedLatestAdvertised(t, "")

	if _, err := NewClient(advertisingServer(t, 200, "v0.9.0")).GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}

	if got := LatestAdvertised(); got != "0.9.0" {
		t.Errorf("LatestAdvertised() = %q, want 0.9.0", got)
	}
}

// The normal case: a server that names no release leaves whatever an earlier
// response named, rather than clearing it.
func TestLatestAdvertisedIgnoresAnAbsentHeader(t *testing.T) {
	seedLatestAdvertised(t, "0.9.0")

	if _, err := NewClient(advertisingServer(t, 200, "")).GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}

	if got := LatestAdvertised(); got != "0.9.0" {
		t.Errorf("LatestAdvertised() = %q, want the value already recorded", got)
	}
}

// The bound is checked on the raw value, before the v is stripped, so a value
// that would fit only once shortened is refused like any other overlong one.
func TestLatestAdvertisedHoldsTheHeaderToItsLengthBound(t *testing.T) {
	const seeded = "0.9.0"

	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"at the bound", strings.Repeat("9", maxLatestVersionBytes), strings.Repeat("9", maxLatestVersionBytes)},
		{"one byte past it", strings.Repeat("9", maxLatestVersionBytes+1), seeded},
		{"past it only before the v is stripped", "v" + strings.Repeat("9", maxLatestVersionBytes), seeded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedLatestAdvertised(t, seeded)

			if _, err := NewClient(advertisingServer(t, 200, tc.header)).GetSite("site123"); err != nil {
				t.Fatalf("GetSite() error = %v", err)
			}

			if got := LatestAdvertised(); got != tc.want {
				t.Errorf("LatestAdvertised() = %q, want %q", got, tc.want)
			}
		})
	}
}

// keyRoute is the method, path and decoded body of the request a key test's
// server received.
type keyRoute struct {
	method string
	path   string
	body   map[string]any
}

// keyServer answers every request with the given status and body. The returned
// function closes the server and then reports what it received: the handler
// records from the server's own goroutine, and Close returns only once that
// goroutine has finished, which is what orders the write against the read.
func keyServer(t *testing.T, status int, answer string) (string, func() *keyRoute) {
	t.Helper()
	var got keyRoute
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &got.body); err != nil {
				t.Errorf("decoding the request body %q: %v", raw, err)
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() *keyRoute {
		server.Close()
		return &got
	}
}

func assertKeyRoute(t *testing.T, got *keyRoute, method, path string, body map[string]any) {
	t.Helper()
	if got.method != method || got.path != path {
		t.Errorf("request = %s %s, want %s %s", got.method, got.path, method, path)
	}
	if !reflect.DeepEqual(got.body, body) {
		t.Errorf("request body = %#v, want %#v", got.body, body)
	}
}

const (
	keyID        = "5a1d7c93-2e8f-4b6a-8d01-7c4e9f3b2a65"
	otherKeyID   = "0f8c2a1e-6b4d-4f0a-9c3e-2d7b5a8e1f40"
	keyJSON      = `{"id":"` + keyID + `","name":"alexis-laptop","kind":"login","resource_ids":[],"sites":null,"created_at":"2026-11-12T10:30:00Z","last_used_at":"2026-11-16T09:00:00Z","current":true}`
	otherKeyJSON = `{"id":"` + otherKeyID + `","name":"unnamed","kind":"deploy","resource_ids":["site-a","site-b"],"sites":[{"id":"site-a","label":"example.com"},{"id":"site-b","label":"docs.kamakiri-pages.jp"}],"created_at":"2026-09-02T08:14:00Z","last_used_at":null,"current":false}`
)

func wantKey() Key {
	lastUsed := "2026-11-16T09:00:00Z"
	return Key{
		ID:          keyID,
		Name:        "alexis-laptop",
		Kind:        "login",
		ResourceIDs: []string{},
		CreatedAt:   "2026-11-12T10:30:00Z",
		LastUsedAt:  &lastUsed,
		Current:     true,
	}
}

func wantOtherKey() Key {
	return Key{
		ID:          otherKeyID,
		Name:        "unnamed",
		Kind:        "deploy",
		ResourceIDs: []string{"site-a", "site-b"},
		Sites: []LabeledSite{
			{ID: "site-a", Label: "example.com"},
			{ID: "site-b", Label: "docs.kamakiri-pages.jp"},
		},
		CreatedAt: "2026-09-02T08:14:00Z",
		Current:   false,
	}
}

// A deploy key's sites decode in the server's order, and a login key's null
// leaves Sites nil, as an older server's answer with no sites at all does.
func TestCurrentKeyDecodesADeployKeysSites(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Key
	}{
		{"deploy key", `{"key":` + otherKeyJSON + `}`, wantOtherKey()},
		{"login key", `{"key":` + keyJSON + `}`, wantKey()},
		{"older server", `{"key":{"id":"` + otherKeyID + `","name":"unnamed","kind":"deploy","resource_ids":["site-a"],"created_at":"2026-09-02T08:14:00Z"}}`, Key{
			ID: otherKeyID, Name: "unnamed", Kind: "deploy", ResourceIDs: []string{"site-a"}, CreatedAt: "2026-09-02T08:14:00Z",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := keyServer(t, 200, tc.body)
			key, err := NewClient(base).CurrentKey()
			if err != nil {
				t.Fatalf("CurrentKey() error = %v", err)
			}
			if !reflect.DeepEqual(*key, tc.want) {
				t.Errorf("CurrentKey() = %+v, want %+v", *key, tc.want)
			}
		})
	}
}

// siteLabels is the one rendering of a list of sites that every command shares:
// each label escaped, the ID standing in for a missing label, a site with
// neither left out, and the separator taken from the language in force.
func TestSiteLabelsJoinsAndEscapes(t *testing.T) {
	sites := []LabeledSite{
		{ID: "k3j9x2m4p7q1w8e5", Label: "example.com"},
		{ID: "a1b2c3d4e5f6g7h8", Label: ""},
		{ID: "", Label: ""},
		{ID: "z9y8x7w6v5u4t3s2", Label: "evil\u202e.example"},
	}

	if got, want := siteLabels(sites), `example.com, a1b2c3d4e5f6g7h8, evil\u{202e}.example`; got != want {
		t.Errorf("siteLabels() = %q, want %q", got, want)
	}
	if got := siteLabels(nil); got != "" {
		t.Errorf("siteLabels(nil) = %q, want empty", got)
	}

	renderIn(t, "ja")
	if got, want := siteLabels(sites), `example.com、a1b2c3d4e5f6g7h8、evil\u{202e}.example`; got != want {
		t.Errorf("siteLabels() in Japanese = %q, want %q", got, want)
	}
}

func TestListKeysDecodesEveryKeyInOrder(t *testing.T) {
	base, got := keyServer(t, 200, `{"keys":[`+otherKeyJSON+`,`+keyJSON+`]}`)

	keys, err := NewClient(base).ListKeys()
	if err != nil {
		t.Fatalf("ListKeys() error = %v", err)
	}
	assertKeyRoute(t, got(), "GET", "/v1/keys", nil)
	if want := []Key{wantOtherKey(), wantKey()}; !reflect.DeepEqual(keys, want) {
		t.Errorf("ListKeys() = %+v, want %+v", keys, want)
	}
}

// SitesText is the labels when there are any, and otherwise the key's resource
// IDs rendered the same way, so a key that names sites is never shown with none.
func TestKeySitesText(t *testing.T) {
	ids := []string{"k3j9x2m4p7q1w8e5", "a1b2c3d4e5f6g7h8"}
	labeled := []LabeledSite{{ID: ids[0], Label: "example.com"}}

	for _, tc := range []struct {
		name string
		key  Key
		want string
	}{
		{"labels win over the IDs", Key{ResourceIDs: ids, Sites: labeled}, "example.com"},
		{"nil sites", Key{ResourceIDs: ids}, "k3j9x2m4p7q1w8e5, a1b2c3d4e5f6g7h8"},
		{"empty sites", Key{ResourceIDs: ids, Sites: []LabeledSite{}}, "k3j9x2m4p7q1w8e5, a1b2c3d4e5f6g7h8"},
		{"unusable sites", Key{ResourceIDs: ids, Sites: []LabeledSite{{}}}, "k3j9x2m4p7q1w8e5, a1b2c3d4e5f6g7h8"},
		{"an ID to escape", Key{ResourceIDs: []string{"a\u202eb"}}, `a\u{202e}b`},
		{"nothing at all", Key{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.SitesText(); got != tc.want {
				t.Errorf("SitesText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// KindText words the two kinds the CLI knows in the language in force, and
// shows any other kind as the server sent it.
func TestKeyKindText(t *testing.T) {
	for _, tc := range []struct {
		lang string
		kind string
		want string
	}{
		{"en", "login", "login"},
		{"en", "deploy", "deploy"},
		{"en", "ci", "ci"},
		{"ja", "login", "ログイン"},
		{"ja", "deploy", "デプロイ"},
		{"ja", "ci", "ci"},
	} {
		t.Run(tc.lang+" "+tc.kind, func(t *testing.T) {
			renderIn(t, tc.lang)
			if got := (Key{Kind: tc.kind}).KindText(); got != tc.want {
				t.Errorf("KindText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCurrentKeyDecodesTheKey(t *testing.T) {
	base, got := keyServer(t, 200, `{"key":`+keyJSON+`}`)

	key, err := NewClient(base).CurrentKey()
	if err != nil {
		t.Fatalf("CurrentKey() error = %v", err)
	}
	assertKeyRoute(t, got(), "GET", "/v1/keys/current", nil)
	if want := wantKey(); !reflect.DeepEqual(*key, want) {
		t.Errorf("CurrentKey() = %+v, want %+v", *key, want)
	}
}

// A 200 that names no key describes nothing: status would render an empty key
// row from it.
func TestCurrentKeyRefusesAnAnswerThatNamesNoKey(t *testing.T) {
	for _, body := range []string{`{"key":{}}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			base, _ := keyServer(t, 200, body)

			key, err := NewClient(base).CurrentKey()
			if err == nil {
				t.Fatalf("CurrentKey() = %+v, want an error", key)
			}
			if got, want := err.Error(), "parse response: the response names no key"; got != want {
				t.Errorf("CurrentKey() error = %q, want %q", got, want)
			}
		})
	}
}

func TestRenameKeySendsTheNameAndDecodesTheStoredKey(t *testing.T) {
	base, got := keyServer(t, 200, `{"key":`+keyJSON+`}`)

	key, err := NewClient(base).RenameKey(keyID, "alexis-laptop")
	if err != nil {
		t.Fatalf("RenameKey() error = %v", err)
	}
	assertKeyRoute(t, got(), "PATCH", "/v1/keys/"+keyID, map[string]any{"name": "alexis-laptop"})
	if want := wantKey(); !reflect.DeepEqual(*key, want) {
		t.Errorf("RenameKey() = %+v, want %+v", *key, want)
	}
}

// A refusal comes back as the coded error response, which is what the key
// commands word for themselves.
func TestRenameKeyReturnsKeyNotFoundAsTheErrorResponse(t *testing.T) {
	base, _ := keyServer(t, 404, `{"code":"key_not_found","message":"Key not found."}`)

	_, err := NewClient(base).RenameKey(keyID, "x")
	apiErr, ok := err.(*ErrorResponse)
	if !ok || apiErr.Code != "key_not_found" {
		t.Fatalf("RenameKey() error = %#v, want a key_not_found *ErrorResponse", err)
	}
}

// The sites go out in the order given, and the name only when there is one:
// with none the field is absent, not empty or null, since the server refuses
// both and applies its default only to an absent name. The 201 decodes into the
// key as the list describes it and the key itself.
func TestCreateKeySendsTheSitesAndTheNameOnlyWhenGiven(t *testing.T) {
	answer := `{"key":` + otherKeyJSON + `,"api_key":"kk_deploy_minted"}`
	cases := []struct {
		label string
		name  string
		body  map[string]any
	}{
		{"no name", "", map[string]any{"sites": []any{"example.com", "docs"}}},
		{"a name", "github-actions", map[string]any{"name": "github-actions", "sites": []any{"example.com", "docs"}}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			base, got := keyServer(t, 201, answer)

			created, err := NewClient(base).CreateKey(tc.name, []string{"example.com", "docs"})
			if err != nil {
				t.Fatalf("CreateKey() error = %v", err)
			}
			assertKeyRoute(t, got(), "POST", "/v1/keys", tc.body)
			want := CreatedKey{Key: wantOtherKey(), APIKey: "kk_deploy_minted"}
			if !reflect.DeepEqual(*created, want) {
				t.Errorf("CreateKey() = %+v, want %+v", *created, want)
			}
		})
	}
}

// A success that carries no key has nothing to hand the user, so it is an
// error rather than an empty key printed where the key belongs.
func TestCreateKeyRefusesAnAnswerThatCarriesNoKey(t *testing.T) {
	for _, body := range []string{`{"key":` + otherKeyJSON + `}`, `{"key":` + otherKeyJSON + `,"api_key":""}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			base, _ := keyServer(t, 201, body)

			created, err := NewClient(base).CreateKey("", []string{"example.com"})
			if err == nil {
				t.Fatalf("CreateKey() = %+v, want an error", created)
			}
			if got, want := err.Error(), "parse response: the response names no key"; got != want {
				t.Errorf("CreateKey() error = %q, want %q", got, want)
			}
		})
	}
}

// A refusal comes back as the coded error response with the fields the
// deploy-key copy is worded from.
func TestCreateKeyReturnsTheRefusalAsTheErrorResponse(t *testing.T) {
	base, _ := keyServer(t, 422, `{"code":"key_site_not_found","message":"No site of this account matches docs.kamakiri-pages.jp.","reference":"docs.kamakiri-pages.jp"}`)

	_, err := NewClient(base).CreateKey("", []string{"docs"})
	apiErr, ok := err.(*ErrorResponse)
	if !ok || apiErr.Code != "key_site_not_found" || apiErr.Reference != "docs.kamakiri-pages.jp" {
		t.Fatalf("CreateKey() error = %#v, want a key_site_not_found *ErrorResponse carrying the reference", err)
	}
}

// The rotation goes out as a POST to the key's own rotate route, the ID
// path-escaped, with no body at all, and the 201 decodes into the new key as the
// list describes it, the key itself, and the key it replaced.
func TestRotateKeyPostsToTheKeysRouteAndDecodesTheAnswer(t *testing.T) {
	answer := `{"key":` + otherKeyJSON + `,"api_key":"kk_deploy_minted","replaced":{"id":"` + keyID + `","name":"unnamed"}}`
	base, got := keyServer(t, 201, answer)

	rotated, err := NewClient(base).RotateKey(keyID)
	if err != nil {
		t.Fatalf("RotateKey() error = %v", err)
	}
	route := got()
	assertKeyRoute(t, route, "POST", "/v1/keys/"+keyID+"/rotate", nil)
	want := RotatedKey{Key: wantOtherKey(), APIKey: "kk_deploy_minted", Replaced: KeyInfo{ID: keyID, Name: "unnamed"}}
	if !reflect.DeepEqual(*rotated, want) {
		t.Errorf("RotateKey() = %+v, want %+v", *rotated, want)
	}
}

// The ID is escaped into the path, so an argument holding a slash cannot reach
// another route. The key commands check the ID first; this holds for any other
// caller.
func TestRotateKeyEscapesTheID(t *testing.T) {
	var rawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPath = r.URL.EscapedPath()
		w.WriteHeader(404)
		io.WriteString(w, `{"code":"key_not_found","message":"Key not found."}`)
	}))
	t.Cleanup(server.Close)

	_, err := NewClient(server.URL).RotateKey("a/b")
	server.Close()
	if err == nil {
		t.Fatal("RotateKey() error = nil, want the refusal")
	}
	if want := "/v1/keys/a%2Fb/rotate"; rawPath != want {
		t.Errorf("request path = %q, want %q", rawPath, want)
	}
}

// A success that carries no key has nothing to hand the user, and the old key
// may already be gone, so it is an error rather than an empty key printed where
// the key belongs.
func TestRotateKeyRefusesAnAnswerThatCarriesNoKey(t *testing.T) {
	for _, body := range []string{
		`{"key":` + otherKeyJSON + `,"replaced":{"id":"` + keyID + `","name":"unnamed"}}`,
		`{"key":` + otherKeyJSON + `,"api_key":""}`,
		`{}`,
	} {
		t.Run(body, func(t *testing.T) {
			base, _ := keyServer(t, 201, body)

			rotated, err := NewClient(base).RotateKey(keyID)
			if err == nil {
				t.Fatalf("RotateKey() = %+v, want an error", rotated)
			}
			if got, want := err.Error(), "parse response: the response names no key"; got != want {
				t.Errorf("RotateKey() error = %q, want %q", got, want)
			}
		})
	}
}

// A refusal comes back as the coded error response, which the key commands word
// for themselves.
func TestRotateKeyReturnsTheRefusalAsTheErrorResponse(t *testing.T) {
	base, _ := keyServer(t, 409, `{"code":"key_not_rotatable","message":"This is a login key, which cannot be rotated."}`)

	_, err := NewClient(base).RotateKey(keyID)
	apiErr, ok := err.(*ErrorResponse)
	if !ok || apiErr.Code != "key_not_rotatable" {
		t.Fatalf("RotateKey() error = %#v, want a key_not_rotatable *ErrorResponse", err)
	}
}

func TestRevokeKeyDecodesTheRevokedKey(t *testing.T) {
	base, got := keyServer(t, 200, `{"key":{"id":"`+otherKeyID+`","name":"unnamed"}}`)

	key, err := NewClient(base).RevokeKey(otherKeyID)
	if err != nil {
		t.Fatalf("RevokeKey() error = %v", err)
	}
	assertKeyRoute(t, got(), "DELETE", "/v1/keys/"+otherKeyID, nil)
	if want := (KeyInfo{ID: otherKeyID, Name: "unnamed"}); *key != want {
		t.Errorf("RevokeKey() = %+v, want %+v", *key, want)
	}
}

func TestRevokeCurrentKeyDecodesTheRevokedKey(t *testing.T) {
	base, got := keyServer(t, 200, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)

	key, err := NewClient(base).RevokeCurrentKey()
	if err != nil {
		t.Fatalf("RevokeCurrentKey() error = %v", err)
	}
	assertKeyRoute(t, got(), "DELETE", "/v1/keys/current", nil)
	if want := (KeyInfo{ID: keyID, Name: "alexis-laptop"}); *key != want {
		t.Errorf("RevokeCurrentKey() = %+v, want %+v", *key, want)
	}
}

// A 301, 302 or 303 makes Go's client re-send a DELETE as a GET, and a GET on
// this path answers 200 with a key, which would read as a revoke.
func TestRevokeCurrentKeyRefusesARedirectThatChangesTheMethod(t *testing.T) {
	for _, status := range []int{301, 302, 303} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				methods = append(methods, r.Method)
				mu.Unlock()
				if r.Method == "DELETE" {
					w.Header().Set("Location", "/v1/keys/current")
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
			}))
			t.Cleanup(server.Close)

			key, err := NewClient(server.URL).RevokeCurrentKey()
			server.Close()
			if err == nil {
				t.Fatalf("RevokeCurrentKey() = %+v, want an error", key)
			}
			if len(methods) != 1 || methods[0] != "DELETE" {
				t.Errorf("methods seen = %v, want only the DELETE", methods)
			}
		})
	}
}

func TestRevokeCurrentKeyFollowsARedirectThatKeepsTheMethod(t *testing.T) {
	for _, status := range []int{307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			var routes, authorizations []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				routes = append(routes, r.Method+" "+r.URL.Path)
				authorizations = append(authorizations, r.Header.Get("Authorization"))
				mu.Unlock()
				if r.URL.Path == "/v1/keys/current" {
					w.Header().Set("Location", "/moved/keys/current")
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
			}))
			t.Cleanup(server.Close)

			client := NewClient(server.URL)
			client.APIKey = "kk_live_test"
			key, err := client.RevokeCurrentKey()
			server.Close()
			if err != nil {
				t.Fatalf("RevokeCurrentKey() error = %v", err)
			}
			if want := (KeyInfo{ID: keyID, Name: "alexis-laptop"}); *key != want {
				t.Errorf("RevokeCurrentKey() = %+v, want %+v", *key, want)
			}
			want := []string{"DELETE /v1/keys/current", "DELETE /moved/keys/current"}
			if !slices.Equal(routes, want) {
				t.Errorf("routes = %v, want %v", routes, want)
			}
			bearer := []string{"Bearer kk_live_test", "Bearer kk_live_test"}
			if !slices.Equal(authorizations, bearer) {
				t.Errorf("Authorization headers = %v, want %v", authorizations, bearer)
			}
		})
	}
}

// Moving from http to https on the same host keeps the key, so the redirected
// DELETE is followed and revokes.
func TestRevokeCurrentKeyFollowsARedirectFromHTTPToHTTPS(t *testing.T) {
	for _, status := range []int{307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			var seenByB, authorizations []string
			b := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seenByB = append(seenByB, r.Method+" "+r.URL.Path)
				authorizations = append(authorizations, r.Header.Get("Authorization"))
				mu.Unlock()
				io.WriteString(w, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
			}))
			t.Cleanup(b.Close)

			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", b.URL+r.URL.Path)
				w.WriteHeader(status)
			}))
			t.Cleanup(a.Close)

			client := NewClient(a.URL)
			client.APIKey = "kk_live_test"
			client.HTTPClient = b.Client()
			key, err := client.RevokeCurrentKey()
			a.Close()
			b.Close()
			if err != nil {
				t.Fatalf("RevokeCurrentKey() error = %v", err)
			}
			if want := (KeyInfo{ID: keyID, Name: "alexis-laptop"}); *key != want {
				t.Errorf("RevokeCurrentKey() = %+v, want %+v", *key, want)
			}
			if want := []string{"DELETE /v1/keys/current"}; !slices.Equal(seenByB, want) {
				t.Errorf("the https host saw %v, want %v", seenByB, want)
			}
			if want := []string{"Bearer kk_live_test"}; !slices.Equal(authorizations, want) {
				t.Errorf("Authorization headers = %v, want %v", authorizations, want)
			}
		})
	}
}

// A 200 may carry no key at all, which does not say which key was revoked, so
// it may not read as a revoke. A 3xx never reaches this check, since every
// request refuses one first; TestAnUnfollowedRedirectIsAnError holds that.
func TestRevokeCurrentKeyRefusesAnAnswerThatNamesNoKey(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"a 200 with an empty object", 200, `{}`},
		{"a 200 with an empty key", 200, `{"key":{}}`},
		{"a 200 with a name but no ID", 200, `{"key":{"name":"alexis-laptop"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := keyServer(t, tc.status, tc.body)

			key, err := NewClient(base).RevokeCurrentKey()
			if err == nil {
				t.Fatalf("RevokeCurrentKey() = %+v, want an error", key)
			}
			if got, want := err.Error(), "parse response: the response names no key"; got != want {
				t.Errorf("RevokeCurrentKey() error = %q, want %q", got, want)
			}
		})
	}
}

// Go strips Authorization from a redirect to another host and keeps the
// DELETE; the API's own 401 to that keyless request would read as the key
// being invalid.
func TestRevokeCurrentKeyRefusesARedirectToAnotherHost(t *testing.T) {
	for _, status := range []int{307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			var seenByB []string
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seenByB = append(seenByB, r.Method+" "+r.URL.Path)
				mu.Unlock()
				w.WriteHeader(401)
				io.WriteString(w, `{"code":"unauthorized","message":"invalid API key"}`)
			}))
			t.Cleanup(b.Close)
			portB := b.URL[strings.LastIndex(b.URL, ":")+1:]

			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://localhost:"+portB+"/v1/keys/current")
				w.WriteHeader(status)
			}))
			t.Cleanup(a.Close)

			client := NewClient(a.URL)
			client.APIKey = "kk_live_test"
			key, err := client.RevokeCurrentKey()
			a.Close()
			b.Close()
			if err == nil {
				t.Fatalf("RevokeCurrentKey() = %+v, want an error", key)
			}
			var apiErr *ErrorResponse
			if errors.As(err, &apiErr) {
				t.Errorf("RevokeCurrentKey() error = %v, want one that is not the API's answer", err)
			}
			if len(seenByB) != 0 {
				t.Errorf("the other host saw %v, want no request", seenByB)
			}
		})
	}
}

func TestRevokeCurrentKeyStopsAfterTenRedirects(t *testing.T) {
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.Header().Set("Location", r.URL.Path)
		w.WriteHeader(307)
	}))
	t.Cleanup(server.Close)

	if _, err := NewClient(server.URL).RevokeCurrentKey(); err == nil {
		t.Fatal("RevokeCurrentKey() error = nil, want one")
	}
	server.Close()
	if count < 2 || count > 11 {
		t.Errorf("requests = %d, want a bounded number between 2 and 11", count)
	}
}

// The guard runs ahead of an existing CheckRedirect, so a permissive one
// cannot let a method change or a dropped key through.
func TestRevokeCurrentKeyGuardsAheadOfAPermissiveCheckRedirect(t *testing.T) {
	cases := []struct {
		name   string
		status int
		other  bool
	}{
		{"a method change", 302, false},
		{"another host", 307, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			record := func(r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Method+" "+r.Host+r.URL.Path)
				mu.Unlock()
			}
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record(r)
				io.WriteString(w, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
			}))
			t.Cleanup(b.Close)
			portB := b.URL[strings.LastIndex(b.URL, ":")+1:]

			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "DELETE" {
					record(r)
					io.WriteString(w, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
					return
				}
				loc := "/v1/keys/current"
				if tc.other {
					loc = "http://localhost:" + portB + "/v1/keys/current"
				}
				w.Header().Set("Location", loc)
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(a.Close)

			client := NewClient(a.URL)
			client.APIKey = "kk_live_test"
			client.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
			key, err := client.RevokeCurrentKey()
			a.Close()
			b.Close()
			if err == nil {
				t.Fatalf("RevokeCurrentKey() = %+v, want the guard's error", key)
			}
			if len(seen) != 0 {
				t.Errorf("requests after the redirect = %v, want none", seen)
			}
		})
	}
}

// NewClient always sets an HTTPClient; a literal may not.
func TestRevokeCurrentKeyGuardsAClientWithoutAnHTTPClient(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if r.Method == "DELETE" {
			w.Header().Set("Location", "/v1/keys/current")
			w.WriteHeader(302)
			return
		}
		io.WriteString(w, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
	}))
	t.Cleanup(server.Close)

	client := &Client{BaseURL: server.URL, APIKey: "kk_live_test"}
	if _, err := client.RevokeCurrentKey(); err == nil {
		t.Fatal("RevokeCurrentKey() error = nil, want one")
	}
	server.Close()
	if !slices.Equal(methods, []string{"DELETE"}) {
		t.Errorf("methods seen = %v, want only the DELETE", methods)
	}
}

func TestRevokeCurrentKeyDoesNotMutateTheClientsHTTPClient(t *testing.T) {
	base, _ := keyServer(t, 200, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
	client := NewClient(base)
	original := client.HTTPClient

	if _, err := client.RevokeCurrentKey(); err != nil {
		t.Fatalf("RevokeCurrentKey() error = %v", err)
	}
	if client.HTTPClient != original || original.CheckRedirect != nil {
		t.Errorf("the client's HTTPClient was changed: same=%v CheckRedirect set=%v",
			client.HTTPClient == original, original.CheckRedirect != nil)
	}
}

func TestRevokeCurrentKeyKeepsAnExistingCheckRedirect(t *testing.T) {
	base, _ := keyServer(t, 200, `{"key":{"id":"`+keyID+`","name":"alexis-laptop"}}`)
	client := NewClient(base)
	errStop := errors.New("stop")
	client.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errStop }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(307)
	}))
	t.Cleanup(server.Close)
	client.BaseURL = server.URL

	if _, err := client.RevokeCurrentKey(); !errors.Is(err, errStop) {
		t.Errorf("RevokeCurrentKey() error = %v, want the original CheckRedirect's", err)
	}
}

// unsafeRedirect is the English copy of a refused redirect. Go reports the
// refusal as a *url.Error naming the original method in its own capitalised
// spelling and the Location as the server sent it, so refusedRedirect builds
// the whole line from those two.
const unsafeRedirect = "the server answered with a redirect that would have changed the request's method, dropped its API key, or moved it from https to http, so it was not followed"

func refusedRedirect(op, location string) string {
	return "request failed: " + op + ` "` + location + `": ` + unsafeRedirect
}

const siteJSON = `{"id":"site123","subdomain":"my-site","subdomain_enabled":true}`

// methodRecorder records the method and path of every request a test server
// sees. Reading what it recorded after the server's Close is what orders the
// handler's writes against the test's reads.
type methodRecorder struct {
	mu     sync.Mutex
	routes []string
}

func (m *methodRecorder) record(r *http.Request) {
	m.mu.Lock()
	m.routes = append(m.routes, r.Method+" "+r.URL.Path)
	m.mu.Unlock()
}

// teardown is the concrete case: Go re-sends the DELETE redirected by a 302 as
// a GET, and the GET's 200 would read as a teardown the server accepted.
func TestDeleteSiteRefusesARedirectThatChangesTheMethod(t *testing.T) {
	const location = "/v1/pages/sites/site123"
	var seen methodRecorder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		if r.Method == "DELETE" {
			w.Header().Set("Location", location)
			w.WriteHeader(302)
			return
		}
		io.WriteString(w, siteJSON)
	}))
	t.Cleanup(server.Close)

	site, err := NewClient(server.URL).DeleteSite("site123")
	server.Close()
	if err == nil {
		t.Fatalf("DeleteSite() = %+v, want the unsafe-redirect error", site)
	}
	if got, want := err.Error(), refusedRedirect("Delete", location); got != want {
		t.Errorf("DeleteSite() error = %q, want %q", got, want)
	}
	if want := []string{"DELETE /v1/pages/sites/site123"}; !slices.Equal(seen.routes, want) {
		t.Errorf("requests = %q, want %q", seen.routes, want)
	}
}

func TestAPostRefusesARedirectThatChangesTheMethod(t *testing.T) {
	const location = "/v1/pages/sites/site123"
	var seen methodRecorder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		if r.Method == "POST" {
			w.Header().Set("Location", location)
			w.WriteHeader(303)
			return
		}
		io.WriteString(w, siteJSON)
	}))
	t.Cleanup(server.Close)

	site, err := NewClient(server.URL).CreateSite("my-site")
	server.Close()
	if err == nil {
		t.Fatalf("CreateSite() = %+v, want the unsafe-redirect error", site)
	}
	if got, want := err.Error(), refusedRedirect("Post", location); got != want {
		t.Errorf("CreateSite() error = %q, want %q", got, want)
	}
	if want := []string{"POST /v1/pages/sites"}; !slices.Equal(seen.routes, want) {
		t.Errorf("requests = %q, want %q", seen.routes, want)
	}
}

// Deploy builds its own request rather than going through doJSON, so this pins
// that the guard covers it too.
func TestDeployRefusesARedirectThatChangesTheMethod(t *testing.T) {
	const location = "/v1/pages/deploys/latest"
	var seen methodRecorder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		if r.Method == "POST" {
			w.Header().Set("Location", location)
			w.WriteHeader(302)
			return
		}
		io.WriteString(w, `{"url":"https://my-site.kamakiri-pages.jp"}`)
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL)
	client.APIKey = "kk_live_test"
	result, err := client.Deploy([]byte(`{}`), strings.NewReader("tarball"))
	server.Close()
	if err == nil {
		t.Fatalf("Deploy() = %+v, want the unsafe-redirect error", result)
	}
	if got, want := err.Error(), refusedRedirect("Post", location); got != want {
		t.Errorf("Deploy() error = %q, want %q", got, want)
	}
	if want := []string{"POST /v1/pages/deploy"}; !slices.Equal(seen.routes, want) {
		t.Errorf("requests = %q, want %q", seen.routes, want)
	}
}

// Deploy streams its body through a pipe, which Go cannot replay, so it does
// not follow a 307 at all and never asks the guard: the answer reaches the
// status check as it came, and only the rule against a 3xx stops it reading as
// a deploy.
func TestDeployReportsA307ItCannotFollow(t *testing.T) {
	var seen methodRecorder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Location", "/v1/pages/deploy-elsewhere")
		w.WriteHeader(307)
		io.WriteString(w, `{"url":"https://my-site.kamakiri-pages.jp"}`)
	}))
	t.Cleanup(server.Close)

	result, err := NewClient(server.URL).Deploy([]byte(`{}`), strings.NewReader("tarball"))
	server.Close()
	if err == nil {
		t.Fatalf("Deploy() = %+v, want an error", result)
	}
	if got, want := err.Error(), "server returned 307"; got != want {
		t.Errorf("Deploy() error = %q, want %q", got, want)
	}
	if want := []string{"POST /v1/pages/deploy"}; !slices.Equal(seen.routes, want) {
		t.Errorf("requests = %q, want %q", seen.routes, want)
	}
}

// The Location names the TLS server's own host, 127.0.0.1, so Go keeps the
// Authorization header across the hop and the scheme is the only thing that
// refuses it. With another host name Go would strip the key, and the header
// check would refuse first.
func TestARedirectFromHTTPSToHTTPIsRefused(t *testing.T) {
	var seenByPlain methodRecorder
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenByPlain.record(r)
		io.WriteString(w, siteJSON)
	}))
	t.Cleanup(plain.Close)
	portPlain := plain.URL[strings.LastIndex(plain.URL, ":")+1:]
	location := "http://127.0.0.1:" + portPlain + "/v1/pages/sites/site123"

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(307)
	}))
	t.Cleanup(secure.Close)

	client := NewClient(secure.URL)
	client.APIKey = "kk_live_test"
	client.HTTPClient = secure.Client()
	site, err := client.GetSite("site123")
	secure.Close()
	plain.Close()
	if err == nil {
		t.Fatalf("GetSite() = %+v, want the unsafe-redirect error", site)
	}
	if got, want := err.Error(), refusedRedirect("Get", location); got != want {
		t.Errorf("GetSite() error = %q, want %q", got, want)
	}
	if len(seenByPlain.routes) != 0 {
		t.Errorf("the http server saw %q, want no request", seenByPlain.routes)
	}
}

// Moving from http to https on the same host keeps the key, so the redirected
// request is followed.
func TestAGetFollowsARedirectFromHTTPToHTTPS(t *testing.T) {
	var mu sync.Mutex
	var seenBySecure, authorizations []string
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenBySecure = append(seenBySecure, r.Method+" "+r.URL.Path)
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, siteJSON)
	}))
	t.Cleanup(secure.Close)

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", secure.URL+r.URL.Path)
		w.WriteHeader(307)
	}))
	t.Cleanup(plain.Close)

	client := NewClient(plain.URL)
	client.APIKey = "kk_live_test"
	client.HTTPClient = secure.Client()
	site, err := client.GetSite("site123")
	plain.Close()
	secure.Close()
	if err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}
	if want := (Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true}); !reflect.DeepEqual(*site, want) {
		t.Errorf("GetSite() = %+v, want %+v", *site, want)
	}
	if want := []string{"GET /v1/pages/sites/site123"}; !slices.Equal(seenBySecure, want) {
		t.Errorf("the https server saw %q, want %q", seenBySecure, want)
	}
	if want := []string{"Bearer kk_live_test"}; !slices.Equal(authorizations, want) {
		t.Errorf("Authorization headers = %q, want %q", authorizations, want)
	}
}

// Go hands back unchanged a 3xx it does not follow, such as a 301 with no
// Location or a 300. Such an answer is not the API's, so it never reads as
// success, whatever its body. UnregisterDomain decodes no result, which pins
// that the rule does not sit inside the decode of one.
func TestAnUnfollowedRedirectIsAnError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		call   func(c *Client) error
		want   string
	}{
		{"GetSite, a 301 with no Location", 301, siteJSON, func(c *Client) error {
			_, err := c.GetSite("site123")
			return err
		}, "server returned 301"},
		{"UnregisterDomain, a 301 with no Location", 301, `{}`, func(c *Client) error {
			return c.UnregisterDomain("example.com")
		}, "server returned 301"},
		{"RevokeCurrentKey, a 301 with no Location", 301, `{"moved":true}`, func(c *Client) error {
			_, err := c.RevokeCurrentKey()
			return err
		}, "server returned 301"},
		{"RevokeCurrentKey, a 300", 300, `{}`, func(c *Client) error {
			_, err := c.RevokeCurrentKey()
			return err
		}, "server returned 300"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := keyServer(t, tc.status, tc.body)

			err := tc.call(NewClient(base))
			if err == nil {
				t.Fatalf("error = nil, want %q", tc.want)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

// Go's HTTP/1 transport hands a 101 back as the final answer, with the rest of
// the connection as its body. It is not a success, whatever follows, so a call
// that decodes a result and one that decodes none both fail on it.
func TestATerminal101IsAnError(t *testing.T) {
	cases := []struct {
		name string
		tail string
		call func(c *Client) error
	}{
		{"UnregisterDomain, a bare 101", "", func(c *Client) error {
			return c.UnregisterDomain("example.com")
		}},
		{"DeleteSite, a 101 with a JSON tail", siteJSON, func(c *Client) error {
			_, err := c.DeleteSite("site123")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijacking: %v", err)
					return
				}
				defer conn.Close()
				buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: x\r\n\r\n" + tc.tail)
				buf.Flush()
			}))
			t.Cleanup(server.Close)

			err := tc.call(NewClient(server.URL))
			if err == nil {
				t.Fatal("error = nil, want server returned 101")
			}
			if got, want := err.Error(), "server returned 101"; got != want {
				t.Errorf("error = %q, want %q", got, want)
			}
		})
	}
}

// An error body with a code and no message would print as a blank line. The
// text falls back to the status line while the code is kept, so every switch
// on it still matches.
func TestAnErrorWithNoMessageNamesTheStatus(t *testing.T) {
	base, _ := keyServer(t, 404, `{"code":"site_not_found"}`)

	_, err := NewClient(base).GetSite("site123")
	if err == nil {
		t.Fatal("GetSite() error = nil, want one")
	}
	if got, want := err.Error(), "server returned 404"; got != want {
		t.Errorf("GetSite() error = %q, want %q", got, want)
	}
	var apiErr *ErrorResponse
	if !errors.As(err, &apiErr) || apiErr.Code != "site_not_found" {
		t.Errorf("GetSite() error = %#v, want a site_not_found *ErrorResponse", err)
	}
}

// NewClient always sets an HTTPClient; a literal may not, and every request
// must still go out. A nil HTTPClient panics inside net/http, so the call is
// recovered to keep a regression from aborting the rest of the package.
func TestARequestWorksWithoutAnHTTPClient(t *testing.T) {
	base, _ := keyServer(t, 200, siteJSON)
	client := &Client{BaseURL: base}

	site, err := func() (site *Site, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panicked: %v", r)
			}
		}()
		return client.GetSite("site123")
	}()
	if err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}
	if want := (Site{ID: "site123", Subdomain: "my-site", SubdomainEnabled: true}); !reflect.DeepEqual(*site, want) {
		t.Errorf("GetSite() = %+v, want %+v", *site, want)
	}
}

func TestARequestDoesNotMutateTheClientsHTTPClient(t *testing.T) {
	base, _ := keyServer(t, 200, siteJSON)
	client := NewClient(base)
	original := client.HTTPClient

	if _, err := client.GetSite("site123"); err != nil {
		t.Fatalf("GetSite() error = %v", err)
	}
	if client.HTTPClient != original || original.CheckRedirect != nil {
		t.Errorf("the client's HTTPClient was changed: same=%v CheckRedirect set=%v",
			client.HTTPClient == original, original.CheckRedirect != nil)
	}
}
