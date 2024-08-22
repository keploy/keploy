package contract

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// Service defines the contract service interface
type Service interface {
	Generate(ctx context.Context) error
	Download(ctx context.Context) error
	Validate(ctx context.Context) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	ChangePath(path string)
}
type MockDB interface {
	GetHTTPMocks(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.HTTPDoc, error)
}
type OpenAPIDB interface {
	GetTestCasesSchema(ctx context.Context, testSetID string, testPath string) ([]*models.OpenAPI, error)
	GetMocksSchemas(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.OpenAPI, error)
	WriteSchema(ctx context.Context, logger *zap.Logger, outputPath, name string, openapi models.OpenAPI, isAppend bool) error
	ChangePath(path string)
}
