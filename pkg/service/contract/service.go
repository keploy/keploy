package contract

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

// Service defines the contract service interface
type Service interface {
	Generate(ctx context.Context, genAllTests bool, genAllMocks bool) error
	Download(_ context.Context, driven string) error
	Validate(ctx context.Context) error
	CheckConfigFile() error
}

type TestDB interface {
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	ChangeTcPath()
}
type MockDB interface {
	GetHttpMocks(ctx context.Context, testSetID string, mockPath string) ([]*models.HTTPSchema2, error)
}
