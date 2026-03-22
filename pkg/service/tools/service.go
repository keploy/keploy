// Package tools provides utility functions for the service package.
package tools

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type Service interface {
	Update(ctx context.Context) error
	CreateConfig(ctx context.Context, filePath string, config string) error
	SendTelemetry(event string, output ...map[string]interface{})
	Login(ctx context.Context) bool
	Export(ctx context.Context) error
	Import(ctx context.Context, path, basePath string) error
	Templatize(ctx context.Context) error
	Sanitize(ctx context.Context) error
	Normalize(ctx context.Context) error
	NormalizeTestCases(ctx context.Context, testRun string, testSetID string, selectedTestCaseIDs []string, testCaseResults []models.TestResult) error
}

type teleDB interface {
	SendTelemetry(event string, output ...map[string]interface{})
}

type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
	ReadSecret(ctx context.Context, testSetID string) (map[string]interface{}, error)
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, testCase *models.TestCase, testSetID string, enableLog bool) error
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
}
