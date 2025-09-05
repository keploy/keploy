package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/invopop/yaml"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// --- V2 Data Structures for Optimized Templatization ---

type PartType int

const (
	RequestHeader PartType = iota
	RequestURL
	RequestBody
	ResponseHeader
	ResponseBody
)

func (pt PartType) String() string {
	return [...]string{"Request Header", "Request URL", "Request Body", "Response Header", "Response Body"}[pt]
}

type ValueLocation struct {
	TestCaseIndex int
	Part          PartType
	Path          string
	Pointer       interface{}
	OriginalType  string
}

type TemplateChain struct {
	TemplateKey string
	Value       string
	Producer    *ValueLocation
	Consumers   []*ValueLocation
}

// --- Canonical Structs for Assertion ---

type CanonicalChain struct {
	VariableName string              `yaml:"variable_name"`
	Value        string              `yaml:"value"`
	Producer     CanonicalProducer   `yaml:"producer"`
	Consumers    []CanonicalConsumer `yaml:"consumers"`
}

type CanonicalProducer struct {
	RequestID string `yaml:"request_id"`
	Part      string `yaml:"part"`
	Path      string `yaml:"path"`
}

type CanonicalConsumer struct {
	RequestID string `yaml:"request_id"`
	Part      string `yaml:"part"`
	Path      string `yaml:"path"`
}

// ProcessTestCasesV2 performs templatization on the provided test cases using an optimized approach.
func (t *Tools) ProcessTestCasesV2(ctx context.Context, tcs []*models.TestCase, testSetID string) error {
	for _, tc := range tcs {
		tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
	}

	reqBodies := make([]interface{}, len(tcs))
	respBodies := make([]interface{}, len(tcs))
	for i, tc := range tcs {
		decoderReq := json.NewDecoder(strings.NewReader(tc.HTTPReq.Body))
		decoderReq.UseNumber()
		_ = decoderReq.Decode(&reqBodies[i])
		decoderResp := json.NewDecoder(strings.NewReader(tc.HTTPResp.Body))
		decoderResp.UseNumber()
		_ = decoderResp.Decode(&respBodies[i])
	}

	valueIndex := t.buildValueIndexV2(ctx, tcs, reqBodies, respBodies)
	chains := t.applyTemplatesFromIndexV2(ctx, valueIndex, utils.TemplatizedValues)

	for i, tc := range tcs {
		if reqBodies[i] != nil {
			newBody, _ := json.Marshal(reqBodies[i])
			tc.HTTPReq.Body = string(newBody)
		}
		if respBodies[i] != nil {
			newBody, _ := json.Marshal(respBodies[i])
			tc.HTTPResp.Body = string(newBody)
		}
		tc.HTTPReq.Body = removeQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = removeQuotesInTemplates(tc.HTTPResp.Body)
		if err := t.testDB.UpdateTestCase(ctx, tc, testSetID, false); err != nil {
			utils.LogError(t.logger, err, "failed to update test case")
			return err
		}
	}

	utils.RemoveDoubleQuotes(utils.TemplatizedValues)

	var existingMetadata map[string]interface{}
	existingTestSet, err := t.testSetConf.Read(ctx, testSetID)
	if err == nil && existingTestSet != nil && existingTestSet.Metadata != nil {
		existingMetadata = existingTestSet.Metadata
	}

	err = t.testSetConf.Write(ctx, testSetID, &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   utils.TemplatizedValues,
		Metadata:   existingMetadata,
	})
	if err != nil {
		utils.LogError(t.logger, err, "failed to write test set")
		return err
	}

	if len(utils.SecretValues) > 0 {
		err = utils.AddToGitIgnore(t.logger, t.config.Path, "/*/secret.yaml")
		if err != nil {
			t.logger.Warn("Failed to add secret files to .gitignore", zap.Error(err))
		}
	}

	t.logAPIChains(chains, tcs)

	if fuzzerYamlPath := os.Getenv("ASSERT_CHAINS_WITH"); fuzzerYamlPath != "" {
		fmt.Println("Asserting chains with fuzzer YAML:", fuzzerYamlPath)
		t.AssertChains(chains, tcs, fuzzerYamlPath)
	}
	return nil
}

