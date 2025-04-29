// Package tools provides utility functions for the service package.
package tools

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	Update(ctx context.Context) error
	CreateConfig(ctx context.Context, filePath string, config string) error
	SendTelemetry(event string, output ...map[string]interface{})
	Login(ctx context.Context) bool
	Export(ctx context.Context) error
	Import(ctx context.Context, path, basePath string) error
	Templatize(ctx context.Context) error
}

type teleDB interface {
	SendTelemetry(event string, output ...map[string]interface{})
}

type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, testCase *models.TestCase, testSetID string, enableLog bool) error
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error
}
