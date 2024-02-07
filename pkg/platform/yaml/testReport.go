package yaml

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestReport struct {
	tests  map[string][]platform.KindSpecifier
	m      sync.Mutex
	Logger *zap.Logger
}

func NewTestReportFS(logger *zap.Logger) *TestReport {
	return &TestReport{
		tests:  make(map[string][]platform.KindSpecifier), // Correctly initialize the map
		m:      sync.Mutex{},
		Logger: logger,
	}
}

func (fe *TestReport) Lock() {
	fe.m.Lock()
}

func (fe *TestReport) Unlock() {
	fe.m.Unlock()
}

func (fe *TestReport) SetResult(runId string, test platform.KindSpecifier) {
	fe.m.Lock()
	tests := fe.tests[runId]
	tests = append(tests, test)
	fe.tests[runId] = tests
	fe.m.Unlock()
}

func (fe *TestReport) GetResults(runId string) ([]platform.KindSpecifier, error) {
	testResults, ok := fe.tests[runId]
	if !ok {
		return nil, fmt.Errorf("%s found no test results for test report with id: %s", Emoji, runId)
	}

	return testResults, nil
}

func (fe *TestReport) Read(ctx context.Context, path, name string) (platform.KindSpecifier, error) {
	testpath, err := util.ValidatePath(filepath.Join(path, name+".yaml"))
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(testpath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return &models.TestReport{}, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return &models.TestReport{}, fmt.Errorf("%s failed to decode the yaml file documents. error: %v", Emoji, err.Error())
	}
	return &doc, nil
}

func (fe *TestReport) Write(ctx context.Context, path string, doc platform.KindSpecifier) error {
	readDock, ok := doc.(*models.TestReport)
	if !ok {
		return fmt.Errorf("%s failed to read test report in yaml file.", Emoji)
	}
	if readDock.Name == "" {
		lastIndex, err := findLastIndex(path, fe.Logger)
		if err != nil {
			return err
		}
		readDock.Name = fmt.Sprintf("report-%v", lastIndex)
	}

	_, err := util.CreateYamlFile(path, readDock.Name, fe.Logger)
	if err != nil {
		return err
	}

	data := []byte{}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("%s failed to marshal document to yaml. error: %s", Emoji, err.Error())
	}
	data = append(data, d...)

	err = os.WriteFile(filepath.Join(path, readDock.Name+".yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("%s failed to write test report in yaml file. error: %s", Emoji, err.Error())
	}
	return nil
}
