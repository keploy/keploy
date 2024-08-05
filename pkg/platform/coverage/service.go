// Package coverage defines the interface for coverage services.
package coverage

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	PreProcess(skipPreview bool) (string, error)
	GetCoverage() (models.TestCoverage, error)
	AppendCoverage(coverage *models.TestCoverage, testRunID string) error
}

type ReportDB interface {
	UpdateReport(ctx context.Context, testRunID string, coverageReport any) error
}
