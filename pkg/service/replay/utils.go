package replay

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"

	// "encoding/json"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
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

// effectiveStreamMockWindow calculates the effective time window for streaming mocks.
// It returns the start time (request timestamp) and end time (anchor + timeout),
// where anchor is the later of request/response timestamps (falling back to time.Now).
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

// retainNoisyTestCaseMocks injects mocks used by noisy test cases into the
// consumed-mocks set used for pruning so those mocks are not deleted.
func retainNoisyTestCaseMocks(noisyTestCaseNames []string, mapping *models.Mapping, consumed map[string]models.MockState) int {
	if len(noisyTestCaseNames) == 0 || mapping == nil || len(mapping.TestCases) == 0 || consumed == nil {
		return 0
	}

	noisySet := make(map[string]struct{}, len(noisyTestCaseNames))
	for _, testCaseName := range noisyTestCaseNames {
		if testCaseName == "" {
			continue
		}
		noisySet[testCaseName] = struct{}{}
	}
	if len(noisySet) == 0 {
		return 0
	}

	added := 0
	for _, mappedTestCase := range mapping.TestCases {
		if _, ok := noisySet[mappedTestCase.ID]; !ok {
			continue
		}

		for _, mock := range mappedTestCase.Mocks {
			if mock.Name == "" {
				continue
			}
			if _, exists := consumed[mock.Name]; exists {
				continue
			}

			consumed[mock.Name] = models.MockState{
				Name:      mock.Name,
				Kind:      models.Kind(mock.Kind),
				Timestamp: mock.Timestamp,
			}
			added++
		}
	}

	return added
}

func isMockSubsetWithConfig(consumedMocks []models.MockState, expectedMocks []string) bool {
	expectedMap := make(map[string]bool)
	for _, name := range expectedMocks {
		expectedMap[name] = true
	}

	for _, m := range consumedMocks {
		if !expectedMap[m.Name] {
			// Found an extra mock in the actual run
			if m.Type != "config" {
				// This is NOT a config mock, so it IS a mismatch
				return false
			}
			// If Type is "config", we ignore it as an extra mock
		}
	}
	return true
}

// recordReqResTimestamps returns the RECORD-TIME request and response
// timestamps for a test case, regardless of kind (HTTP vs gRPC). The fallback
// to tc.Created covers very old fixtures that only carry the coarse creation
// timestamp. Either value may be zero when the recording did not populate it.
func recordReqResTimestamps(tc *models.TestCase) (time.Time, time.Time) {
	if tc == nil {
		return time.Time{}, time.Time{}
	}
	var req, resp time.Time
	if !tc.HTTPReq.Timestamp.IsZero() {
		req = tc.HTTPReq.Timestamp
	} else if !tc.GrpcReq.Timestamp.IsZero() {
		req = tc.GrpcReq.Timestamp
	}
	if !tc.HTTPResp.Timestamp.IsZero() {
		resp = tc.HTTPResp.Timestamp
	} else if !tc.GrpcResp.Timestamp.IsZero() {
		resp = tc.GrpcResp.Timestamp
	}
	if req.IsZero() && tc.Created > 0 {
		req = time.Unix(tc.Created, 0)
	}
	if resp.IsZero() && !req.IsZero() {
		// When the recording has no response timestamp, fall back to
		// the request timestamp plus a 1-second grace. That covers the
		// common case where the app responds quickly and the missing
		// resp timestamp comes from a fixture gap rather than a long
		// request. Longer-running requests will simply lose some
		// trailing mocks from the window — still deterministic.
		resp = req.Add(time.Second)
	}
	return req, resp
}

