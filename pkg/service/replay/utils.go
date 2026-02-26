package replay

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	// "encoding/json"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total     int
	passed    int
	failed    int
	obsolete  int
	ignored   int
	status    bool
	duration  time.Duration
	timeTaken string
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := CloneGlobalNoise(globalNoise)

	if _, ok := noise["body"]; !ok {
		noise["body"] = make(map[string][]string)
	}
	if tsNoiseBody, ok := tsNoise["body"]; ok {
		for field, regexArr := range tsNoiseBody {
			noise["body"][field] = regexArr
		}
	}

	if _, ok := noise["header"]; !ok {
		noise["header"] = make(map[string][]string)
	}
	if tsNoiseHeader, ok := tsNoise["header"]; ok {
		for field, regexArr := range tsNoiseHeader {
			noise["header"][field] = regexArr
		}
	}

	return noise
}

func CloneGlobalNoise(src config.GlobalNoise) config.GlobalNoise {
	cloned := make(config.GlobalNoise, len(src))
	for section, fields := range src {
		fieldCopy := make(map[string][]string, len(fields))
		for field, patterns := range fields {
			patternCopy := make([]string, len(patterns))
			copy(patternCopy, patterns)
			fieldCopy[field] = patternCopy
		}
		cloned[section] = fieldCopy
	}
	return cloned
}

// PrepareHeaderNoiseConfig prepares the header noise configuration for mock matching.
// It merges global and test-set specific noise, then extracts only the header noise.
func PrepareHeaderNoiseConfig(globalNoise config.GlobalNoise, testSetNoise config.TestsetNoise, testSetID string) map[string]map[string][]string {
	noiseConfig := CloneGlobalNoise(globalNoise)
	if tsNoise, ok := testSetNoise[testSetID]; ok {
		noiseConfig = LeftJoinNoise(noiseConfig, tsNoise)
	}

	// Extract only header noise for mock matching
	headerNoiseConfig := map[string]map[string][]string{}
	if headerNoise, ok := noiseConfig["header"]; ok {
		headerNoiseConfig["header"] = headerNoise
	}

	return headerNoiseConfig
}

// ReplaceBaseURL replaces the baseUrl of the old URL with the new URL's.
func ReplaceBaseURL(newURL, oldURL string) (string, error) {
	parsedOldURL, err := url.Parse(oldURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the old URL: %v", err)
	}

	parsedNewURL, err := url.Parse(newURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse the new URL: %v", err)
	}
	// if scheme is empty, then add the scheme from the old URL in order to parse it correctly
	if parsedNewURL.Scheme == "" {
		parsedNewURL.Scheme = parsedOldURL.Scheme
		parsedNewURL, err = url.Parse(parsedNewURL.String())
		if err != nil {
			return "", fmt.Errorf("failed to parse the scheme added new URL: %v", err)
		}
	}

	parsedOldURL.Scheme = parsedNewURL.Scheme
	parsedOldURL.Host = parsedNewURL.Host
	apiPath := path.Join(parsedNewURL.Path, parsedOldURL.Path)

	parsedOldURL.Path = apiPath
	parsedOldURL.RawPath = apiPath
	replacedURL := parsedOldURL.String()
	decodedURL, err := url.PathUnescape(replacedURL)
	if err != nil {
		return "", fmt.Errorf("failed to decode the URL: %v", err)
	}
	return decodedURL, nil
}

func mergeMaps(map1, map2 map[string][]string) map[string][]string {
	for key, values := range map2 {
		if _, exists := map1[key]; exists {
			map1[key] = append(map1[key], values...)
		} else {
			map1[key] = values
		}
	}
	return map1
}

func removeFromMap(map1, map2 map[string][]string) map[string][]string {
	for key := range map2 {
		delete(map1, key)
	}
	return map1
}

func testCaseRequestTimestamp(tc *models.TestCase) time.Time {
	if tc == nil {
		return time.Time{}
	}
	switch tc.Kind {
	case models.HTTP:
		return tc.HTTPReq.Timestamp
	case models.GRPC_EXPORT:
		return tc.GrpcReq.Timestamp
	default:
		return time.Time{}
	}
}

// effectiveStreamMockWindow calculates the effective time window for streaming mocks.
// It returns the start time (request timestamp) and end time (request timestamp + timeout).
// The timeout is calculated using pkg.ComputeStreamingTimeoutSeconds which considers the test case's timeout configuration.
func effectiveStreamMockWindow(tc *models.TestCase, defaultAPITimeout uint64) (time.Time, time.Time) {
	if tc == nil {
		return time.Time{}, time.Time{}
	}

	reqTs := tc.HTTPReq.Timestamp
	respTs := tc.HTTPResp.Timestamp
	timeoutSeconds := pkg.ComputeStreamingTimeoutSeconds(tc, defaultAPITimeout)

	anchor := reqTs
	if anchor.IsZero() || (!respTs.IsZero() && respTs.After(anchor)) {
		anchor = respTs
	}
	if anchor.IsZero() {
		anchor = time.Now().UTC()
	}

	return reqTs, anchor.Add(time.Duration(timeoutSeconds) * time.Second)
}

