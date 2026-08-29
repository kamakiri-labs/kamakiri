package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
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
	resp, err := client.Register("user@example.com", true, "abc123hash")
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
	_, err := client.Register("bad", true, "somehash")
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
	resp, err := client.Verify("user@example.com", "ABC123", "my_secret")
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
	_, err := client.Verify("user@example.com", "WRONG1", "some_secret")
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
	_, err := client.Verify("user@example.com", "ABC123", "some_secret")

	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "too_many_attempts" {
		t.Errorf("code = %q, want too_many_attempts", apiErr.Code)
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
	result, err := client.RemoveDomain("www.example.com")
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
	_, err := client.RemoveDomain("nonexistent.com")
	apiErr, ok := err.(*ErrorResponse)
	if !ok {
		t.Fatalf("expected *ErrorResponse, got %T", err)
	}
	if apiErr.Code != "domain_not_found" {
		t.Errorf("code = %q", apiErr.Code)
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
		{"forbidden", "you don't own this site"},
		{"unauthorized", "invalid API key"},
		{"invalid_subdomain", "invalid subdomain"},
		{"subdomain_reserved", "that subdomain is reserved. Choose another"},
		{"subdomain_taken", "subdomain is already taken"},
		{"domain_taken", "already attached to another site. One host serves one site"},
		{"registration_limit", "too many pending registrations"},
		{"domain_reserved", "that domain is part of Kamakiri's own namespace, so it can't be used as a custom domain"},
		{"canonical_exists", "site already has a canonical domain"},
		{"no_canonical", "set a canonical domain first"},
		{"has_redirect_domains", "remove redirect domains"},
		{"is_canonical", "kamakiri domain unset"},
		{"domain_not_found", "domain not found"},
		{"invalid_domain", "invalid domain name"},
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
		"deploy_too_many_files": "Deploy has 60000 files; the limit is 50000 files per deploy.",
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

func TestMapErrorInvalidCredentials(t *testing.T) {
	err := MapError(&ErrorResponse{Code: "invalid_credentials"})
	if err.Error() != "CDN credentials are invalid" {
		t.Errorf("MapError(invalid_credentials) = %q", err.Error())
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

// A command can exit 0 having seen a response that was not a success: the status
// command degrades to a shorter page on any API failure other than a version
// refusal and still exits 0, so recording only on success would drop the release
// such a run was told about.
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
