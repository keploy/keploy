package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

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

// Struct to hold information about an API chain
type TemplateChain struct {
	TemplateKey string
	Value       string
	Producer    *ValueLocation
	Consumers   []*ValueLocation
}

// --- V2 Optimized Templatization Logic ---

func (t *Tools) ProcessTestCases(ctx context.Context, tcs []*models.TestCase, testSetID string) error {
	for _, tc := range tcs {
		tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
	}

	reqBodies := make([]interface{}, len(tcs))
	respBodies := make([]interface{}, len(tcs))
	for i, tc := range tcs {
		decoderReq := json.NewDecoder(strings.NewReader(tc.HTTPReq.Body))
		decoderReq.UseNumber()
		decoderReq.Decode(&reqBodies[i])

		decoderResp := json.NewDecoder(strings.NewReader(tc.HTTPResp.Body))
		decoderResp.UseNumber()
		decoderResp.Decode(&respBodies[i])
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
	return nil
}

func (t *Tools) logAPIChains(chains []*TemplateChain, testCases []*models.TestCase) {
	if len(chains) == 0 {
		return
	}
	t.logger.Info("✨ Detected API chains from templatization ✨")
	for _, chain := range chains {
		var logFields []zap.Field
		logFields = append(logFields, zap.String("template_variable", fmt.Sprintf("{{.%s}}", chain.TemplateKey)))
		logFields = append(logFields, zap.String("original_value", chain.Value))
		logFields = append(logFields, zap.String("producer", formatLocation(chain.Producer, testCases)))
		var consumerStrings []string
		for _, consumer := range chain.Consumers {
			consumerStrings = append(consumerStrings, formatLocation(consumer, testCases))
		}
		logFields = append(logFields, zap.Strings("consumers", consumerStrings))
		t.logger.Info("Chain Details", logFields...)
	}
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
			loc := &ValueLocation{TestCaseIndex: i, Part: RequestHeader, Path: k, Pointer: &tcs[i].HTTPReq.Header, OriginalType: "string"}
			if k == "Authorization" && strings.HasPrefix(val, "Bearer ") {
				token := strings.TrimPrefix(val, "Bearer ")
				valueIndex[token] = append(valueIndex[token], loc)
			} else {
				valueIndex[val] = append(valueIndex[val], loc)
			}
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

// *** FIXED: Function signature now includes the return type ***
func (t *Tools) applyTemplatesFromIndexV2(ctx context.Context, index map[string][]*ValueLocation, templateConfig map[string]interface{}) []*TemplateChain {
	var chains []*TemplateChain

	for value, locations := range index {
		if len(locations) < 2 {
			continue
		}

		sort.Slice(locations, func(i, j int) bool {
			return locations[i].TestCaseIndex < locations[j].TestCaseIndex
		})

		var producer *ValueLocation
		for _, loc := range locations {
			if loc.Part == ResponseBody {
				producer = loc
				break
			}
		}
		if producer == nil {
			producer = locations[0]
		}
		producerType := producer.OriginalType

		var consumers []*ValueLocation
		for _, loc := range locations {
			if loc != producer && loc.TestCaseIndex >= producer.TestCaseIndex {
				consumers = append(consumers, loc)
			}
		}

		if len(consumers) == 0 {
			if producer == locations[0] && len(locations) > 1 {
				consumers = append(consumers, locations[1:]...)
			} else {
				continue
			}
		}

		var baseKey string
		if producer.Part == RequestURL {
			baseKey = value
		} else {
			baseKey = producer.Path
			if parts := strings.Split(baseKey, "."); len(parts) > 0 {
				baseKey = parts[len(parts)-1]
			}
		}
		templateKey := insertUnique(baseKey, value, templateConfig)

		chain := &TemplateChain{
			TemplateKey: templateKey,
			Value:       value,
			Producer:    producer,
			Consumers:   consumers,
		}
		chains = append(chains, chain)

		allLocs := append(consumers, producer)
		for _, loc := range allLocs {
			templateString := fmt.Sprintf("{{%s .%s}}", producerType, templateKey)
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
	}
	// *** FIXED: Added the return statement ***
	return chains
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

// --- Kept Helper Functions ---

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
		return val, fmt.Errorf("failed to parse the testcase using template %v", zap.Error(err))
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
