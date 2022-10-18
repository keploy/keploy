package regression

import (
	"context"
	"net/http"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	DeNoise(ctx context.Context, cid, id, app, body string, h http.Header, path string) error
	Test(ctx context.Context, cid, app, runID, id, testCasePath, mockPath string, resp models.HttpResp) (bool, error)
	StartTestRun(ctx context.Context, runId, testCasePath, mockPath, testReportPath string) error
	StopTestRun(ctx context.Context, runId, testReportPath string) error
}
