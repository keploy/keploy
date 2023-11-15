package mgo

import (
	"context"
	"fmt"
	"sync"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

type TestReport struct {
	Tests           map[string][]models.TestResult
	M               sync.Mutex
	Logger          *zap.Logger
	MongoCollection *mongo.Client
}

func NewTestReportFS(logger *zap.Logger, MongoCollection *mongo.Client) *TestReport {
	return &TestReport{
		Tests:           map[string][]models.TestResult{},
		M:               sync.Mutex{},
		Logger:          logger,
		MongoCollection: MongoCollection,
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
		return nil, fmt.Errorf(yaml.Emoji, "found no test results for test report with id: %s", runId)
	}
	return val, nil
}

func (fe *TestReport) Read(ctx context.Context, path, name string) (models.TestReport, error) {
	var doc models.TestReport
	return doc, nil
}

func (fe *TestReport) Write(ctx context.Context, path string, doc *models.TestReport) error {
	collection := fe.MongoCollection.Database(models.Keploy).Collection(models.TestReports)
	doc.Name = path
	_, err := collection.InsertOne(context.TODO(), doc)
	if err != nil {
		return fmt.Errorf(yaml.Emoji, "failed to create the report. error: %s", err.Error())
	}
	return nil
}
