package platform

import (
	"context"

	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type TestCaseDB interface {
	WriteTestcase(tc *models.TestCase, ctx context.Context) error
	WriteMock(tc *models.Mock, ctx context.Context) error

	ReadTestcase(path string, lastSeenId *primitive.ObjectID, options interface{}) ([]*models.TestCase, error)
	ReadTcsMocks(tc *models.TestCase, path string) ([]*models.Mock, error)
	ReadConfigMocks(path string) ([]*models.Mock, error)
}

type TestReportDB interface {
	Lock()
	Unlock()
	SetResult(runId string, test models.TestResult)
	GetResults(runId string) ([]models.TestResult, error)
	Read(ctx context.Context, path, name string) (models.TestReport, error)
	Write(ctx context.Context, path string, doc *models.TestReport) error
}
