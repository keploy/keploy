package Normalise

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

// newNormaliser initializes a new normaliser instance.
func NewNormaliser(logger *zap.Logger) Normaliser {
	return &normaliser{
		logger: logger,
	}
}

type normaliser struct {
	logger *zap.Logger
}

func (n *normaliser) Normalise(path string) {
	n.logger.Info("Test cases and Mock Path", zap.String("path", path))
	testReportPath := filepath.Join(path, "testReports")

	// Get a list of directories in the testReportPath
	dirs, err := getDirectories(testReportPath)
	if err != nil {
		n.logger.Error("Failed to get directories", zap.Error(err))
		return
	}

	// Find the last-run folder
	sort.Strings(dirs)
	var lastRunFolder string
	for i := len(dirs) - 1; i >= 0; i-- {
		if strings.HasPrefix(dirs[i], "test-run-") {
			lastRunFolder = dirs[i]
			break
		}
	}
	lastRunFolderPath := filepath.Join(testReportPath, lastRunFolder)
	n.logger.Info("Latest Test Run", zap.String("folder", lastRunFolderPath))

	// Get list of YAML files in the last run folder
	files, err := ioutil.ReadDir(lastRunFolderPath)
	if err != nil {
		n.logger.Error("Failed to read directory", zap.Error(err))
		return
	}

	// Iterate over each YAML file
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".yaml") {
			filePath := filepath.Join(lastRunFolderPath, file.Name())

			// Read the YAML file
			yamlData, err := ioutil.ReadFile(filePath)
			if err != nil {
				n.logger.Error("Failed to read YAML file", zap.Error(err))
				continue
			}

			// Unmarshal YAML into TestReport
			var testReport models.TestReport
			err = yaml.Unmarshal(yamlData, &testReport)
			if err != nil {
				n.logger.Error("Failed to unmarshal YAML", zap.Error(err))
				continue
			}

			// Iterate over tests in the TestReport
			for _, test := range testReport.Tests {
				if test.Status == models.TestStatusFailed {
					// Process failing test case here
					fmt.Println("test.Result.Bodyresult is  ")
					fmt.Println(test.Result.BodyResult)
					for _, Result := range test.Result.BodyResult {
						fmt.Println(Result.Expected)
						fmt.Println(Result.Actual)
					}

				}
			}
		}
	}
}
