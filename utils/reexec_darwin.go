//go:build darwin

package utils

import "go.uber.org/zap"

// ReexecWithSudo is a no-op on macOS.
// Docker Desktop/Colima access does not require sudo re-exec at the keploy CLI level.
func ReexecWithSudo(logger *zap.Logger) {
	logger.Debug("ReexecWithSudo called on macOS - this is a no-op")
}

// ShouldReexecWithSudo always returns false on macOS.
// Docker commands should run with the current user and rely on the active Docker context.
func ShouldReexecWithSudo() bool {
	return false
}
