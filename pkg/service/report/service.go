package report

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	GenerateReport(ctx context.Context) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetTestCaseResults(ctx context.Context, testRunID string, testSetID string) ([]models.TestResult, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
	ClearTestCaseResults(_ context.Context, testRunID string, testSetID string)
	InsertTestCaseResult(ctx context.Context, testRunID string, testSetID string, result *models.TestResult) error // 1
	InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error     // 2
	UpdateReport(ctx context.Context, testRunID string, testCoverage any) error
}
