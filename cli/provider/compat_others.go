//go:build !linux

// This is a placeholder file for other OSes.

package provider

import "go.uber.org/zap"

func isCompatible(logger *zap.Logger) error {
	return nil
}