// logAPIChains prints a human-readable representation of the detected template chains.
func (t *Tools) logAPIChains(chains []*TemplateChain, testCases []*models.TestCase) {
	if len(chains) == 0 {
		return
	}
	fmt.Println("\n✨ API Chain Analysis ✨")
	fmt.Println("========================")
	for i, chain := range chains {
		if i > 0 {
			fmt.Println("--------------------")
		}
		truncatedValue := chain.Value
		if len(truncatedValue) > 50 {
			truncatedValue = truncatedValue[:47] + "..."

		}
		fmt.Printf("🔗 Chain for {{.%s}} (value: \"%s\")\n", chain.TemplateKey, truncatedValue)
		fmt.Printf("  [PRODUCER] %s\n", formatLocation(chain.Producer, testCases))
		for j, consumer := range chain.Consumers {
			branch := "├─>"
			if j == len(chain.Consumers)-1 {
				branch = "    └─>"
			}
			fmt.Printf("    %s [CONSUMER] %s\n", branch, formatLocation(consumer, testCases))
		}
	}
	fmt.Println("========================")
}

// UpdateTemplateValues updates the global template values map with values from the HTTP response.
func formatLocation(loc *ValueLocation, testCases []*models.TestCase) string {
	if loc == nil || loc.TestCaseIndex >= len(testCases) {
		return "unknown location"
	}
	testCaseName := testCases[loc.TestCaseIndex].Name
	switch loc.Part {
	case RequestHeader:
		return fmt.Sprintf("%s (%s '%s')", testCaseName, loc.Part, loc.Path)
	case ResponseBody, RequestBody:
		return fmt.Sprintf("%s (%s at '%s')", testCaseName, loc.Part, loc.Path)
	case RequestURL:
		return fmt.Sprintf("%s (%s)", testCaseName, loc.Part)
	default:
		return fmt.Sprintf("%s (%s)", testCaseName, loc.Part)
	}
}

// buildValueIndexV2 constructs an index of observed values to their locations in test cases.
func (t *Tools) buildValueIndexV2(ctx context.Context, tcs []*models.TestCase, reqBodies, respBodies []interface{}) map[string][]*ValueLocation {
	// Build an inverted index mapping observed values -> list of locations
	// where that value occurs across all testcases. Locations include
	// request headers, request URL path segments, request bodies and
	// response bodies. This index is used later to detect producer ->
	// consumer chains.
	valueIndex := make(map[string][]*ValueLocation)
	for i := range tcs {
		tc := tcs[i]
		// collect header values
		t.addHeaderValuesToIndex(tc, i, valueIndex)

		// collect URL path segment values
		t.addURLSegmentsToIndex(tc, i, valueIndex)

		// collect values from parsed JSON bodies (request and response)
		if reqBodies[i] != nil {
			findValuesInInterface(reqBodies[i], []string{}, valueIndex, i, RequestBody, &reqBodies[i])
		}
		if respBodies[i] != nil {
			findValuesInInterface(respBodies[i], []string{}, valueIndex, i, ResponseBody, &respBodies[i])
		}
	}
	return valueIndex
}

// addHeaderValuesToIndex extracts header values and indexes them. For
// Authorization headers we special-case Bearer tokens and index the token
// value instead of the full header value.
func (t *Tools) addHeaderValuesToIndex(tc *models.TestCase, tcIndex int, index map[string][]*ValueLocation) {
	for k, val := range tc.HTTPReq.Header {
		loc := &ValueLocation{TestCaseIndex: tcIndex, Part: RequestHeader, Path: k, Pointer: &tc.HTTPReq.Header, OriginalType: "string"}
		if k == "Authorization" && strings.HasPrefix(val, "Bearer ") {
			token := strings.TrimPrefix(val, "Bearer ")
			index[token] = append(index[token], loc)
		} else {
			index[val] = append(index[val], loc)
		}
	}
}

