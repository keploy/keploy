package contract

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

// Service defines the contract service interface
type Service interface {
	Generate(ctx context.Context) error
	Download(ctx context.Context) error
	Validate(ctx context.Context) error
	CheckConfigFile() error
}

type TestDB interface {
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	ChangeTcPath(path string)
}
type MockDB interface {
	GetHTTPMocks(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.HTTPDoc, error)
}
type OpenAPIDB interface {
	GetTestCasesSchema(ctx context.Context, testSetID string, testPath string) ([]*models.OpenAPI, error)
	GetMocksSchemas(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.OpenAPI, error)
	ChangeTcPath(path string)
}
