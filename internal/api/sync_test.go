package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// scriptedHandler returns successive JSON bodies from responses on each call,
// repeating the last entry once exhausted so a "stays running" test can poll on.
func scriptedHandler(t *testing.T, responses []Site) (http.HandlerFunc, *int64) {
	t.Helper()
	var counter int64
	return func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt64(&counter, 1) - 1
		if int(idx) >= len(responses) {
			idx = int64(len(responses) - 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responses[idx])
	}, &counter
}

func newClient(url string) *Client {
	return &Client{
		BaseURL:    url,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func ptr[T any](v T) *T { return &v }

func TestWaitForSyncResolvesOnOk(t *testing.T) {
	t.Parallel()

	siteRunning := Site{
		ID:        "site1",
		Subdomain: "newname",
		Sync:      &Sync{LatestAttemptID: 100, Outcome: "running"},
	}
	siteOk := Site{
		ID:        "site1",
		Subdomain: "newname",
		Sync:      &Sync{LatestAttemptID: 100, Outcome: "ok", Changes: []string{"caddy_push"}},
	}

	handler, _ := scriptedHandler(t, []Site{siteRunning, siteOk})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := newClient(srv.URL)
	final, err := c.WaitForSync("site1", 100, 3*time.Second)
	if err != nil {
		t.Fatalf("expected ok, got error: %v", err)
	}
	if final.Sync == nil || final.Sync.Outcome != "ok" {
		t.Fatalf("expected outcome=ok, got %+v", final.Sync)
	}
}

func TestWaitForSyncResolvesOnErrorAndSurfacesMessage(t *testing.T) {
	t.Parallel()

	siteRunning := Site{
		ID:   "site2",
		Sync: &Sync{LatestAttemptID: 200, Outcome: "running"},
	}
	siteErr := Site{
		ID: "site2",
		Sync: &Sync{
			LatestAttemptID: 200,
			Outcome:         "error",
			Error:           "caddy push failed: connection refused",
		},
	}

	handler, _ := scriptedHandler(t, []Site{siteRunning, siteErr})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := newClient(srv.URL)
	_, err := c.WaitForSync("site2", 200, 3*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	want := "reconcile failed: caddy push failed: connection refused"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestWaitForSyncTimesOutWhenStuckRunning(t *testing.T) {
	t.Parallel()

	siteRunning := Site{
		ID:   "site3",
		Sync: &Sync{LatestAttemptID: 300, Outcome: "running"},
	}

	handler, _ := scriptedHandler(t, []Site{siteRunning})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := newClient(srv.URL)
	_, err := c.WaitForSync("site3", 300, 200*time.Millisecond)
	if !errors.Is(err, ErrSyncTimeout) {
		t.Fatalf("expected ErrSyncTimeout, got %v", err)
	}
}

func TestWaitForSyncIgnoresOlderOkRows(t *testing.T) {
	t.Parallel()

	// Mock returns ok with id=5, but the test waits for id >= 10, so it must not
	// resolve.
	siteOldOk := Site{
		ID:   "site4",
		Sync: &Sync{LatestAttemptID: 5, Outcome: "ok"},
	}

	handler, _ := scriptedHandler(t, []Site{siteOldOk})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := newClient(srv.URL)
	_, err := c.WaitForSync("site4", 10, 200*time.Millisecond)
	if !errors.Is(err, ErrSyncTimeout) {
		t.Fatalf("expected ErrSyncTimeout (id <  sinceAttemptID), got %v", err)
	}
}

func TestWaitForSyncSurfacesGenericMessageWhenErrorEmpty(t *testing.T) {
	t.Parallel()

	siteErr := Site{
		ID:   "site5",
		Sync: &Sync{LatestAttemptID: 500, Outcome: "error", Error: ""},
	}

	handler, _ := scriptedHandler(t, []Site{siteErr})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := newClient(srv.URL)
	_, err := c.WaitForSync("site5", 500, 1*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "see `kamakiri status`") {
		t.Errorf("error = %q, want fallback hint", err.Error())
	}
}

func TestWaitForSyncToleratesTransientErrors(t *testing.T) {
	t.Parallel()

	// The server fails the first three calls, then returns an `ok` site.
	var counter int64
	siteOk := Site{
		ID:        "site6",
		Subdomain: "fine",
		Sync:      &Sync{LatestAttemptID: 600, Outcome: "ok"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt64(&counter, 1)
		if idx <= 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(siteOk)
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	final, err := c.WaitForSync("site6", 600, 10*time.Second)
	if err != nil {
		t.Fatalf("expected ok after transient errors, got %v", err)
	}
	if final.Sync == nil || final.Sync.Outcome != "ok" {
		t.Fatalf("expected outcome=ok, got %+v", final.Sync)
	}
}

func TestWaitForSyncPropagatesPersistentError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	_, err := c.WaitForSync("site7", 700, 10*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrSyncTimeout) {
		t.Fatalf("expected propagated server error, got ErrSyncTimeout")
	}
}

// A refused CLI version answers every poll the same way, so the wait spends no
// budget on it: one request, then the sentinel bare for the command layer to
// render as the upgrade message.
func TestWaitForSyncAbortsOnAVersionRefusal(t *testing.T) {
	t.Parallel()

	var polls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&polls, 1)
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	_, err := c.WaitForSync("site8", 800, 10*time.Second)
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("err = %v, want ErrUpgradeRequired", err)
	}
	// The command layer echoes this error verbatim, so anything wrapped around
	// the sentinel prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	if got := atomic.LoadInt64(&polls); got != 1 {
		t.Errorf("polls = %d, want 1: the refusal must not be retried", got)
	}
}

func TestPollIntervalStaysWithinJitterBounds(t *testing.T) {
	t.Parallel()

	// 500ms ± 100ms → all samples must land in [400ms, 600ms). 1000 trials
	// is enough to catch a regression where someone breaks the spread math.
	for i := 0; i < 1000; i++ {
		got := pollInterval()
		if got < 400*time.Millisecond || got >= 600*time.Millisecond {
			t.Fatalf("pollInterval() = %v, want in [400ms, 600ms)", got)
		}
	}
}

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
