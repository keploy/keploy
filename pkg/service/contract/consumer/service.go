package consumer

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

// Service defines the consumer service interface
type Service interface {
	ConsumerDrivenValidation(ctx context.Context) error
}

type TestDB interface {
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	ChangeTcPath(path string)
}

type OpenAPIDB interface {
	GetTestCasesSchema(ctx context.Context, testSetID string, testPath string) ([]*models.OpenAPI, error)
	GetMocksSchemas(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.OpenAPI, error)
	ChangeTcPath(path string)
}
