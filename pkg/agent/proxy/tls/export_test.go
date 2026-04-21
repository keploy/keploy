package tls

import "sync"

// This file is only compiled into the test binary (suffix _test.go). It
// exposes a controlled way for tests in other packages to reset and
// close the package-level CAReady signal without widening the public
// API surface for production callers.
//
// Production code MUST NOT depend on these helpers — SetupCA is the
// only way to legitimately close caReadyCh in a running agent.

// ResetCAReadyForTest rebuilds caReadyOnce and caReadyCh so a fresh
// test can observe the "CA not ready" state regardless of test
// ordering. Tests that need a closed channel should follow this with
// CloseCAReadyForTest.
func ResetCAReadyForTest() {
	caReadyOnce = sync.Once{}
	caReadyCh = make(chan struct{})
}

// CloseCAReadyForTest closes the CAReady channel via the same
// markCAReady path used by SetupCA, so production semantics are
// exercised by tests.
func CloseCAReadyForTest() { markCAReady() }
