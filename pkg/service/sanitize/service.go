package sanitize

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	Sanitize(ctx context.Context) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
}
