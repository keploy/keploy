package diagnose

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/diagnostic"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
}

type TestDB interface {
	GetReportTestSets(ctx context.Context, latestRunID string) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, tc *models.TestCase, testSetID string, enableLog bool) error
}

type Service interface {
	Diagnose(ctx context.Context) error
}

type Diagnoser struct {
	logger   *zap.Logger
	config   *config.Config
	reportDB ReportDB
	testDB   TestDB
}

func New(logger *zap.Logger, cfg *config.Config, reportDB ReportDB, testDB TestDB) *Diagnoser {
	return &Diagnoser{
		logger:   logger,
		config:   cfg,
		reportDB: reportDB,
		testDB:   testDB,
	}
}

func (d *Diagnoser) Diagnose(ctx context.Context) error {
	runID, err := d.getLatestTestRunID(ctx)
	if err != nil {
		return err
	}
	if runID == "" {
		d.logger.Warn("no test runs found")
		return nil
	}

	testSetIDs := d.extractTestSetIDs()
	if len(testSetIDs) == 0 {
		testSetIDs, err = d.testDB.GetReportTestSets(ctx, runID)
		if err != nil {
			return fmt.Errorf("failed to get test sets for diagnose: %w", err)
		}
	}
	if len(testSetIDs) == 0 {
		d.logger.Warn("no test sets found for diagnose")
		return nil
	}

	minConfidence := d.config.Diagnose.MinConfidence
	if minConfidence == 0 {
		minConfidence = 95
	}

	reader := bufio.NewReader(os.Stdin)
	appliedAny := false

	for _, testSetID := range testSetIDs {
		cleanTestSetID := strings.TrimSuffix(testSetID, "-report")
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		rep, err := d.reportDB.GetReport(ctx, runID, cleanTestSetID)
		if err != nil || rep == nil {
			if err != nil {
				d.logger.Warn("failed to read report", zap.String("testSet", testSetID), zap.Error(err))
			}
			continue
		}

		casesByID := map[string]*models.TestCase{}
		tcs, err := d.testDB.GetTestCases(ctx, cleanTestSetID)
		if err != nil {
			d.logger.Warn("failed to load test cases", zap.String("testSet", testSetID), zap.Error(err))
			continue
		}
		for _, tc := range tcs {
			casesByID[tc.Name] = tc
		}

		backupCreated := false
		for _, tr := range rep.Tests {
			if tr.Status != models.TestStatusFailed {
				continue
			}
			if len(d.config.Diagnose.TestCaseIDs) > 0 && !contains(d.config.Diagnose.TestCaseIDs, tr.TestCaseID) {
				continue
			}
			if tr.Diagnostic == nil {
				fmt.Printf("%s/%s: no diagnostic data\n", testSetID, tr.TestCaseID)
				continue
			}

			d.printHeader(cleanTestSetID, tr)

			expBody := tr.Diagnostic.ExpectedBody
			actBody := tr.Diagnostic.ActualBody

			if d.config.Diagnose.ShowDiff {
				d.renderDiff(expBody, actBody)
			}

			report, _ := diagnostic.ComputeJSONDiff(expBody, actBody, d.config.Test.IgnoreOrdering)
			confidence := 0
			category := models.CategoryDataUpdate
			if report != nil {
				confidence = report.Confidence
				category = report.Category
			}

			fmt.Printf("Category: %s\n", category)
			fmt.Printf("Confidence: %d\n", confidence)

			if d.config.Diagnose.AutoFix {
				if confidence < minConfidence {
					fmt.Printf("skip auto-fix: confidence %d < %d\n\n", confidence, minConfidence)
					continue
				}
				if !backupCreated {
					if err := d.createBackup(cleanTestSetID); err != nil {
						d.logger.Warn("failed to create backup", zap.String("testSet", testSetID), zap.Error(err))
					}
					backupCreated = true
				}
				if d.applyFix(ctx, cleanTestSetID, tr, casesByID) {
					appliedAny = true
					fmt.Println("applied fix")
				}
				fmt.Println()
				continue
			}

			fmt.Print("Apply fix? [y/N]: ")
			ans, _ := reader.ReadString('\n')
			ans = strings.TrimSpace(strings.ToLower(ans))
			if ans == "y" || ans == "yes" {
				if !backupCreated {
				if err := d.createBackup(cleanTestSetID); err != nil {
					d.logger.Warn("failed to create backup", zap.String("testSet", testSetID), zap.Error(err))
				}
				backupCreated = true
			}
			if d.applyFix(ctx, cleanTestSetID, tr, casesByID) {
				appliedAny = true
				fmt.Println("applied fix")
			}
			}
			fmt.Println()
		}
	}

	if appliedAny {
		fmt.Println("diagnose completed with applied fixes")
	}
	return nil
}

