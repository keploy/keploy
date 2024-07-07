//go:build linux

package replay

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	// "encoding/json"
	"github.com/7sDream/geko"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total  int
	passed int
	failed int ``
	status bool
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
	path, err := url.JoinPath(parsedNewURL.Path, parsedOldURL.Path)
	if err != nil {
		return "", fmt.Errorf("failed to join '%v' and '%v' paths: %v", parsedNewURL.Path, parsedOldURL.Path, err)
	}
	parsedOldURL.Path = path

	replacedURL := parsedOldURL.String()
	return replacedURL, nil
}

type requestMockUtil struct {
	logger     *zap.Logger
	path       string
	mockName   string
	apiTimeout uint64
	basePath   string
}

func NewRequestMockUtil(logger *zap.Logger, path, mockName string, apiTimeout uint64, basePath string) RequestMockHandler {
	return &requestMockUtil{
		path:       path,
		logger:     logger,
		mockName:   mockName,
		apiTimeout: apiTimeout,
		basePath:   basePath,
	}
}
func (t *requestMockUtil) SimulateRequest(ctx context.Context, _ uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		resp, err := pkg.SimulateHTTP(ctx, tc, testSetID, t.logger, t.apiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func (t *requestMockUtil) AfterTestHook(_ context.Context, testRunID, testSetID string, tsCnt int) (*models.TestReport, error) {
	t.logger.Debug("AfterTestHook", zap.Any("testRunID", testRunID), zap.Any("testSetID", testSetID), zap.Any("totalTestSetCount", tsCnt))
	return nil, nil
}

func (t *requestMockUtil) ProcessTestRunStatus(_ context.Context, status bool, testSetID string) {
	if status {
		t.logger.Debug("Test case passed for", zap.String("testSetID", testSetID))
	} else {
		t.logger.Debug("Test case failed for", zap.String("testSetID", testSetID))
	}
}

func (t *requestMockUtil) FetchMockName() string {
	return t.mockName
}

func (t *requestMockUtil) ProcessMockFile(_ context.Context, testSetID string) {
	if t.basePath != "" {
		t.logger.Debug("Mocking is disabled when basePath is given", zap.String("testSetID", testSetID), zap.String("basePath", t.basePath))
		return
	}
	t.logger.Debug("Mock file for test set", zap.String("testSetID", testSetID))
}

func parseIntoJson(response string) (interface{}, error) {
	// Parse the response into a json object.
	if response == "" {
		return nil, nil
	}
	result, err := geko.JSONUnmarshal([]byte(response))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal the response: %v", err)
	}
	return result, nil
}

func compareVals(map1 interface{}, map2 *interface{}) {
	switch v := map1.(type) {
	case geko.ObjectItems:
		keys := v.Keys()
		vals := v.Values()
		for i := range keys {
			vString, ok := vals[i].(string)
			if ok {
				if strings.HasPrefix(vString, "{{") && strings.HasSuffix(vString, "}}") {
					// Get the value from the template.
					stringVal, ok := vals[i].(string)
					if ok {
						vals[i], _ = render(stringVal)
						if !strings.Contains(stringVal, "string") {
							vals[i] = utils.ToInt(vals[i])
						}
					}
				}
			}
			compareVals(vals[i], map2)
		}
	case map[string]interface{}:
		for key, val1 := range v {
			vString, ok := val1.(string)
			if ok {
				if strings.HasPrefix(vString, "{{") && strings.HasSuffix(vString, "}}") {
					// Get the value from the template.
					stringVal, ok := val1.(string)
					if ok {
						val1, _ = render(stringVal)
						if !strings.Contains(stringVal, "string") {
							val1 = utils.ToInt(val1)
						}
					}
				}
			}
			compareVals(val1, map2)
			v[key] = val1
		}
	case map[string]string:
		for key, val1 := range v {
			authType := ""
			if key == "Authorization" && len(strings.Split(val1, " ")) > 1 {
				authType = strings.Split(val1, " ")[0]
				val1 = strings.Split(val1, " ")[1]
			}
			if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
				// Get the value from the template.
				val1, _ = render(val1)
			}
			ok := parseBody(&val1, map2)
			if !ok {
				continue
			}
			// Add the authtype to the string.
			val1 = authType + " " + val1
			v[key] = val1
		}
	case *string:
		if strings.HasPrefix(*v, "{{") && strings.HasSuffix(*v, "}}") {
			return
		}
		var ok bool
		ok = parseBody(v, map2)
		if ok {
			return
		}
		url, err := url.Parse(*v)
		if err == nil {
			urlParts := strings.Split(url.Path, "/")
			ok = parseBody(&urlParts[len(urlParts)-1], map2)
			url.Path = strings.Join(urlParts, "/")
			if url.RawQuery != "" {
				// Only pass the values of the query parameters to the parseBody function.
				queryParams := strings.Split(url.RawQuery, "&")
				for i, param := range queryParams {
					param = strings.Split(param, "=")[1]
					parseBody(&param, map2)
					queryParams[i] = strings.Split(queryParams[i], "=")[0] + "=" + param
				}
				url.RawQuery = strings.Join(queryParams, "&")
				*v = fmt.Sprintf("%s://%s%s?%s", url.Scheme, url.Host, url.Path, url.RawQuery)
			} else {
				*v = fmt.Sprintf("%s://%s%s", url.Scheme, url.Host, url.Path)
			}
		} else {
			ok = parseBody(v, map2)
		}
		if !ok {
			return
		}
	case float64, int64, int, float32:
		val := toString(v)
		valPointer := &val
		if strings.HasPrefix(*valPointer, "{{") && strings.HasSuffix(*valPointer, "}}") {
			return
		}
		var ok bool
		ok = parseBody(valPointer, map2)
		parts := strings.Split(*valPointer, " ")
		if len(parts) > 1 {
			parts1 := strings.Split(parts[0], "{{")
			if len(parts1) > 1 {
				*valPointer = parts1[0] + "{{" + getType(v) + " " + parts[1] + "}}"
			}
		}
		if !ok {
			return
		}
	}
}