// addURLSegmentsToIndex parses the request URL and indexes each non-empty
// segment as a potential value. Each segment is given a path like
// "path.0", "path.1" which is later used to reconstruct templated URLs.
func (t *Tools) addURLSegmentsToIndex(tc *models.TestCase, tcIndex int, index map[string][]*ValueLocation) {
	parsedURL, err := url.Parse(tc.HTTPReq.URL)
	if err != nil {
		return
	}
	pathSegments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	for j, segment := range pathSegments {
		if segment != "" {
			path := fmt.Sprintf("path.%d", j)
			loc := &ValueLocation{TestCaseIndex: tcIndex, Part: RequestURL, Path: path, Pointer: &tc.HTTPReq.URL, OriginalType: "string"}
			index[segment] = append(index[segment], loc)
		}
	}
}

// findValuesInInterface recursively traverses a JSON-like structure (maps, slices)
func findValuesInInterface(data interface{}, path []string, index map[string][]*ValueLocation, tcIndex int, part PartType, containerPtr interface{}) {
	if data == nil {
		return
	}
	if m, ok := data.(map[string]interface{}); ok {
		for k, v := range m {
			newPath := append(path, k)
			findValuesInInterface(v, newPath, index, tcIndex, part, containerPtr)
		}

		return
	}
	if s, ok := data.([]interface{}); ok {
		for i, v := range s {
			newPath := append(path, strconv.Itoa(i))
			findValuesInInterface(v, newPath, index, tcIndex, part, containerPtr)

		}
		return
	}

	currentPath := strings.Join(path, ".")
	switch v := data.(type) {
	case string:
		if len(v) > 2 {
			loc := &ValueLocation{TestCaseIndex: tcIndex, Part: part, Path: currentPath, Pointer: containerPtr, OriginalType: "string"}
			index[v] = append(index[v], loc)
		}
	case json.Number:
		loc := &ValueLocation{TestCaseIndex: tcIndex, Part: part, Path: currentPath, Pointer: containerPtr}
		if strings.Contains(v.String(), ".") {
			loc.OriginalType = "float"
		} else {
			loc.OriginalType = "int"

		}
		index[v.String()] = append(index[v.String()], loc)
	}
}

// applyTemplatesFromIndexV2 applies templates based on the value index and updates the global template values map.
func (t *Tools) applyTemplatesFromIndexV2(ctx context.Context, index map[string][]*ValueLocation, templateConfig map[string]interface{}) []*TemplateChain {
	var chains []*TemplateChain
	for value, locations := range index {
		// We need at least two occurrences to form a producer -> consumer chain
		if len(locations) < 2 {
			continue
		}

		// Sort locations by testcase index so we reliably find a producer
		sort.Slice(locations, func(i, j int) bool { return locations[i].TestCaseIndex < locations[j].TestCaseIndex })

		// The producer is assumed to be the first occurrence in a response
		// (response body or response header). If none exist, skip.
		producer := selectProducer(locations)
		if producer == nil {
			continue
		}

		// Consumers are request-side occurrences that happen after the producer
		var subsequentConsumers []*ValueLocation
		for _, loc := range locations {
			if (loc.Part == RequestHeader || loc.Part == RequestURL || loc.Part == RequestBody) && loc.TestCaseIndex > producer.TestCaseIndex {
				subsequentConsumers = append(subsequentConsumers, loc)
			}
		}
		if len(subsequentConsumers) == 0 {
			continue
		}

		// We will templatize all producer occurrences and any consumer
		// occurrences that happen at or after the producer's test index.
		allOccurrencesToTemplatize := gatherAllOccurrencesToTemplatize(locations, producer)

		chain := &TemplateChain{
			TemplateKey: "", // generated next
			Value:       value,
			Producer:    producer,
			Consumers:   subsequentConsumers,
		}

		// Build a base key for the template name from the producer context
		producerType := producer.OriginalType
		baseKey := baseKeyFromProducer(producer, value)

		templateKey := insertUnique(baseKey, value, templateConfig)
		chain.TemplateKey = templateKey
		chains = append(chains, chain)

		// Compose the template string and apply it to every occurrence
		templateString := fmt.Sprintf("{{%s .%s}}", producerType, templateKey)
		for _, loc := range allOccurrencesToTemplatize {
			applyTemplateToLocation(templateString, loc)
		}
	}
	return chains
}

// selectProducer picks the first response occurrence from locations.
func selectProducer(locations []*ValueLocation) *ValueLocation {
	for _, loc := range locations {
		if loc.Part == ResponseBody || loc.Part == ResponseHeader {
			return loc
		}
	}
	return nil
}

