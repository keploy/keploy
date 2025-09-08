package replay

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	// "encoding/json"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
)

type TestReportVerdict struct {
	total     int
	passed    int
	failed    int
	ignored   int
	status    bool
	duration  time.Duration
	timeTaken string
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := globalNoise

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

func compareMockArrays(arr1, arr2 []string) bool {
	counts1 := make(map[string]int)
	counts2 := make(map[string]int)

	for _, mock := range arr1 {
		counts1[mock]++
	}

	for _, mock := range arr2 {
		counts2[mock]++
	}

	if len(counts1) != len(counts2) {
		return false
	}

	for mock, count := range counts1 {
		if counts2[mock] != count {
			return false
		}
	}

	return true
}

type TestFailure struct {
	TestSetID     string
	TestID        string
	ExpectedMocks []string
	ActualMocks   []string
	FailureReason string
}

type TestFailureStore struct {
	failures []TestFailure
}

func NewTestFailureStore() *TestFailureStore {
	return &TestFailureStore{
		failures: make([]TestFailure, 0),
	}
}

func (tfs *TestFailureStore) AddFailure(testSetID, testID string, expectedMocks, actualMocks []string) {
	failure := TestFailure{
		TestSetID:     testSetID,
		TestID:        testID,
		ExpectedMocks: expectedMocks,
		ActualMocks:   actualMocks,
	}
	tfs.failures = append(tfs.failures, failure)
}

func (tfs *TestFailureStore) GetFailures() []TestFailure {
	return tfs.failures
}

func (tfs *TestFailureStore) GetFailuresByTestSet(testSetID string) []TestFailure {
	var filtered []TestFailure
	for _, failure := range tfs.failures {
		if failure.TestSetID == testSetID {
			filtered = append(filtered, failure)
		}
	}
	return filtered
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

		for _, failure := range failures {
			differences := CompareMockSlices(failure.ExpectedMocks, failure.ActualMocks)

			var missingMocks []string
			var extraMocks []string

			for _, diff := range differences {
				switch diff.DiffType {
				case "missing":
					missingMocks = append(missingMocks, diff.Key)
				case "extra":
					extraMocks = append(extraMocks, diff.Key)
				}
			}

			var diffStrings []string
			if len(missingMocks) > 0 {
				diffStrings = append(diffStrings, fmt.Sprintf("Missing: %s", strings.Join(missingMocks, ", ")))
			}
			if len(extraMocks) > 0 {
				diffStrings = append(diffStrings, fmt.Sprintf("Extra: %s", strings.Join(extraMocks, ", ")))
			}

			diffText := strings.Join(diffStrings, "; ")
			if diffText == "" {
				diffText = "No differences"
			}

			if !testSetPrinted {
				table.Append([]string{testSetID, failure.TestID, diffText})
				testSetPrinted = true
			} else {
				table.Append([]string{"", failure.TestID, diffText})
			}
		}
	}

	table.Render()
}
