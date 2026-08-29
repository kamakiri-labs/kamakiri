package api

import "testing"

// SubdomainLine is one line assembled from several catalog lookups, and every
// state it can render reaches a user through `kamakiri subdomain get` and the
// subdomain row of `kamakiri status`. The whole line is compared rather than a
// marker inside it, since a substring check passes just as happily when one of
// the lookups reads the wrong key.
func TestSubdomainLineRendersEveryState(t *testing.T) {
	observed := func(s string) *string { return &s }

	tests := []struct {
		name string
		site *Site
		want string
	}{
		{
			name: "reconciled and in sync",
			site: &Site{Subdomain: "demo", SubdomainEnabled: true, SubdomainObserved: observed("demo"),
				Sync: &Sync{Outcome: "ok"}},
			want: "demo.kamakiri-pages.jp  ✓",
		},
		{
			name: "never reconciled and no sync row",
			site: &Site{Subdomain: "demo", SubdomainEnabled: true},
			want: "demo.kamakiri-pages.jp",
		},
		{
			name: "first reconcile queued",
			site: &Site{Subdomain: "demo", SubdomainEnabled: true, Sync: &Sync{Outcome: "queued"}},
			want: "demo.kamakiri-pages.jp  ⧗ never reconciled",
		},
		{
			name: "rename queued",
			site: &Site{Subdomain: "new", SubdomainEnabled: true, SubdomainObserved: observed("old"),
				Sync: &Sync{Outcome: "queued"}},
			want: "new → old.kamakiri-pages.jp ⧗ pending",
		},
		{
			name: "rename running",
			site: &Site{Subdomain: "new", SubdomainEnabled: true, SubdomainObserved: observed("old"),
				Sync: &Sync{Outcome: "running"}},
			want: "new → old.kamakiri-pages.jp ⧗ syncing",
		},
		{
			name: "rename failed with the server's reason",
			site: &Site{Subdomain: "new", SubdomainEnabled: true, SubdomainObserved: observed("old"),
				Sync: &Sync{Outcome: "error", Error: "edge refused the rename"}},
			want: "new → old.kamakiri-pages.jp ✗ failed (edge refused the rename)",
		},
		{
			name: "rename failed with no reason falls back to the status pointer",
			site: &Site{Subdomain: "new", SubdomainEnabled: true, SubdomainObserved: observed("old"),
				Sync: &Sync{Outcome: "error"}},
			want: "new → old.kamakiri-pages.jp ✗ failed (see `kamakiri status`)",
		},
		{
			name: "disabled carries the marker",
			site: &Site{Subdomain: "demo", SubdomainObserved: observed("demo"), Sync: &Sync{Outcome: "ok"}},
			want: "demo.kamakiri-pages.jp (disabled)  ✓",
		},
		{
			name: "disabled while a rename is queued",
			site: &Site{Subdomain: "new", SubdomainObserved: observed("old"), Sync: &Sync{Outcome: "queued"}},
			want: "new → old.kamakiri-pages.jp (disabled) ⧗ pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubdomainLine(tt.site); got != tt.want {
				t.Errorf("SubdomainLine() =\n  %q\nwant:\n  %q", got, tt.want)
			}
		})
	}
}
