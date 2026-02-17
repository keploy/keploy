// Package testdb provides functionality for working with test databases.
package testdb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
}

func New(logger *zap.Logger, tcsPath string) *TestYaml {
	return &TestYaml{
		TcsPath: tcsPath,
		logger:  logger,
	}
}

type tcsInfo struct {
	name string
	path string
}

func (ts *TestYaml) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string, enableLog bool) error {
	// Skip curl generation for either form data requests or large body (>1MB)
	if len(tc.HTTPReq.Body) <= LargeBodyThreshold && len(tc.HTTPReq.Form) == 0 {
		tc.Curl = pkg.MakeCurlCommand(tc.HTTPReq)
	}
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

	// Sort test cases by request timestamp, response timestamp, then test name numerically
	sort.SliceStable(tcs, func(i, j int) bool {
		var reqTimeI, reqTimeJ, respTimeI, respTimeJ time.Time

		// Get request and response timestamps for test case i
		if tcs[i].Kind == models.HTTP {
			reqTimeI = tcs[i].HTTPReq.Timestamp
			respTimeI = tcs[i].HTTPResp.Timestamp
		} else if tcs[i].Kind == models.GRPC_EXPORT {
			reqTimeI = tcs[i].GrpcReq.Timestamp
			respTimeI = tcs[i].GrpcResp.Timestamp
		}

		// Get request and response timestamps for test case j
		if tcs[j].Kind == models.HTTP {
			reqTimeJ = tcs[j].HTTPReq.Timestamp
			respTimeJ = tcs[j].HTTPResp.Timestamp
		} else if tcs[j].Kind == models.GRPC_EXPORT {
			reqTimeJ = tcs[j].GrpcReq.Timestamp
			respTimeJ = tcs[j].GrpcResp.Timestamp
		}

		// First, compare by request timestamp
		if !reqTimeI.Equal(reqTimeJ) {
			return reqTimeI.Before(reqTimeJ)
		}

		// If request timestamps are equal, compare by response timestamp
		if !respTimeI.Equal(respTimeJ) {
			return respTimeI.Before(respTimeJ)
		}

		// If both timestamps are equal, compare by test name numerically
		// Extract numeric part from test names (e.g., "test-2" -> 2, "test-11" -> 11)
		numI := extractTestNumber(tcs[i].Name)
		numJ := extractTestNumber(tcs[j].Name)
		return numI < numJ
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

	err := ts.saveAssets(testSetID, tc, tcsName)
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, err
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
	path := filepath.Join(ts.TcsPath, testSetID, "tests")
	for _, testCaseID := range testCaseIDs {
		err := yaml.DeleteFile(ctx, ts.logger, path, testCaseID)
		if err != nil {
			ts.logger.Error("failed to delete the testcase", zap.String("testcase id", testCaseID), zap.String("testset id", testSetID))
			return err
		}
	}
	return nil
}

func (ts *TestYaml) DeleteTestSet(ctx context.Context, testSetID string) error {
	path := filepath.Join(ts.TcsPath, testSetID)
	err := yaml.DeleteDir(ctx, ts.logger, path)
	if err != nil {
		ts.logger.Error("failed to delete the testset", zap.String("testset id", testSetID))
		return err
	}
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

// extractTestNumber extracts the numeric part from test names like "test-2" or "test-11"
// Returns the number as an integer, or 0 if no number is found
func extractTestNumber(name string) int {
	// Find the last occurrence of "-" and extract everything after it
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return 0
	}

	// Try to parse the last part as a number
	numStr := parts[len(parts)-1]
	num, err := strconv.Atoi(numStr)
	if err != nil {
		return 0
	}

	return num
}

// LargeBodyThreshold is the size threshold (1MB) above which request bodies
// are offloaded to the assets directory and response bodies are stored as hashes.
const LargeBodyThreshold = 1 * 1024 * 1024 // 1 MB

func (ts *TestYaml) saveAssets(testSetID string, tc *models.TestCase, tcsName string) error {
	// 1. Offload large request body (>1MB) to assets directory
	if len(tc.HTTPReq.Body) > LargeBodyThreshold {
		assetDir := filepath.Join(ts.TcsPath, testSetID, "assets", tcsName)
		if err := os.MkdirAll(assetDir, 0o755); err != nil {
			utils.LogError(ts.logger, err, "failed to create assets directory for body", zap.String("path", assetDir))
			return err
		}
		bodyPath := filepath.Join(assetDir, "body.txt")
		bodyBytes := []byte(tc.HTTPReq.Body)
		if err := os.WriteFile(bodyPath, bodyBytes, 0o644); err != nil {
			utils.LogError(ts.logger, err, "failed to write request body asset", zap.String("path", bodyPath))
			return err
		}
		// Store path relative to keploy directory so it stays portable
		relBodyPath, relErr := filepath.Rel(ts.TcsPath, bodyPath)
		if relErr != nil {
			relBodyPath = bodyPath // fallback to absolute if Rel fails
		}
		tc.HTTPReq.BodyRef = models.BodyRef{
			Path: relBodyPath,
			Size: int64(len(bodyBytes)),
		}
		tc.HTTPReq.Body = "" // clear the body since it's now stored in assets
		ts.logger.Debug("offloaded large request body to assets",
			zap.String("testcase", tcsName),
			zap.Int64("size", tc.HTTPReq.BodyRef.Size),
			zap.String("path", bodyPath))
	}

	// 2. Skip large response body (>1MB) — save only metadata, not the body
	if len(tc.HTTPResp.Body) > LargeBodyThreshold {
		contentType := tc.HTTPResp.Header["Content-Type"]
		if contentType == "" {
			contentType = "unknown"
		}
		tc.HTTPResp.BodySize = int64(len(tc.HTTPResp.Body))
		tc.HTTPResp.BodySkipped = true
		ts.logger.Debug("response body exceeds 1MB, skipping body storage",
			zap.String("testcase", tcsName),
			zap.Int64("body_size_bytes", tc.HTTPResp.BodySize),
			zap.String("content_type", contentType),
			zap.Int("status_code", tc.HTTPResp.StatusCode))
		tc.HTTPResp.Body = ""
	}

	// 3. Handle form data assets (files and large values)
	if tc.HTTPReq.Form == nil {
		return nil
	}

	for i, form := range tc.HTTPReq.Form {
		// 3a. Offload large form field values (>1MB) to assets (that are not actual files)
		if len(form.FileNames) == 0 && len(form.Paths) == 0 {
			hasLargeValue := false
			for _, value := range form.Values {
				if len(value) > LargeBodyThreshold {
					hasLargeValue = true
					break
				}
			}
			if hasLargeValue {
				// Pre-initialize Paths to same length as Values so indices stay aligned
				tc.HTTPReq.Form[i].Paths = make([]string, len(form.Values))
				for j, value := range form.Values {
					if len(value) > LargeBodyThreshold {
						formKey := filepath.Base(form.Key)
						if formKey == "." || formKey == string(filepath.Separator) || formKey == "" {
							formKey = "form"
						}
						assetDir := filepath.Join(ts.TcsPath, testSetID, "assets", tcsName, formKey)
						if err := os.MkdirAll(assetDir, 0o755); err != nil {
							utils.LogError(ts.logger, err, "failed to create assets directory for form value", zap.String("path", assetDir))
							return err
						}
						fileName := fmt.Sprintf("value_%d.txt", j)
						destPath := filepath.Join(assetDir, fileName)
						if err := os.WriteFile(destPath, []byte(value), 0o644); err != nil {
							utils.LogError(ts.logger, err, "failed to write large form value asset", zap.String("path", destPath))
							return err
						}
						// Replace value with empty string and store relative path at the same index
						tc.HTTPReq.Form[i].Values[j] = ""
						relPath, relErr := filepath.Rel(ts.TcsPath, destPath)
						if relErr != nil {
							relPath = destPath
						}
						tc.HTTPReq.Form[i].Paths[j] = relPath
						ts.logger.Debug("offloaded large form value to assets",
							zap.String("testcase", tcsName),
							zap.String("key", form.Key),
							zap.Int("size", len(value)),
							zap.String("path", destPath))
					}
					// Small values: Paths[j] stays "" — no offload needed
				}
			}
			continue
		}

		// 3b. Handle file-based form data (existing logic)
		if len(form.FileNames) > 0 {
			formKey := filepath.Base(form.Key)
			if formKey == "." || formKey == string(filepath.Separator) || formKey == "" {
				formKey = "form"
			}
			assetDir := filepath.Join(ts.TcsPath, testSetID, "assets", tcsName, formKey)
			if err := os.MkdirAll(assetDir, 0o755); err != nil {
				utils.LogError(ts.logger, err, "failed to create assets directory", zap.String("path", assetDir))
				return err
			}

			// We need to rebuild Paths to point to assets
			var newPaths []string
			allFilesPersisted := true

			for j, fileName := range form.FileNames {
				if fileName == "" {
					continue
				}
				safeFileName := filepath.Base(fileName)
				if safeFileName == "." || safeFileName == string(filepath.Separator) || safeFileName == "" {
					safeFileName = "asset_file"
				}
				destPath := filepath.Join(assetDir, safeFileName)
				wroteFile := false

				// Case 1: File is in Paths (downloaded to local temp)
				if j < len(form.Paths) && form.Paths[j] != "" {
					srcPath := form.Paths[j]
					// Check if srcPath exists
					if _, err := os.Stat(srcPath); err == nil {
						input, err := os.Open(srcPath)
						if err != nil {
							utils.LogError(ts.logger, err, "failed to open temp asset file", zap.String("path", srcPath))
							return err
						}
						output, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
						if err != nil {
							input.Close()
							utils.LogError(ts.logger, err, "failed to create asset file", zap.String("path", destPath))
							return err
						}
						if _, err := io.Copy(output, input); err != nil {
							output.Close()
							input.Close()
							utils.LogError(ts.logger, err, "failed to copy asset file", zap.String("path", destPath))
							return err
						}
						if err := output.Close(); err != nil {
							input.Close()
							utils.LogError(ts.logger, err, "failed to close asset file", zap.String("path", destPath))
							return err
						}
						if err := input.Close(); err != nil {
							utils.LogError(ts.logger, err, "failed to close temp asset file", zap.String("path", srcPath))
							return err
						}
						wroteFile = true
						// Cleanup temp
						os.Remove(srcPath)
					} else {
						utils.LogError(ts.logger, fmt.Errorf("asset source file not found: %s", srcPath), "failed to persist form file - ensure the temp file exists before saving", zap.String("path", srcPath))
					}
				} else if j < len(form.Values) {
					// Case 2: File content is in Values (legacy/text fallback)
					content := []byte(form.Values[j])
					if err := os.WriteFile(destPath, content, 0o644); err != nil {
						utils.LogError(ts.logger, err, "failed to write asset file", zap.String("path", destPath))
						return err
					}
					wroteFile = true
				}

				if wroteFile {
					// Store path relative to keploy directory so it stays portable
					relPath, relErr := filepath.Rel(ts.TcsPath, destPath)
					if relErr != nil {
						relPath = destPath
					}
					newPaths = append(newPaths, relPath)
				} else {
					allFilesPersisted = false
					// Do not append non-existent paths to newPaths — they would
					// cause replay failures when the system tries to read them.
					utils.LogError(ts.logger, fmt.Errorf("file entry could not be persisted"), "skipping file entry - check that the source file exists and is accessible",
						zap.String("fileName", form.FileNames[j]),
						zap.String("key", form.Key))
				}
			}

			tc.HTTPReq.Form[i].Paths = newPaths
			if allFilesPersisted {
				tc.HTTPReq.Form[i].Values = nil
			}
		}
	}
	return nil
}
