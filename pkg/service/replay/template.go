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

func (r *Replayer) Templatize(ctx context.Context, testSets []string) error {
	if len(testSets) == 0 {
		if len(r.config.Templatize.TestSets) == 0 {
			allTestSets, err := r.GetAllTestSetIDs(ctx)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get all test sets")
				return err
			}
			testSets = allTestSets
		} else {
			testSets = r.config.Templatize.TestSets
		}
	}
	for _, testSetID := range testSets {
		testSet, err := r.TestSetConf.Read(ctx, testSetID)
		if err != nil || testSet == nil {
			utils.TemplatizedValues = map[string]interface{}{}
		} else {
			utils.TemplatizedValues = testSet.Template
		}
		tcs, err := r.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get test cases")
			return err
		}
		if len(tcs) == 0 {
			r.logger.Warn("The test set is empty. Please record some testcases to templatize.", zap.String("testSet:", testSetID))
			continue
		}
		// Add the quotes back to the templates before using it.
		for _, tc := range tcs {
			tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
			tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
		}
		// Compare the response to the header.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json. Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			} else if jsonResponse == nil {
				continue
			}
			// Compare the keys to the headers.
			for j := i + 1; j < len(tcs); j++ {
				addTemplates(tcs[j].HTTPReq.Header, &jsonResponse)
			}
			// Add the jsonResponse back to tcs.
			jsonData, err := json.Marshal(jsonResponse)
			if err != nil {
				utils.LogError(r.logger, err, "failed to marshal json data")
				return err
			}
			tcs[i].HTTPResp.Body = string(jsonData)
		}

		// Compare the requests for the common fields.
		for i := 0; i < len(tcs)-1; i++ {
			// Check for headers first.
			for j := i + 1; j < len(tcs); j++ {
				compareReqHeaders(tcs[i].HTTPReq.Header, tcs[j].HTTPReq.Header)
			}
		}

		// Check the url for any common fields.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json.  Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			} else if jsonResponse == nil {
				continue
			}
			for j := i + 1; j < len(tcs); j++ {
				addTemplates(&tcs[j].HTTPReq.URL, &jsonResponse)
			}
			// Record the new testcase.
			jsonData, err := json.Marshal(jsonResponse)
			if err != nil {
				utils.LogError(r.logger, err, "failed to marshal json data")
				return err
			}
			tcs[i].HTTPResp.Body = string(jsonData)
		}

		// Check the location header for any common fields.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json.  Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			} else if jsonResponse == nil {
				continue
			}
			for j := i + 1; j < len(tcs); j++ {
				// Check if there is the Location header in the headers.
				for key, val := range tcs[j].HTTPReq.Header {
					if key == "Location" {
						addTemplates(&val, &jsonResponse)
					}
				}
			}
		}

		// Compare the req and resp body for any common fields.
		for i := 0; i < len(tcs)-1; i++ {
			jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
			if err != nil {
				r.logger.Debug("failed to parse response into json. Not templatizing the response of this test.", zap.Error(err), zap.Any("testcase:", tcs[i].Name))
				continue
			} else if jsonResponse == nil {
				continue
			}
			for j := i + 1; j < len(tcs); j++ {
				jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
				if err != nil {
					r.logger.Debug("failed to parse request into json. Not templatizing the request of this test.", zap.Error(err), zap.Any("testcase:", tcs[j].Name))
					continue
				} else if jsonRequest == nil {
					continue
				}
				addTemplates(jsonResponse, &jsonRequest)
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

		noQuotes(utils.TemplatizedValues)
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
// Parse the json string into a geko type variable.
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

func checkForTemplate(val interface{}) interface{} {
	stringVal, ok := val.(string)
	if ok {
		if strings.HasPrefix(stringVal, "{{") && strings.HasSuffix(stringVal, "}}") {
			// Get the value from the template.
			val, _ = render(stringVal)
			if !strings.Contains(stringVal, "string") {
				// Convert to its appropriate type.
				if strings.Contains(stringVal, "int") {
					val = utils.ToInt(val)
				} else if strings.Contains(stringVal, "float") {
					val = utils.ToFloat(val)
				}
			}
		}
	}
	return val
}

// Here we simplify the first interface to a string form and then call the second function to simplify the second interface.
func addTemplates(interface1 interface{}, interface2 *interface{}) {
	switch v := interface1.(type) {
	case geko.ObjectItems:
		keys := v.Keys()
		vals := v.Values()
		for i := range keys {
			vals[i] = checkForTemplate(vals[i])
			addTemplates(vals[i], interface2)
			v.SetValueByIndex(i, vals[i])
		}
	case geko.Array:
		for _, val := range v.List {
			addTemplates(&val, interface2)
		}
	case map[string]interface{}:
		for key, val := range v {
			val = checkForTemplate(val)
			addTemplates(val, interface2)
			v[key] = val
		}
	case map[string]string:
		for key, val := range v {
			val, ok := checkForTemplate(val).(string)
			if !ok {
				return
			}
			// Saving the auth type to add it to the template later.
			authType := ""
			if key == "Authorization" && len(strings.Split(val, " ")) > 1 {
				authType = strings.Split(val, " ")[0]
				val = strings.Split(val, " ")[1]
			}
			ok = addTemplates1(&val, interface2)
			if !ok {
				return
			}
			// Add the authtype to the string.
			val = authType + " " + val
			v[key] = val
		}
	case *string:
		*v = checkForTemplate(*v).(string)
		ok := addTemplates1(v, interface2)
		if ok {
			return
		}
		url, err := url.Parse(*v)
		if err == nil {
			urlParts := strings.Split(url.Path, "/")
			addTemplates1(&urlParts[len(urlParts)-1], interface2)
			url.Path = strings.Join(urlParts, "/")
			if url.RawQuery != "" {
				// Only pass the values of the query parameters to the addTemplates1 function.
				queryParams := strings.Split(url.RawQuery, "&")
				for i, param := range queryParams {
					param = strings.Split(param, "=")[1]
					addTemplates1(&param, interface2)
					queryParams[i] = strings.Split(queryParams[i], "=")[0] + "=" + param
				}
				url.RawQuery = strings.Join(queryParams, "&")
				*v = fmt.Sprintf("%s://%s%s?%s", url.Scheme, url.Host, url.Path, url.RawQuery)
			} else {
				*v = fmt.Sprintf("%s://%s%s", url.Scheme, url.Host, url.Path)
			}
		} else {
			addTemplates1(v, interface2)
		}
	case float64, int64, int, float32:
		val := toString(v)
		addTemplates1(&val, interface2)
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
func addTemplates1(val1 *string, body *interface{}) bool {
	switch b := (*body).(type) {
	case geko.ObjectItems:
		keys := b.Keys()
		vals := b.Values()
		for i, key := range keys {
			vals[i] = checkForTemplate(vals[i])
			ok := addTemplates1(val1, &vals[i])
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
			addTemplates1(val1, &v)
		}
	case map[string]string:
		for key, val2 := range b {
			val2, ok := checkForTemplate(val2).(string)
			if !ok {
				return false
			}
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
		b, ok := checkForTemplate(b).(string)
		if !ok {
			return false
		}
		if *val1 == b {
			return true
		}
	case map[string]interface{}:
		for key, val2 := range b {
			val2 = checkForTemplate(val2)
			ok := addTemplates1(val1, &val2)
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
			addTemplates1(val1, &val)
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

// This function renders the template using the templatized values.
func render(testCaseStr string) (string, error) {
	// This maps the contents inside the
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
		return testCaseStr, fmt.Errorf("failed to parse the testcase using template %v", zap.Error(err))
	}
	var output bytes.Buffer
	err = tmpl.Execute(&output, utils.TemplatizedValues)
	if err != nil {
		return testCaseStr, fmt.Errorf("failed to execute the template %v", zap.Error(err))
	}
	if ok {
		outputString := strings.Trim(output.String(), `"`)
		return outputString, nil
	}
	return output.String(), nil
}

// Compare the headers of 2 requests and add the templates.
func compareReqHeaders(req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		// Check if the value is already present in the templatized values.
		val, ok := checkForTemplate(val1).(string)
		if !ok {
			return
		}
		val1 = val
		if val2, ok := req2[key]; ok {
			val, ok = checkForTemplate(val2).(string)
			if !ok {
				return
			}
			val2 = val
			if val1 == val2 {
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

// Removing quotes in templates because they interfere with the templating engine.
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

// Add quotes to the template if it is of the type string. eg: "{{string .key}}"
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

func noQuotes(tempMap map[string]interface{}) {
	// Remove double quotes
	for key, val := range tempMap {
		if str, ok := val.(string); ok {
			tempMap[key] = strings.ReplaceAll(str, `"`, "")
		}
	}
}
