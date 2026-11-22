// Package tools provides utility functions for the service package.
package tools

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

type Service interface {
	Update(ctx context.Context) error
	CreateConfig(ctx context.Context, filePath string, config string) error
	SendTelemetry(event string, output ...map[string]interface{})
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
	// ReadForUpdate reports a config.yaml that did not parse instead of
	// handing back a zero value. Read deliberately swallows that so
	// secret hydration continues; a caller about to REPLACE the whole
	// document must not.
	ReadForUpdate(ctx context.Context, testSetID string) (*models.TestSet, error)
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

// TestSetFiles is a TestDB that keeps each test set as files, which keploy
// templatize needs: it reads a set's files as they are written, and puts
// every file it changes in place together or none. The OSS store
// (testdb.TestYaml) is one. It is its own interface, asserted only by
// templatize, so a TestDB that keeps no files (k8s-proxy's, for keploy
// normalize) still builds and serves every other tool.
type TestSetFiles interface {
	// TestCaseFiles returns the test case files of a test set as they are
	// stored: every file GetTestCases reads, and no other. It fails when the
	// set holds what an interrupted ReplaceFiles left.
	TestCaseFiles(ctx context.Context, testSetID string) ([]yaml.File, error)
	// TestCaseFile returns the file UpdateTestCase stores testCase in and the
	// bytes it would hold, without writing it.
	TestCaseFile(ctx context.Context, testCase *models.TestCase, testSetID string) (yaml.File, error)
	// ReplaceFiles stores every file of files, or none of them: an error
	// means the test set is as it was.
	ReplaceFiles(ctx context.Context, files []yaml.File) error
}

// ConfigFiles is a TestSetConfig that keeps config.yaml as a file, which
// keploy templatize needs (TestSetFiles): it is replaced together with the
// test cases templatize changes. Asserted only by templatize.
type ConfigFiles interface {
	// ConfigFile returns the file Write stores testSet in and the bytes it
	// would hold, without writing it.
	ConfigFile(ctx context.Context, testSetID string, testSet *models.TestSet) (yaml.File, error)
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
}