func timeWithUnits(duration time.Duration) string {
	if duration.Seconds() < 1 {
		return fmt.Sprintf("%v ms", duration.Milliseconds())
	} else if duration.Minutes() < 1 {
		return fmt.Sprintf("%.2f s", duration.Seconds())
	} else if duration.Hours() < 1 {
		return fmt.Sprintf("%.2f min", duration.Minutes())
	}
	return fmt.Sprintf("%.2f hr", duration.Hours())
}

func getFailedTCs(results []models.TestResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r.Status == models.TestStatusFailed {
			ids = append(ids, r.TestCaseID)
		}
	}
	return ids
}

func isMockSubset(subset, superset []string) bool {
	supersetCounts := make(map[string]int)
	for _, mock := range superset {
		supersetCounts[mock]++
	}

	for _, mock := range subset {
		if supersetCounts[mock] == 0 {
			return false
		}
		supersetCounts[mock]--
	}

	return true
}

type TestFailure struct {
	TestSetID     string
	TestID        string
	ExpectedMocks []string
	ActualMocks   []string
	FailureReason models.ParserErrorType
}

type TestFailureStore struct {
	mu       sync.Mutex
	failures []TestFailure
}

func NewTestFailureStore() *TestFailureStore {
	return &TestFailureStore{
		failures: make([]TestFailure, 0),
	}
}

func (tfs *TestFailureStore) AddFailure(testSetID, testID string, expectedMocks, actualMocks []string) {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	failure := TestFailure{
		TestSetID:     testSetID,
		TestID:        testID,
		ExpectedMocks: expectedMocks,
		ActualMocks:   actualMocks,
	}
	tfs.failures = append(tfs.failures, failure)
}

func (tfs *TestFailureStore) AddProxyErrorForTest(testSetID string, testCaseID string, proxyErr models.ParserError) {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	failure := TestFailure{
		TestSetID:     testSetID,
		TestID:        testCaseID,
		ExpectedMocks: []string{},
		ActualMocks:   []string{},
		FailureReason: proxyErr.ParserErrorType,
	}
	tfs.failures = append(tfs.failures, failure)
}

func (tfs *TestFailureStore) GetFailures() []TestFailure {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	// Return a copy to prevent external modifications
	failures := make([]TestFailure, len(tfs.failures))
	copy(failures, tfs.failures)
	return failures
}

type MockDifference struct {
	Key            string
	ExpectedValues []string
	ActualValues   []string
	DiffType       string // "missing", "extra", "different"
}

// CompareMockSlices compares two mock slices and returns the differences
func CompareMockSlices(expected, actual []string) []MockDifference {
	var differences []MockDifference

	// Convert slices to maps for easier comparison
	expectedMap := make(map[string]bool)
	actualMap := make(map[string]bool)

	for _, mock := range expected {
		expectedMap[mock] = true
	}
	for _, mock := range actual {
		actualMap[mock] = true
	}

	// Get all unique keys
	allKeys := make(map[string]bool)
	for mock := range expectedMap {
		allKeys[mock] = true
	}
	for mock := range actualMap {
		allKeys[mock] = true
	}

	// Sort keys for consistent output
	var keys []string
	for key := range allKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		_, expectedExists := expectedMap[key]
		_, actualExists := actualMap[key]

		if !expectedExists && actualExists {
			differences = append(differences, MockDifference{
				Key:            key,
				ExpectedValues: []string{},
				ActualValues:   []string{key},
				DiffType:       "extra",
			})
		} else if expectedExists && !actualExists {
			differences = append(differences, MockDifference{
				Key:            key,
				ExpectedValues: []string{key},
				ActualValues:   []string{},
				DiffType:       "missing",
			})
		}
	}

	return differences
}

