package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olekukonko/tablewriter"
	"go.uber.org/zap"

	// "encoding/json"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
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

// generateProducerCombinations recursively builds all possible valid combinations.
func (r *Replayer) generateProducerCombinations(consumers []TemplateCandidate) []combination {
	var allCombinations []combination
	var backtrack func(consumerIndex int, currentCombination combination)

	backtrack = func(consumerIndex int, currentCombination combination) {
		// Base case: we have made a choice for every consumer
		if consumerIndex == len(consumers) {
			comboCopy := make(combination, len(currentCombination))
			for k, v := range currentCombination {
				comboCopy[k] = v
			}
			allCombinations = append(allCombinations, comboCopy)
			return
		}

		consumer := consumers[consumerIndex]

		// Option 1: Don't templatize this consumer
		backtrack(consumerIndex+1, currentCombination)

		// Option 2: Try each potential producer for this consumer
		for _, producer := range consumer.Options {
			currentCombination[consumer.ConsumerPath] = producer
			backtrack(consumerIndex+1, currentCombination)
			delete(currentCombination, consumer.ConsumerPath) // Backtrack
		}
	}

	backtrack(0, make(combination))
	return allCombinations
}

// findPassingCombination loops through combinations, executes them, and returns the first one that passes.
// **This is the new, simplified version.**
func (r *Replayer) findPassingCombination(ctx context.Context, testSetID string, tc *models.TestCase, allCombinations []combination) (combination, bool, *models.Result, interface{}, error) {

	// 1. Back up the *current* global template state.
	// (This contains confirmed variables from previous test cases)
	originalGlobalTemplates := make(map[string]interface{}, len(utils.TemplatizedValues))
	for k, v := range utils.TemplatizedValues {
		originalGlobalTemplates[k] = v
	}

	// 2. Defer restoring the global state. This ensures that after this
	//    function, the global map is in the *exact* state it was before.
	defer func() {
		utils.TemplatizedValues = originalGlobalTemplates
	}()

	for _, combo := range allCombinations {
		// 3. Create the temporary template data for *this* run.
		//    Start with the confirmed templates from *previous* tests.
		tempTemplateData := make(map[string]interface{})
		for k, v := range originalGlobalTemplates {
			tempTemplateData[k] = v
		}

		// 4. Add the *new* candidate values for this specific combination.
		for _, producer := range combo {
			tempTemplateData[producer.TemplateName] = producer.ProducerValue
		}

		// 5. **Set the global map.** This is what SimulateHTTP will read.
		utils.TemplatizedValues = tempTemplateData

		// 6. Call SimulateRequest. It will now use our 'tempTemplateData'
		//    when it renders internally. We pass the *original* tc.
		resp, loopErr := HookImpl.SimulateRequest(ctx, tc, testSetID)
		if loopErr != nil {
			r.logger.Warn("Execution failed for combination", zap.Error(loopErr), zap.String("tc", tc.Name))
			continue // Try next combination
		}

		// 7. Compare the response.
		var testPass bool
		var testResult *models.Result

		switch tc.Kind {
		case models.HTTP:
			httpResp, ok := resp.(*models.HTTPResp)
			if !ok {
				r.logger.Error("Invalid response type for HTTP test case", zap.String("tc", tc.Name))
				continue
			}
			testPass, testResult = r.CompareHTTPResp(tc, httpResp, testSetID)
		case models.GRPC_EXPORT:
			grpcResp, ok := resp.(*models.GrpcResp)
			if !ok {
				r.logger.Error("Invalid response type for gRPC test case", zap.String("tc", tc.Name))
				continue
			}
			// ... (proto conversion logic as in your original RunTestSet) ...
			respCopy := *grpcResp
			// ...
			testPass, testResult = r.CompareGRPCResp(tc, &respCopy, testSetID)
		}

		if testPass {
			// Found a winner!
			// **Crucially, we restore the global map** to what it was *before*
			// this function was called. The caller (`RunTestSet`) will then
			// call `applyPermanentTemplates` which *updates* that map.
			return combo, true, testResult, resp, nil
		}

		// If not pass, the loop continues. `utils.TemplatizedValues` will be
		// overwritten by the next combination.
	}

	// No combination passed. The deferred function will restore the map.
	return nil, false, nil, nil, nil
}

