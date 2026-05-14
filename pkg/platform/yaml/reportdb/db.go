// Package reportdb provides functionality for managing test reports in a database.
package reportdb

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestReport struct {
	tests  map[string]map[string][]models.TestResult
	m      sync.Mutex
	Logger *zap.Logger
	Path   string
	Name   string
	Format yaml.Format
}

func New(logger *zap.Logger, reportPath string) *TestReport {
	return NewWithFormat(logger, reportPath, yaml.FormatYAML)
}

func NewWithFormat(logger *zap.Logger, reportPath string, format yaml.Format) *TestReport {
	return &TestReport{
		tests:  make(map[string]map[string][]models.TestResult),
		m:      sync.Mutex{},
		Logger: logger,
		Path:   reportPath,
		Format: format,
	}
}

func (fe *TestReport) ClearTestCaseResults(_ context.Context, testRunID string, testSetID string) {
	fe.m.Lock()
	defer fe.m.Unlock()

	fe.tests[testRunID] = make(map[string][]models.TestResult)
}

func (fe *TestReport) GetAllTestRunIDs(ctx context.Context) ([]string, error) {
	return yaml.ReadSessionIndices(ctx, fe.Path, fe.Logger, yaml.ModeDir)
}

func (fe *TestReport) InsertTestCaseResult(_ context.Context, testRunID string, testSetID string, result *models.TestResult) error {
	fe.m.Lock()
	defer fe.m.Unlock()

	testSet := fe.tests[testRunID]
	if testSet == nil {
		testSet = make(map[string][]models.TestResult)
		testSet[testSetID] = []models.TestResult{*result}
	} else {
		testSet[testSetID] = append(testSet[testSetID], *result)
	}
	fe.tests[testRunID] = testSet
	return nil
}

func (fe *TestReport) GetTestCaseResults(_ context.Context, testRunID string, testSetID string) ([]models.TestResult, error) {
	testRun, ok := fe.tests[testRunID]
	if !ok {
		return []models.TestResult{}, fmt.Errorf("%s found no test results for test report with id: %s", utils.Emoji, testRunID)
	}
	testSetResults, ok := testRun[testSetID]
	if !ok {
		return []models.TestResult{}, fmt.Errorf("%s found no test results for test set with id: %s", utils.Emoji, testSetID)
	}
	return testSetResults, nil
}

func (fe *TestReport) GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error) {
	path := filepath.Join(fe.Path, testRunID)
	reportName := testSetID + "-report"
	// Auto-detect the report format — `keploy report` keeps working for
	// reports written by a differently-configured prior run.
	data, detected, err := yaml.ReadFileAny(ctx, fe.Logger, path, reportName, fe.Format)
	if err != nil {
		utils.LogError(fe.Logger, err, "failed to read the test-set report", zap.String("reportName", reportName), zap.String("session", filepath.Base(path)))
		return nil, err
	}

	var doc models.TestReport
	if detected == yaml.FormatJSON {
		err = yaml.UnmarshalGeneric(yaml.FormatJSON, data, &doc)
	} else {
		decoder := yamlLib.NewDecoder(bytes.NewReader(data))
		err = decoder.Decode(&doc)
	}
	if err != nil {
		return &models.TestReport{}, fmt.Errorf("%s failed to decode the report file. error: %v", utils.Emoji, err.Error())
	}
	return &doc, nil
}

func (fe *TestReport) InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error {

	reportPath := filepath.Join(fe.Path, testRunID)

	if testReport.Name == "" {
		testReport.Name = testSetID + "-report"
	}

	testReport.CreatedAt = time.Now().Unix()
	report := sanitizeReportForYAML(*testReport)

	d, err := yaml.MarshalGeneric(fe.Format, &report)
	if err != nil {
		return fmt.Errorf("%s failed to marshal report document. error: %s", utils.Emoji, err.Error())
	}

	data := d
	if fe.Format == yaml.FormatYAML {
		data = append([]byte(utils.GetVersionAsComment()), data...)
	}

	err = yaml.WriteFileF(ctx, fe.Logger, reportPath, testReport.Name, data, false, fe.Format)
	if err != nil {
		utils.LogError(fe.Logger, err, "failed to write the report", zap.String("session", filepath.Base(reportPath)))
		return err
	}
	return nil
}

func sanitizeReportForYAML(report models.TestReport) models.TestReport {
	report.AppLogs = normalizeReportYAMLText(report.AppLogs)
	report.FailureReason = normalizeReportYAMLText(report.FailureReason)
	report.CmdUsed = normalizeReportYAMLText(report.CmdUsed)
	return report
}

func normalizeReportYAMLText(value string) string {
	if value == "" {
		return ""
	}

	value = strings.ToValidUTF8(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\t", "  ")

	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if r == '\n' {
			builder.WriteRune(r)
			continue
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		builder.WriteRune(r)
	}

	return builder.String()
}

func (fe *TestReport) UpdateReport(ctx context.Context, testRunID string, coverageReport any) error {
	reportPath := filepath.Join(fe.Path, testRunID)

	data, err := yaml.MarshalGeneric(fe.Format, &coverageReport)
	if err != nil {
		return fmt.Errorf("%s failed to marshal coverage document. error: %s", utils.Emoji, err.Error())
	}

	err = yaml.WriteFileF(ctx, fe.Logger, reportPath, "coverage", data, false, fe.Format)
	if err != nil {
		utils.LogError(fe.Logger, err, "failed to write the coverage report", zap.String("session", filepath.Base(reportPath)))
		return err
	}
	return nil
}
