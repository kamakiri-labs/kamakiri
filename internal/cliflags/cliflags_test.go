package cliflags

import (
	"os"
	"strings"
	"testing"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// The catalog is pinned to English so the assertions below hold whatever
// locale the suite runs under: every flag error renders from the message
// catalog. Load rather than Setup: nothing here reports which language is
// in force, only renders in it.
func TestMain(m *testing.M) {
	i18n.Load("en")
	os.Exit(m.Run())
}

func TestSplitArg(t *testing.T) {
	cases := []struct {
		input    string
		wantName string
		wantVal  string
		wantHas  bool
	}{
		{"--token=foo", "--token", "foo", true},
		{"--token", "--token", "", false},
		{"--token=", "--token", "", true}, // empty equals-form value
		{"--alias", "--alias", "", false},
		{"--domain-id=example.com=1001", "--domain-id", "example.com=1001", true}, // splits on the first `=` only
		{"-t", "-t", "", false},
		{"=foo", "=foo", "", false}, // a leading `=` is not a split point (the split index must be > 0)
		{"--", "--", "", false},
	}
	for _, tc := range cases {
		name, val, has := SplitArg(tc.input)
		if name != tc.wantName || val != tc.wantVal || has != tc.wantHas {
			t.Errorf("SplitArg(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.input, name, val, has, tc.wantName, tc.wantVal, tc.wantHas)
		}
	}
}

func TestValueSpaceForm(t *testing.T) {
	args := []string{"--token", "wa-token", "--secret", "wa-secret"}
	v, next, err := Value(args, 0, false, "", "--token", "")
	if err != nil {
		t.Fatal(err)
	}
	if v != "wa-token" {
		t.Errorf("value = %q", v)
	}
	if next != 1 {
		t.Errorf("next = %d, want 1", next)
	}
}

func TestValueEqualsForm(t *testing.T) {
	args := []string{"--token=wa-token"}
	v, next, err := Value(args, 0, true, "wa-token", "--token", "")
	if err != nil {
		t.Fatal(err)
	}
	if v != "wa-token" {
		t.Errorf("value = %q", v)
	}
	if next != 0 {
		t.Errorf("next = %d, want 0 (same index for equals-form)", next)
	}
}

func TestValueEqualsFormEmptyRejected(t *testing.T) {
	args := []string{"--token="}
	_, _, err := Value(args, 0, true, "", "--token", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "requires a value after the equals sign") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestValueMissingNoArgs(t *testing.T) {
	args := []string{"--token"}
	_, _, err := Value(args, 0, false, "", "--token", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "--token requires a value") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestValueMissingErrorIncludesHint(t *testing.T) {
	args := []string{"--status"}
	_, _, err := Value(args, 0, false, "", "--status", "301, 302, 307, or 308")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "(301, 302, 307, or 308)") {
		t.Errorf("error should contain hint in parens: %q", err.Error())
	}
}

func TestValueEmptyEqualsErrorIncludesHint(t *testing.T) {
	args := []string{"--cdn-id="}
	_, _, err := Value(args, 0, true, "", "--cdn-id", "numeric WebAccel site ID")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "(numeric WebAccel site ID)") {
		t.Errorf("error should contain hint: %q", err.Error())
	}
}

func TestRequireNoValueAcceptsBoolean(t *testing.T) {
	if err := RequireNoValue("--alias", false); err != nil {
		t.Errorf("space-form boolean should be accepted: %v", err)
	}
}

func TestRequireNoValueRejectsEqualsForm(t *testing.T) {
	cases := []string{"--alias", "--redirect", "--temporary"}
	for _, name := range cases {
		err := RequireNoValue(name, true)
		if err == nil {
			t.Errorf("RequireNoValue(%q, true) expected error", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should mention flag name %q: %v", name, err)
		}
		if !strings.Contains(err.Error(), "takes no value") {
			t.Errorf("error should say 'takes no value': %v", err)
		}
	}
}

func TestFormatHint(t *testing.T) {
	if got := FormatHint(""); got != "" {
		t.Errorf("FormatHint(\"\") = %q, want \"\"", got)
	}
	if got := FormatHint("301, 302"); got != " (301, 302)" {
		t.Errorf("FormatHint = %q", got)
	}
}

func TestValueIntegrationWithSplitArg(t *testing.T) {
	// SplitArg feeding Value, the way a real dispatch loop uses them.
	args := []string{"--cdn-id=1001", "--alias", "--status", "301"}

	name, val, has := SplitArg(args[0])
	if name != "--cdn-id" {
		t.Fatalf("name[0] = %q", name)
	}
	v, next, err := Value(args, 0, has, val, name, "")
	if err != nil || v != "1001" || next != 0 {
		t.Errorf("equals-form: got (%q, %d, %v)", v, next, err)
	}

	name, val, has = SplitArg(args[1])
	if val != "" {
		t.Errorf("boolean flag should have empty value, got %q", val)
	}
	if err := RequireNoValue(name, has); err != nil {
		t.Errorf("--alias should not error: %v", err)
	}

	name, val, has = SplitArg(args[2])
	v, next, err = Value(args, 2, has, val, name, "")
	if err != nil || v != "301" || next != 3 {
		t.Errorf("space-form: got (%q, %d, %v)", v, next, err)
	}
}
