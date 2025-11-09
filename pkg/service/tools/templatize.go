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
	"unicode"
	"unicode/utf8"

	"github.com/invopop/yaml"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
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

// --- V2 Optimized Templatization Logic ---
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

func (t *Tools) buildValueIndexV2(ctx context.Context, tcs []*models.TestCase, reqBodies, respBodies []interface{}) map[string][]*ValueLocation {
	valueIndex := make(map[string][]*ValueLocation)
	for i := range tcs {
		for k, val := range tcs[i].HTTPReq.Header {
			// Special handling for Cookie: split "a=1; b=2"
			if strings.EqualFold(k, "Cookie") {
				kvs := parseCookiePairs(val)
				for _, kv := range kvs {
					if kv.Name == "" {
						continue
					}
					loc := &ValueLocation{
						TestCaseIndex: i, Part: RequestHeader,
						Path:    fmt.Sprintf("Cookie.%s", kv.Name),
						Pointer: &tcs[i].HTTPReq.Header, OriginalType: "string",
					}
					valueIndex[kv.Value] = append(valueIndex[kv.Value], loc)
				}
				continue
			}
			loc := &ValueLocation{
				TestCaseIndex: i, Part: RequestHeader,
				Path: k, Pointer: &tcs[i].HTTPReq.Header, OriginalType: "string",
			}
			if k == "Authorization" && strings.HasPrefix(val, "Bearer ") {
				token := strings.TrimPrefix(val, "Bearer ")
				valueIndex[token] = append(valueIndex[token], loc)
			} else {
				valueIndex[val] = append(valueIndex[val], loc)
			}
		}
		// --- Response headers as potential producers ---
		for k, val := range tcs[i].HTTPResp.Header {
			if strings.EqualFold(k, "Set-Cookie") {
				cookies := splitSetCookie(val)
				for _, sc := range cookies {
					name, cval := splitSetCookieNameValue(sc)
					if name == "" {
						continue
					}
					loc := &ValueLocation{
						TestCaseIndex: i, Part: ResponseHeader,
						Path:    fmt.Sprintf("Set-Cookie.%s", name),
						Pointer: &tcs[i].HTTPResp.Header, OriginalType: "string",
					}
					valueIndex[cval] = append(valueIndex[cval], loc)
				}
				continue
			}
			// index other response headers too
			loc := &ValueLocation{
				TestCaseIndex: i, Part: ResponseHeader,
				Path: k, Pointer: &tcs[i].HTTPResp.Header, OriginalType: "string",
			}
			valueIndex[val] = append(valueIndex[val], loc)
		}

		parsedURL, err := url.Parse(tcs[i].HTTPReq.URL)
		if err == nil {
			pathSegments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
			for j, segment := range pathSegments {
				if segment != "" {
					path := fmt.Sprintf("path.%d", j)
					loc := &ValueLocation{TestCaseIndex: i, Part: RequestURL, Path: path, Pointer: &tcs[i].HTTPReq.URL, OriginalType: "string"}
					valueIndex[segment] = append(valueIndex[segment], loc)
				}
			}
		}
		if reqBodies[i] != nil {
			findValuesInInterface(reqBodies[i], []string{}, valueIndex, i, RequestBody, &reqBodies[i])
		}
		if respBodies[i] != nil {
			findValuesInInterface(respBodies[i], []string{}, valueIndex, i, ResponseBody, &respBodies[i])
		}
	}
	return valueIndex
}

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
		loc := &ValueLocation{TestCaseIndex: tcIndex, Part: part, Path: currentPath, Pointer: containerPtr, OriginalType: "string"}
		index[v] = append(index[v], loc)
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

