// Package testdb provides functionality for working with test databases.
package testdb

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Format  yaml.Format

	// lastIndex caches the max test-index per test-set path so upsert
	// can mint "test-N" names without re-reading tests/ on every
	// insert. pprof measured 16% of the record client's CPU in
	// FindLastIndex at ~3000 accumulated tests; at steady state every
	// new test caused a getdents64 of the full directory.
	lastIndex sync.Map // map[string]*atomic.Int64
}

func New(logger *zap.Logger, tcsPath string) *TestYaml {
	return NewWithFormat(logger, tcsPath, yaml.FormatYAML)
}

func NewWithFormat(logger *zap.Logger, tcsPath string, format yaml.Format) *TestYaml {
	return &TestYaml{
		TcsPath: tcsPath,
		logger:  logger,
		Format:  format,
	}
}

// nextTestIndex returns the next available test-N index for tcsPath.
// yaml.FindLastIndex already returns "max existing + 1" (i.e. the
// next available index on disk), so the first call seeds the counter
// with that value and returns it directly. Subsequent calls on the
// same tcsPath take the atomic Add(1) path. Safe for concurrent
// upserts on the same test-set.
func (ts *TestYaml) nextTestIndex(tcsPath string) (int, error) {
	if v, ok := ts.lastIndex.Load(tcsPath); ok {
		return int(v.(*atomic.Int64).Add(1)), nil
	}
	// Seed the cache with a format-aware scan so a tests/ directory recorded
	// in the opposite StorageFormat seeds against the right files instead of
	// returning 1 and clobbering existing test-N.* entries.
	seed, err := yaml.FindLastIndexF(tcsPath, ts.logger, ts.Format)
	if err != nil {
		return 0, err
	}
	// Store seed so the next caller sees Add(1)=seed+1, preserving
	// the "one monotonically-increasing counter per tcsPath" contract.
	var counter atomic.Int64
	counter.Store(int64(seed))
	actual, loaded := ts.lastIndex.LoadOrStore(tcsPath, &counter)
	if loaded {
		// Another goroutine raced us and won the LoadOrStore; advance
		// on the winning counter instead of overwriting it.
		return int(actual.(*atomic.Int64).Add(1)), nil
	}
	return seed, nil
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
		ts.logger.Debug("No latest run ID provided, returning empty test set IDs")
		return []string{}, nil
	}

	runReportPath := filepath.Join(ts.TcsPath, "reports", latestRunID)
	// Accept reports saved in either format so that replay-side tools
	// (e.g. "keploy report") keep working after a StorageFormat switch.
	reportNames, err := yaml.ReadSessionIndicesAny(ctx, runReportPath, ts.logger, yaml.ModeFile)
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

