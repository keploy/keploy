package yaml

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/persistence"
)

type testReport struct {
	fileSystem persistence.FileSystem
	tests      map[string][]models.TestResult
	m          sync.Mutex
	logger     *zap.Logger
}

func NewTestReportFS(fileSystem persistence.FileSystem, logger *zap.Logger) *testReport {
	return &testReport{
		fileSystem: fileSystem,
		tests:      map[string][]models.TestResult{},
		m:          sync.Mutex{},
		logger:     logger,
	}
}

func (tr *testReport) Lock() {
	tr.m.Lock()
}

func (tr *testReport) Unlock() {
	tr.m.Unlock()
}

func (tr *testReport) SetResult(runId string, test models.TestResult) {
	tests, _ := tr.tests[runId]
	tests = append(tests, test)
	tr.tests[runId] = tests
	tr.m.Unlock()
}

func (tr *testReport) GetResults(runId string) ([]models.TestResult, error) {
	val, ok := tr.tests[runId]
	if !ok {
		return nil, fmt.Errorf("found no test results for test report with id: %v", runId)
	}
	return val, nil
}

func (tr *testReport) Read(ctx context.Context, path, name string) (models.TestReport, error) {

	file, err := tr.fileSystem.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return models.TestReport{}, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return models.TestReport{}, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
	}
	return doc, nil
}

func (tr *testReport) Write(ctx context.Context, path string, doc *models.TestReport) error {

	if doc.Name == "" {
		lastIndex, err := tr.fileSystem.FindNextUsableIndexForYaml(path)
		if err != nil {
			return err
		}
		doc.Name = fmt.Sprintf("report-%v", lastIndex)
	}

	_, err := tr.fileSystem.CreateFile(path, doc.Name, "yaml")
	if err != nil {
		return err
	}

	var data []byte
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	err = tr.fileSystem.WriteFile(filepath.Join(path, doc.Name+".yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write test report in yaml file. error: %s", err.Error())
	}
	return nil
}
