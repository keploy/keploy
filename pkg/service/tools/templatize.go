package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/7sDream/geko"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func (t *Tools) Templatize(ctx context.Context) error {

	testSets := t.config.Templatize.TestSets
	if len(testSets) == 0 {
		all, err := t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get all test sets")
			return err
		}
		testSets = all
	}

	if len(testSets) == 0 {
		t.logger.Warn("No test sets found to templatize")
		return nil
	}

	for _, testSetID := range testSets {

		testSet, err := t.testSetConf.Read(ctx, testSetID)
		if err == nil && (testSet != nil && testSet.Template != nil) {
			utils.TemplatizedValues = testSet.Template
		} else {
			utils.TemplatizedValues = make(map[string]interface{})
		}

		// Get test cases from the database
		tcs, err := t.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get test cases")
			return err
		}

		if len(tcs) == 0 {
			t.logger.Warn("The test set is empty. Please record some test cases to templatize.", zap.String("testSet", testSetID))
			continue
		}

		err = t.ProcessTestCases(ctx, tcs, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to process test cases")
			return err
		}
	}
	return nil
}

func (t *Tools) ProcessTestCases(ctx context.Context, tcs []*models.TestCase, testSetID string) error {

	// In test cases, we often use placeholders like {{float .id}} for templatized variables. Ideally, we should wrap
	// them in double quotes, i.e., "{{float .id}}", to prevent errors during JSON unmarshaling. However, we avoid doing
	// this to prevent user confusion. If a user sees "{{float .id}}", they might wonder whether it's a string or a float.
	//
	// To maintain clarity, we remove these placeholders during marshalling and reintroduce them during unmarshalling.
	//
	// Note: This conversion is applied only to `reqBody` and `respBody` because all other fields are strings, and
	// templatized variables in those cases are simply concatenated.
	//
	// Example:
	//
	// Request:
	//   method: GET
	//   url: http://localhost:8080/api/employees/{{string .id}}
	//
	// Response:
	//   status_code: 200
	//   header:
	//     Content-Type: application/json
	//     Date: Fri, 19 Jan 2024 06:06:03 GMT
	//   body: '{"id":{{float .id}},"firstName":"0","lastName":"0","email":"0"}'
	//
	// Notice that even if we omit quotes in the URL, marshalling does not fail. However, when unmarshalling `respBody`,
	// it will throw an error if placeholders like `{{float .id}}` are not properly handled.
	for _, tc := range tcs {
		tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
	}

	// Process test cases for different scenarios and update the tcs and utils.TemplatizedValues
	// Case 1: Response Body of one test case to Request Headers of other test cases
	// (use case: Authorization token)
	t.processRespBodyToReqHeader(ctx, tcs)

	// Case 2: Request Headers of one test case to Request Headers of other test cases
	// (use case: Authorization token if Login API is not present in the test set)
	t.processReqHeadersToReqHeader(ctx, tcs)

	// Case 3: Response Body of one test case to Response Headers of other
	// (use case: POST - GET scenario)
	t.processRespBodyToReqURL(ctx, tcs)

	// Case 4: Compare the req and resp body of one to other.
	// (use case: POST - PUT scenario)
	t.processRespBodyToReqBody(ctx, tcs)

	// Case 5: Compare the req and resp for same test case for any common fields.
	// (use case: POST) request and response both have same fields.
	t.processBody(ctx, tcs)

	// Case 6: Compare the req url with the response body of same test for any common fields.
	// (use case: GET) URL might container same fields as response body.
	t.processReqURLToRespBodySameTest(ctx, tcs)

	// case 7: Compare the resp body of one test with the response body of other tests for any common fields.
	// (use case: POST - GET scenario)
	t.processRespBodyToRespBody(ctx, tcs)

	// case 7: Compare the req body of one test with the response body of other tests for any common fields.
	// (use case: POST - GET scenario)
	t.processReqBodyToRespBody(ctx, tcs)

	// case 8: Compare the req body of one test with the req URL of other tests for any common fields.
	// (use case: POST - GET scenario)
	t.processReqBodyToReqURL(ctx, tcs)

	// case 9: Compare the req body of one test with the req body of other tests for any common fields.
	// (use case: POST - PUT scenario)
	t.processReqBodyToReqBody(ctx, tcs)

	// case 10: Compare the req URL of one test with the req body of other tests for any common fields.
	// (use case: GET - PUT scenario)
	t.processReqURLToReqBody(ctx, tcs)

	// case 11: Compare the req URL of one test with the req URL of other tests for any common fields
	// (use case: GET - PUT scenario)
	t.processReqURLToReqURL(ctx, tcs)

	// case 12: Compare the req URL of one test with the resp Body of other tests for any common fields
	// (use case: GET - PUT scenario)
	t.processReqURLToRespBody(ctx, tcs)

	for _, tc := range tcs {
		tc.HTTPReq.Body = removeQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = removeQuotesInTemplates(tc.HTTPResp.Body)
		err := t.testDB.UpdateTestCase(ctx, tc, testSetID, false)
		if err != nil {
			utils.LogError(t.logger, err, "failed to update test case")
			return err
		}
	}

	utils.RemoveDoubleQuotes(utils.TemplatizedValues)
	err := t.testSetConf.Write(ctx, testSetID, &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   utils.TemplatizedValues,
	})
	if err != nil {
		utils.LogError(t.logger, err, "failed to write test set")
		return err
	}
	return nil
}

