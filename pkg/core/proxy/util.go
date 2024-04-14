package proxy

import (
	"os"

	"go.uber.org/zap"
)

// writeNsswitchConfig writes the content to nsswitch.conf file
func writeNsswitchConfig(logger *zap.Logger, nsSwitchConfig string, data []byte, perm os.FileMode) error {

	err := os.WriteFile(nsSwitchConfig, data, perm)
	if err != nil {
		logger.Error("failed to write the configuration to the nsswitch.conf file to redirect the DNS queries to proxy", zap.Error(err))
		return err
	}
	return nil
}
