package platform

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type TestCaseDB interface {
	WriteTestcase(tc *models.TestCase, ctx context.Context, filters *models.Filters) error
	WriteMock(tc *models.Mock, ctx context.Context) error

	ReadTestcase(path string, options interface{}) ([]*models.TestCase, error)
	ReadMocks(path string) ([]*models.Mock, []*models.Mock, error)
}