func (t *Tools) processRespBodyToReqHeader(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil {
			t.logger.Error("failed to parse response body, skipping RespBodyToReqHeader Template processing", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		if jsonResponse == nil {
			t.logger.Debug("Skipping RespBodyToReqHeader Template processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, tcs[j].HTTPReq.Header, jsonResponse)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processReqHeadersToReqHeader(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			compareReqHeaders(t.logger, tcs[j].HTTPReq.Header, tcs[i].HTTPReq.Header)
		}
	}
}

func (t *Tools) processRespBodyToReqURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, &tcs[j].HTTPReq.URL, jsonResponse)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processRespBodyToReqBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonRequest, jsonResponse)
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs); i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		addTemplates(t.logger, jsonResponse, jsonRequest)
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processReqURLToRespBodySameTest(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs); i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		addTemplates(t.logger, &tcs[i].HTTPReq.URL, jsonResponse)
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processRespBodyToRespBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonResponse2, err := parseIntoJSON(tcs[j].HTTPResp.Body)
			if err != nil || jsonResponse2 == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonResponse2, jsonResponse)
			tcs[j].HTTPResp.Body = marshalJSON(jsonResponse2, t.logger)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processReqBodyToRespBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonResponse, err := parseIntoJSON(tcs[j].HTTPResp.Body)
			if err != nil || jsonResponse == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonResponse, jsonRequest)
			tcs[j].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
		}
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
	}
}

func (t *Tools) processReqBodyToReqURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, &tcs[j].HTTPReq.URL, jsonRequest)
		}
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
	}
}

func (t *Tools) processReqBodyToReqBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonRequest2, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest2 == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonRequest2, jsonRequest)
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest2, t.logger)
		}
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
	}
}

func (t *Tools) processReqURLToReqBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonRequest, &tcs[i].HTTPReq.URL)
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		}
	}
}

func (t *Tools) processReqURLToRespBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := 0; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonResponse, err := parseIntoJSON(tcs[j].HTTPResp.Body)
			if err != nil || jsonResponse == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonResponse, &tcs[i].HTTPReq.URL)
			tcs[j].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
		}
	}
}

func (t *Tools) processReqURLToReqURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, &tcs[j].HTTPReq.URL, &tcs[i].HTTPReq.URL)
		}
	}
}

// Utility function to safely marshal JSON and log errors
func marshalJSON(data interface{}, logger *zap.Logger) string {
	jsonData, err := json.Marshal(data)
	if err != nil {
		utils.LogError(logger, err, "failed to marshal JSON data")
		return ""
	}
	return string(jsonData)
}

func parseIntoJSON(response string) (interface{}, error) {
	if response == "" {
		return nil, nil
	}
	// geko lib will maintain the order of the keys in the json.
	result, err := geko.JSONUnmarshal([]byte(response))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal the response: %v", err)
	}
	return result, nil
}

