package yaml

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type TestReportFS interface {
	Lock()
	Unlock()
	SetResult(runId string, test models.TestResult)
	GetResults(runId string) ([]models.TestResult, error)
	Read(ctx context.Context, path, name string) (models.TestReport, error)
	Write(ctx context.Context, path string, doc *models.TestReport) error
}
