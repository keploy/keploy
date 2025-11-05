// Package coverage defines the interface for coverage services.
package coverage

import (
	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	PreProcess(appCmd string, testSetID string) (string, error)
	GetCoverage() (models.TestCoverage, error)
}