func parseBody(val1 *string, body *interface{}) bool {
	switch b := (*body).(type) {
	case geko.ObjectItems:
		keys := b.Keys()
		vals := b.Values()
		for i, key := range keys {
			stringVal, ok := vals[i].(string)
			if ok {
				if strings.HasPrefix(stringVal, "{{") && strings.HasSuffix(stringVal, "}}") {
					// Get the value from the template.
					vals[i], _ = render(stringVal)
					if ! strings.Contains(stringVal, "string") {
					vals[i] = utils.ToInt(vals[i])
				}
				}
			}
			ok = parseBody(val1, &vals[i])
			if ok && reflect.TypeOf(vals[i]) != reflect.TypeOf(b) {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				vals[i] = fmt.Sprintf("{{%s .%v }}", getType(vals[i]), newKey)
				b.SetValueByIndex(i, vals[i])
				// fmt.Println("This is the value at index i in the map", b.GetByIndex(i))
				// fmt.Println("This is the val1 before", reflect.TypeOf(*val1))
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)
				// fmt.Println("This is the val1 after", *val1)
				return true
			}
		}
	case geko.Array:
		for _, v := range b.List {
			parseBody(val1, &v)
			// fmt.Println("This is the value that we are sending, ", v)
			// fmt.Println("This is the value of the jsonResponse map", jsonResponse)
		}
	case map[string]string:
		for key, val2 := range b {
			if *val1 == val2 {
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{.%s}}", newKey)
				*val1 = fmt.Sprintf("{{.%s}}", newKey)
				return true
			}
		}
		return false
	case string:
		if strings.HasPrefix(b, "{{") && strings.HasSuffix(b, "}}") {
			return false
		}
		if *val1 == b {
			return true
		}
	case map[string]interface{}:
		for key, val2 := range b {
			ok := parseBody(val1, &val2)
			if ok {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{ %s .%s}}", getType(b[key]), newKey)
				*val1 = fmt.Sprintf("{{ %s .%s }}", getType(*val1), newKey)
			}
		}
	case float64, int64, int, float32:
		if *val1 == toString(b) {
			return true
		}

	case []interface{}:
		for i, val := range b {
			parseBody(val1, &val)
			b[i] = val
		}
	}
	return false
}

func reverseMap(m map[string]interface{}) map[interface{}]string {
	var reverseMap = make(map[interface{}]string)
	for key, val := range m {
		reverseMap[val] = key
	}
	return reverseMap
}

func getType(val interface{}) string {
	switch val.(type) {
	case string:
		return "string"
	case int64, int, int32:
		return "int"
	case float64, float32:
		return "float"
	}
	return ""
}