// gatherAllOccurrencesToTemplatize returns every location which is either a
// producer or a consumer (request-side) occurring at or after the producer.
func gatherAllOccurrencesToTemplatize(locations []*ValueLocation, producer *ValueLocation) []*ValueLocation {
	var out []*ValueLocation
	for _, loc := range locations {
		isProducer := loc.Part == ResponseBody || loc.Part == ResponseHeader
		isConsumer := (loc.Part == RequestHeader || loc.Part == RequestURL || loc.Part == RequestBody) && loc.TestCaseIndex >= producer.TestCaseIndex
		if isProducer || isConsumer {
			out = append(out, loc)
		}
	}
	return out
}

// baseKeyFromProducer derives a sensible base key for template naming from
// the producer location. It mirrors the original code's heuristics for
// array indices and parent names.
func baseKeyFromProducer(producer *ValueLocation, value string) string {
	if producer.Part == RequestURL {
		return value
	}
	baseKey := producer.Path
	parts := strings.Split(baseKey, ".")
	if len(parts) > 0 {
		baseKey = parts[len(parts)-1]
	}
	if _, err := strconv.Atoi(baseKey); err == nil {
		partsFull := strings.Split(producer.Path, ".")
		parent := "arr"
		if len(partsFull) >= 2 {
			parent = partsFull[len(partsFull)-2]
			for i := len(partsFull) - 2; i >= 0; i-- {
				if _, numErr := strconv.Atoi(partsFull[i]); numErr != nil {
					parent = partsFull[i]
					break
				}
			}
		}
		baseKey = fmt.Sprintf("%s_ix_%s", sanitizeKey(parent), baseKey)
	}
	return baseKey
}

// applyTemplateToLocation updates the underlying container referenced by loc
// to replace the matched value with the provided template string. Different
// handling is required for headers, URL segments and JSON body values.
func applyTemplateToLocation(templateString string, loc *ValueLocation) {
	if loc.Part == RequestHeader {
		if headerMap, ok := loc.Pointer.(*map[string]string); ok {
			originalHeaderValue := (*headerMap)[loc.Path]
			if loc.Path == "Authorization" && strings.HasPrefix(originalHeaderValue, "Bearer ") {
				(*headerMap)[loc.Path] = "Bearer " + templateString
			} else {
				(*headerMap)[loc.Path] = templateString
			}
		}
	} else if loc.Part == RequestURL {
		if urlPtr, ok := loc.Pointer.(*string); ok {
			reconstructURL(urlPtr, loc.Path, templateString)
		}
	} else {
		setValueByPath(loc.Pointer, loc.Path, templateString)
	}
}

// In your tools package (tools.go)
// REPLACE the AssertChains function and ADD the new buildCanonicalChainsFromMap helper.
// AssertChains is the main entry point for the verification process.
func (t *Tools) AssertChains(keployChains []*TemplateChain, testCases []*models.TestCase, fuzzerYamlPath string) {
	fmt.Println("\n🔎 Chain Assertion against Fuzzer Output")
	fmt.Println("==========================================")

	// 1. Load fuzzer's baseline chains from the YAML file.
	yamlFile, err := os.ReadFile(fuzzerYamlPath)

	if err != nil {
		fmt.Printf("🔴 ERROR: Could not read fuzzer's chain file at %s: %v\n", fuzzerYamlPath, err)
		return
	}

	// Use a generic map to bypass struct tag parsing issues.
	var genericFuzzerData map[string]interface{}
	if err := yaml.Unmarshal(yamlFile, &genericFuzzerData); err != nil {
		fmt.Printf("🔴 ERROR: Could not parse fuzzer's YAML file into a generic map: %v\n", err)
		return
	}

	// Manually build the canonical chains from the generic map. This is the FIX.
	fuzzerChains, err := buildCanonicalChainsFromMap(genericFuzzerData)
	if err != nil {
		fmt.Printf("🔴 ERROR: Could not process the parsed fuzzer data: %v\n", err)
		return
	}
	fmt.Printf("✅ Loaded %d chains from fuzzer baseline file.\n", len(fuzzerChains))

	// 2. Convert and Normalize Keploy's detected chains.
	canonicalKeployChains := t.convertToCanonical(keployChains, testCases)
	normalizeCanonicalChains(canonicalKeployChains)
	fmt.Printf("✅ Converted and Normalized %d detected Keploy chains for comparison.\n", len(canonicalKeployChains))

	// 3. Perform the comparison.
	passed, report := t.compareChainSets(fuzzerChains, canonicalKeployChains)

	// 4. Print the result.
	fmt.Println("\n--- Comparison Report ---")
	fmt.Print(report)
	if passed {
		fmt.Println("\n✅ PASSED: Keploy's detected chains match the fuzzer's baseline.")
	} else {
		fmt.Println("\n❌ FAILED: Keploy's detected chains DO NOT match the fuzzer's baseline.")
	}
	fmt.Println("==========================================")
}

