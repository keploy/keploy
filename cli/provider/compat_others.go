//go:build !linux

package provider

import "go.uber.org/zap"

// check if keploy is compatable with underlying client os
func isCompatible(logger *zap.Logger) error {
	return nil
}