// upsertActualTestMockMapping updates the actual test-to-mock mappings with the mocks
// consumed during the replay of a specific test case.
//
// tcReq / tcResp are the RECORD-TIME request/response timestamps of the test case.
// When both are non-zero, consumed mocks are filtered by their recorded
// ReqTimestampMock / ResTimestampMock falling inside [tcReq, tcResp]. This keeps
// mapping assignment deterministic on the record data instead of tracking
// replay-time consumption order, which can drift when a dependency reconnects
// across tests (e.g. a Redis client re-handshaking during a later test's
// window would otherwise attribute the startup-time HELLO mock to the wrong
// test case). When both timestamps are zero, filtering is skipped and the
// legacy append-all behavior is used.
func upsertActualTestMockMapping(actualTestMockMappings *models.Mapping, testCaseID string, consumedMocks []models.MockState, tcReq, tcResp time.Time) {
	if actualTestMockMappings == nil || testCaseID == "" || len(consumedMocks) == 0 {
		return
	}

	filter := !tcReq.IsZero() && !tcResp.IsZero() && !tcResp.Before(tcReq)

	newMocks := make([]models.MockEntry, 0, len(consumedMocks))
	for _, m := range consumedMocks {
		timestamp := m.Timestamp
		var parsedReqTime, parsedResTime time.Time
		if m.ReqTimestampMock != "" {
			if t, err := time.Parse(time.RFC3339Nano, m.ReqTimestampMock); err == nil {
				parsedReqTime = t
				timestamp = t.Unix()
			}
		}
		if m.ResTimestampMock != "" {
			if t, err := time.Parse(time.RFC3339Nano, m.ResTimestampMock); err == nil {
				parsedResTime = t
			}
		}

		if filter {
			// Keep the mock only if its recorded request or response
			// timestamp overlaps the test case's recorded window.
			// Mocks recorded strictly before the test case's request
			// or strictly after its response belong to a different
			// test (or to session-level traffic that should not be
			// per-test). DNS mocks have no stable timestamps and are
			// intentionally never filtered out here.
			if strings.EqualFold(string(m.Kind), string(models.DNS)) {
				// DNS: always keep.
			} else {
				reqInWindow := !parsedReqTime.IsZero() && !parsedReqTime.Before(tcReq) && !parsedReqTime.After(tcResp)
				resInWindow := !parsedResTime.IsZero() && !parsedResTime.Before(tcReq) && !parsedResTime.After(tcResp)
				hasAnyTimestamp := !parsedReqTime.IsZero() || !parsedResTime.IsZero()
				// If we have ANY record timestamp and none of them
				// fall in the test's window, drop it. If the mock has
				// no record timestamp at all, we can't tell — keep it
				// to preserve the legacy behavior for that edge case.
				if hasAnyTimestamp && !reqInWindow && !resInWindow {
					continue
				}
			}
		}

		newMocks = append(newMocks, models.MockEntry{
			Name:             m.Name,
			Kind:             string(m.Kind),
			Timestamp:        timestamp,
			ReqTimestampMock: m.ReqTimestampMock,
			ResTimestampMock: m.ResTimestampMock,
		})
	}

	if len(newMocks) == 0 {
		return
	}

	// If the test case already has an entry, append the new mocks to it.
	for i := range actualTestMockMappings.TestCases {
		if actualTestMockMappings.TestCases[i].ID == testCaseID {
			actualTestMockMappings.TestCases[i].Mocks = append(actualTestMockMappings.TestCases[i].Mocks, newMocks...)
			return
		}
	}

	// No existing entry — create a new one.
	actualTestMockMappings.TestCases = append(actualTestMockMappings.TestCases, models.MappedTestCase{
		ID:    testCaseID,
		Mocks: newMocks,
	})
}

type TestFailure struct {
	TestSetID      string
	TestID         string
	ExpectedMocks  []string
	ActualMocks    []string
	FailureReason  models.ParserErrorType
	MismatchReport *models.MockMismatchReport
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
		TestSetID:      testSetID,
		TestID:         testCaseID,
		ExpectedMocks:  []string{},
		ActualMocks:    []string{},
		FailureReason:  proxyErr.ParserErrorType,
		MismatchReport: proxyErr.MismatchReport,
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

// GetFailuresForTestCase returns failures for a specific test set + test case.
func (tfs *TestFailureStore) GetFailuresForTestCase(testSetID, testCaseID string) []TestFailure {
	tfs.mu.Lock()
	defer tfs.mu.Unlock()

	var result []TestFailure
	for _, f := range tfs.failures {
		if f.TestSetID == testSetID && f.TestID == testCaseID {
			result = append(result, f)
		}
	}
	return result
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

	colWidths := tw.NewMapper[int, int]().Set(0, 15).Set(1, 12).Set(2, 50)
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithRendition(tw.Rendition{
			Settings: tw.Settings{
				Separators: tw.Separators{
					BetweenRows: tw.On,
				},
			},
		}),
		tablewriter.WithRowAutoWrap(1),
		tablewriter.WithHeaderAlignment(tw.AlignCenter),
		tablewriter.WithRowAlignment(tw.AlignCenter),
		tablewriter.WithMaxWidth(120),
		tablewriter.WithColumnWidths(colWidths),
	)
	table.Header([]string{"TEST SET", "TEST ID", "MOCK DIFFERENCES"})

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
					if failure.MismatchReport != nil {
						r := failure.MismatchReport
						detail := fmt.Sprintf("[%s] %s", r.Protocol, r.ActualSummary)
						if r.ClosestMock != "" {
							detail += fmt.Sprintf(" | closest: %s", r.ClosestMock)
						}
						if r.Diff != "" {
							detail += fmt.Sprintf(" | %s", strings.ReplaceAll(r.Diff, "\n", " "))
						}
						if r.NextSteps != "" {
							detail += fmt.Sprintf(" | hint: %s", strings.ReplaceAll(r.NextSteps, "\n", " "))
						}
						allDiffStrings = append(allDiffStrings, detail)
					} else {
						allDiffStrings = append(allDiffStrings, "Outgoing call mock was not matched")
					}
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