// buildCanonicalChainsFromMap manually constructs the chain structs from a generic map,
// avoiding any reliance on struct tags which were failing.
func buildCanonicalChainsFromMap(data map[string]interface{}) ([]CanonicalChain, error) {
	var chains []CanonicalChain

	chainsData, ok := data["chains"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("could not find 'chains' array in fuzzer YAML")
	}

	for _, chainInterface := range chainsData {
		chainMap, ok := chainInterface.(map[string]interface{})
		if !ok {
			continue // Skip malformed entries
		}

		var canonicalChain CanonicalChain
		if val, ok := chainMap["variable_name"].(string); ok {
			canonicalChain.VariableName = val
		}
		if val, ok := chainMap["value"].(string); ok {
			canonicalChain.Value = val
		}

		// Manually parse the producer
		if prodInterface, ok := chainMap["producer"].(map[string]interface{}); ok {
			if val, ok := prodInterface["request_id"].(string); ok {
				canonicalChain.Producer.RequestID = val
			}
			if val, ok := prodInterface["part"].(string); ok {
				canonicalChain.Producer.Part = val
			}
			if val, ok := prodInterface["path"].(string); ok {
				canonicalChain.Producer.Path = val
			}
		}

		// Manually parse consumers
		if consumersInterface, ok := chainMap["consumers"].([]interface{}); ok {
			for _, consInterface := range consumersInterface {
				consMap, ok := consInterface.(map[string]interface{})
				if !ok {
					continue
				}
				var consumer CanonicalConsumer
				if val, ok := consMap["request_id"].(string); ok {
					consumer.RequestID = val
				}
				if val, ok := consMap["part"].(string); ok {
					consumer.Part = val
				}
				if val, ok := consMap["path"].(string); ok {
					consumer.Path = val
				}
				canonicalChain.Consumers = append(canonicalChain.Consumers, consumer)
			}
		}
		chains = append(chains, canonicalChain)
	}

	return chains, nil
}

// convertToCanonical transforms Keploy's internal chain representation to the common format.
func (t *Tools) convertToCanonical(chains []*TemplateChain, tcs []*models.TestCase) []CanonicalChain {
	var canonicalChains []CanonicalChain
	for _, chain := range chains {
		producerReqID := fmt.Sprintf("test-%d", chain.Producer.TestCaseIndex+1)
		producer := CanonicalProducer{
			RequestID: producerReqID,
			Part:      chain.Producer.Part.String(),
			Path:      chain.Producer.Path,
		}
		var consumers []CanonicalConsumer
		for _, c := range chain.Consumers {
			consumerReqID := fmt.Sprintf("test-%d", c.TestCaseIndex+1)
			consumers = append(consumers, CanonicalConsumer{
				RequestID: consumerReqID,
				Part:      c.Part.String(),
				Path:      c.Path,
			})
		}
		canonicalChains = append(canonicalChains, CanonicalChain{
			VariableName: "{{" + chain.TemplateKey + "}}",
			Value:        chain.Value,
			Producer:     producer,
			Consumers:    consumers,
		})
	}
	return canonicalChains
}

// normalizeCanonicalChains standardizes the representation of chains in-place.
func normalizeCanonicalChains(chains []CanonicalChain) {
	for i := range chains {
		// Normalize producer
		chains[i].Producer.Part = strings.ReplaceAll(chains[i].Producer.Part, " ", "")

		// Normalize consumers
		for j := range chains[i].Consumers {
			chains[i].Consumers[j].Part = strings.ReplaceAll(chains[i].Consumers[j].Part, " ", "")
			if chains[i].Consumers[j].Part == "RequestURL" {
				// Standardize all specific URL paths (e.g., path.1) to a generic one.
				chains[i].Consumers[j].Path = "URL_PATH"

			}
		}
	}
}