func compareResponses(response1, response2 *interface{}, key string) {
	switch v1 := (*response1).(type) {
	case geko.Array:
		for _, val1 := range v1.List {
			compareResponses(&val1, response2, "")
		}
	case geko.ObjectItems:
		keys := v1.Keys()
		vals := v1.Values()
		for i, _ := range keys {
			compareResponses(&vals[i], response2, keys[i])
		}
	case map[string]interface{}:
		for key, val := range v1 {
			compareResponses(&val, response2, key)
			v1[key] = val
		}
	case string:
		compareSecondResponse(&v1, response2, key, "")
	case float64, int64, int, float32:
		v1String := toString(v1)
		compareSecondResponse(&(v1String), response2, key, "")
	}
}

func compareSecondResponse(val1 *string, response2 *interface{}, key1 string, key2 string) {
	switch v2 := (*response2).(type) {
	case geko.Array:
		for _, val2 := range v2.List {
			compareSecondResponse(val1, &val2, key1, "")
		}

	case geko.ObjectItems:
		keys := v2.Keys()
		vals := v2.Values()
		for i, _ := range keys {
			compareSecondResponse(val1, &vals[i], key1, keys[i])
		}
	case map[string]interface{}:
		for key, val := range v2 {
			compareSecondResponse(val1, &val, key1, key)
		}
	case string:
		if *val1 != v2 {
			// Reverse the templatized values map.
			revMap := reverseMap(utils.TemplatizedValues)
			if _, ok := revMap[*val1]; ok && key1 == key2 {
				key := revMap[*val1]
				utils.TemplatizedValues[key] = v2
				*val1 = v2
			}
		}
	case float64, int64, int, float32:
		if *val1 != toString(v2) && key1 == key2 {
			revMap := reverseMap(utils.TemplatizedValues)
			if _, ok := revMap[*val1]; ok {
				key := revMap[*val1]
				utils.TemplatizedValues[key] = v2
				*val1 = toString(v2)
			}
		}
	}
}

func insertUnique(baseKey, value string, myMap map[string]interface{}) string {
	// If the key has more than one word seperated by a delimiter, remove the delimiter and add the key to the map.
	if strings.Contains(baseKey, "-") {
		baseKey = strings.ReplaceAll(baseKey, "-", "")
	}
	if myMap[baseKey] == value {
		return baseKey
	}
	key := baseKey
	i := 0
	for {
		if myMap[key] == value {
			break
		}
		if _, exists := myMap[key]; !exists {
			myMap[key] = value
			break
		}
		i++
		key = baseKey + strconv.Itoa(i)
	}
	return key
}

func toString(val interface{}) string {
	switch v := val.(type) {
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case string:
		return v
	}
	return ""
}

func render(testCaseStr string) (string, error) {
	funcMap := template.FuncMap{
		"int":    utils.ToInt,
		"string": utils.ToString,
		"float":  utils.ToFloat,
	}
	var ok bool
	// Remove the double quotes if the template does not contain the word string.
	if !strings.Contains(testCaseStr, "string") {
		ok = true
	}
	tmpl, err := template.New("template").Funcs(funcMap).Parse(string(testCaseStr))
	if err != nil {
		return testCaseStr, fmt.Errorf("failed to parse the testcase using template", zap.Error(err))
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, utils.TemplatizedValues)
	if err != nil {
		return testCaseStr, fmt.Errorf("failed to execute the template", zap.Error(err))
	}
	if ok {
		outputString := strings.Trim(output.String(), `"`)
		return outputString, nil
	}
	return output.String(), nil
}

func compareReqHeaders(req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		val1, _ = render(val1)
		var val interface{}
		// Check if the value is already present in the templatized values.
		if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
			// Get the value from the template.
			stringVal, ok := val.(string)
			if ok {
				val, _ = render(stringVal)
				if !strings.Contains(stringVal, "string") {
					val = utils.ToInt(val)
				}
			}
		} else {
			val = val1
		}
		if val2, ok := req2[key]; ok {
			val2, _ = render(val2)
			if val == val2 {
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				req2[key] = fmt.Sprintf("{{.%s}}", newKey)
				req1[key] = fmt.Sprintf("{{.%s}}", newKey)
			}
		}
	}
}

func removeQuotesInTemplates(jsonStr string) string {
	// Regular expression to find patterns with {{ and }}
	re := regexp.MustCompile(`"\{\{[^{}]*\}\}"`)
	// Function to replace matches by removing surrounding quotes
	result := re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}
		// Remove the surrounding quotes
		return strings.Trim(match, `"`)
	})

	return result
}

func noQuotes(tempMap map[string]interface{}) {
	// Remove double quotes
	for key, val := range tempMap {
		if str, ok := val.(string); ok {
			tempMap[key] = strings.ReplaceAll(str, `"`, "")
		}
	}
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
