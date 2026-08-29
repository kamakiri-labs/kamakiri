package domain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kamakiri-labs/kamakiri/internal/api"
	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// regScript yields the next frame on each call and holds on the last, driving
// the registration watch without a real poll loop.
func regScript(frames ...*api.RegistrationList) func() (*api.RegistrationList, error) {
	idx := 0
	return func() (*api.RegistrationList, error) {
		f := frames[idx]
		if idx < len(frames)-1 {
			idx++
		}
		return f, nil
	}
}

func regWatchOpts(clk *fakeClock, ctx context.Context) watchOpts {
	return watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return time.Millisecond },
		now:          clk.Now,
		isTTY:        false,
		ctx:          ctx,
	}
}

func awaitingFrame(name string) *api.RegistrationList {
	return &api.RegistrationList{Registrations: []api.Registration{{Name: name, Status: "awaiting"}}}
}

func verifiedFrame(name string) *api.RegistrationList {
	return &api.RegistrationList{Registrations: []api.Registration{{Name: name, Status: "verified"}}}
}

func TestRegisterNoWaitPrintsRecordAndReturns(t *testing.T) {
	client := &mockClient{
		registerDomainFn: func(name string) (*api.Registration, error) {
			return &api.Registration{
				Name:        name,
				Status:      "awaiting",
				VerifyToken: "tok123",
				TXT:         api.RegistrationTXT{Name: "_kamakiri-verify." + name, Type: "TXT", Value: "kamakiri-verify=tok123"},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := register(client, "example.com", false, &out); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"_kamakiri-verify.example.com",
		"kamakiri-verify=tok123",
		"Registration requested for example.com",
		// Two accounts can hold a pending registration for one name, so both
		// values have to coexist. The `--no-wait` path must carry that warning
		// too.
		"keep any other `_kamakiri-verify` value already there",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestRegisterAlreadyVerifiedExactName(t *testing.T) {
	client := &mockClient{
		registerDomainFn: func(name string) (*api.Registration, error) {
			return &api.Registration{Name: name, Status: "verified", VerifiedAt: "2026-06-27T00:00:00Z"}, nil
		},
	}

	var out bytes.Buffer
	if err := register(client, "example.com", false, &out); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "already registered") {
		t.Errorf("output missing already-registered line:\n%s", s)
	}
	if !strings.Contains(s, "kamakiri domain set example.com") {
		t.Errorf("output missing next-step pointer:\n%s", s)
	}
}

func TestRegisterCoveredByAncestor(t *testing.T) {
	client := &mockClient{
		registerDomainFn: func(_ string) (*api.Registration, error) {
			// The server answers with the verified ancestor covering this host.
			return &api.Registration{Name: "example.com", Status: "verified", VerifiedAt: "2026-06-27T00:00:00Z"}, nil
		},
	}

	var out bytes.Buffer
	if err := register(client, "www.example.com", false, &out); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "already covered by your verified registration of example.com") {
		t.Errorf("output missing covered-by-ancestor line:\n%s", s)
	}
	// The next step names the requested host, not the ancestor.
	if !strings.Contains(s, "kamakiri domain set www.example.com") {
		t.Errorf("next-step pointer must name the requested host:\n%s", s)
	}
}

func TestRegisterUnknownStatusIsTreatedAsAwaiting(t *testing.T) {
	// Anything but verified still needs its record published, so a status this
	// CLI does not know must print the record rather than fail.
	client := &mockClient{
		registerDomainFn: func(name string) (*api.Registration, error) {
			return &api.Registration{
				Name:   name,
				Status: "some_future_status",
				TXT:    api.RegistrationTXT{Name: "_kamakiri-verify." + name, Type: "TXT", Value: "kamakiri-verify=t"},
			}, nil
		},
	}

	var out bytes.Buffer
	if err := register(client, "example.com", false, &out); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if !strings.Contains(out.String(), "_kamakiri-verify.example.com") {
		t.Errorf("an unverified registration must print the TXT instructions:\n%s", out.String())
	}
}

func TestRegisterInteractiveEntersWatch(t *testing.T) {
	client := &mockClient{
		registerDomainFn: func(name string) (*api.Registration, error) {
			return &api.Registration{
				Name:   name,
				Status: "awaiting",
				TXT:    api.RegistrationTXT{Name: "_kamakiri-verify." + name, Type: "TXT", Value: "kamakiri-verify=t"},
			}, nil
		},
	}

	var gotName, gotTxt string
	orig := watchRegisterFn
	watchRegisterFn = func(_ context.Context, _ APIClient, name, txtName string, _ io.Writer) error {
		gotName, gotTxt = name, txtName
		return nil
	}
	t.Cleanup(func() { watchRegisterFn = orig })

	var out bytes.Buffer
	if err := register(client, "example.com", true, &out); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if gotName != "example.com" {
		t.Errorf("watch name = %q, want example.com", gotName)
	}
	if gotTxt != "_kamakiri-verify.example.com" {
		t.Errorf("watch txtName = %q", gotTxt)
	}
}

func TestRegisterWatchVerified(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &mockClient{
		listRegistrationsFn: regScript(awaitingFrame("example.com"), verifiedFrame("example.com")),
	}

	var out bytes.Buffer
	err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, regWatchOpts(clk, context.Background()))
	if err != nil {
		t.Fatalf("registerWatch() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "✓ domain verified: example.com") {
		t.Errorf("missing verified milestone:\n%s", s)
	}
	if !strings.Contains(s, "kamakiri domain set example.com") {
		t.Errorf("missing next-step pointer:\n%s", s)
	}
}

func TestRegisterWatchWaitingTickNamesTheRecord(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &mockClient{
		listRegistrationsFn: regScript(awaitingFrame("example.com"), verifiedFrame("example.com")),
	}

	var out bytes.Buffer
	if err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, regWatchOpts(clk, context.Background())); err != nil {
		t.Fatalf("registerWatch() error = %v", err)
	}
	if !strings.Contains(out.String(), "waiting for your verification record (_kamakiri-verify.example.com TXT)") {
		t.Errorf("waiting tick must name the TXT record:\n%s", out.String())
	}
}

func TestRegisterWatchUnknownStatusKeepsWaiting(t *testing.T) {
	// Verified and vanished are the watch's only terminal states, so a status it
	// does not know keeps it polling rather than failing a registration that is
	// about to verify.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	unknown := &api.RegistrationList{Registrations: []api.Registration{{Name: "example.com", Status: "some_future_status"}}}
	client := &mockClient{
		listRegistrationsFn: regScript(unknown, verifiedFrame("example.com")),
	}

	var out bytes.Buffer
	if err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, regWatchOpts(clk, context.Background())); err != nil {
		t.Fatalf("registerWatch() error = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "waiting for your verification record") {
		t.Errorf("an unknown status must render the waiting tick:\n%s", s)
	}
	if !strings.Contains(s, "✓ domain verified: example.com") {
		t.Errorf("missing verified milestone:\n%s", s)
	}
}

func TestRegisterWatchVanishedReturnsError(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &mockClient{
		listRegistrationsFn: regScript(awaitingFrame("example.com"), &api.RegistrationList{}),
	}

	var out bytes.Buffer
	err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, regWatchOpts(clk, context.Background()))
	if err == nil {
		t.Fatal("registerWatch() expected error when the registration vanishes")
	}
	if !strings.Contains(err.Error(), "no longer present") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRegisterWatchCtrlCInterrupts(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the first select
	client := &mockClient{
		listRegistrationsFn: regScript(awaitingFrame("example.com")),
	}

	var out bytes.Buffer
	err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, regWatchOpts(clk, ctx))
	if err != ErrWatchInterrupted {
		t.Fatalf("registerWatch() error = %v, want ErrWatchInterrupted", err)
	}
	if !strings.Contains(out.String(), "Stopped watching") {
		t.Errorf("missing detach copy:\n%s", out.String())
	}
}