// compareChainSets compares the fuzzer's chains with Keploy's detected chains and generates a report.
func (t *Tools) compareChainSets(fuzzerChains, keployChains []CanonicalChain) (bool, string) {
	var report strings.Builder
	passed := true

	fuzzerChains = filterInsignificantChains(fuzzerChains)
	keployChains = filterInsignificantChains(keployChains)

	fuzzerMap := make(map[string]CanonicalChain)
	for _, c := range fuzzerChains {
		fuzzerMap[c.Value] = c
	}
	keployMap := make(map[string]CanonicalChain)
	for _, c := range keployChains {
		keployMap[c.Value] = c
	}

	normalizeConsumer := func(c CanonicalConsumer) string {
		part := strings.ReplaceAll(c.Part, " ", "")
		path := c.Path
		if part == "RequestURL" {
			path = "URL_PATH"
		}
		return fmt.Sprintf("%s|%s|%s", c.RequestID, part, path)
	}

	// Check 1: Does Keploy find every chain the fuzzer found?
	for value, fChain := range fuzzerMap {
		kChain, exists := keployMap[value]
		if !exists {
			report.WriteString(fmt.Sprintf("❌ MISSING CHAIN: Fuzzer found chain for value '%s', but Keploy did not.\n", value))
			passed = false
			continue
		}

		// --- NEW, MORE ROBUST PRODUCER COMPARISON ---
		producersMatch := false
		// Normalize part names for comparison
		fProducerPart := strings.ReplaceAll(fChain.Producer.Part, " ", "")
		kProducerPart := strings.ReplaceAll(kChain.Producer.Part, " ", "")

		if fChain.Producer.RequestID == kChain.Producer.RequestID && fProducerPart == kProducerPart {
			// Paths are identical, this is a clear match.
			if fChain.Producer.Path == kChain.Producer.Path {
				producersMatch = true
			} else {
				// Handle cases like "id" vs "data.id". If one path is a suffix of the other,
				// consider it a match because they refer to the same value semantically.
				if strings.HasSuffix(fChain.Producer.Path, "."+kChain.Producer.Path) || strings.HasSuffix(kChain.Producer.Path, "."+fChain.Producer.Path) {
					producersMatch = true
				}
			}
		}

		if !producersMatch {
			report.WriteString(fmt.Sprintf("❌ PRODUCER MISMATCH for value '%s':\n", value))
			report.WriteString(fmt.Sprintf("  - Expected (Fuzzer): %+v\n", fChain.Producer))
			report.WriteString(fmt.Sprintf("  - Actual (Keploy):   %+v\n", kChain.Producer))
			passed = false
		}
		// --- END OF NEW PRODUCER COMPARISON ---

		fConsumerSet := make(map[string]bool)
		for _, c := range fChain.Consumers {
			fConsumerSet[normalizeConsumer(c)] = true
		}
		kConsumerSet := make(map[string]bool)
		for _, c := range kChain.Consumers {
			kConsumerSet[normalizeConsumer(c)] = true
		}

		if !reflect.DeepEqual(fConsumerSet, kConsumerSet) {
			report.WriteString(fmt.Sprintf("❌ CONSUMER MISMATCH for value '%s':\n", value))
			for cKey := range fConsumerSet {
				if !kConsumerSet[cKey] {
					report.WriteString(fmt.Sprintf("  - Keploy MISSED consumer: %s\n", cKey))
				}
			}
			for cKey := range kConsumerSet {
				if !fConsumerSet[cKey] {
					report.WriteString(fmt.Sprintf("  - Keploy found EXTRA consumer: %s\n", cKey))
				}
			}
			passed = false
		}
	}

	// Check 2: Did Keploy find any extra chains the fuzzer didn't?
	for value, kChain := range keployMap {
		if _, exists := fuzzerMap[value]; !exists {
			report.WriteString(fmt.Sprintf("❌ EXTRA CHAIN: Keploy found chain for value '%s' (var: %s), but fuzzer did not.\n", value, kChain.VariableName))
			passed = false
		}
	}

	if passed {
		report.WriteString("All checks passed.\n")
	}
	return passed, report.String()
}

