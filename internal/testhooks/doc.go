// Package testhooks is the only reader of the CLI's test-only environment
// variables, the switches an automated suite uses to drive the CLI against a
// local stack: one bypasses the CDN certificate gate, which has no real edge to
// probe there, and one shortens the freshness wait's poll cadence.
//
// Getenv returns a variable's value only in a build made with the testhooks
// build tag. In any other build it returns "" without reading the environment,
// so the variables are compiled out. Release builds and `go install` builds
// carry no tag, so a user's binary never honours them, whatever its
// environment holds. A check on the "(dev)" version string cannot stand in for
// the tag: `go install` builds are unstamped, so they report "(dev)" too.
package testhooks
