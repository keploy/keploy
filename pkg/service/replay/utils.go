package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type TestReportVerdict struct {
	total  int
	passed int
	failed int
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
	var jsonResponse interface{}
	if response == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(response), &jsonResponse); err != nil {
		return nil, err
	}
	return jsonResponse, nil
}

func compareVals(map1 interface{}, map2 interface{}) {
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
			*v = fmt.Sprintf("%s://%s%s", url.Scheme, url.Host, url.Path)
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
		parseBody(valPointer, map2)
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

func parseBody(val1 *string, body interface{}) bool {
	switch b := body.(type) {
	case map[string]string:
		for key, val2 := range b {
			if *val1 == val2 {
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{ %s }}", newKey)
				*val1 = fmt.Sprintf("{{ %s }}", newKey)
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
			ok := parseBody(val1, val2)
			if ok {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{ %s }}", newKey)
				*val1 = fmt.Sprintf("{{ %s }}", newKey)
			}
		}
	case float64, int64, int, float32:
		if *val1 == toString(b) {
			return true
		}

	case []interface{}:
		for i, val := range b {
			parseBody(val1, val)
			b[i] = val
		}
	}
	return false
}

func insertUnique(baseKey, value string, myMap map[string]interface{}) string {
	if myMap[baseKey] == value {
		return ""
	}
	key := baseKey
	i := 0
	for {
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
				req2[key] = fmt.Sprintf("{{ %s }}", newKey)
				req1[key] = fmt.Sprintf("{{ %s }}", newKey)
			}
		}
	}
}
