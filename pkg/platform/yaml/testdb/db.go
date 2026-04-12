// Package testdb provides functionality for working with test databases.
package testdb

import (
	"bytes"
	"context"
	"errors"
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

// NamingStrategy selects how default test case filenames are generated
// during recording.
type NamingStrategy string

const (
	// NamingDescriptive derives slugs from the HTTP method+path or
	// gRPC service/method of the recorded request.
	NamingDescriptive NamingStrategy = "descriptive"
	// NamingSequential preserves the legacy test-N.yaml numbering.
	NamingSequential NamingStrategy = "sequential"
)

type TestYaml struct {
	TcsPath        string
	logger         *zap.Logger
	namingStrategy NamingStrategy
}

func New(logger *zap.Logger, tcsPath string) *TestYaml {
	return NewWithNaming(logger, tcsPath, NamingDescriptive)
}

// NewWithNaming constructs a TestYaml that uses the given naming
// strategy for test cases without an explicit name.
func NewWithNaming(logger *zap.Logger, tcsPath string, strategy NamingStrategy) *TestYaml {
	if strategy == "" {
		strategy = NamingDescriptive
	}
	return &TestYaml{
		TcsPath:        tcsPath,
		logger:         logger,
		namingStrategy: strategy,
	}
}

// ParseNamingStrategy converts a user-supplied config string into a
// NamingStrategy, tolerating leading/trailing whitespace and case
// differences. It returns an error for unrecognised values so that
// callers can surface config typos instead of silently falling back.
// The returned strategy is always usable — unknown inputs default to
// NamingDescriptive so the caller may choose to log-and-continue.
func ParseNamingStrategy(s string) (NamingStrategy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(NamingDescriptive):
		return NamingDescriptive, nil
	case string(NamingSequential):
		return NamingSequential, nil
	default:
		return NamingDescriptive, fmt.Errorf(
			"unknown testCaseNaming %q: supported values are %q and %q",
			s, NamingDescriptive, NamingSequential,
		)
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
	reportNames, err := yaml.ReadSessionIndices(ctx, runReportPath, ts.logger, yaml.ModeFile)
	if err != nil {
		return nil, err
	}

	testSetReports := make([]string, 0, len(reportNames))
	for _, name := range reportNames {
		if strings.HasSuffix(name, "-report") {
			testSetReports = append(testSetReports, name)
		}
	}

	return testSetReports, nil
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
	var reservedPlaceholder bool
	if tc.Name == "" {
		claimed, err := ts.claimName(tcsPath, tc)
		if err != nil {
			return tcsInfo{name: "", path: tcsPath}, err
		}
		tcsName = claimed
		reservedPlaceholder = true
	} else {
		tcsName = tc.Name
	}

	// If anything after the atomic filename reservation fails before
	// we successfully write the final yaml, remove the empty
	// placeholder we left behind in claimName so the testset
	// directory doesn't accumulate stale 0-byte files that would
	// also skew subsequent NextIndexForPrefix scans.
	writeSucceeded := false
	defer func() {
		if reservedPlaceholder && !writeSucceeded {
			placeholder := filepath.Join(tcsPath, tcsName+".yaml")
			if rmErr := os.Remove(placeholder); rmErr != nil && !os.IsNotExist(rmErr) {
				// This is a secondary failure during error handling —
				// the primary upsert error is already returned to the
				// caller. Surface the cleanup miss at Debug so it
				// stays discoverable without polluting normal logs.
				ts.logger.Debug("failed to remove placeholder after upsert error",
					zap.String("path", placeholder),
					zap.Error(rmErr))
			}
		}
	}()

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

	writeSucceeded = true
	return tcsInfo{name: tcsName, path: tcsPath}, nil
}

// generateName produces a default filename for a recorded test case
// based on the active naming strategy. Descriptive mode derives a slug
// from the request and appends a collision-resolving suffix; sequential
// mode preserves the legacy test-N numbering.
func (ts *TestYaml) generateName(tcsPath string, tc *models.TestCase) (string, error) {
	if ts.namingStrategy == NamingSequential {
		idx, err := yaml.FindLastIndex(tcsPath, ts.logger)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("test-%d", idx), nil
	}

	slug := BuildTestCaseSlug(tc)
	idx, err := yaml.NextIndexForPrefix(tcsPath, slug)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", slug, idx), nil
}

// maxNameClaimAttempts bounds the retry loop in claimName so a
// persistent filesystem error can't spin forever. It must comfortably
// exceed the number of recorders a user could realistically run
// against the same testset directory in parallel.
const maxNameClaimAttempts = 32

// claimName atomically reserves a unique filename for an auto-named
// test case. It calls generateName, then attempts to create the target
// file with O_CREATE|O_EXCL so two concurrent recorders writing to the
// same testset directory cannot end up writing to the same path. On a
// collision (another process wrote that name between our directory
// scan and the create), it rescans the directory for a new index and
// retries. WriteFile later truncates the placeholder we created here
// and replaces it with the encoded testcase body.
func (ts *TestYaml) claimName(tcsPath string, tc *models.TestCase) (string, error) {
	// Intentionally stricter than yaml.CreateYamlFile's legacy 0o777:
	// directories 0o755, files 0o644 (same defaults Go's os.Create and
	// os.MkdirAll pick when the caller doesn't care). A separate
	// hardening pass can bring the explicit-name path through
	// CreateYamlFile down to the same modes — doing that in this PR
	// would bloat the diff and risk breaking unrelated flows that
	// happen to rely on 0o777. Tracked inline so a future reviewer
	// doesn't re-litigate the discrepancy.
	if err := os.MkdirAll(tcsPath, 0o755); err != nil {
		return "", fmt.Errorf("create testcase directory: %w", err)
	}
	var lastName string
	for attempt := 0; attempt < maxNameClaimAttempts; attempt++ {
		name, err := ts.generateName(tcsPath, tc)
		if err != nil {
			return "", err
		}
		lastName = name
		target := filepath.Join(tcsPath, name+".yaml")
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("reserve testcase file %q: %w", name, err)
		}
	}
	return "", fmt.Errorf("failed to allocate a unique testcase name after %d attempts (last candidate %q)", maxNameClaimAttempts, lastName)
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
