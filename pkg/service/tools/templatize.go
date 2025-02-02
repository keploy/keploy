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

	matcher "go.keploy.io/server/v2/pkg/matcher"

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

	// by-default
	for _, testSetID := range testSets {
		// Read test set configuration
		testSet, err := t.testSetConf.Read(ctx, testSetID)
		utils.TemplatizedValues = map[string]interface{}{}
		if err == nil && (testSet != nil && testSet.Template != nil) {
			utils.TemplatizedValues = testSet.Template
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

		// set the isChain to false for cli templatization.
		err = t.ProcessTestCases(ctx, tcs, false, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to process test cases")
			return err
		}
	}

	return nil
}

// Refactored method to process test cases
func (t *Tools) ProcessTestCases(ctx context.Context, tcs []*models.TestCase, isChain bool, testSetID string) error {
	utils.TemplatizedValues = map[string]interface{}{}
	// Add quotes back to templates
	for _, tc := range tcs {
		tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
	}

	t.ChainSet = make(map[string][]models.TestCase)

	// Process test cases for different scenarios
	t.processResponseToHeader(ctx, tcs)
	t.processRequestHeaders(ctx, tcs)
	t.processResponseToURL(ctx, tcs)
	t.processRequestResponseBodies(ctx, tcs)

	// Add the quotes back to the templates before using it.
	// Because the templating engine needs the quotes to properly parse the JSON.
	// Instead of {{float .key}} it should be "{{float .key}}" but in the response body it is saved as {{float .key}}
	for _, tc := range tcs {
		tc.HTTPReq.Body = removeQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = removeQuotesInTemplates(tc.HTTPResp.Body)
		err := t.testDB.UpdateTestCase(ctx, tc, testSetID, false)
		if err != nil {
			utils.LogError(t.logger, err, "failed to update test case")
			return err
		}
	}

	// Write the updated test set configuration
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

// CASE:1
// Compare the response of ith testcase with i+1->n request headers.
func (t *Tools) processResponseToHeader(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to header processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}

		// addTemplates where response key is matched to some header key in the next testcases.
		for j := i + 1; j < len(tcs); j++ {
			addTemplates(t.logger, tcs[j].HTTPReq.Header, &jsonResponse)
			// check if the tcs[j].HTTPReq.Header is modified that means the template is added log it.
			// if tcs[j].HTTPReq.Header != nil {
			// 	for key, val := range tcs[j].HTTPReq.Header {
			// 		if isTemplatized(tcs[j].HTTPReq.Header, val) {
			// 			// Log the addition of the new template
			// 			t.logger.Info("New template added for test",
			// 				zap.String("testcase", tcs[j].Name),
			// 				zap.String("templateKey", key),
			// 				zap.String("templateValue", val),
			// 				zap.String("context", "HTTPReq.Header"),
			// 			)
			// 		}
			// 	}
			// }
		}
		// Now modify the response body to get templatized body if any.
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

// CASE:2
// Compare the requests headers for the common fields.
func (t *Tools) processRequestHeaders(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		// Check for headers first.
		for j := i + 1; j < len(tcs); j++ {
			compareReqHeaders(t.logger, tcs[i].HTTPReq.Header, tcs[j].HTTPReq.Header)
			// check if the tcs[j].HTTPReq.Header is modified that means the template is added log it.
			// if tcs[j].HTTPReq.Header != nil {
			// 	for key, val := range tcs[j].HTTPReq.Header {
			// 		if isTemplatized(tcs[i].HTTPReq.Header[key], val) {
			// 			// Log the addition of the new template
			// 			t.logger.Info("New template added for test",
			// 				zap.String("testcase", tcs[j].Name),
			// 				zap.String("templateKey", key),
			// 				zap.String("templateValue", val),
			// 				zap.String("context", "HTTPReq.Header"),
			// 			)
			// 		}
			// 	}
			// }
		}
	}
}

// CASE:3
// Check the url of the request for any common fields in the response.
// Compare the response of ith testcase with i+1->n reques urls.
func (t *Tools) processResponseToURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		// parent
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		var tempList []models.TestCase
		// Add the templates where the response key is matched to some url in the next testcases.
		for j := i + 1; j < len(tcs); j++ {
			// prev := tcs[j].HTTPReq.URL
			isTemplatize := addTemplates(t.logger, &tcs[j].HTTPReq.URL, &jsonResponse)
			// check if the tcs[j].HTTPReq.URL is modified that means the template is added log it.
			if tcs[j].HTTPReq.URL != "" {
				// check the validity if its already templatized by any other test case.
				if isTemplatize {
					t.logger.Info("New template added for test",
						zap.String("testcase", tcs[j].Name),
						zap.String("templateKey", "URL"),
						zap.String("templateValue", tcs[j].HTTPReq.URL),
						zap.String("context", "HTTPReq.URL"),
					)
					tempList = append(tempList, *tcs[j])
				}

			}
		}

		if len(tempList) != 0 {
			fmt.Println("Adding parent: ", tcs[i].Name)
			tempList = append([]models.TestCase{*tcs[i]}, tempList...)
			t.ChainSet[tcs[i].Name] = tempList
		}

		// Now modify the response body to get templatized body if any.
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