func TestRegisterWatchNonTTYNoANSI(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0), step: time.Second}
	client := &mockClient{
		listRegistrationsFn: regScript(awaitingFrame("example.com"), verifiedFrame("example.com")),
	}

	var out bytes.Buffer
	if err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, regWatchOpts(clk, context.Background())); err != nil {
		t.Fatalf("registerWatch() error = %v", err)
	}
	if strings.Contains(out.String(), "\033[") {
		t.Errorf("non-TTY output must not contain ANSI escapes:\n%q", out.String())
	}
}

func TestUnregisterSuccess(t *testing.T) {
	var gotName string
	client := &mockClient{
		unregisterDomainFn: func(name string) error {
			gotName = name
			return nil
		},
	}

	var out bytes.Buffer
	if err := Unregister(client, "example.com", &out); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}
	if gotName != "example.com" {
		t.Errorf("unregistered %q, want example.com", gotName)
	}
	if !strings.Contains(out.String(), "Unregistered example.com") {
		t.Errorf("output = %q", out.String())
	}
}

func TestUnregisterInUse(t *testing.T) {
	client := &mockClient{
		unregisterDomainFn: func(_ string) error {
			return &api.ErrorResponse{
				Code:    "domain_in_use",
				Message: "example.com is in use by one of your sites; remove it from that site first.",
			}
		},
	}

	var out bytes.Buffer
	err := Unregister(client, "example.com", &out)
	if err == nil {
		t.Fatal("Unregister() expected error")
	}
	if !strings.Contains(err.Error(), "in use by one of your sites") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestUnregisterIdempotentUnknown(t *testing.T) {
	// An unknown name is a no-op server-side, so the CLI reports success.
	client := &mockClient{
		unregisterDomainFn: func(_ string) error { return nil },
	}

	var out bytes.Buffer
	if err := Unregister(client, "never-registered.com", &out); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}
	if !strings.Contains(out.String(), "Unregistered never-registered.com") {
		t.Errorf("output = %q", out.String())
	}
}

