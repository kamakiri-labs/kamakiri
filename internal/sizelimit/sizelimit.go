// Package sizelimit renders a byte limit the way the server's own refusal does.
package sizelimit

import (
	"math"
	"strconv"
	"strings"
)

// MB renders a byte limit in MB. It matches the server's own refusal: one
// decimal, rounded half away from zero, with a trailing ".0" dropped, so a
// limit reads "100 MB", "1.5 MB" or "0.5 MB" on every surface. MB here is
// 1024*1024 bytes, as on the server. A limit under about 0.05 MB renders
// "0 MB", as the server renders it; only an operator sets such a limit.
//
// Measured sizes are not limits and do not go through here.
func MB(bytes int64) string {
	rounded := math.Round(float64(bytes)/(1024*1024)*10) / 10
	return strings.TrimSuffix(strconv.FormatFloat(rounded, 'f', 1, 64), ".0") + " MB"
}
