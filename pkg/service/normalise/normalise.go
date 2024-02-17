package Normalise

import (
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
		n.logger.Error("Failed to get TestReports", zap.Error(err))
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
	n.logger.Info("Test Run Folder", zap.String("folder", lastRunFolderPath))

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

					// Read the contents of the testcase file
					testCaseFilePath := filepath.Join(test.TestCasePath, "tests", test.TestCaseID+".yaml")
					n.logger.Info("Updating testcase file", zap.String("filePath", testCaseFilePath))
					testCaseContent, err := ioutil.ReadFile(testCaseFilePath)
					if err != nil {
						n.logger.Error("Failed to read testcase file", zap.Error(err))
						continue
					}

					// Unmarshal YAML into TestCase
					var testCase TestCaseFile
					err = yaml.Unmarshal(testCaseContent, &testCase)
					if err != nil {
						n.logger.Error("Failed to unmarshal YAML", zap.Error(err))
						continue
					}
					n.logger.Info("Updating Response body from :" + testCase.Spec.Resp.Body + " to :" + test.Result.BodyResult[0].Actual)
					testCase.Spec.Resp.Body = test.Result.BodyResult[0].Actual

					// Marshal TestCase back to YAML
					updatedYAML, err := yaml.Marshal(&testCase)
					if err != nil {
						n.logger.Error("Failed to marshal YAML", zap.Error(err))
						continue
					}

					// Write the updated YAML content back to the file
					err = ioutil.WriteFile(testCaseFilePath, updatedYAML, 0644)
					if err != nil {
						n.logger.Error("Failed to write updated YAML to file", zap.Error(err))
						continue
					}

					n.logger.Info("Updated testcase file successfully", zap.String("testCaseFilePath", testCaseFilePath))
				}
			}
		}
	}
}