// applyPermanentTemplates modifies the *original* TC and updates the final template map
func (r *Replayer) applyPermanentTemplates(tc *models.TestCase, reqBody interface{}, combo combination, finalTemplates map[string]interface{}) {

	for consumerPath, producer := range combo {
		// Find a unique key
		finalKey := producer.TemplateName
		i := 1
		for {
			existingVal, exists := finalTemplates[finalKey]
			if !exists {
				break // Found a free key
			}
			if existingVal == producer.ProducerValue {
				break // Same key, same value, reuse it
			}
			finalKey = fmt.Sprintf("%s%d", producer.TemplateName, i)
			i++
		}

		// 1. Update the main template map (conf.Template)
		finalTemplates[finalKey] = producer.ProducerValue

		// 2. Update the global map for the *next* test case in this run
		utils.TemplatizedValues[finalKey] = producer.ProducerValue

		// 3. Apply the template string to the *in-memory* test case object
		templateStr := fmt.Sprintf("{{%s .%s}}", producer.OriginalType, finalKey)

		if tc.Kind == models.HTTP {
			// We need to find the *original* location info.
			// This is a bit tricky. We should get this from the tools.go analysis.
			// For now, I'll use the path string, but this should be more robust.
			// (The `tools.go` should really save the `ValueLocation` struct)

			if strings.HasPrefix(consumerPath, "body.") {
				setValueByPath(&reqBody, strings.TrimPrefix(consumerPath, "body."), templateStr)
			} else if strings.HasPrefix(consumerPath, "path.") {
				reconstructURL(&tc.HTTPReq.URL, consumerPath, templateStr)
			} else if strings.HasPrefix(consumerPath, "Cookie.") {
				name := strings.TrimPrefix(consumerPath, "Cookie.")
				tc.HTTPReq.Header["Cookie"] = replaceCookieValue(tc.HTTPReq.Header["Cookie"], name, templateStr)
			} else {
				// Assume it's a normal header
				tc.HTTPReq.Header[consumerPath] = templateStr
			}
		} // TODO: Add gRPC logic here
	}

	// Re-marshal the body back into the test case
	if reqBody != nil && tc.Kind == models.HTTP {
		newBody, _ := json.Marshal(reqBody)
		tc.HTTPReq.Body = string(newBody)
	}
}

// replaceCookieValue replaces only the value of a named cookie in a Cookie header string.
func replaceCookieValue(cookieHdr, name, newVal string) string {
	kvs := parseCookiePairs(cookieHdr)
	if len(kvs) == 0 {
		// if none, create fresh single cookie
		return name + "=" + newVal
	}
	for i := range kvs {
		if kvs[i].Name == name {
			kvs[i].Value = newVal
			break
		}
	}

	var b strings.Builder
	for i, kv := range kvs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(kv.Name)
		b.WriteByte('=')
		b.WriteString(kv.Value)
	}
	return b.String()
}

type cookiePair struct {
	Name  string
	Value string
}

// parseCookiePairs parses a Cookie header (request) of form "a=1; b=2".
func parseCookiePairs(s string) []cookiePair {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]cookiePair, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		out = append(out, cookiePair{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	return out
}

func reconstructURL(urlPtr *string, segmentPath string, template string) {
	parsedURL, err := url.Parse(*urlPtr)
	if err != nil {
		return
	}
	var segmentIndex int
	if _, err := fmt.Sscanf(segmentPath, "path.%d", &segmentIndex); err != nil {
		return
	}
	pathSegments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if segmentIndex < len(pathSegments) {
		pathSegments[segmentIndex] = template
	}
	newPath := "/" + strings.Join(pathSegments, "/")
	reconstructed := fmt.Sprintf("%s://%s%s", parsedURL.Scheme, parsedURL.Host, newPath)
	if parsedURL.RawQuery != "" {
		reconstructed += "?" + parsedURL.RawQuery
	}
	*urlPtr = reconstructed
}

func setValueByPath(root interface{}, path string, value interface{}) {
	parts := strings.Split(path, ".")
	var current interface{} = root
	for i := 0; i < len(parts)-1; i++ {
		key := parts[i]
		if reflect.ValueOf(current).Kind() == reflect.Ptr {
			current = reflect.ValueOf(current).Elem().Interface()
		}
		if m, ok := current.(map[string]interface{}); ok {
			current = m[key]
		} else if s, ok := current.([]interface{}); ok {
			if idx, err := strconv.Atoi(key); err == nil && idx < len(s) {
				current = s[idx]
			} else {
				return
			}
		} else {
			return
		}
	}
	lastKey := parts[len(parts)-1]
	if reflect.ValueOf(current).Kind() == reflect.Ptr {
		current = reflect.ValueOf(current).Elem().Interface()
	}
	if m, ok := current.(map[string]interface{}); ok {
		m[lastKey] = value
	} else if s, ok := current.([]interface{}); ok {
		if idx, err := strconv.Atoi(lastKey); err == nil && idx < len(s) {
			s[idx] = value
		}
	}
}
