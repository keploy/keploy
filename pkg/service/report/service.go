package report

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type Service interface {
	GenerateReport(ctx context.Context) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
}

type TestDB interface {
	GetReportTestSets(ctx context.Context, reportID string) ([]string, error)
}
