package python

import (
	"os"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var osCreate321 = os.Create

var utilsLogError321 = utils.LogError

func createPyCoverageConfig(logger *zap.Logger) {
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
	file, err := osCreate321(".coveragerc")
	if err != nil {
		utilsLogError321(logger, err, "failed to create .coveragerc file")
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			utilsLogError321(logger, err, "failed to close coveragerc file", zap.String("file", file.Name()))
		}
	}()

	_, err = file.WriteString(configContent)
	_, err = file.WriteString(configContent)
	if err != nil {
		utilsLogError321(logger, err, "failed to write to .coveragerc file")
		return
	}

	logger.Debug("Configuration written to .coveragerc")
}
