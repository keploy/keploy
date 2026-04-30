//go:build !windows

package util

import (
	"testing"
)

// TestSIGUSR1Trips exercises the signal-handler code path by calling
// the exported-for-testing hook [tripOnSignalForTest], which performs
// exactly the action the SIGUSR1 goroutine would perform on receipt
// of the signal. We deliberately avoid syscall.Kill(self, SIGUSR1):
// on CI runners with non-trivial signal masks, self-signalling can
// be swallowed or race the test runner's own handlers, making the
// test flaky for reasons unrelated to the code under test.
func TestSIGUSR1Trips(t *testing.T) {
	ks := New()
	// Simulate the goroutine receiving SIGUSR1.
	tripOnSignalForTest(ks)
	if !ks.Enabled() {
		t.Fatalf("expected SIGUSR1 handler to trip the kill switch")
	}

	ks.Reset()
	if ks.Enabled() {
		t.Fatalf("expected Reset to clear the kill switch")
	}

	// Re-tripping is idempotent and compatible with Reset.
	tripOnSignalForTest(ks)
	if !ks.Enabled() {
		t.Fatalf("expected second simulated SIGUSR1 to trip again")
	}
}

// TestNewFromEnvInstallsSignalHandler verifies that NewFromEnv does
// not panic when it installs the signal handler, and that the
// returned switch is still usable afterwards.
func TestNewFromEnvInstallsSignalHandler(t *testing.T) {
	t.Setenv(envDisableParsing, "")
	ks := NewFromEnv()
	if ks == nil {
		t.Fatalf("NewFromEnv returned nil")
	}
	if ks.Enabled() {
		t.Fatalf("NewFromEnv with unset env should not be tripped")
	}
	// Calling installSignalHandler twice on the same switch must
	// be a no-op (sync.Once).
	installSignalHandler(ks)
	installSignalHandler(ks)
}
