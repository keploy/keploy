// Package testdb provides functionality for working with test databases.
package testdb

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestYaml struct {
	TcsPath string
	logger  *zap.Logger
	expiryDuration time.Duration
}

func New(logger *zap.Logger, tcsPath string) *TestYaml {
	ts := &TestYaml{
		TcsPath:        tcsPath,
		logger:         logger,
		expiryDuration: 18 * time.Hour,
	}
	// start background cleanup of expired testcases
	go ts.startExpiredCleanup()
	return ts
}

// startExpiredCleanup kicks off a background goroutine that periodically
// removes expired testcases moved to the "expired" folder. It runs once
// for the lifetime of the process.
func (ts *TestYaml) startExpiredCleanup() {
	ticker := time.NewTicker(time.Hour)
	// run cleanup once immediately to remove any stale files from previous runs
	ts.cleanupExpired()
	for range ticker.C {
		ts.cleanupExpired()
	}
}

// cleanupExpired scans all testset "expired" directories and removes any
// testcase files whose expiry metadata indicates they have passed their TTL.
func (ts *TestYaml) cleanupExpired() {
	base := ts.TcsPath
	// list testset directories
	dirs, err := os.ReadDir(base)
	if err != nil {
		ts.logger.Debug("failed to read tests base dir for cleanup", zap.Error(err), zap.String("path", base))
		return
	}
	now := time.Now()
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		expiredPath := filepath.Join(base, d.Name(), "expired")
		edirs, err := os.ReadDir(expiredPath)
		if err != nil {
			// ignore missing expired directories
			continue
		}
		for _, f := range edirs {
			// skip metadata files if any
			if f.IsDir() {
				continue
			}
			name := f.Name()
			// metadata files use .expiry suffix
			if strings.HasSuffix(name, ".expiry") {
				// pair with the actual testcase file name
				baseName := strings.TrimSuffix(name, ".expiry")
				metaPath := filepath.Join(expiredPath, name)
				data, err := os.ReadFile(metaPath)
				if err != nil {
					// if meta unreadable, fallback to file modtime
					ts.logger.Debug("failed to read expiry meta, will fallback to file modtime", zap.Error(err), zap.String("meta", metaPath))
					continue
				}
				expTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
				if err != nil {
					ts.logger.Debug("invalid expiry timestamp, skipping", zap.Error(err), zap.String("meta", metaPath))
					continue
				}
				if now.After(expTime) {
					// delete both meta and testcase file (if exists)
					tcPath := filepath.Join(expiredPath, baseName)
					_ = os.Remove(tcPath)
					_ = os.Remove(metaPath)
					ts.logger.Info("removed expired testcase", zap.String("file", tcPath))
				}
			}
		}
	}
}

type tcsInfo struct {
	name string
	path string
}

func (ts *TestYaml) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string, enableLog bool) error {
	tc.Curl = pkg.MakeCurlCommand(tc.HTTPReq)
	tcsInfo, err := ts.upsert(ctx, testSetID, tc)
	if err != nil {
		return err
	}

	if enableLog {
		ts.logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", tcsInfo.path), zap.String("testcase name", tcsInfo.name))
	}

	return nil
}

func (ts *TestYaml) GetAllTestSetIDs(ctx context.Context) ([]string, error) {
	return yaml.ReadSessionIndices(ctx, ts.TcsPath, ts.logger, yaml.ModeDir)
}

func (ts *TestYaml) GetReportTestSets(ctx context.Context, latestRunID string) ([]string, error) {
	if latestRunID == "" {
		ts.logger.Warn("No latest run ID provided, returning empty test set IDs")
		return []string{}, nil
	}

	runReportPath := filepath.Join(ts.TcsPath, "reports", latestRunID)

	return yaml.ReadSessionIndices(ctx, runReportPath, ts.logger, yaml.ModeFile)
}