func RenderIfTemplatized(val interface{}) (bool, interface{}, error) {
	stringVal, ok := val.(string)
	if !ok {
		return false, val, nil
	}

	// Check if the value is a template.
	// Applied this nolint to ignore the staticcheck error here because of readability
	// nolint:staticcheck
	if !(strings.Contains(stringVal, "{{") && strings.Contains(stringVal, "}}")) {
		return false, val, nil
	}

	// Get the value from the template.
	val, err := render(stringVal)
	if err != nil {
		return false, val, err
	}

	return true, val, nil
}

func addTemplates(logger *zap.Logger, interface1 interface{}, interface2 interface{}) bool {
	switch v := interface1.(type) {
	case geko.ObjectItems:
		keys := v.Keys()
		vals := v.Values()
		for i := range keys {
			var err error
			var isTemplatized bool
			original := vals[i]
			isTemplatized, vals[i], err = RenderIfTemplatized(vals[i])
			if err != nil {
				return false
			}
			switch vals[i].(type) {
			case string:
				x := vals[i].(string)
				addTemplates(logger, &x, interface2)
				vals[i] = x
			case float32, float64, int, int64:
				x := interface{}(vals[i])
				addTemplates(logger, &x, interface2)
				vals[i] = x
			default:
				addTemplates(logger, vals[i], interface2)
			}
			if isTemplatized {
				v.SetValueByIndex(i, original)
			} else {
				v.SetValueByIndex(i, vals[i])
			}
		}
	case geko.Array:
		for i, val := range v.List {
			var err error
			var isTemplatized bool
			original := val
			isTemplatized, val, err = RenderIfTemplatized(val)
			if err != nil {
				return false
			}
			switch x := val.(type) {
			case string:
				addTemplates(logger, &x, interface2)
				v.List[i] = x
			case float32, float64, int, int64:
				x = interface{}(x)
				addTemplates(logger, &x, interface2)
				v.List[i] = x
			default:
				addTemplates(logger, v.List[i], interface2)
			}
			if isTemplatized {
				v.Set(i, original)
			} else {
				v.Set(i, v.List[i])
			}
		}
	case map[string]string:
		for key, val := range v {
			var isTemplatized bool
			original := val
			isTemplatized, val1, err := RenderIfTemplatized(val)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			// just a type assertion check though it should always be string.
			val, ok := (val1).(string)
			if !ok {
				continue
			}
			// Saving the auth type to add it to the template latet.
			authType := ""
			if key == "Authorization" && len(strings.Split(val, " ")) > 1 {
				authType = strings.Split(val, " ")[0]
				val = strings.Split(val, " ")[1]
			}
			ok = valueMatchAndTemplatize(logger, &val, interface2)
			if !ok {
				continue
			}
			if key == "Authorization" && len(strings.Split(val, " ")) > 1 {
				val = authType + " " + val
			}

			if isTemplatized {
				v[key] = original
			} else {
				v[key] = val
			}
		}
	case *string:
		original := *v
		isTemplatized, tempVal, err := RenderIfTemplatized(*v)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return false
		}
		var ok bool
		// just a type assertion check though it should always be string.
		*v, ok = (tempVal).(string)
		if !ok {
			return false
		}

		// passing this v as reference so that it can be changed in the valueMatchAndTemplatize function if required.
		ok = valueMatchAndTemplatize(logger, v, interface2)
		if ok {
			return true
		}

		originalURL, err := url.Parse(original)
		if err != nil {
			return false
		}

		url, err := url.Parse(*v)
		if err != nil || url.Scheme == "" || url.Host == "" {
			return false
		}

		// Checking the special case of the URL for path and query parameters.
		urlParts := strings.Split(url.Path, "/")
		originalURLParts := strings.Split(originalURL.Path, "/")
		// checking if the last part of the URL is a template.
		ok = valueMatchAndTemplatize(logger, &urlParts[len(urlParts)-1], interface2)
		if isTemplatized {
			urlParts[len(urlParts)-1] = originalURLParts[len(originalURLParts)-1]
		}

		url.Path = strings.Join(urlParts, "/")

		if url.RawQuery != "" {
			// Only pass the values of the query parameters to the valueMatchAndTemplatize function.
			queryParams := strings.Split(url.RawQuery, "&")
			for i, param := range queryParams {
				param = strings.Split(param, "=")[1]
				valueMatchAndTemplatize(logger, &param, interface2)
				// reconstruct the query parameter with the templatized value if any.
				queryParams[i] = strings.Split(queryParams[i], "=")[0] + "=" + param
			}
			// reconstruct the URL with the templatized query parameters.
			url.RawQuery = strings.Join(queryParams, "&")
			*v = fmt.Sprintf("%s://%s%s?%s", url.Scheme, url.Host, url.Path, url.RawQuery)
			return true
		}
		// reconstruct the URL with the templatized path.
		*v = fmt.Sprintf("%s://%s%s", url.Scheme, url.Host, url.Path)
		return ok
	case *interface{}:
		switch w := (*v).(type) {
		case float64, int64, int, float32:
			var val string
			switch x := w.(type) {
			case float64:
				val = utils.ToString(x)
			case int64:
				val = utils.ToString(x)
			case int:
				val = utils.ToString(x)
			case float32:
				val = utils.ToString(x)
			}
			valueMatchAndTemplatize(logger, &val, interface2)
			parts := strings.Split(val, " ")
			if len(parts) > 1 { // if the value is a template.
				parts1 := strings.Split(parts[0], "{{")
				if len(parts1) > 1 {
					val = parts1[0] + "{{" + getType(w) + " " + parts[1] + "}}"
				}
				*v = val
				return true
			}
		default:
			logger.Error("unsupported type while templatizing", zap.Any("type", w))
			return false
		}
	}
	return false
}

