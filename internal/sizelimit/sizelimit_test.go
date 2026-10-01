package sizelimit

import "testing"

func TestMB(t *testing.T) {
	const mib = 1024 * 1024
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{100 * mib, "100 MB"},
		{500 * mib, "500 MB"},
		{mib, "1 MB"},
		{mib - 1, "1 MB"},
		{3 * mib / 2, "1.5 MB"},
		{512 * 1024, "0.5 MB"},
		{mib / 4, "0.3 MB"},
		{25*mib + 512*1024, "25.5 MB"},
		{40000, "0 MB"},
		{0, "0 MB"},
	} {
		if got := MB(tc.bytes); got != tc.want {
			t.Errorf("MB(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