func (ts *TestYaml) GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error) {
	path := filepath.Join(ts.TcsPath, testSetID, "tests")

	tcs := []*models.TestCase{}
	TestPath, err := yaml.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(TestPath)
	if err != nil {
		ts.logger.Debug("no tests are recorded for the session", zap.String("index", testSetID))
		return nil, nil
	}
	dir, err := yaml.ReadDir(TestPath, fs.ModePerm)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to open the directory containing yaml testcases", zap.String("path", TestPath))
		return nil, err
	}
	files, err := dir.ReadDir(0)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to read the file names of yaml testcases", zap.String("path", TestPath))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		data, err := yaml.ReadFile(ctx, ts.logger, TestPath, name)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the testcase from yaml")
			return nil, err
		}

		if len(data) == 0 {
			ts.logger.Warn("skipping empty testcase", zap.String("testcase name", name))
			continue
		}

		var testCase *yaml.NetworkTrafficDoc
		err = yamlLib.Unmarshal(data, &testCase)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to unmarshall YAML data")
			return nil, err
		}

		if testCase == nil {
			ts.logger.Warn("skipping invalid testCase yaml", zap.String("testcase name", name))
			continue
		}

		tc, err := Decode(testCase, ts.logger)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to decode the testcase")
			return nil, err
		}
		tcs = append(tcs, tc)
	}

	// Sort test cases by their actual timestamp, whether HTTP or gRPC
	sort.SliceStable(tcs, func(i, j int) bool {
		var timeI, timeJ time.Time

		// Determine which timestamp to use for test case i based on its Kind
		if tcs[i].Kind == models.HTTP {
			timeI = tcs[i].HTTPReq.Timestamp
		} else if tcs[i].Kind == models.GRPC_EXPORT {
			timeI = tcs[i].GrpcReq.Timestamp
		}

		// Determine which timestamp to use for test case j based on its Kind
		if tcs[j].Kind == models.HTTP {
			timeJ = tcs[j].HTTPReq.Timestamp
		} else if tcs[j].Kind == models.GRPC_EXPORT {
			timeJ = tcs[j].GrpcReq.Timestamp
		}

		return timeI.Before(timeJ)
	})

	return tcs, nil
}

func (ts *TestYaml) UpdateTestCase(ctx context.Context, tc *models.TestCase, testSetID string, enableLog bool) error {

	tcsInfo, err := ts.upsert(ctx, testSetID, tc)
	if err != nil {
		return err
	}

	if enableLog {
		ts.logger.Info("🔄 Keploy has updated the test cases for the user's application.", zap.String("path", tcsInfo.path), zap.String("testcase name", tcsInfo.name))
	}
	return nil
}

func (ts *TestYaml) upsert(ctx context.Context, testSetID string, tc *models.TestCase) (tcsInfo, error) {
	tcsPath := filepath.Join(ts.TcsPath, testSetID, "tests")
	var tcsName string
	if tc.Name == "" {
		lastIndx, err := yaml.FindLastIndex(tcsPath, ts.logger)
		if err != nil {
			return tcsInfo{name: "", path: tcsPath}, err
		}
		tcsName = fmt.Sprintf("test-%v", lastIndx)
	} else {
		tcsName = tc.Name
	}
	yamlTc, err := EncodeTestcase(*tc, ts.logger)
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}
	yamlTc.Name = tcsName

	var buf bytes.Buffer
	encoder := yamlLib.NewEncoder(&buf)
	encoder.SetIndent(2) // Set indent to 2 spaces to match the original style
	err = encoder.Encode(&yamlTc)
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}
	data := buf.Bytes()

	_, err = yaml.FileExists(ctx, ts.logger, tcsPath, tcsName)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to find yaml file", zap.String("path directory", tcsPath), zap.String("yaml", tcsName))
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}

	data = append([]byte(utils.GetVersionAsComment()), data...)

	err = yaml.WriteFile(ctx, ts.logger, tcsPath, tcsName, data, false)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to write testcase yaml file")
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}

	return tcsInfo{name: tcsName, path: tcsPath}, nil
}

