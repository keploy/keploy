package mock

import (
	"os"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// exportRecordStart sets models.RecordStartEnv for the command keploy wraps,
// which inherits keploy's environment like KEPLOY_MOCK_AGENT. An empty start
// (a replay of a recording made before the stamp) clears any value inherited
// from a parent keploy, so the command never anchors on another recording's
// start.
func exportRecordStart(logger *zap.Logger, start string) {
	if start == "" {
		if err := os.Unsetenv(models.RecordStartEnv); err != nil {
			logger.Debug("failed to clear the recording start", zap.Error(err))
		}
		return
	}
	if err := os.Setenv(models.RecordStartEnv, start); err != nil {
		logger.Debug("failed to export the recording start", zap.String("start", start), zap.Error(err))
		return
	}
	logger.Debug("exported the recording start to the test command", zap.String(models.RecordStartEnv, start))
}