// valueMatchAndTemplatize performs a recursive deep comparison between a target value and a JSON structure
// to identify matching values that should be templatized. It works by:
//  1. Taking a target string value (val1) to compare against values in the JSON structure (body)
//  2. Recursively traversing the JSON structure (body), handling all nested object types
//  3. When a matching value is found, it creates a template variable in utils.TemplatizedValues
//  4. It replaces both the original value and the matching value with template expressions
//  5. It handles various data types (string, float, int) and ensures proper template formatting
//
// This function is a core part of the templatization process that identifies common values
// across different parts of requests and responses, allowing dynamic values to be consistently
// replaced during test replay.
//
// Returns true if a match was found and templatization was performed.
func valueMatchAndTemplatize(logger *zap.Logger, val1 *string, body interface{}) bool {
	// Handle different types of JSON structures with type switching
	switch b := body.(type) {
	case geko.ObjectItems:
		// Process key-value pairs in JSON objects
		keys := b.Keys()
		vals := b.Values()
		for i, key := range keys {
			// Check if the value is already templatized
			var err error
			var isTemplatized bool
			original := vals[i]
			isTemplatized, vals[i], err = RenderIfTemplatized(vals[i])
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}

			// Recursively process different value types
			var ok bool
			switch vals[i].(type) {
			case string:
				x := vals[i].(string)
				ok = valueMatchAndTemplatize(logger, val1, &x)
				vals[i] = x
			case float32:
				x := vals[i].(float32)
				ok = valueMatchAndTemplatize(logger, val1, &x)
				vals[i] = x
			case int:
				x := vals[i].(int)
				ok = valueMatchAndTemplatize(logger, val1, &x)
				vals[i] = x
			case int64:
				x := vals[i].(int64)
				ok = valueMatchAndTemplatize(logger, val1, &x)
				vals[i] = x
			case float64:
				x := vals[i].(float64)
				ok = valueMatchAndTemplatize(logger, val1, &x)
				vals[i] = x
			default:
				// Handle other types recursively
				ok = valueMatchAndTemplatize(logger, val1, vals[i])
			}

			// If a match was found and the value type isn't another object, templatize it
			if ok && reflect.TypeOf(vals[i]) != reflect.TypeOf(b) {
				// Create a unique key for this value in the template map
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)

				// Format the value as a template expression
				vals[i] = fmt.Sprintf("{{%s .%v }}", getType(vals[i]), newKey)

				// Update the object with the templatized value
				b.SetValueByIndex(i, vals[i])

				// Also templatize the target value
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)
				return true
			}

			// Restore original value if it was already templatized
			if isTemplatized {
				vals[i] = original
			}

		}

	case geko.Array:
		// Process array elements
		for i, v := range b.List {
			// Handle different element types
			switch x := v.(type) {
			case string:
				valueMatchAndTemplatize(logger, val1, &x)
				b.List[i] = x
			case float32:
				valueMatchAndTemplatize(logger, val1, &x)
				b.List[i] = x
			case int:
				valueMatchAndTemplatize(logger, val1, &x)
				b.List[i] = x
			case int64:
				valueMatchAndTemplatize(logger, val1, &x)
				b.List[i] = x
			case float64:
				valueMatchAndTemplatize(logger, val1, &x)
				b.List[i] = x
			default:
				// Process other array element types
				valueMatchAndTemplatize(logger, val1, b.List[i])
			}
			// Update the array with processed values
			b.Set(i, b.List[i])
		}

	case map[string]string:
		// Process string map entries
		for key, val2 := range b {
			// Check if value is already templatized
			var isTemplatized bool
			original := val2
			isTemplatized, tempVal, err := RenderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}

			// Type assertion to string
			val2, ok := (tempVal).(string)
			if !ok {
				continue
			}

			// Direct value comparison for string maps
			if *val1 == val2 {
				// Values match - create template variable and update both
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				b[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)
				return true
			}

			// Restore original if templatized
			if isTemplatized {
				b[key] = original
			}

		}
		return false

	case *string:
		// Direct string comparison
		_, tempVal, err := RenderIfTemplatized(b)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return false
		}

		// Type assertion
		b, ok := (tempVal).(*string)
		if !ok {
			return false
		}

		// Check if strings match exactly
		if *val1 == *b {
			return true
		}

	case map[string]interface{}:
		// Process generic map entries
		for key, val2 := range b {
			// Check if already templatized
			var err error
			var isTemplatized bool
			original := val2
			isTemplatized, val2, err = RenderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}

			// Process different value types
			var ok bool
			switch x := val2.(type) {
			case string:
				ok = valueMatchAndTemplatize(logger, val1, &x)
			case float32:
				ok = valueMatchAndTemplatize(logger, val1, &x)
			case int:
				ok = valueMatchAndTemplatize(logger, val1, &x)
			case int64:
				ok = valueMatchAndTemplatize(logger, val1, &x)
			case float64:
				ok = valueMatchAndTemplatize(logger, val1, &x)
			default:
				// Handle other map value types
				ok = valueMatchAndTemplatize(logger, val1, val2)
			}

			// If match found, templatize both values
			if ok {
				// Create unique template key
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}

				// Format as templates
				b[key] = fmt.Sprintf("{{%s .%v}}", getType(b[key]), newKey)
				*val1 = fmt.Sprintf("{{%s .%v}}", getType(*val1), newKey)
			} else if isTemplatized {
				// Restore original if no match but was templatized
				b[key] = original
			}
		}
	case *float64, *int64, *int, *float32:
		// Compare numeric pointers by converting to string
		var val string
		switch x := b.(type) {
		case *float64:
			val = utils.ToString(*x)
		case *int64:
			val = utils.ToString(*x)
		case *int:
			val = utils.ToString(*x)
		case *float32:
			val = utils.ToString(*x)
		}

		// Direct string comparison with numeric value
		if *val1 == val {
			return true
		}

	case []interface{}:
		// Process slice elements
		for i, val := range b {
			// Process different element types
			switch x := val.(type) {
			case string:
				valueMatchAndTemplatize(logger, val1, &x)
				b[i] = x
			case float32:
				valueMatchAndTemplatize(logger, val1, &x)
				b[i] = x
			case int:
				valueMatchAndTemplatize(logger, val1, &x)
				b[i] = x
			case int64:
				valueMatchAndTemplatize(logger, val1, &x)
				b[i] = x
			case float64:
				valueMatchAndTemplatize(logger, val1, &x)
				b[i] = x
			default:
				// Handle other slice element types
				valueMatchAndTemplatize(logger, val1, b[i])
			}
			// Update the slice with processed values
			b[i] = val
		}
	}

	// No match found
	return false
}

