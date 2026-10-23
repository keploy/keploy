//go:build !linux

package tls

import "testing"

// trustStoreLockForTest: off Linux the lock is a no-op.
func trustStoreLockForTest(*testing.T) {}

// setTestXattr: no extended attributes are copied off Linux.
func setTestXattr(*testing.T, string, string, string) bool { return false }

func testXattr(*testing.T, string, string) string { return "" }

// relabelForTest: no SELinux off Linux.
func relabelForTest(*testing.T, string, string) string { return "" }

// breakTrustStoreLockForTest: off Linux the lock is a no-op that cannot fail.
func breakTrustStoreLockForTest(t *testing.T) { t.Skip("the trust-store lock is Linux's") }
