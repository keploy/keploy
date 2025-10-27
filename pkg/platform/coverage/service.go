// Package coverage defines the interface for coverage services.
package coverage

import (
	"context"

	"github.com/keploy/keploy-integrations-shared/pkg/models"
)

type Service interface {
	PreProcess(disableLineCoverage bool) (string, error)
	GetCoverage() (models.TestCoverage, error)
	AppendCoverage(coverage *models.TestCoverage, testRunID string) error
}

type ReportDB interface {
	UpdateReport(ctx context.Context, testRunID string, coverageReport any) error
}
