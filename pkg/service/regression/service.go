package regression

import (
	"context"
	"net/http"
	"time"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	DeNoise(ctx context.Context, cid, id, app, body string, h http.Header, path, tcsType string) error
	Test(ctx context.Context, cid, app, runID, id, testCasePath, mockPath string, resp models.HttpResp) (bool, error)
	// For Grpc
	TestGrpc(ctx context.Context, resp models.GrpcResp, cid, app, runID, id, testCasePath, mockPath string) (bool, error)
	Normalize(ctx context.Context, cid, id string) error
	GetTestRun(ctx context.Context, summary bool, cid string, user, app, id *string, from, to *time.Time, offset *int, limit *int) ([]*models.TestRun, error)
	PutTest(ctx context.Context, run models.TestRun, testExport bool, runId, testCasePath, mockPath, testReportPath string, totalTcs int) error
}

type TestRunDB interface {
	Read(ctx context.Context, cid string, user, app, id *string, from, to *time.Time, offset int, limit int) ([]*models.TestRun, error)
	Upsert(ctx context.Context, run models.TestRun) error
	ReadOne(ctx context.Context, id string) (*models.TestRun, error)
	ReadTest(ctx context.Context, id string) (models.Test, error)
	ReadTests(ctx context.Context, runID string) ([]models.Test, error)
	PutTest(ctx context.Context, t models.Test) error
	Increment(ctx context.Context, success, failure bool, id string) error
}

type TestReportFS interface {
	Write(ctx context.Context, path string, doc models.TestReport) error
	Read(ctx context.Context, path, name string) (models.TestReport, error)
	SetResult(runId string, test models.TestResult)
	GetResults(runId string) ([]models.TestResult, error)
	Lock()
	Unlock()
}
