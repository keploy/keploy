package reportdb

import (
	"context"
	"fmt"
	"os"
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

func (fe *TestReport) GetAllTestRunIds(ctx context.Context) ([]string, error) {
	return yaml.ReadSessionIndices(ctx, fe.Path, fe.Logger)
}

func (fe *TestReport) InsertTestCaseResult(ctx context.Context, testRunId string, testSetId string, testCaseId string, result *models.TestResult) error {
	fe.m.Lock()
	defer fe.m.Unlock()

	testSet := fe.tests[testRunId]
	if testSet == nil {
		testSet = make(map[string][]models.TestResult)
		testSet[testSetId] = []models.TestResult{*result}
	} else {
		testSet[testSetId] = append(testSet[testSetId], *result)
	}
	fe.tests[testRunId] = testSet
	return nil
}

func (fe *TestReport) GetTestCaseResults(ctx context.Context, testRunId string, testSetId string) ([]models.TestResult, error) {
	testRun, ok := fe.tests[testRunId]
	if !ok {
		return nil, fmt.Errorf("%s found no test results for test report with id: %s", utils.Emoji, testRunId)
	}
	testSetResults, ok := testRun[testSetId]
	if !ok {
		return nil, fmt.Errorf("%s found no test results for test set with id: %s", utils.Emoji, testSetId)
	}
	return testSetResults, nil
}

func (fe *TestReport) GetReport(ctx context.Context, testRunId string, testSetId string) (*models.TestReport, error) {

	testpath, err := yaml.ValidatePath(filepath.Join(fe.Path, fe.Name+".yaml"))
	if err != nil {
		return nil, err
	}
	file, err := yaml.ReadDir(testpath, os.ModePerm)
	if err != nil {
		return &models.TestReport{}, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return &models.TestReport{}, fmt.Errorf("%s failed to decode the yaml file documents. error: %v", utils.Emoji, err.Error())
	}
	return &doc, nil
}

func (fe *TestReport) InsertReport(ctx context.Context, testRunId string, testSetId string, testReport *models.TestReport) error {

	if testReport.Name == "" {
		lastIndex, err := yaml.FindLastIndex(fe.Path, fe.Logger)
		if err != nil {
			return err
		}
		testReport.Name = fmt.Sprintf("report-%v", lastIndex)
	}

	_, err := yaml.CreateYamlFile(ctx, fe.Logger, fe.Path, testReport.Name)
	if err != nil {
		return err
	}

	data := []byte{}
	d, err := yamlLib.Marshal(&testReport)
	if err != nil {
		return fmt.Errorf("%s failed to marshal document to yaml. error: %s", utils.Emoji, err.Error())
	}
	data = append(data, d...)

	err = yaml.WriteFile(ctx, fe.Logger, filepath.Join(fe.Path), testReport.Name+".yaml", data)

	if err != nil {
		fe.Logger.Error("failed to write report yaml file", zap.Error(err))
		return err
	}

	if err != nil {
		return fmt.Errorf("%s failed to write test report in yaml file. error: %s", utils.Emoji, err.Error())
	}
	return nil
}
