//go:build windows

package util

// installSignalHandler is a no-op on Windows: SIGUSR1 does not
// exist there. The KillSwitch can still be tripped via the env
// var at startup or via [KillSwitch.Trip] from admin endpoints.
func installSignalHandler(ks *KillSwitch) {
	// Consume signalOnce so tests that call NewFromEnv repeatedly
	// behave the same as on Unix (idempotent setup, no panic).
	ks.signalOnce.Do(func() {})
}
