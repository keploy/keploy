package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
)

type testReport struct {
	isTestMode bool
	tests      sync.Map
}

func NewTestReportFS(isTestMode bool) *testReport {
	return &testReport{
		isTestMode: isTestMode,
		tests:      sync.Map{},
	}
}

func (fe *testReport) SetResult(runId string, test models.TestResult) {

	val, ok := fe.tests.Load(runId)
	tests := []models.TestResult{}
	if ok {
		tests = val.([]models.TestResult)
	}
	tests = append(tests, test)
	fe.tests.Store(runId, tests)
}

func (fe *testReport) GetResults(runId string) ([]models.TestResult, error) {
	val, ok := fe.tests.Load(runId)
	if !ok {
		return nil, fmt.Errorf("found no test results for test report with id: %v", runId)
	}
	return val.([]models.TestResult), nil
}

func (fe *testReport) Read(ctx context.Context, path, name string) (models.TestReport, error) {
	if !pkg.IsValidPath(path) {
		return models.TestReport{}, fmt.Errorf("file path should be absolute. got test report path: %s and its name: %s", pkg.SanitiseInput(path), pkg.SanitiseInput(name))
	}
	if strings.Contains(name, "/") || !pkg.IsValidPath(name) {
		return models.TestReport{}, errors.New("invalid name for test-report. It should not include any slashes")
	}
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return models.TestReport{}, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return models.TestReport{}, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
	}
	return doc, nil
}

func (fe *testReport) Write(ctx context.Context, path string, doc models.TestReport) error {
	if fe.isTestMode {
		return nil
	}
	if strings.Contains(doc.Name, "/") || !pkg.IsValidPath(doc.Name) {
		return errors.New("invalid name for test-report. It should not include any slashes")
	}

	_, err := createMockFile(path, doc.Name)
	if err != nil {
		return err
	}

	data := []byte{}
	d, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	err = os.WriteFile(filepath.Join(path, doc.Name+".yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write test report in yaml file. error: %s", err.Error())
	}
	return nil
}