func TestVerifyRoutesToRegisterWhenPendingRegistrationExists(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	registerCalled := false
	client := &mockClient{
		recheckDomainFn: func(_ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_not_found", Message: "Domain not found."}
		},
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return &api.RegistrationList{Registrations: []api.Registration{{Name: "example.com", Status: "awaiting"}}}, nil
		},
		registerDomainFn: func(name string) (*api.Registration, error) {
			registerCalled = true
			return &api.Registration{Name: name, Status: "awaiting", TXT: api.RegistrationTXT{Name: "_kamakiri-verify." + name, Type: "TXT", Value: "kamakiri-verify=t"}}, nil
		},
	}

	// Non-interactive, so it does not enter the blocking watch.
	var out bytes.Buffer
	if err := verify(client, "example.com", false, &out); err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if !registerCalled {
		t.Error("verify should route to register when a pending registration exists at the exact name")
	}
	if !strings.Contains(out.String(), "_kamakiri-verify.example.com") {
		t.Errorf("expected the register flow's TXT output:\n%s", out.String())
	}
}

func TestVerifyNotRoutedWhenNoPendingRegistration(t *testing.T) {
	t.Chdir(t.TempDir())
	setupCredentials(t)
	setupProjectConfig(t, "site123")

	client := &mockClient{
		recheckDomainFn: func(_ string) (*api.Domain, error) {
			return nil, &api.ErrorResponse{Code: "domain_not_found", Message: "Domain not found."}
		},
		listRegistrationsFn: func() (*api.RegistrationList, error) {
			return &api.RegistrationList{}, nil // no pending registration
		},
	}

	var out bytes.Buffer
	err := verify(client, "example.com", false, &out)
	if err == nil {
		t.Fatal("verify() expected domain_not_found error")
	}
	if !strings.Contains(err.Error(), "domain not found") {
		t.Errorf("error = %q", err.Error())
	}
}