// filterInsignificantChains removes chains based on short, numeric values that are likely coincidental.
func filterInsignificantChains(chains []CanonicalChain) []CanonicalChain {
	var significantChains []CanonicalChain
	for _, chain := range chains {
		// Keep the chain if the value is long, or if it's not a simple number.
		if len(chain.Value) >= 4 {
			significantChains = append(significantChains, chain)
			continue
		}
		if _, err := strconv.ParseFloat(chain.Value, 64); err != nil {
			// It's not a number, so it's significant (e.g., "true", "v1")
			significantChains = append(significantChains, chain)
		}
	}
	return significantChains
}

// --- Utility and Helper Functions ---
// (These remain unchanged)

// helper to ensure parent segment forms a valid key (reuses existing conventions)
func sanitizeKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "-", "")
	k = strings.ReplaceAll(k, "_", "")
	return k
}

// reconstructURL replaces the specified path segment in the URL with the template string.
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

// setValueByPath sets a value in a nested structure (map/slice) given a dot-separated path.
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

// RenderIfTemplatized checks if the value is a string containing template markers and renders it if so.
func RenderIfTemplatized(val interface{}) (bool, interface{}, error) {
	stringVal, ok := val.(string)
	if !ok {
		return false, val, nil
	}
	if !(strings.Contains(stringVal, "{{") && strings.Contains(stringVal, "}}")) {
		return false, val, nil
	}
	val, err := render(stringVal)
	if err != nil {
		return false, val, err
	}
	return true, val, nil
}

// render processes the template string and returns the rendered value.
func render(val string) (interface{}, error) {
	funcMap := template.FuncMap{
		"int":    utils.ToInt,
		"string": utils.ToString,
		"float":  utils.ToFloat,
	}
	tmpl, err := template.New("template").Funcs(funcMap).Parse(val)
	if err != nil {
		// If parsing fails, it's likely not a valid template string, but a literal string
		// that happens to contain "{{" and "}}". In this case, we should not treat it as an
		// error but return the original value, as no substitution is possible.
		return val, nil
	}
	data := make(map[string]interface{})
	for k, v := range utils.TemplatizedValues {
		data[k] = v
	}
	if len(utils.SecretValues) > 0 {
		data["secret"] = utils.SecretValues
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, data)
	if err != nil {
		// An execution error (e.g., missing key) is a genuine problem and should be propagated.
		return val, fmt.Errorf("failed to execute the template: %v", err)
	}

	if strings.Contains(val, "string") {
		return output.String(), nil
	}
	outputString := strings.Trim(output.String(), `"`)
	switch {
	case strings.Contains(val, "int"):
		return utils.ToInt(outputString), nil
	case strings.Contains(val, "float"):

		return utils.ToFloat(outputString), nil
	}
	return outputString, nil
}

// insertUnique adds the baseKey to myMap with the given value, ensuring uniqueness by appending an index if necessary.
func insertUnique(baseKey, value string, myMap map[string]interface{}) string {
	baseKey = strings.ToLower(baseKey)
	baseKey = strings.ReplaceAll(baseKey, "-", "")
	baseKey = strings.ReplaceAll(baseKey, "_", "")
	if myMap[baseKey] == value {
		return baseKey
	}
	key := baseKey
	i := 0
	for {
		if existingVal, exists := myMap[key]; !exists {
			myMap[key] = value
			break
		} else if existingVal == value {
			break
		}
		i++
		key = baseKey + strconv.Itoa(i)
	}
	return key
}

// removeQuotesInTemplates removes extraneous quotes around template expressions in JSON strings.
func removeQuotesInTemplates(jsonStr string) string {
	re := regexp.MustCompile(`"\{\{[^{}]*\}\}"`)
	return re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}

		return strings.Trim(match, `"`)
	})
}

// addQuotesInTemplates adds quotes around template expressions in JSON strings where missing.
func addQuotesInTemplates(jsonStr string) string {
	if jsonStr == "" {
		return ""
	}
	re := regexp.MustCompile(`\{\{[^{}]*\}\}`)
	return re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}
		return `"` + match + `"`
	})

}
