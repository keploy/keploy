package replay

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

func (r *Replayer) Templatize(ctx context.Context) error {
	testSets := r.config.Templatize.TestSets

	if len(testSets) == 0 {
		all, err := r.GetAllTestSetIDs(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get all test sets")
			return err
		}
		testSets = all
	}

	if len(testSets) == 0 {
		r.logger.Warn("No test sets found to templatize")
		return nil
	}

	for _, testSetID := range testSets {

		testSet, err := r.TestSetConf.Read(ctx, testSetID)
		utils.TemplatizedValues = map[string]interface{}{}
		if err == nil && testSet != nil {
			utils.TemplatizedValues = testSet.Template
		}

		tcs, err := r.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get test cases")
			return err
		}

		if len(tcs) == 0 {
			r.logger.Warn("The test set is empty. Please record some testcases to templatize.", zap.String("testSet", testSetID))
			continue
		}

		// Add the quotes back to the templates before using it.
		// Because the templating engine needs the quotes to properly parse the JSON.
		// Instead of {{float .key}} it should be "{{float .key}}" but in the response body it is saved as {{float .key}}
		for _, tc := range tcs {
			tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
			tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
		}

		// CASE:1
		// Compare the response of ith testcase with i+1->n request headers.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json. Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			}
			if jsonResponse == nil {
				continue
			}

			// addTemplates where response key is matched to some header key in the next testcases.
			for j := i + 1; j < len(tcs); j++ {
				addTemplates(r.logger, tcs[j].HTTPReq.Header, &jsonResponse)
			}

			// Now modify the response body to get templatized body if any.
			jsonData, err := json.Marshal(jsonResponse)
			if err != nil {
				utils.LogError(r.logger, err, "failed to marshal json data of templatized response")
				return err
			}
			tcs[i].HTTPResp.Body = string(jsonData)
		}

		// CASE:2
		// Compare the requests headers for the common fields.
		for i := 0; i < len(tcs)-1; i++ {
			// Check for headers first.
			for j := i + 1; j < len(tcs); j++ {
				compareReqHeaders(r.logger, tcs[i].HTTPReq.Header, tcs[j].HTTPReq.Header)
			}
		}

		// CASE:3
		// Check the url of the request for any common fields in the response.
		// Compare the response of ith testcase with i+1->n reques urls.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json.  Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			}

			if jsonResponse == nil {
				continue
			}

			// Add the templates where the response key is matched to some url in the next testcases.
			for j := i + 1; j < len(tcs); j++ {
				addTemplates(r.logger, &tcs[j].HTTPReq.URL, &jsonResponse)
			}

			// Now modify the response body to get templatized body if any.
			jsonData, err := json.Marshal(jsonResponse)
			if err != nil {
				utils.LogError(r.logger, err, "failed to marshal json data")
				return err
			}
			tcs[i].HTTPResp.Body = string(jsonData)
		}

		// CASE:4
		// Compare the req and resp body for any common fields.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json. Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			}

			if jsonResponse == nil {
				continue
			}

			for j := i + 1; j < len(tcs); j++ {
				jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
				if err != nil {
					r.logger.Debug("failed to parse request into json. Not templatizing the request of this test.", zap.Error(err), zap.Any("testcase:", tcs[j].Name))
					continue
				}

				if jsonRequest == nil {
					continue
				}
				addTemplates(r.logger, jsonRequest, &jsonResponse)
				jsonData, err := json.Marshal(jsonRequest)
				if err != nil {
					utils.LogError(r.logger, err, "failed to marshal json data")
					continue
				}
				tcs[j].HTTPReq.Body = string(jsonData)
			}
			jsonData, err := json.Marshal(jsonResponse)
			if err != nil {
				utils.LogError(r.logger, err, "failed to marshal json data")
				return err
			}
			tcs[i].HTTPResp.Body = string(jsonData)
		}

		// Updating all the testcases.
		for _, tc := range tcs {
			tc.HTTPReq.Body = removeQuotesInTemplates(tc.HTTPReq.Body)
			tc.HTTPResp.Body = removeQuotesInTemplates(tc.HTTPResp.Body)
			err = r.testDB.UpdateTestCase(ctx, tc, testSetID)
			if err != nil {
				utils.LogError(r.logger, err, "failed to update test case")
				return err
			}
		}

		// Remove the double quotes from the templatized values in testSet configuration.
		removeDoubleQuotes(utils.TemplatizedValues)

		err = r.TestSetConf.Write(ctx, testSetID, &models.TestSet{
			PreScript:  "",
			PostScript: "",
			Template:   utils.TemplatizedValues,
		})
		if err != nil {
			utils.LogError(r.logger, err, "failed to write test set")
			return err
		}
	}

	return nil
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

