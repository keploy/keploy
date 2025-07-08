package python

import (
	"os"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func createPyCoverageConfig(logger *zap.Logger) {
	// In the below config, in the concurrency section, we are setting the concurrency to multiprocessing and thread.
	// Where multiprocessing is for collecting coverage for processes spawned by the Python application,
	// and thread is for collecting coverage for the main thread.

	// Define the content of the .coveragerc file
	configContent := `[run]
omit =
    /usr/*
sigterm = true
concurrency  = multiprocessing, thread
parallel = true
data_file = .coverage.keploy
`

	// Create or overwrite the .coveragerc file
	file, err := os.Create(".coveragerc")
	if err != nil {
		utils.LogError(logger, err, "failed to create .coveragerc file")
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close coveragerc file", zap.String("file", file.Name()))
		}
	}()

	_, err = file.WriteString(configContent)
	if err != nil {
		utils.LogError(logger, err, "failed to write to .coveragerc file")
	}

	logger.Debug("Configuration written to .coveragerc")
}
