//go:build linux

package replay

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

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

// func assignValues(jsonResponse map[string]interface{}, result interface{}) interface{} {
// 	switch v := result.(type) {
// 	case map[string]interface{}:
// 		for key, val := range v {
// 			jsonResponse[key] = val
// 		}
// 	case string:
// 		return v
// 	case float64, int64, int, float32:
// 		return v
// 	case geko.ObjectItems:
// 		keys := v.Keys()
// 		vals := v.Values()
// 		for i, key := range keys {
// 			jsonResponse[key] = assignValues(jsonResponse, vals[i])
// 			fmt.Println("This is the value that we are sending, ", vals[i])
// 		}
// 	case geko.Array:
// 		for _, v := range v.List {
// 			assignValues(jsonResponse, v)
// 			// fmt.Println("This is the value that we are sending, ", v)
// 			// fmt.Println("This is the value of the jsonResponse map", jsonResponse)
// 		}
// 		return v
// 	default:
// 		return nil
// 	}
// 	return jsonResponse
// }

func compareVals(map1 interface{}, map2 *interface{}) {
	switch v := map1.(type) {
	case map[string]interface{}:
		for key, val1 := range v {
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
				continue
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
		if !ok {
			return
		}
	}

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

func parseBody(val1 *string, body *interface{}) bool {
	// Check if the value is already present in the templatized values.
	// Reverse the templatized value map.
	// revMap := reverseMap(utils.TemplatizedValues)
	// if _, ok := revMap[*val1]; ok {
	// 	return false
	// }
	switch b := (*body).(type) {
	case geko.ObjectItems:
		keys := b.Keys()
		vals := b.Values()
		for i, key := range keys {
			ok := parseBody(val1, &vals[i])
			if ok {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				vals[i] = fmt.Sprintf("{{%s .%s }}", getType(vals[i]), newKey)
				b.SetValueByIndex(i, vals[i])
				// fmt.Println("This is the value at index i in the map", b.GetByIndex(i))
				*val1 = fmt.Sprintf("{{%s .%s }}", getType(vals[i]), newKey)
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
				b[key] = fmt.Sprintf("{{.%s}}", newKey)
				*val1 = fmt.Sprintf("{{render .%s %d}}", newKey, reflect.TypeOf(val2).Kind())
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
		if _, exists := myMap[key]; !exists{
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

func compareReqHeaders(req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		// Check if the value is already present in the templatized values.
		if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
			continue
		}
		if val2, ok := req2[key]; ok {
			if val1 == val2 {
				newKey := insertUnique(key, val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				req2[key] = fmt.Sprintf("{{.%s}}", newKey)
				req1[key] = fmt.Sprintf("{{.%s}}", newKey)
			}
		}
	}
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