func (ts *TestYaml) GetTestCase(ctx context.Context, testSetID string, testCaseName string) (*models.TestCase, error) {
	// Validate inputs to prevent directory traversal.
	if filepath.Base(testSetID) != testSetID || strings.Contains(testSetID, "..") {
		return nil, fmt.Errorf("invalid test set ID %q: must not contain path separators or '..'", testSetID)
	}
	if filepath.Base(testCaseName) != testCaseName || strings.Contains(testCaseName, "..") {
		return nil, fmt.Errorf("invalid test case name %q: must not contain path separators or '..'", testCaseName)
	}
	path := filepath.Join(ts.TcsPath, testSetID, "tests")
	testPath, err := yaml.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	// Auto-detect format on read so replay works even when StorageFormat
	// differs from the format the testcase was originally recorded in.
	data, detected, err := yaml.ReadFileAny(ctx, ts.logger, testPath, testCaseName, ts.Format)
	if err != nil {
		return nil, fmt.Errorf("failed to read test case %q: %w", testCaseName, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("test case %q is empty", testCaseName)
	}
	// JSON file → decode directly from json.RawMessage spec; no yaml.Node
	// bridge on the hot path.
	if detected == yaml.FormatJSON {
		var jd yaml.NetworkTrafficDocJSON
		if err := json.Unmarshal(data, &jd); err != nil {
			return nil, fmt.Errorf("failed to unmarshal json test case %q: %w", testCaseName, err)
		}
		return DecodeJSON(&jd, ts.logger)
	}
	doc, err := yaml.UnmarshalDoc(detected, data)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal test case %q: %w", testCaseName, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("test case %q is nil after unmarshal", testCaseName)
	}
	return Decode(doc, ts.logger)
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
	// Accept both .yaml and .json so a directory containing a mix of
	// formats (e.g. mid-migration, or a yaml-default `keploy normalize`
	// run that wrote .yaml siblings next to existing .json testcases) is
	// fully visible to replay. Each file is decoded using its own
	// extension-derived format.
	//
	// When both test-N.yaml and test-N.json exist for the same basename,
	// prefer the one matching the configured StorageFormat. Pre-sort the
	// directory listing so format-matching files come first; the
	// "first-wins" dedup below then picks the preferred file every time.
	// Without this sort, the dedup keeps whichever extension the
	// filesystem returned first (creation order on ext4), which silently
	// loaded stale pre-normalize testcases on the JSON-recorded /
	// YAML-normalized path.
	preferredExt := "." + ts.Format.FileExtension()
	sort.SliceStable(files, func(i, j int) bool {
		iMatch := filepath.Ext(files[i].Name()) == preferredExt
		jMatch := filepath.Ext(files[j].Name()) == preferredExt
		// Stable sort: format-matching files come before others; ties
		// retain their original (filesystem) order.
		return iMatch && !jMatch
	})

	seen := make(map[string]yaml.Format) // base-name -> format already loaded
	for _, j := range files {
		fileExt := filepath.Ext(j.Name())
		var fileFormat yaml.Format
		switch fileExt {
		case ".yaml":
			fileFormat = yaml.FormatYAML
		case ".json":
			fileFormat = yaml.FormatJSON
		default:
			continue
		}
		if strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), fileExt)
		// First-wins: pre-sort guarantees the format-matching file is
		// processed before its sibling, so any later occurrence of the
		// same basename is by definition the non-preferred format.
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = fileFormat

		data, err := yaml.ReadFileF(ctx, ts.logger, TestPath, name, fileFormat)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the testcase")
			return nil, err
		}

		if len(data) == 0 {
			ts.logger.Debug("skipping empty testcase", zap.String("testcase name", name))
			continue
		}

		// JSON files decode directly into TestCase via DecodeJSON — no
		// yaml.Node allocation on the per-testcase hot path.
		if fileFormat == yaml.FormatJSON {
			var jd yaml.NetworkTrafficDocJSON
			if err := json.Unmarshal(data, &jd); err != nil {
				utils.LogError(ts.logger, err, "failed to unmarshal json testcase data", zap.String("testcase name", name))
				return nil, err
			}
			tc, err := DecodeJSON(&jd, ts.logger)
			if err != nil {
				utils.LogError(ts.logger, err, "failed to decode json testcase", zap.String("testcase name", name))
				return nil, err
			}
			tcs = append(tcs, tc)
			continue
		}

		testCase, err := yaml.UnmarshalDoc(fileFormat, data)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to unmarshal testcase data")
			return nil, err
		}

		if testCase == nil {
			ts.logger.Debug("skipping invalid testcase document", zap.String("testcase name", name))
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
		// nextTestIndex seeds itself off FindLastIndexF (so the format
		// chosen at construction time picks the right files) and then
		// hands out monotonically-increasing indices from a sync.Map cache.
		lastIndx, err := ts.nextTestIndex(tcsPath)
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

	// For YAML we build a NetworkTrafficDoc (whose Spec is a yaml.Node);
	// for JSON we build a NetworkTrafficDocJSON directly (Spec is
	// json.RawMessage). The JSON path skips the yaml.Node intermediate
	// entirely — this is where the big win comes from.
	var yamlTc *yaml.NetworkTrafficDoc
	var jsonTc *yaml.NetworkTrafficDocJSON
	if ts.Format == yaml.FormatJSON {
		jsonTc, err = EncodeTestcaseJSON(*tc, ts.logger)
		if err != nil {
			return tcsInfo{name: tcsName, path: tcsPath}, err
		}
		jsonTc.Name = tcsName
	} else {
		yamlTc, err = EncodeTestcase(*tc, ts.logger)
		if err != nil {
			return tcsInfo{name: tcsName, path: tcsPath}, err
		}
		yamlTc.Name = tcsName
	}

	// Stream the encoded testcase directly to disk via a temp file + rename
	// instead of buffering the full document in memory. This avoids allocating
	// a large bytes.Buffer per testcase. The final file's extension and
	// encoding are both driven by ts.Format (yaml or json).

	ext := "." + ts.Format.FileExtension()

	// Validate the output path to prevent directory traversal.
	outPath, err := yaml.ValidatePath(filepath.Join(tcsPath, tcsName+ext))
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("invalid testcase path: %w", err)
	}

	if err := os.MkdirAll(tcsPath, 0o777); err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to create tests directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(tcsPath, tcsName+"*"+ext+".tmp")
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	writer := bufio.NewWriter(tmpFile)

	// Version comment is a YAML-only concept (JSON has no comment syntax).
	if ts.Format == yaml.FormatYAML {
		if version := utils.GetVersionAsComment(); version != "" {
			if _, err := writer.WriteString(version); err != nil {
				return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to write version comment: %w", err)
			}
		}
	}

	switch ts.Format {
	case yaml.FormatJSON:
		// Pretty-print testcases so they're human-editable; json.Encoder
		// streams directly to writer with no intermediate []byte. jsonTc
		// was built via EncodeTestcaseJSON which skips the yaml.Node
		// intermediate (no yaml_emitter_emit, no yaml parse-back).
		enc := json.NewEncoder(writer)
		enc.SetIndent("", "  ")
		if err := enc.Encode(jsonTc); err != nil {
			return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to encode testcase json: %w", err)
		}
	default:
		encoder := yamlLib.NewEncoder(writer)
		encoder.SetIndent(2)
		if err := encoder.Encode(&yamlTc); err != nil {
			return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to encode testcase yaml: %w", err)
		}
		if err := encoder.Close(); err != nil {
			return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to close yaml encoder: %w", err)
		}
	}

	if err := writer.Flush(); err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to flush writer: %w", err)
	}
	// Set file permissions to 0777 to match the original CreateYamlFile behavior
	if err := tmpFile.Chmod(0o777); err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to close temp file: %w", err)
	}
	cleanup = false

	// Use remove-then-rename for cross-platform compatibility (Windows)
	if _, statErr := os.Stat(outPath); statErr == nil {
		if err := os.Remove(outPath); err != nil {
			os.Remove(tmpPath)
			return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to remove existing testcase: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		os.Remove(tmpPath)
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to stat existing testcase: %w", statErr)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		return tcsInfo{name: tcsName, path: tcsPath}, fmt.Errorf("failed to rename temp file: %w", err)
	}

	return tcsInfo{name: tcsName, path: tcsPath}, nil
}

