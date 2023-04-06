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

type HistoryConfig struct {
	app_path        string       `json:"app_path" yaml:"app_path"`
	test_case_paths []string     `json:"test_case_paths" yaml:"test_case_paths"`
	mocks_paths     []string     `json:"mocks_paths" yaml:"mocks_paths"`
	test_runs       []AppTestRun `json:"test_runs" yaml:"test_runs"`
	m               sync.Mutex
}
type AppTestRun struct {
	test_run_path string
	test_run_id   string
}

func NewHistoryConfigFS(isTestMode bool) *HistoryConfig {
	return &HistoryConfig{
		app_path:        "",
		test_case_paths: []string{},
		mocks_paths:     []string{},
		test_runs:       []AppTestRun{},
		m:               sync.Mutex{},
	}
}

// func (hc *HistoryConfig) Lock() {
// 	fe.m.Lock()
// }

// func (hc *HistoryConfig) Unlock() {
// 	fe.m.Unlock()
// }

// func (hc *HistoryConfig) SetResult(runId string, test models.TestResult) {
// 	tests, _ := fe.tests[runId]
// 	tests = append(tests, test)
// 	fe.tests[runId] = tests
// 	fe.m.Unlock()
// }

func (hc *HistoryConfig) GetResults(runId string) ([]models.TestResult, error) {
	val, ok := fe.tests[runId]
	if !ok {
		return nil, fmt.Errorf("found no test results for test report with id: %v", runId)
	}
	return val, nil
}

func (hc *HistoryConfig) Read(ctx context.Context, path, name string) (models.TestReport, error) {
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

func (hc *HistoryConfig) Write(ctx context.Context, ) error {
	
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
