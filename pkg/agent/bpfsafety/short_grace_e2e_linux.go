//go:build linux && bpfsafety_short_grace

package bpfsafety

// shortGraceBuildEnabled lifts the 15-minute production floor on
// KEPLOY_BPF_GRACE_DURATION ONLY in binaries explicitly built with
// -tags bpfsafety_short_grace for end-to-end reap verification.
const shortGraceBuildEnabled = true
