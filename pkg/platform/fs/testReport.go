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
	tests      map[string][]models.TestResult
	m          sync.Mutex
}

func NewTestReportFS(isTestMode bool) *testReport {
	return &testReport{
		isTestMode: isTestMode,
		tests:      map[string][]models.TestResult{},
		m:          sync.Mutex{},
	}
}

func (fe *testReport) Lock() {
	fe.m.Lock()
}

func (fe *testReport) Unlock() {
	fe.m.Unlock()
}

func (fe *testReport) SetResult(runId string, test models.TestResult) {
	// TODO: send runId to the historyConfig
	tests, _ := fe.tests[runId]
	tests = append(tests, test)
	fe.tests[runId] = tests
	fe.m.Unlock()
}

func (fe *testReport) GetResults(runId string) ([]models.TestResult, error) {
	val, ok := fe.tests[runId]
	if !ok {
		return nil, fmt.Errorf("found no test results for test report with id: %v", runId)
	}
	return val, nil
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

	_, err := CreateMockFile(path, doc.Name)
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
