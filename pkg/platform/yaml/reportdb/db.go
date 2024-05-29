// Package reportdb provides functionality for managing test reports in a database.
package reportdb

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestReport struct {
	tests  map[string]map[string][]models.TestResult
	m      sync.Mutex
	Logger *zap.Logger
	Path   string
	Name   string
}

func New(logger *zap.Logger, reportPath string) *TestReport {
	return &TestReport{
		tests:  make(map[string]map[string][]models.TestResult),
		m:      sync.Mutex{},
		Logger: logger,
		Path:   reportPath,
	}
}

func (fe *TestReport) GetAllTestRunIDs(ctx context.Context) ([]string, error) {
	return yaml.ReadSessionIndices(ctx, fe.Path, fe.Logger)
}

func (fe *TestReport) InsertTestCaseResult(_ context.Context, testRunID string, testSetID string, result *models.TestResult) error {
	fe.m.Lock()
	defer fe.m.Unlock()

	testSet := fe.tests[testRunID]
	if testSet == nil {
		testSet = make(map[string][]models.TestResult)
		testSet[testSetID] = []models.TestResult{*result}
	} else {
		testSet[testSetID] = append(testSet[testSetID], *result)
	}
	fe.tests[testRunID] = testSet
	return nil
}

func (fe *TestReport) GetTestCaseResults(_ context.Context, testRunID string, testSetID string) ([]models.TestResult, error) {
	testRun, ok := fe.tests[testRunID]
	if !ok {
		return []models.TestResult{}, fmt.Errorf("%s found no test results for test report with id: %s", utils.Emoji, testRunID)
	}
	testSetResults, ok := testRun[testSetID]
	if !ok {
		return []models.TestResult{}, fmt.Errorf("%s found no test results for test set with id: %s", utils.Emoji, testSetID)
	}
	return testSetResults, nil
}

func (fe *TestReport) GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error) {
	fmt.Println("PAth is ", fe.Path)
	path := filepath.Join(fe.Path, testRunID)
	reportName := testSetID + "-report"
	_, err := yaml.ValidatePath(filepath.Join(path, reportName+".yaml"))
	if err != nil {
		return nil, err
	}
	data, err := yaml.ReadFile(ctx, fe.Logger, path, reportName)
	if err != nil {
		utils.LogError(fe.Logger, err, "failed to read the testset report ", zap.Any("session", filepath.Base(path)))
		return nil, err
	}

	decoder := yamlLib.NewDecoder(bytes.NewReader(data))
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return &models.TestReport{}, fmt.Errorf("%s failed to decode the yaml file documents. error: %v", utils.Emoji, err.Error())
	}
	return &doc, nil
}

func (fe *TestReport) InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error {

	reportPath := filepath.Join(fe.Path, testRunID)

	if testReport.Name == "" {
		testReport.Name = testSetID + "-report"
	}

	data := []byte{}
	d, err := yamlLib.Marshal(&testReport)
	if err != nil {
		return fmt.Errorf("%s failed to marshal document to yaml. error: %s", utils.Emoji, err.Error())
	}
	data = append(data, d...)

	err = yaml.WriteFile(ctx, fe.Logger, reportPath, testReport.Name, data, false)
	if err != nil {
		utils.LogError(fe.Logger, err, "failed to write the report to yaml", zap.Any("session", filepath.Base(reportPath)))
		return err
	}
	return nil
}
