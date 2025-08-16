// Package schema provides functionality for API schema generation and assertion
package schema

import (
	"context"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// Service defines the interface for schema operations
type Service interface {
	// GenerateSchema parses API file and generates OpenAPI schemas
	GenerateSchema(ctx context.Context, filePath string) error
	// AssertSchema validates requests/responses against stored schemas
	AssertSchema(ctx context.Context, filePath string) (*models.SchemaAssertionResult, error)
}

// OpenAPIDB interface for storing and retrieving schemas
type OpenAPIDB interface {
	WriteSchema(ctx context.Context, logger *zap.Logger, outputPath, name string, openapi models.OpenAPI, isAppend bool) error
	GetTestCasesSchema(ctx context.Context, testSetID string, testPath string) ([]*models.OpenAPI, error)
}

// TestSetConfig interface for configuration management
type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

// Telemetry interface for tracking usage
type Telemetry interface {
	SendTelemetry(event string, output ...*sync.Map)
}
