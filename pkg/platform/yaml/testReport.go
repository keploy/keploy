package yaml

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestReport struct {
	Tests           map[string][]models.TestResult
	M               sync.Mutex
	Logger          *zap.Logger
	MongoCollection *mongo.Client
}

func NewTestReportFS(logger *zap.Logger) *TestReport {
	return &TestReport{
		Tests:  map[string][]models.TestResult{},
		M:      sync.Mutex{},
		Logger: logger,
	}
}

func (fe *TestReport) Lock() {
	fe.M.Lock()
}

func (fe *TestReport) Unlock() {
	fe.M.Unlock()
}

func (fe *TestReport) SetResult(runId string, test models.TestResult) {
	fe.M.Lock()
	tests := fe.Tests[runId]
	tests = append(tests, test)
	fe.Tests[runId] = tests
	fe.M.Unlock()
}

func (fe *TestReport) GetResults(runId string) ([]models.TestResult, error) {
	val, ok := fe.Tests[runId]
	if !ok {
		return nil, fmt.Errorf("%s found no test results for test report with id: %s", Emoji, runId)
	}
	return val, nil
}

func (fe *TestReport) Read(ctx context.Context, path, name string) (models.TestReport, error) {

	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return models.TestReport{}, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.TestReport
	err = decoder.Decode(&doc)
	if err != nil {
		return models.TestReport{}, fmt.Errorf(Emoji, "failed to decode the yaml file documents. error: %v", err.Error())
	}
	return doc, nil
}

func (fe *TestReport) Write(ctx context.Context, path string, doc *models.TestReport) error {

	if doc.Name == "" {
		lastIndex, err := findLastIndex(path, fe.Logger)
		if err != nil {
			return err
		}
		doc.Name = fmt.Sprintf("report-%v", lastIndex)
	}

	_, err := util.CreateYamlFile(path, doc.Name, fe.Logger)
	if err != nil {
		return err
	}

	data := []byte{}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		return fmt.Errorf(Emoji, "failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	err = os.WriteFile(filepath.Join(path, doc.Name+".yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf(Emoji, "failed to write test report in yaml file. error: %s", err.Error())
	}
	return nil
}
