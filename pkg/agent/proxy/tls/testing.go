package tls

import "sync"

// Testing helpers for the CAReady signal. These are regular package
// functions (NOT _test.go) so sibling-package tests (e.g.
// pkg/agent/routes) can reset and close the channel. The "ForTest"
// suffix signals intent — production code MUST NOT call them;
// SetupCA is the only legitimate path to close caReadyCh in a
// running agent.
//
// These helpers mutate package-global state (caReadyOnce, caReadyCh,
// caFailure). Go compiles one test binary per package and runs
// different packages in separate processes, so cross-package parallel
// runs do NOT share these globals. Serialization is needed within a
// single test binary — most concretely, when multiple tests inside
// pkg/agent/routes (which imports this package) reset/close the signal
// and may run in parallel via t.Parallel() or stress-run with -count.
// Tests in pkg/agent/proxy/tls itself likewise serialize on the same
// mutex so a future contributor who adds t.Parallel() there doesn't
// introduce a race on caReadyCh.
//
// CAReadyTestMu is the single serializer every test that touches the
// signal must hold. The canonical pattern is:
//
//	tls.CAReadyTestMu.Lock()
//	defer tls.CAReadyTestMu.Unlock()
//	tls.ResetCAReadyForTest()
//	// ... exercise code under test ...
//
// Or, for a callback-scoped guard, use WithCAReadyTestLock below which
// wraps the Lock/defer Unlock idiom so callers can't forget to unlock.

// CAReadyTestMu serializes access to the CAReady package-global state
// (caReadyOnce, caReadyCh, caFailure) within a single test binary.
// All test helpers below acquire it; external callers (tests in
// packages that import this one) must hold it for the duration of any
// read-then-write sequence on the signal.
var CAReadyTestMu sync.Mutex

// ResetCAReadyForTest rebuilds caReadyOnce and caReadyCh and clears any
// recorded CA-setup failure so a fresh test can observe the "CA not
// ready, no error" baseline regardless of test ordering. Tests that
// need a closed channel should follow this with CloseCAReadyForTest;
// tests exercising the failure path should call MarkCAFailed after
// reset.
//
// Acquires CAReadyTestMu internally so a test that only resets before
// assertions does not need explicit locking. Tests performing
// read-then-write sequences (e.g. reset → assert behaviour → close →
// assert again) must take the mutex themselves to make the sequence
// atomic relative to concurrent tests in sibling packages.
func ResetCAReadyForTest() {
	CAReadyTestMu.Lock()
	defer CAReadyTestMu.Unlock()
	caReadyOnce = sync.Once{}
	caReadyCh = make(chan struct{})
	caFailure.Store(nil)
}

// CloseCAReadyForTest closes the CAReady channel via the same
// markCAReady path used by SetupCA, so production semantics are
// exercised by tests. Acquires CAReadyTestMu internally.
func CloseCAReadyForTest() {
	CAReadyTestMu.Lock()
	defer CAReadyTestMu.Unlock()
	markCAReady()
}

// WithCAReadyTestLock runs fn while holding CAReadyTestMu, so a test can
// perform a multi-step sequence (reset, configure, exercise, assert)
// atomically relative to concurrent tests. Prefer this over manual
// Lock/Unlock in tests to avoid missed-Unlock bugs on assertion failure
// paths.
func WithCAReadyTestLock(fn func()) {
	CAReadyTestMu.Lock()
	defer CAReadyTestMu.Unlock()
	fn()
}
