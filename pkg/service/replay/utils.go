package replay

import (
	"context"
	"fmt"
	"net/url"


	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
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

// func parseIntoJson(response string) (map[string]interface{}, error) {
// 	// Parse the response into a json object.
// 	var jsonResponse map[string]interface{}
// 	if err := json.Unmarshal([]byte(response), &jsonResponse); err != nil {
// 		return nil, err
// 	}
// 	return jsonResponse, nil
// }

// func compareVals(map1 interface{}, map2 map[string]interface{}) {
// 	switch v := map1.(type) {
// 	case map[string]string:
// 		for key, val1 := range v {
// 			authType := ""
// 			if key == "Authorization" && len(strings.Split(val1, " ")) > 1 {
// 				authType = strings.Split(val1, " ")[0]
// 				val1 = strings.Split(val1, " ")[1]
// 			}
// 			if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
// 				continue
// 			}
// 			newKey, ok := parseBody(val1, map2)
// 			if !ok {
// 				continue
// 			}
// 			// Add the template.
// 			val1 = strings.Replace(val1, val1, fmt.Sprintf("%s {{ %s }}", authType, newKey), -1)
// 			v[key] = val1
// 		}
// 	case string:
// 		if strings.HasPrefix(v, "{{") && strings.HasSuffix(v, "}}") {
// 			return
// 		}
// 		newKey, ok := parseBody(v, map2)
// 		if !ok {
// 			return
// 		}
// 		// Add the template
// 		v = strings.Replace(v, v, fmt.Sprintf("{{ %s }}", newKey), -1)
// 		map1 = v
// 	}

// }

// func parseBody(val1 string, map2 map[string]interface{}) (string, bool) {
// 	for key1, val2 := range map2 {
// 		valType := checkType(val2)
// 		if valType == "map" {
// 			map3, _ := val2.(map[string]interface{})
// 			for key2, v := range map3 {
// 				if _, ok := utils.TemplatizedValues[val1]; ok {
// 					continue
// 				}
// 				v := checkType(v)
// 				if val1 == v {
// 					newKey := insertUnique(key2, v, utils.TemplatizedValues)
// 					if newKey == "" {
// 						newKey = key2
// 					}
// 					map3[newKey] = fmt.Sprintf("{{ %s }}", newKey)
// 					return newKey, true
// 				}
// 			}
// 		} else if val1 == checkType(val2) {
// 			newKey := insertUnique(key1, checkType(val2), utils.TemplatizedValues)
// 			if newKey == "" {
// 				newKey = key1
// 			}
// 			map2[key1] = fmt.Sprintf("{{ %s }}", newKey)
// 			return newKey, true
// 		}
// 	}
// 	return "", false
// }

// func insertUnique(baseKey, value string, myMap map[string]string) string {
// 	if myMap[baseKey] == value {
// 		return ""
// 	}
// 	key := baseKey
// 	i := 0
// 	for {
// 		if _, exists := myMap[key]; !exists {
// 			myMap[key] = value
// 			break
// 		}
// 		i++
// 		key = baseKey + strconv.Itoa(i)
// 	}
// 	return key
// }

// func checkType(val interface{}) string {
// 	switch v := val.(type) {
// 	case map[string]interface{}:
// 		return "map"
// 	case int:
// 		return strconv.Itoa(v)
// 	case float64:
// 		return strconv.FormatFloat(v, 'f', -1, 64)
// 	case string:
// 		return v
// 	}
// 	return ""
// }

// func compareReqHeaders(req1 map[string]string, req2 map[string]string) {
// 	for key, val1 := range req1 {
// 		// Check if the value is already present in the templatized values.
// 		if strings.HasPrefix(val1, "{{") && strings.HasSuffix(val1, "}}") {
// 			continue
// 		}
// 		if val2, ok := req2[key]; ok {
// 			if val1 == val2 {
// 				newKey := insertUnique(key, val1, utils.TemplatizedValues)
// 				if newKey == "" {
// 					newKey = key
// 				}
// 				req2[key] = fmt.Sprintf("{{ %s }}", newKey)
// 				req1[key] = fmt.Sprintf("{{ %s }}", newKey)
// 			}
// 		}
// 	}
// }