// The give-up frame is written as "%s: %w", so the localized sentence sits in
// front of the last error and that error still unwraps out of the result.
func TestRegisterWatchGiveUpFrameStillUnwraps(t *testing.T) {
	regClient := &mockClient{listRegistrationsFn: func() (*api.RegistrationList, error) {
		return nil, errors.New("network is down")
	}}
	var out bytes.Buffer
	err := registerWatch(regClient, "example.com", "_kamakiri-verify.example.com", &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	if err == nil || !strings.Contains(err.Error(), i18n.Tf("domain.err_reg_fetch_give_up", 10)) ||
		errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), "network is down") {
		t.Errorf("registerWatch give-up error = %v, want the keyed frame over the last error", err)
	}
}

// A refused CLI version answers every poll the same way, so the registration
// watch aborts on the first one: no retry, nothing printed (the give-up copy
// would point at `kamakiri domain list`, which the same floor refuses), and the
// sentinel bare.
func TestRegisterWatchAbortsOnAVersionRefusal(t *testing.T) {
	polls := 0
	client := &mockClient{listRegistrationsFn: func() (*api.RegistrationList, error) {
		polls++
		return nil, api.ErrUpgradeRequired
	}}
	var out bytes.Buffer
	err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("registerWatch error = %v, want the refusal returned bare", err)
	}
	// main echoes this error verbatim, so anything wrapped around the sentinel
	// prints in front of the upgrade copy.
	if got := err.Error(); got != i18n.T("api.err_upgrade_required") {
		t.Errorf("err.Error() = %q, want the upgrade copy unframed", got)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1: the refusal must not be retried", polls)
	}
	if got := out.String(); got != "" {
		t.Errorf("output = %q, want nothing printed before the upgrade message", got)
	}
}

// The floor is armed while the watch is already running, so the refusal normally
// arrives with a waiting frame on screen. It is rewound before the watch
// returns, or the upgrade message would print under a frame that stopped
// meaning anything.
func TestRegisterWatchClearsTheFrameBeforeAbortingOnARefusal(t *testing.T) {
	var out bytes.Buffer
	var drawn string
	polls := 0
	client := &mockClient{listRegistrationsFn: func() (*api.RegistrationList, error) {
		polls++
		if polls == 1 {
			return awaitingFrame("example.com"), nil
		}
		drawn = out.String()
		return nil, api.ErrUpgradeRequired
	}}
	err := registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		isTTY:        true,
		ctx:          context.Background(),
	})
	if !errors.Is(err, api.ErrUpgradeRequired) {
		t.Fatalf("registerWatch error = %v, want the refusal returned bare", err)
	}
	if drawn == "" {
		t.Fatal("the first poll must leave a frame on screen, or the rewind pins nothing")
	}
	want := drawn + rewind(strings.Count(drawn, "\n"))
	if got := out.String(); got != want {
		t.Errorf("output = %q, want the drawn frame rewound: %q", got, want)
	}
}

// The registration watch has a fetch-failure line of its own, printed on each
// failed read rather than only at the end.
func TestRegisterWatchFetchFailureLine(t *testing.T) {
	var out bytes.Buffer
	client := &mockClient{listRegistrationsFn: func() (*api.RegistrationList, error) {
		return nil, errors.New("network is down")
	}}
	_ = registerWatch(client, "example.com", "_kamakiri-verify.example.com", &out, watchOpts{
		pollSchedule: func(time.Duration) time.Duration { return 0 },
		now:          time.Now,
		ctx:          context.Background(),
	})
	want := i18n.Tf("domain.status_fetch_failed", "network is down")
	if !strings.Contains(out.String(), want) {
		t.Errorf("registerWatch output = %q, want it to carry %q", out.String(), want)
	}
}
