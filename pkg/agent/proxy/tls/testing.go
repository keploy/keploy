package tls

import "sync"

// Testing helpers for the CAReady signal. These are regular package
// functions (NOT _test.go) so sibling-package tests (e.g.
// pkg/agent/routes) can reset and close the channel. The "ForTest"
// suffix signals intent — production code MUST NOT call them;
// SetupCA is the only legitimate path to close caReadyCh in a
// running agent.

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