func getType(val interface{}) string {
	switch val.(type) {
	case string, *string:
		return "string"
	case int64, int, int32, *int64, *int, *int32:
		return "int"
	case float64, float32, *float64, *float32:
		return "float"
	case bool, *bool:
		return "bool"
	case []interface{}, map[string]interface{}, geko.ObjectItems, geko.Array:
		return "object"
	case nil:
		return ""
	default:
		// Log the unknown type to help with future type additions
		typeName := fmt.Sprintf("%T", val)
		return fmt.Sprintf("unknown-%s", typeName)
	}
}

// This function returns a unique key for each value, for instance if id already exists, it will return id1.
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
	for myMap[key] != value {
		if _, exists := myMap[key]; !exists {
			myMap[key] = value
			break
		}
		i++
		key = baseKey + strconv.Itoa(i)
	}
	return key
}

// render function processes template strings containing one or more template expressions.
// It handles various data types (string, int, float) and returns the appropriate Go type.
func render(val string) (interface{}, error) {
	// This is a map of helper functions that is used to convert the values to their appropriate types.
	funcMap := template.FuncMap{
		"int":    utils.ToInt,
		"string": utils.ToString,
		"float":  utils.ToFloat,
	}

	// Extract all template expressions using regex
	re := regexp.MustCompile(`\{\{[^{}]*\}\}`)
	matches := re.FindAllString(val, -1)

	// If there are multiple template expressions, process each one separately
	if len(matches) > 1 {
		result := val
		for _, match := range matches {
			processed, err := processSingleTemplate(match, funcMap)
			if err != nil {
				return val, err
			}
			result = strings.Replace(result, match, fmt.Sprintf("%v", processed), 1)
		}
		return result, nil
	}

	// For a single template or no templates, use the standard processing
	return processSingleTemplate(val, funcMap)
}