func (ts *TestYaml) DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error {
	path := filepath.Join(ts.TcsPath, testSetID, "tests")
	for _, testCaseID := range testCaseIDs {
		err := yaml.DeleteFileF(ctx, ts.logger, path, testCaseID, ts.Format)
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
	tcsPath := filepath.Join(ts.TcsPath, testSetID, "tests")
	// Read whichever format the testcase was recorded in.
	data, detected, err := yaml.ReadFileAny(ctx, ts.logger, tcsPath, testCaseID, ts.Format)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to read the testcase")
		return err
	}
	if len(data) == 0 {
		ts.logger.Debug("skipping empty testcase", zap.String("testcase name", testCaseID))
		return nil
	}

	// Decode via the path that matches the file on disk — yaml.Node for
	// YAML files, json.RawMessage for JSON files. Write back in the SAME
	// format we read: don't silently convert an existing .yaml testcase
	// into .json just because StorageFormat flipped between record and
	// replay.
	var tc *models.TestCase
	if detected == yaml.FormatJSON {
		var jd yaml.NetworkTrafficDocJSON
		if err := json.Unmarshal(data, &jd); err != nil {
			utils.LogError(ts.logger, err, "failed to unmarshal json testcase data")
			return err
		}
		tc, err = DecodeJSON(&jd, ts.logger)
	} else {
		testCase, err := yaml.UnmarshalDoc(detected, data)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to unmarshal testcase data")
			return err
		}
		if testCase == nil {
			ts.logger.Warn("skipping invalid testCase", zap.String("testcase name", testCaseID))
			return nil
		}
		tc, err = Decode(testCase, ts.logger)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to decode the testcase")
			return err
		}
	}
	// Both branches above already returned on a nil document or decode
	// error; only the JSON branch's DecodeJSON err is left dangling here.
	if err != nil {
		utils.LogError(ts.logger, err, "failed to decode the testcase")
		return err
	}
	tc.Assertions = assertions

	// Re-encode in the same format we read.
	if detected == yaml.FormatJSON {
		jsonDoc, err := EncodeTestcaseJSON(*tc, ts.logger)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to encode the testcase (json)")
			return err
		}
		jsonDoc.Name = testCaseID
		out, err := json.MarshalIndent(jsonDoc, "", "  ")
		if err != nil {
			utils.LogError(ts.logger, err, "failed to marshal the json testcase")
			return err
		}
		out = append(out, '\n')
		if err := yaml.WriteFileF(ctx, ts.logger, tcsPath, testCaseID, out, false, detected); err != nil {
			utils.LogError(ts.logger, err, "failed to write testcase file")
			return err
		}
		return nil
	}

	yamlTc, err := EncodeTestcase(*tc, ts.logger)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to encode the testcase")
		return err
	}
	yamlTc.Name = testCaseID
	data, err = yaml.MarshalDoc(detected, yamlTc)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to marshal the testcase")
		return err
	}
	err = yaml.WriteFileF(ctx, ts.logger, tcsPath, testCaseID, data, false, detected)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to write testcase file")
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