// PrintFailuresTable prints all failures in a formatted table
func (tfs *TestFailureStore) PrintFailuresTable() {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	if len(tfs.failures) == 0 {
		fmt.Println("No test failures recorded.")
		return
	}

	fmt.Println("\n======================= MOCKS MISMATCH SUMMARY =======================")

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"TEST SET", "TEST ID", "MOCK DIFFERENCES"})
	table.SetBorder(true)
	table.SetRowLine(true)
	table.SetCenterSeparator("|")
	table.SetColumnSeparator("|")
	table.SetRowSeparator("-")
	table.SetAutoWrapText(true)
	table.SetReflowDuringAutoWrap(false)
	table.SetHeaderAlignment(tablewriter.ALIGN_CENTER)
	table.SetAlignment(tablewriter.ALIGN_CENTER)
	table.SetColWidth(120)
	table.SetTablePadding(" ")
	table.SetColMinWidth(0, 15) // TEST SET column min width
	table.SetColMinWidth(1, 12) // TEST ID column min width
	table.SetColMinWidth(2, 50) // MOCK DIFFERENCES column min width

	// Group failures by test set for better presentation
	testSetGroups := make(map[string][]TestFailure)
	for _, failure := range tfs.failures {
		testSetGroups[failure.TestSetID] = append(testSetGroups[failure.TestSetID], failure)
	}

	// Sort test set IDs for consistent output
	var testSetIDs []string
	for testSetID := range testSetGroups {
		testSetIDs = append(testSetIDs, testSetID)
	}
	sort.Strings(testSetIDs)

	for _, testSetID := range testSetIDs {
		failures := testSetGroups[testSetID]
		testSetPrinted := false

		// Group failures by test ID to combine mock differences and proxy errors
		testIDGroups := make(map[string][]TestFailure)
		for _, failure := range failures {
			testIDGroups[failure.TestID] = append(testIDGroups[failure.TestID], failure)
		}

		// Sort test IDs for consistent output
		var testIDs []string
		for testID := range testIDGroups {
			testIDs = append(testIDs, testID)
		}
		sort.Strings(testIDs)

		for _, testID := range testIDs {
			testFailures := testIDGroups[testID]
			var combinedDiffText string
			var allDiffStrings []string

			for _, failure := range testFailures {
				if failure.FailureReason == models.ErrMockNotFound {
					allDiffStrings = append(allDiffStrings, "Outgoing call mock was not matched")
				}

				if len(failure.ExpectedMocks) > 0 || len(failure.ActualMocks) > 0 {
					differences := CompareMockSlices(failure.ExpectedMocks, failure.ActualMocks)
					var missingMocks, extraMocks []string
					for _, diff := range differences {
						switch diff.DiffType {
						case "missing":
							missingMocks = append(missingMocks, diff.Key)
						case "extra":
							extraMocks = append(extraMocks, diff.Key)
						}
					}
					if len(missingMocks) > 0 {
						allDiffStrings = append(allDiffStrings, fmt.Sprintf("Missing mocks: %s", strings.Join(missingMocks, ", ")))
					}
					if len(extraMocks) > 0 {
						allDiffStrings = append(allDiffStrings, fmt.Sprintf("Extra mocks: %s", strings.Join(extraMocks, ", ")))
					}
				}
				if len(allDiffStrings) > 0 {
					combinedDiffText = strings.Join(allDiffStrings, " | ")
				} else {
					combinedDiffText = "No differences"
				}
			}

			if !testSetPrinted {
				table.Append([]string{testSetID, testID, combinedDiffText})
				testSetPrinted = true
			} else {
				table.Append([]string{"", testID, combinedDiffText})
			}
		}
	}

	table.Render()
}

// preserveRecordedRequestTiming ensures that streaming/SSE test cases are
// replayed with the same relative timing gaps they had during recording.
//
// Problem: In streaming scenarios (e.g. SSE pub/sub), a subscriber must connect
// before the publisher sends events. If we replay all tests instantly, the
// ordering breaks. This function preserves the original recorded delays.
//
// How it works — time-mapping via two reference points:
//
//   - recordedStreamStartTime: the recorded timestamp of the first test case in a
//     timing-sensitive sequence (the origin in "recording time").
//   - replayedStreamStartTime: the real wall-clock time when that first test case was
//     actually replayed (the origin in "replay time").
//
// For each subsequent test case, we compute how far its recorded timestamp is
// from the first one, then sleep until the same offset has elapsed in real time.
//
// Example:
//
//	test-1 recorded at T=10:00:00  →  replayed at T=14:30:00  (anchors set)
//	test-2 recorded at T=10:00:02  →  should replay at 14:30:00 + 2s = 14:30:02
//
// Parameters:
//   - preserveTiming: if false, resets the reference points so normal (non-streaming)
//     tests run back-to-back without artificial delays.
//   - recordedStreamStartTime: pointer to the recorded timestamp reference (mutated on first call).
//   - replayedStreamStartTime: pointer to the wall-clock reference (mutated on first call).
func (r *Replayer) preserveRecordedRequestTiming(
	ctx context.Context,
	testCase *models.TestCase,
	preserveTiming bool,
	recordedStreamStartTime *time.Time,
	replayedStreamStartTime *time.Time,
) error {
	if !preserveTiming {
		// Not in a streaming-sensitive path — reset the reference points so synchronous
		// tests don't inherit recorded wall-clock gaps from a prior sequence.
		*recordedStreamStartTime = time.Time{}
		*replayedStreamStartTime = time.Time{}
		return nil
	}

	currentRecordedTS := testCaseRequestTimestamp(testCase)
	if currentRecordedTS.IsZero() {
		return nil
	}

	// First streaming test case: establish the time-mapping reference points.
	if recordedStreamStartTime.IsZero() {
		*recordedStreamStartTime = currentRecordedTS
		*replayedStreamStartTime = time.Now()
		return nil
	}

	// Compute when this test case should fire in real time:
	// wall-clock target = replayedStreamStartTime + (currentRecordedTS - recordedStreamStartTime)
	offsetSinceFirst := currentRecordedTS.Sub(*recordedStreamStartTime)
	targetReplayTime := replayedStreamStartTime.Add(offsetSinceFirst)
	delay := time.Until(targetReplayTime)

	if delay <= 0 {
		// We're already past the target time — no need to wait.
		return nil
	}

	r.logger.Debug("delaying test to preserve recorded inter-request timing",
		zap.String("testcase", testCase.Name),
		zap.Duration("delay", delay))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
