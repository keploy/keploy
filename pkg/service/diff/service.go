package diff

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type Service interface {
	Compare(ctx context.Context, run1 string, run2 string, testSets []string) error
}

type ReportDB interface {
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
}

type TestDB interface {
	GetReportTestSets(ctx context.Context, reportID string) ([]string, error)
}