// Here we simplify the first interface to a string form and then call the second function to simplify the second interface.
// TODO: add better comment here.
func addTemplates(logger *zap.Logger, interface1 interface{}, interface2 *interface{}) {
	switch v := interface1.(type) {
	case geko.ObjectItems:
		keys := v.Keys()
		vals := v.Values()
		for i := range keys {
			var err error
			vals[i], err = renderIfTemplatized(vals[i])
			if err != nil {
				return
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
				return
			}
			addTemplates(logger, val, interface2)
			v[key] = val
		}
	case map[string]string:
		for key, val := range v {
			val1, err := renderIfTemplatized(val)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return
			}
			val, ok := (val1).(string)
			if !ok {
				return
			}
			// Saving the auth type to add it to the template later.
			authType := ""
			if key == "Authorization" && len(strings.Split(val, " ")) > 1 {
				authType = strings.Split(val, " ")[0]
				val = strings.Split(val, " ")[1]
			}
			ok = addTemplates1(logger, &val, interface2)
			if !ok {
				return
			}
			// Add the authtype to the string.
			val = authType + " " + val
			v[key] = val
		}
	case *string:
		tempVal, err := renderIfTemplatized(*v)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return
		}
		*v = (tempVal).(string)
		ok := addTemplates1(logger, v, interface2)
		if ok {
			return
		}
		url, err := url.Parse(*v)
		if err == nil {
			urlParts := strings.Split(url.Path, "/")
			addTemplates1(logger, &urlParts[len(urlParts)-1], interface2)
			url.Path = strings.Join(urlParts, "/")
			if url.RawQuery != "" {
				// Only pass the values of the query parameters to the addTemplates1 function.
				queryParams := strings.Split(url.RawQuery, "&")
				for i, param := range queryParams {
					param = strings.Split(param, "=")[1]
					addTemplates1(logger, &param, interface2)
					queryParams[i] = strings.Split(queryParams[i], "=")[0] + "=" + param
				}
				url.RawQuery = strings.Join(queryParams, "&")
				*v = fmt.Sprintf("%s://%s%s?%s", url.Scheme, url.Host, url.Path, url.RawQuery)
			} else {
				*v = fmt.Sprintf("%s://%s%s", url.Scheme, url.Host, url.Path)
			}
		} else {
			addTemplates1(logger, v, interface2)
		}
	case float64, int64, int, float32:
		val := toString(v)
		addTemplates1(logger, &val, interface2)
		parts := strings.Split(val, " ")
		if len(parts) > 1 {
			parts1 := strings.Split(parts[0], "{{")
			if len(parts1) > 1 {
				val = parts1[0] + "{{" + getType(v) + " " + parts[1] + "}}"
			}
		}
	}
}

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
			if ok && reflect.TypeOf(vals[i]) != reflect.TypeOf(b) {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				vals[i] = fmt.Sprintf("{{%s .%v }}", getType(vals[i]), newKey)
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
				return false
			}
			if *val1 == val2 {
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
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
		if *val1 == toString(b) {
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

// Simplify the first response into type string for comparison.
func compareResponses(response1, response2 *interface{}, key string) {
	switch v1 := (*response1).(type) {
	case geko.Array:
		for _, val1 := range v1.List {
			compareResponses(&val1, response2, "")
		}
	case geko.ObjectItems:
		keys := v1.Keys()
		vals := v1.Values()
		for i := range keys {
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

// Simplify the second response into type string for comparison.
func compareSecondResponse(val1 *string, response2 *interface{}, key1 string, key2 string) {
	switch v2 := (*response2).(type) {
	case geko.Array:
		for _, val2 := range v2.List {
			compareSecondResponse(val1, &val2, key1, "")
		}

	case geko.ObjectItems:
		keys := v2.Keys()
		vals := v2.Values()
		for i := range keys {
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

// Remove all types of value to strings for comparison.
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
		return utils.ToInt(val), nil
	case strings.Contains(val, "float"):
		return utils.ToFloat(val), nil
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
			return
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
				return
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

// TODO: check why without single quotes values are being passed in the template map.
// This is used to handle the case where the value gets both single quotes and double quotes and the templating engine is not able to handle it.
// eg: '"Not/A)Brand";v="8", "Chromium";v="126", "Brave";v="126"' can't be handled by the templating engine.
// Not/A)Brand;v=8, Chromium;v=126, Brave;v=126 can be handled.
func removeDoubleQuotes(tempMap map[string]interface{}) {
	// Remove double quotes
	for key, val := range tempMap {
		if str, ok := val.(string); ok {
			tempMap[key] = strings.ReplaceAll(str, `"`, "")
		}
	}
}
