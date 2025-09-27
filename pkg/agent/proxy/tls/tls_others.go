//go:build !linux

package tls

import (
	"go.uber.org/zap"
)

func SetupCaCertEnv(logger *zap.Logger) error {
	return nil
}