func (d *Diagnoser) printHeader(testSetID string, tr models.TestResult) {
	fmt.Printf("\n[%s] %s\n", testSetID, tr.TestCaseID)
	fmt.Printf("Request: %s\n", tr.Diagnostic.RequestSignature)
	if tr.Diagnostic.ExpectedStatus != 0 || tr.Diagnostic.ActualStatus != 0 {
		fmt.Printf("Status: expected %d, actual %d\n", tr.Diagnostic.ExpectedStatus, tr.Diagnostic.ActualStatus)
	}
	if len(tr.Diagnostic.ExpectedMocks) > 0 || len(tr.Diagnostic.ActualMocks) > 0 {
		fmt.Printf("Mocks: expected=%v actual=%v\n", tr.Diagnostic.ExpectedMocks, tr.Diagnostic.ActualMocks)
	}
}

func (d *Diagnoser) renderDiff(expBody, actBody string) {
	if pkg.IsJSON([]byte(expBody)) && pkg.IsJSON([]byte(actBody)) {
		if report, err := diagnostic.ComputeJSONDiff(expBody, actBody, d.config.Test.IgnoreOrdering); err == nil && report != nil {
			for _, e := range report.Entries {
				fmt.Printf("- %s %s (expected=%v actual=%v) [%s]\n", e.Path, e.Kind, e.Expected, e.Actual, e.Category)
			}
			return
		}
	}
	if expBody != actBody {
		fmt.Printf("- body changed (expected_len=%d actual_len=%d)\n", len(expBody), len(actBody))
	}
}

func (d *Diagnoser) applyFix(ctx context.Context, testSetID string, tr models.TestResult, casesByID map[string]*models.TestCase) bool {
	tc := casesByID[tr.TestCaseID]
	if tc == nil {
		return false
	}
	switch tc.Kind {
	case models.HTTP:
		tc.HTTPResp = tr.Res
	case models.GRPC_EXPORT:
		tc.GrpcResp = tr.GrpcRes
	default:
		return false
	}
	if err := d.testDB.UpdateTestCase(ctx, tc, testSetID, false); err != nil {
		d.logger.Warn("failed to update testcase", zap.String("testSet", testSetID), zap.String("testCase", tr.TestCaseID), zap.Error(err))
		return false
	}
	return true
}

func (d *Diagnoser) extractTestSetIDs() []string {
	var testSetIDs []string
	for testSet := range d.config.Diagnose.SelectedTestSets {
		testSetIDs = append(testSetIDs, strings.TrimSpace(testSet))
	}
	sort.Strings(testSetIDs)
	return testSetIDs
}

func (d *Diagnoser) getLatestTestRunID(ctx context.Context) (string, error) {
	testRunIDs, err := d.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		return "", err
	}
	if len(testRunIDs) == 0 {
		return "", nil
	}
	sort.Slice(testRunIDs, func(i, j int) bool {
		numi, erri := strconv.Atoi(strings.TrimPrefix(testRunIDs[i], "test-run-"))
		numj, errj := strconv.Atoi(strings.TrimPrefix(testRunIDs[j], "test-run-"))
		if erri != nil && errj != nil {
			return testRunIDs[i] < testRunIDs[j]
		}
		if erri != nil {
			return true
		}
		if errj != nil {
			return false
		}
		return numi < numj
	})
	return testRunIDs[len(testRunIDs)-1], nil
}

func (d *Diagnoser) createBackup(testSetID string) error {
	srcPath := filepath.Join(d.config.Path, testSetID)
	timestamp := time.Now().Format("20060102T150405")
	backupDestPath := filepath.Join(srcPath, ".backup", "diagnose-"+timestamp)

	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("source directory for backup does not exist: %s", srcPath)
	}

	if err := os.MkdirAll(backupDestPath, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	if err := d.copyDirContents(srcPath, backupDestPath); err != nil {
		_ = os.RemoveAll(backupDestPath)
		return fmt.Errorf("failed to copy contents for backup: %w", err)
	}

	d.logger.Info("backup created", zap.String("testSet", testSetID), zap.String("location", backupDestPath))
	return nil
}

func (d *Diagnoser) copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.Name() == ".backup" {
			continue
		}

		fileInfo, err := os.Stat(srcPath)
		if err != nil {
			return err
		}

		if fileInfo.IsDir() {
			if err := os.MkdirAll(dstPath, fileInfo.Mode()); err != nil {
				return err
			}
			if err := d.copyDirContents(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			srcFile, err := os.Open(srcPath)
			if err != nil {
				return err
			}
			defer srcFile.Close()

			dstFile, err := os.Create(dstPath)
			if err != nil {
				return err
			}
			defer dstFile.Close()

			if _, err := io.Copy(dstFile, srcFile); err != nil {
				return err
			}

			if err := os.Chmod(dstPath, fileInfo.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func contains(list []string, val string) bool {
	for _, v := range list {
		if strings.TrimSpace(v) == val {
			return true
		}
	}
	return false
}
