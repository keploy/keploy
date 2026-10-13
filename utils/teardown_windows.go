//go:build windows

package utils

// DieOnEscalation does nothing on Windows: the CLI ends a Windows agent
// itself (taskkill), with no elevation between them to relay a signal.
func DieOnEscalation() {}