// processSingleTemplate handles a single template expression
func processSingleTemplate(val string, funcMap template.FuncMap) (interface{}, error) {
	tmpl, err := template.New("template").Funcs(funcMap).Parse(val)
	if err != nil {
		return val, fmt.Errorf("failed to parse the testcase using template %v", zap.Error(err))
	}

	var output bytes.Buffer
	err = tmpl.Execute(&output, utils.TemplatizedValues)
	if err != nil {
		return val, fmt.Errorf("failed to execute the template %v", zap.Error(err))
	}

	/*
		Determine the appropriate return type based on the template directive
		The type conversion is already handled by the template functions in funcMap,
		so we don't need an additional switch statement
	*/
	// if strings.Contains(val, "string") {
	// 	return output.String(), nil
	// } else if strings.Contains(val, "int") {
	// 	return utils.ToInt(output.String()), nil
	// } else if strings.Contains(val, "float") {
	// 	return utils.ToFloat(output.String()), nil
	// }

	// Remove quotes for non-string values
	return strings.Trim(output.String(), `"`), nil
}

// Compare the headers of 2 utils.TemplatizedValues requests and add the templates.
func compareReqHeaders(logger *zap.Logger, req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		// Check if the value is already present in the templatized values.
		var isTemplatized1 bool
		original1 := val1
		isTemplatized1, tempVal, err := RenderIfTemplatized(val1)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return
		}
		val, ok := (tempVal).(string)
		if !ok {
			continue
		}
		val1 = val
		if val2, ok := req2[key]; ok {
			var isTemplatized2 bool
			original2 := val2
			isTemplatized2, tempVal, err := RenderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return
			}
			val, ok = (tempVal).(string)
			if !ok {
				continue
			}
			val2 = val
			if val1 == val2 {
				// Trim the extra space in the string.
				val2 = strings.Trim(val2, " ")
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				req2[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
				req1[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
			} else {
				if isTemplatized2 {
					req2[key] = original2
				}
				if isTemplatized1 {
					req1[key] = original1
				}
			}
		}
	}
}

// Removing quotes in templates for non string types like float, int, etc. Because they interfere with the templating engine.
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

// Add quotes to the template if it is not of the type string. eg: "{{float .key}}" ,{{int .key}}
func addQuotesInTemplates(jsonStr string) string {
	// Regular expression to find patterns with {{ and }}
	re := regexp.MustCompile(`\{\{[^{}]*\}\}`)
	// Function to replace matches by removing surrounding quotes
	result := re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}
		//Add the surrounding quotes.
		return `"` + match + `"`
	})
	return result
}