func (t *Tools) applyTemplatesFromIndexV2(ctx context.Context, index map[string][]*ValueLocation, templateConfig map[string]interface{}) []*TemplateChain {
	// We need deterministic variable naming so that earlier producer test cases
	// receive the base key without suffix and later ones get incremental suffixes.
	// Strategy:
	// 1. Build candidate chains first (without assigning template keys).
	// 2. Group candidates by their derived base key.
	// 3. Sort each group by producer test index (ascending).
	// 4. Assign template keys deterministically (base, base1, base2 ...) within each group.
	// 5. Apply the template substitutions using the assigned keys.

	type candidate struct {
		value        string
		locations    []*ValueLocation
		producer     *ValueLocation
		consumers    []*ValueLocation
		occurrences  []*ValueLocation // all producer+consumer occurrences to templatize
		baseKey      string
		producerType string
	}

	var candidates []*candidate

	// Step 1: collect candidates.
	for value, locations := range index {

		switch strings.ToLower(value) {
		case "true", "false", "null", "nil":
			continue // Do not templatize booleans, null, or nil.
		}

		if floatVal, err := strconv.ParseFloat(value, 64); err == nil && floatVal == 0 {
			continue // Do not templatize zero.
		}

		if len(locations) < 2 { // need at least producer + consumer
			continue
		}

		sort.Slice(locations, func(i, j int) bool { return locations[i].TestCaseIndex < locations[j].TestCaseIndex })

		var producer *ValueLocation
		for _, loc := range locations {
			if loc.Part == ResponseBody || loc.Part == ResponseHeader { // allow response headers future
				producer = loc
				break
			}
		}
		if producer == nil { // can't form a chain without a producer
			continue
		}

		var subsequentConsumers []*ValueLocation
		for _, loc := range locations {
			if (loc.Part == RequestHeader || loc.Part == RequestURL || loc.Part == RequestBody) && loc.TestCaseIndex > producer.TestCaseIndex && typesCompatible(producer, loc) {
				subsequentConsumers = append(subsequentConsumers, loc)
			}
		}
		if len(subsequentConsumers) == 0 { // no data flow
			continue
		}

		// Determine all occurrences (producers + valid consumers at/after producer index)
		var occurrences []*ValueLocation
		for _, loc := range locations {
			isSelectedProducer := loc == producer
			isConsumer := (loc.Part == RequestHeader || loc.Part == RequestURL || loc.Part == RequestBody) &&
				loc.TestCaseIndex > producer.TestCaseIndex

			if isSelectedProducer || (isConsumer && typesCompatible(producer, loc)) {
				occurrences = append(occurrences, loc)
			}
		}

		// Derive base key (replicating previous logic for stability)
		var baseKey string
		if producer.Part == RequestURL {
			baseKey = value
		} else {
			baseKey = producer.Path
			parts := strings.Split(baseKey, ".")
			if len(parts) > 0 {
				baseKey = parts[len(parts)-1]
			}
			if _, err := strconv.Atoi(baseKey); err == nil { // numeric leaf => use parent context
				partsFull := strings.Split(producer.Path, ".")
				parent := "arr"
				if len(partsFull) >= 2 {
					parent = partsFull[len(partsFull)-2]
					for i := len(partsFull) - 2; i >= 0; i-- { // find first non-numeric ancestor
						if _, numErr := strconv.Atoi(partsFull[i]); numErr != nil {
							parent = partsFull[i]
							break
						}
					}
				}
				parent = normalizeBaseKey(parent)
				baseKey = fmt.Sprintf("%sIx%s", parent, baseKey)
			} else {
				baseKey = normalizeBaseKey(baseKey)
			}
		}

		candidates = append(candidates, &candidate{
			value:        value,
			locations:    locations,
			producer:     producer,
			consumers:    subsequentConsumers,
			occurrences:  occurrences,
			baseKey:      baseKey,
			producerType: producer.OriginalType,
		})
	}

	// Step 2: group by baseKey
	groups := make(map[string][]*candidate)
	for _, c := range candidates {
		groups[c.baseKey] = append(groups[c.baseKey], c)
	}

	// To keep overall deterministic ordering across different base keys, create sorted list of base keys.
	baseKeys := make([]string, 0, len(groups))
	for k := range groups {
		baseKeys = append(baseKeys, k)
	}
	sort.Strings(baseKeys)

	var resultChains []*TemplateChain

	for _, bk := range baseKeys {
		cs := groups[bk]
		// Step 3: sort candidates in this group by producer test index (ascending)
		sort.Slice(cs, func(i, j int) bool { return cs[i].producer.TestCaseIndex < cs[j].producer.TestCaseIndex })

		// Maintain a counter for suffix assignment within this base key group.
		occurrenceIdx := 0
		for _, cand := range cs {
			// Determine deterministic key: first gets baseKey, subsequent get baseKey + number (skipping existing collisions with different value)
			var desiredKey string
			if occurrenceIdx == 0 {
				desiredKey = bk
			} else {
				desiredKey = fmt.Sprintf("%s%d", bk, occurrenceIdx)
			}
			occurrenceIdx++

			// Ensure uniqueness versus existing templateConfig but deterministic for this ordering.
			// If the key already exists with same value -> reuse. If exists different value -> find next free suffix.
			finalKey := desiredKey
			if existingVal, exists := templateConfig[finalKey]; exists && existingVal != cand.value {
				// find next available with incrementing suffix while preserving ordering.
				suffix := 1
				for {
					try := fmt.Sprintf("%s%d", bk, suffix)
					if existingVal2, exists2 := templateConfig[try]; !exists2 || existingVal2 == cand.value {
						finalKey = try
						break
					}
					suffix++
				}
			}
			// Record in templateConfig if not present
			if existingVal, exists := templateConfig[finalKey]; !exists {
				templateConfig[finalKey] = cand.value
			} else if existingVal != cand.value {
				// Extremely unlikely due to above handling; fallback to insertUnique just in case.
				finalKey = insertUnique(bk, cand.value, templateConfig)
			}

			// Build chain and apply substitutions
			chain := &TemplateChain{
				TemplateKey: finalKey,
				Value:       cand.value,
				Producer:    cand.producer,
				Consumers:   cand.consumers,
			}
			resultChains = append(resultChains, chain)

			templateString := fmt.Sprintf("{{%s .%s}}", cand.producerType, finalKey)
			for _, loc := range cand.occurrences {
				if loc.Part == RequestHeader {
					if headerMap, ok := loc.Pointer.(*map[string]string); ok {
						// Cookie.<name> ⇒ replace only that cookie value inside Cookie header
						if strings.HasPrefix(loc.Path, "Cookie.") {
							name := strings.TrimPrefix(loc.Path, "Cookie.")
							(*headerMap)["Cookie"] = replaceCookieValue((*headerMap)["Cookie"], name, templateString)
							continue
						}
						// Authorization: Bearer <token>
						if loc.Path == "Authorization" {
							orig := (*headerMap)[loc.Path]
							if strings.HasPrefix(orig, "Bearer ") {
								(*headerMap)[loc.Path] = "Bearer " + templateString
								continue
							}
						}
						// Normal header key
						(*headerMap)[loc.Path] = templateString
					}
				} else if loc.Part == ResponseHeader {
					if headerMap, ok := loc.Pointer.(*map[string]string); ok {
						// Set-Cookie.<name> ⇒ replace only that cookie’s value, keep attrs
						if strings.HasPrefix(loc.Path, "Set-Cookie.") {
							name := strings.TrimPrefix(loc.Path, "Set-Cookie.")
							(*headerMap)["Set-Cookie"] = replaceSetCookieValue((*headerMap)["Set-Cookie"], name, templateString)
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
		}
	}

	return resultChains
}

// Utility function to safely marshal JSON and log errors
var jsonMarshal987 = json.Marshal

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

func toCamelCase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "v"
	}
	// Tokenize on non-alphanumeric
	var tokens []string
	var cur []rune
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else if len(cur) > 0 {
			tokens = append(tokens, string(cur))
			cur = cur[:0]
		}
	}
	if len(cur) > 0 {
		tokens = append(tokens, string(cur))
	}
	if len(tokens) == 0 {
		return "v"
	}

	// Build camelCase
	var b strings.Builder
	for i, t := range tokens {
		t = strings.ToLower(t)
		if i == 0 {
			b.WriteString(t)
		} else {
			r, size := utf8.DecodeRuneInString(t)
			if size == 0 || r == utf8.RuneError {
				// invalid/empty, just append as-is
				b.WriteString(t)
			} else {
				b.WriteRune(unicode.ToUpper(r))
				b.WriteString(t[size:])
			}
		}
	}
	out := b.String()
	// Must not start with digit for Go templates: {{ .<ident> }}
	if out != "" {
		r, size := utf8.DecodeRuneInString(out)
		if size == 0 || r == utf8.RuneError || unicode.IsDigit(r) {
			out = "v" + out
		}
	}

	return out
}

// Use this everywhere you finalize a base key.
func normalizeBaseKey(s string) string {
	return toCamelCase(s)
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
		return val, fmt.Errorf("failed to execute the template %v", zap.Error(err))
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

func insertUnique(baseKey, value string, m map[string]interface{}) string {
	key := baseKey
	i := 0
	for {
		if existingVal, exists := m[key]; !exists {
			m[key] = value
			return key
		} else if existingVal == value {
			return key
		}
		i++
		key = fmt.Sprintf("%s%d", baseKey, i) // camelCase + numeric suffix
	}
}

func removeQuotesInTemplates(jsonStr string) string {
	re := regexp.MustCompile(`"\{\{[^{}]*\}\}"`)
	return re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}
		return strings.Trim(match, `"`)
	})
}

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

