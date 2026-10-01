//go:build !testhooks

package testhooks

// Enabled reports whether this build honours the test-only environment
// variables. It is true only in a build made with the testhooks tag.
const Enabled = false

// Getenv returns the named test-only variable's value. It reads the
// environment only in a build made with the testhooks tag and returns ""
// in any other build.
func Getenv(string) string {
	return ""
}
