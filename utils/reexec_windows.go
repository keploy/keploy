//go:build windows

package utils

import (
	"go.uber.org/zap"
)

// ReexecWithSudo is a no-op on Windows.
// Docker Desktop on Windows handles permissions differently and doesn't require sudo.
// If this is called on Windows, it means there's a logic error - we should never
// try to re-exec with sudo on Windows.
func ReexecWithSudo(logger *zap.Logger) {
	logger.Debug("ReexecWithSudo called on Windows - this is a no-op")
}

// ShouldReexecWithSudo always returns false on Windows.
// Docker Desktop on Windows handles permissions differently and doesn't require sudo.
func ShouldReexecWithSudo() bool {
	return false
}
