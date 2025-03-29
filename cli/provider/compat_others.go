//go:build !linux

// This is a placeholder file for other OSes.

package provider

import "go.uber.org/zap"

func isCompatible(_ *zap.Logger) error {
	return nil
}
