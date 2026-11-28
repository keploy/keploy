//go:build linux && bpfsafety_short_grace

package bpfsafety

// shortGraceBuildEnabled enables tagged `grace-test-<tag>-*` directory
// isolation in binaries explicitly built with `-tags bpfsafety_short_grace`
// for end-to-end reap verification.
const shortGraceBuildEnabled = true