func splitSetCookie(s string) []string {
	if s == "" {
		return nil
	}
	// Prefer newline-separated multiple Set-Cookie lines if available.
	if strings.Contains(s, "\n") {
		lines := strings.Split(s, "\n")
		out := make([]string, 0, len(lines))
		for _, ln := range lines {
			if ln = strings.TrimSpace(ln); ln != "" {
				out = append(out, ln)
			}
		}
		return out
	}

	// Split on comma (with or without space). Rejoin pieces that are NOT cookie starts
	// (e.g., the ", 23 Oct ..." part of Expires).
	raw := strings.Split(s, ",")
	if len(raw) == 1 {
		return []string{strings.TrimSpace(raw[0])}
	}

	out := []string{strings.TrimSpace(raw[0])}
	for _, token := range raw[1:] {
		t := strings.TrimSpace(token)
		name, _, hasEq := strings.Cut(t, "=")

		// Consider a token a new cookie if it looks like "name=value".
		// (Allow letters, digits, '_' as a simple/robust check.)
		startsName := hasEq && name != "" && isValidFirstRune(name)

		if startsName {
			out = append(out, t)
		} else {
			// Part of previous cookie (e.g., Expires=Thu, 23 Oct ...). Preserve comma.
			out[len(out)-1] = out[len(out)-1] + "," + token
		}
	}
	return out
}