func (ts *TestYaml) DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error {
	testsPath := filepath.Join(ts.TcsPath, testSetID, "tests")
	expiredPath := filepath.Join(ts.TcsPath, testSetID, "expired")
	if err := os.MkdirAll(expiredPath, 0o755); err != nil {
		ts.logger.Error("failed to create expired dir", zap.Error(err), zap.String("path", expiredPath))
		return err
	}

	expiry := time.Now().Add(ts.expiryDuration)

	for _, testCaseID := range testCaseIDs {
		// move the testcase yaml to expired folder instead of deleting
		src := filepath.Join(testsPath, testCaseID+".yaml")
		dst := filepath.Join(expiredPath, testCaseID+".yaml")
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				ts.logger.Warn("testcase file not found while trying to expire", zap.String("testcase id", testCaseID), zap.String("testset id", testSetID))
				continue
			}
			ts.logger.Error("failed to stat testcase file", zap.Error(err), zap.String("file", src))
			return err
		}

		if err := os.Rename(src, dst); err != nil {
			// fallback to delete if move fails
			ts.logger.Error("failed to move testcase to expired folder, attempting delete", zap.Error(err), zap.String("src", src), zap.String("dst", dst))
			if err := yaml.DeleteFile(ctx, ts.logger, testsPath, testCaseID); err != nil {
				ts.logger.Error("failed to delete testcase after failed move", zap.Error(err), zap.String("testcase id", testCaseID))
				return err
			}
			continue
		}

		// write expiry metadata alongside the file
		metaPath := filepath.Join(expiredPath, testCaseID+".expiry")
		if err := os.WriteFile(metaPath, []byte(expiry.Format(time.RFC3339)), 0o644); err != nil {
			ts.logger.Warn("failed to write expiry metadata for testcase", zap.Error(err), zap.String("meta", metaPath))
			// do not fail the whole operation for metadata write failure
		}
		ts.logger.Info("expired testcase (moved) — will be removed after expiry", zap.String("testcase", dst), zap.Time("expiresAt", expiry))
	}
	return nil
}

func (ts *TestYaml) DeleteTestSet(ctx context.Context, testSetID string) error {
	// Instead of immediate deletion, move the whole testset directory to an
	// "expired" area and write an expiry timestamp. A background cleaner
	// (started in New) will permanently remove expired files after
	// ts.expiryDuration. This prevents accidental immediate loss of
	// artifacts when a user requests deletion while some tests are failing.
	src := filepath.Join(ts.TcsPath, testSetID)
	expiredBase := filepath.Join(ts.TcsPath, "expired")
	if err := os.MkdirAll(expiredBase, 0o755); err != nil {
		ts.logger.Error("failed to create expired dir", zap.Error(err), zap.String("path", expiredBase))
		// fallback to direct delete
		return yaml.DeleteDir(ctx, ts.logger, src)
	}

	// create a stable destination name to avoid collisions
	dst := filepath.Join(expiredBase, testSetID+"-"+time.Now().UTC().Format("20060102150405"))

	if err := os.Rename(src, dst); err != nil {
		ts.logger.Error("failed to move testset to expired folder, attempting delete", zap.Error(err), zap.String("src", src), zap.String("dst", dst))
		// fallback to delete if move fails
		return yaml.DeleteDir(ctx, ts.logger, src)
	}

	// write expiry metadata alongside the moved directory
	metaPath := dst + ".expiry"
	expiry := time.Now().Add(ts.expiryDuration)
	if err := os.WriteFile(metaPath, []byte(expiry.Format(time.RFC3339)), 0o644); err != nil {
		ts.logger.Warn("failed to write expiry metadata for testset", zap.Error(err), zap.String("meta", metaPath))
		// do not fail the whole operation for metadata write failure
	}
	ts.logger.Info("expired testset (moved) — will be removed after expiry", zap.String("testset", dst), zap.Time("expiresAt", expiry))
	return nil
}
func (ts *TestYaml) ChangePath(path string) {

	ts.TcsPath = path
}

func (ts *TestYaml) UpdateAssertions(ctx context.Context, testCaseID string, testSetID string, assertions map[models.AssertionType]interface{}) error {
	// get the test case and fill the assertion and update the test case
	tcsPath := filepath.Join(ts.TcsPath, testSetID, "tests")
	data, err := yaml.ReadFile(ctx, ts.logger, tcsPath, testCaseID)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to read the testcase from yaml")
		return err
	}
	if len(data) == 0 {
		ts.logger.Warn("skipping empty testcase", zap.String("testcase name", testCaseID))
		return nil
	}
	var testCase *yaml.NetworkTrafficDoc

	err = yamlLib.Unmarshal(data, &testCase)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to unmarshall YAML data")
		return err
	}

	if testCase == nil {
		ts.logger.Warn("skipping invalid testCase yaml", zap.String("testcase name", testCaseID))
		return nil
	}

	tc, err := Decode(testCase, ts.logger)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to decode the testcase")
		return err
	}
	tc.Assertions = assertions
	yamlTc, err := EncodeTestcase(*tc, ts.logger)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to encode the testcase")
		return err
	}
	yamlTc.Name = testCaseID
	data, err = yamlLib.Marshal(&yamlTc)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to marshall the testcase")
		return err
	}
	err = yaml.WriteFile(ctx, ts.logger, tcsPath, testCaseID, data, false)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to write testcase yaml file")
		return err
	}
	return nil
}