// CASE:4
// Compare the req and resp body for any common fields.
func (t *Tools) processRequestResponseBodies(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}

		// Add the templates where the response key is matched to some request body in the next testcases.
		var tempList []models.TestCase
		for j := i + 1; j < len(tcs); j++ {
			jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}

			isTemplatized := addTemplates(t.logger, jsonRequest, &jsonResponse)
			// check if the tcs[j].HTTPReq.Body is modified that means the template is added log it.
			if tcs[j].HTTPReq.Body != "" {
				if isTemplatized {
					// Log the addition of the new template
					t.logger.Info("New template added for test",
						zap.String("testcase", tcs[j].Name),
						zap.String("templateKey", "Body"),
						zap.String("templateValue", tcs[j].HTTPReq.Body),
						zap.String("context", "HTTPReq.Body"),
					)
					tempList = append(tempList, *tcs[j])
				}
			}
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		}

		if len(tempList) != 0 {
			// add the parent testcase at the beginning of the list.
			fmt.Println("Adding parent: ", tcs[i].Name)
			tempList = append([]models.TestCase{*tcs[i]}, tempList...)
			t.ChainSet[tcs[i].Name] = tempList
		}

		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
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

// Below are the helper functions for templatize.

// Parse the json string into a geko type variable, it will maintain the order of the keys in the json.
func parseIntoJSON(response string) (interface{}, error) {
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

// renderIfTemplatized is used to check if the value is a template and then render it.
func renderIfTemplatized(val interface{}) (interface{}, error) {
	stringVal, ok := val.(string)
	if !ok {
		return val, nil
	}

	// Check if the value is a template.
	if !(strings.Contains(stringVal, "{{") && strings.Contains(stringVal, "}}")) {
		return val, nil
	}

	// Get the value from the template.
	val, err := render(stringVal)
	if err != nil {
		return val, err
	}

	return val, nil
}

func isTemplatized(original, templatized interface{}) bool {
	// Use reflection or go-cmp to compare the structures
	if !reflect.DeepEqual(original, templatized) {
		fmt.Println("Original: ", original)
		fmt.Println("Templatized: ", templatized)
		return true
	}

	// Additional logic to check for template markers like `{{` and `}}`
	// originalStr, ok1 := original.(string)
	// templatizedStr, ok2 := templatized.(string)
	// if ok1 && ok2 && strings.Contains(templatizedStr, "{{") {
	// 	// Check if the template is derived from the original
	// 	return strings.Contains(templatizedStr, originalStr)
	// }

	return false
}

// Here we simplify the first interface to a string form and then call the second function to simplify the second interface.
// TODO: add better comment here. & rename this function

func addTemplates(logger *zap.Logger, interface1 interface{}, interface2 *interface{}) bool {
	switch v := interface1.(type) {
	case geko.ObjectItems:
		keys := v.Keys()
		vals := v.Values()
		for i := range keys {
			var err error
			vals[i], err = renderIfTemplatized(vals[i])
			if err != nil {
				return false
			}
			addTemplates(logger, vals[i], interface2)
			// we change the current value also in the interface1
			v.SetValueByIndex(i, vals[i])
		}
	case geko.Array:
		for _, val := range v.List {
			addTemplates(logger, &val, interface2)
		}
	case map[string]interface{}:
		for key, val := range v {
			var err error
			val, err = renderIfTemplatized(val)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			addTemplates(logger, val, interface2)
			// we change the current value also in the interface1
			v[key] = val
		}
	case map[string]string:
		for key, val := range v {
			val1, err := renderIfTemplatized(val)
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
			ok = addTemplates1(logger, &val, interface2)
			if !ok {
				continue
			}
			// Add the authtype to the string.
			val = authType + " " + val
			v[key] = val
		}
	case *string:
		tempVal, err := renderIfTemplatized(*v)
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

		// passing this v as reference so that it can be changed in the addTemplates1 function if required.
		ok = addTemplates1(logger, v, interface2)
		if ok {
			return true
		}

		url, err := url.Parse(*v)
		if err != nil {
			ok = addTemplates1(logger, v, interface2)
			return ok
		}

		// Checking the special case of the URL for path and query parameters.
		urlParts := strings.Split(url.Path, "/")
		// checking if the last part of the URL is a template.

		ok = addTemplates1(logger, &urlParts[len(urlParts)-1], interface2)
		url.Path = strings.Join(urlParts, "/")

		if url.RawQuery != "" {
			// Only pass the values of the query parameters to the addTemplates1 function.
			queryParams := strings.Split(url.RawQuery, "&")
			for i, param := range queryParams {
				param = strings.Split(param, "=")[1]
				addTemplates1(logger, &param, interface2)
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
	case float64, int64, int, float32:
		//TODO: inspect this case because it is not being used.
		val := matcher.ToString(v)
		addTemplates1(logger, &val, interface2)
		// we convert the value to string in order to compare and add the template.
		// But we need to convert it back to the original type.
		parts := strings.Split(val, " ")
		if len(parts) > 1 { // if the value is a template.
			parts1 := strings.Split(parts[0], "{{")
			if len(parts1) > 1 {
				val = parts1[0] + "{{" + getType(v) + " " + parts[1] + "}}"
			}
		}
		return true
	}
	return false
}

// TODO: add better comment here and rename this function.
// Here we simplify the second interface and finally add the templates.
func addTemplates1(logger *zap.Logger, val1 *string, body *interface{}) bool {
	switch b := (*body).(type) {
	case geko.ObjectItems:
		keys := b.Keys()
		vals := b.Values()
		for i, key := range keys {
			var err error
			vals[i], err = renderIfTemplatized(vals[i])
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			ok := addTemplates1(logger, val1, &vals[i])
			// we can't change if the type of vals[i] is also object item.
			if ok && reflect.TypeOf(vals[i]) != reflect.TypeOf(b) {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				// if newKey == "" {
				// 	newKey = key
				// }
				vals[i] = fmt.Sprintf("{{%s .%v }}", getType(vals[i]), newKey)
				// Now change the value of the key in the object.
				b.SetValueByIndex(i, vals[i])
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)

				return true
			}
		}
	case geko.Array:
		for _, v := range b.List {
			addTemplates1(logger, val1, &v)
		}
	case map[string]string:
		for key, val2 := range b {
			tempVal, err := renderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			val2, ok := (tempVal).(string)
			if !ok {
				continue
			}
			if *val1 == val2 {
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				// if newKey == "" {
				// 	newKey = key
				// }
				b[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)

				return true
			}
		}
		return false
	case string:
		tempVal, err := renderIfTemplatized(b)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return false
		}
		b, ok := (tempVal).(string)
		if !ok {
			return false
		}
		if *val1 == b {
			return true
		}
	case map[string]interface{}:
		for key, val2 := range b {
			var err error
			val2, err = renderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			ok := addTemplates1(logger, val1, &val2)
			if ok {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{%s .%v}}", getType(b[key]), newKey)
				*val1 = fmt.Sprintf("{{%s .%v}}", getType(*val1), newKey)

			}
		}
	case float64, int64, int, float32:
		if *val1 == matcher.ToString(b) {
			return true
		}

	case []interface{}:
		for i, val := range b {
			addTemplates1(logger, val1, &val)
			b[i] = val
		}
	}
	return false
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
	//TODO: handle the default case properly, return some errot.
	return ""
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

// TODO: Make this function generic for one value of string containing more than one template value.
// Duplicate function is being used in Simulate function as well.

// render function gives the value of the templatized field.
func render(val string) (interface{}, error) {
	// This is a map of helper functions that is used to convert the values to their appropriate types.
	funcMap := template.FuncMap{
		"int":    utils.ToInt,
		"string": utils.ToString,
		"float":  utils.ToFloat,
	}

	tmpl, err := template.New("template").Funcs(funcMap).Parse(val)
	if err != nil {
		return val, fmt.Errorf("failed to parse the testcase using template %v", zap.Error(err))
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, utils.TemplatizedValues)
	if err != nil {
		return val, fmt.Errorf("failed to execute the template %v", zap.Error(err))
	}

	if strings.Contains(val, "string") {
		return output.String(), nil
	}

	// Remove the double quotes from the output for rest of the values. (int, float)
	outputString := strings.Trim(output.String(), `"`)

	// TODO: why do we need this when we have already declared the funcMap.
	// Convert this to the appropriate type and return.
	switch {
	case strings.Contains(val, "int"):
		return utils.ToInt(output.String()), nil
	case strings.Contains(val, "float"):
		return utils.ToFloat(output.String()), nil
	}

	return outputString, nil
}

// Compare the headers of 2 requests and add the templates.
func compareReqHeaders(logger *zap.Logger, req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		// Check if the value is already present in the templatized values.
		tempVal, err := renderIfTemplatized(val1)
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
			tempVal, err := renderIfTemplatized(val2)
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