func isValidFirstRune(name string) bool {
	if name == "" {
		return false
	}
	r, size := utf8.DecodeRuneInString(name)
	if size == 0 || r == utf8.RuneError {
		return false
	}
	// Keep original semantics (letters/digits/_/$ allowed):
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$'
}

// splitSetCookieNameValue extracts the cookie name and value (before attributes).
// E.g. "sid=abc123; Path=/; HttpOnly" => ("sid", "abc123")
func splitSetCookieNameValue(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	firstSemi := strings.IndexByte(line, ';')
	head := line
	if firstSemi >= 0 {
		head = line[:firstSemi]
	}
	name, val, ok := strings.Cut(head, "=")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(name), strings.TrimSpace(val)
}

func replaceSetCookieValue(setCookieHdr, name, newVal string) string {
	lines := splitSetCookie(setCookieHdr)
	if len(lines) == 0 {
		// create a basic cookie line
		return name + "=" + newVal
	}
	for i, ln := range lines {
		cn, _ := splitSetCookieNameValue(ln)
		if cn == name {
			rest := ""
			if idx := strings.IndexByte(ln, ';'); idx >= 0 {
				rest = ln[idx:] // includes leading ';'
			}
			lines[i] = name + "=" + newVal + rest
			break
		}
	}
	return strings.Join(lines, "\n")
}

func isNum(t string) bool {
	return t == "int" || t == "float"
}

func typesCompatible(p, c *ValueLocation) bool {
	if isNum(p.OriginalType) && isNum(c.OriginalType) {
		return true
	}
	return p.OriginalType == c.OriginalType
}
