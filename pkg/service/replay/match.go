// Package replay provides functions for replaying requests and comparing responses.
package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"bytes"
	"os"

	"go.uber.org/zap"

	"github.com/fatih/color"
	"github.com/k0kubun/pp/v3"
	"github.com/olekukonko/tablewriter"
	"github.com/tidwall/gjson"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
)

type ValidatedJSON struct {
	expected    interface{} // The expected JSON
	actual      interface{} // The actual JSON
	isIdentical bool
}

type JSONComparisonResult struct {
	matches     bool     // Indicates if the JSON strings match according to the criteria
	isExact     bool     // Indicates if the match is exact, considering ordering and noise
	differences []string // Lists the keys or indices of values that are not the same
}

func match(tc *models.TestCase, actualResponse *models.HTTPResp, noiseConfig map[string]map[string][]string, ignoreOrdering bool, logger *zap.Logger) (bool, *models.Result) {
	bodyType := models.BodyTypePlain
	if json.Valid([]byte(actualResponse.Body)) {
		bodyType = models.BodyTypeJSON
	}
	pass := true
	hRes := &[]models.HeaderResult{}

	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tc.HTTPResp.StatusCode,
			Actual:   actualResponse.StatusCode,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Type:     bodyType,
			Expected: tc.HTTPResp.Body,
			Actual:   actualResponse.Body,
		}},
	}
	noise := tc.Noise

	var (
		bodyNoise   = noiseConfig["body"]
		headerNoise = noiseConfig["header"]
	)

	if bodyNoise == nil {
		bodyNoise = map[string][]string{}
	}
	if headerNoise == nil {
		headerNoise = map[string][]string{}
	}

	for field, regexArr := range noise {
		a := strings.Split(field, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise[strings.ToLower(x)] = regexArr
		} else if a[0] == "header" {
			headerNoise[strings.ToLower(a[len(a)-1])] = regexArr
		}
	}

	// stores the json body after removing the noise
	cleanExp, cleanAct := tc.HTTPResp.Body, actualResponse.Body
	var jsonComparisonResult JSONComparisonResult
	if !Contains(MapToArray(noise), "body") && bodyType == models.BodyTypeJSON {
		//validate the stored json
		validatedJSON, err := ValidateAndMarshalJSON(logger, &cleanExp, &cleanAct)
		if err != nil {
			return false, res
		}
		if validatedJSON.isIdentical {
			jsonComparisonResult, err = JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering)
			pass = jsonComparisonResult.isExact
			if err != nil {
				return false, res
			}
		} else {
			pass = false
		}

		// debug log for cleanExp and cleanAct
		logger.Debug("cleanExp", zap.Any("", cleanExp))
		logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !Contains(MapToArray(noise), "body") && tc.HTTPResp.Body != actualResponse.Body {
			pass = false
		}
	}

	res.BodyResult[0].Normal = pass

	if !CompareHeaders(pkg.ToHTTPHeader(tc.HTTPResp.Header), pkg.ToHTTPHeader(actualResponse.Header), hRes, headerNoise) {

		pass = false
	}

	res.HeadersResult = *hRes
	if tc.HTTPResp.StatusCode == actualResponse.StatusCode {
		res.StatusCode.Normal = true
	} else {

		pass = false
	}

	if !pass {
		logDiffs := NewDiffsPrinter(tc.Name)

		newLogger := pp.New()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetFailingColorScheme())
		var logs = ""

		logs = logs + newLogger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)

		// ------------ DIFFS RELATED CODE -----------
		if !res.StatusCode.Normal {
			logDiffs.PushStatusDiff(fmt.Sprint(res.StatusCode.Expected), fmt.Sprint(res.StatusCode.Actual))
		}

		var (
			actualHeader   = map[string][]string{}
			expectedHeader = map[string][]string{}
			unmatched      = true
		)

		for _, j := range res.HeadersResult {
			if !j.Normal {
				unmatched = false
				actualHeader[j.Actual.Key] = j.Actual.Value
				expectedHeader[j.Expected.Key] = j.Expected.Value
			}
		}

		if !unmatched {
			for i, j := range expectedHeader {
				logDiffs.PushHeaderDiff(fmt.Sprint(j), fmt.Sprint(actualHeader[i]), i, headerNoise)
			}
		}

		if !res.BodyResult[0].Normal {
			if json.Valid([]byte(actualResponse.Body)) {
				patch, err := jsondiff.Compare(tc.HTTPResp.Body, actualResponse.Body)
				if err != nil {
					logger.Warn("failed to compute json diff", zap.Error(err))
				}
				for _, op := range patch {
					if jsonComparisonResult.matches {
						logDiffs.hasarrayIndexMismatch = true
						logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.differences, ", "))
					}
					logDiffs.PushBodyDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value), bodyNoise)

				}
			} else {
				logDiffs.PushBodyDiff(fmt.Sprint(tc.HTTPResp.Body), fmt.Sprint(actualResponse.Body), bodyNoise)
			}
		}
		_, err := newLogger.Printf(logs)
		if err != nil {
			utils.LogError(logger, err, "failed to print the logs")
		}

		err = logDiffs.Render()
		if err != nil {
			utils.LogError(logger, err, "failed to render the diffs")
		}
	} else {
		newLogger := pp.New()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetPassingColorScheme())
		var log2 = ""
		log2 += newLogger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)
		_, err := newLogger.Printf(log2)
		if err != nil {
			utils.LogError(logger, err, "failed to print the logs")
		}
	}
	return pass, res
}

func FlattenHTTPResponse(h http.Header, body string) (map[string][]string, error) {
	m := map[string][]string{}
	for k, v := range h {
		m["header."+k] = []string{strings.Join(v, "")}
	}
	err := AddHTTPBodyToMap(body, m)
	if err != nil {
		return m, err
	}
	return m, nil
}

// UnmarshallJSON returns unmarshalled JSON object.
func UnmarshallJSON(s string, log *zap.Logger) (interface{}, error) {
	var result interface{}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		utils.LogError(log, err, "cannot convert json string into json object")
		return nil, err
	}
	return result, nil
}

func ArrayToMap(arr []string) map[string]bool {
	res := map[string]bool{}
	for i := range arr {
		res[arr[i]] = true
	}
	return res
}

func InterfaceToString(val interface{}) string {
	switch v := val.(type) {
	case int:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%f", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func JSONDiffWithNoiseControl(validatedJSON ValidatedJSON, noise map[string][]string, ignoreOrdering bool) (JSONComparisonResult, error) {
	var matchJSONComparisonResult JSONComparisonResult
	matchJSONComparisonResult, err := matchJSONWithNoiseHandling("", validatedJSON.expected, validatedJSON.actual, noise, ignoreOrdering)
	if err != nil {
		return matchJSONComparisonResult, err
	}

	return matchJSONComparisonResult, nil
}

func ValidateAndMarshalJSON(log *zap.Logger, exp, act *string) (ValidatedJSON, error) {
	var validatedJSON ValidatedJSON
	expected, err := UnmarshallJSON(*exp, log)
	if err != nil {
		return validatedJSON, err
	}
	actual, err := UnmarshallJSON(*act, log)
	if err != nil {
		return validatedJSON, err
	}
	validatedJSON.expected = expected
	validatedJSON.actual = actual
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		validatedJSON.isIdentical = false
		return validatedJSON, nil
	}
	cleanExp, err := json.Marshal(expected)
	if err != nil {
		return validatedJSON, err
	}
	cleanAct, err := json.Marshal(actual)
	if err != nil {
		return validatedJSON, err
	}
	*exp = string(cleanExp)
	*act = string(cleanAct)
	validatedJSON.isIdentical = true
	return validatedJSON, nil
}

// matchJSONWithNoiseHandling returns strcut if expected and actual JSON objects matches(are equal) and in exact order(isExact).
func matchJSONWithNoiseHandling(key string, expected, actual interface{}, noiseMap map[string][]string, ignoreOrdering bool) (JSONComparisonResult, error) {
	var matchJSONComparisonResult JSONComparisonResult
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return matchJSONComparisonResult, errors.New("type not matched")
	}
	if expected == nil && actual == nil {
		matchJSONComparisonResult.isExact = true
		matchJSONComparisonResult.matches = true
		return matchJSONComparisonResult, nil
	}
	x := reflect.ValueOf(expected)
	prefix := ""
	if key != "" {
		prefix = key + "."
	}
	switch x.Kind() {
	case reflect.Float64, reflect.String, reflect.Bool:
		regexArr, isNoisy := CheckStringExist(key, noiseMap)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(InterfaceToString(expected), regexArr)
		}
		if expected != actual && !isNoisy {
			return matchJSONComparisonResult, nil
		}

	case reflect.Map:
		expMap := expected.(map[string]interface{})
		actMap := actual.(map[string]interface{})
		copiedExpMap := make(map[string]interface{})
		copiedActMap := make(map[string]interface{})

		// Copy each key-value pair from expMap to copiedExpMap
		for key, value := range expMap {
			copiedExpMap[key] = value
		}

		// Repeat the same process for actual map
		for key, value := range actMap {
			copiedActMap[key] = value
		}
		isExact := true
		differences := []string{}
		for k, v := range expMap {
			val, ok := actMap[k]
			if !ok {
				return matchJSONComparisonResult, nil
			}
			if valueMatchJSONComparisonResult, er := matchJSONWithNoiseHandling(strings.ToLower(prefix+k), v, val, noiseMap, ignoreOrdering); !valueMatchJSONComparisonResult.matches || er != nil {
				return valueMatchJSONComparisonResult, nil
			} else if !valueMatchJSONComparisonResult.isExact {
				isExact = false
				differences = append(differences, k)
				differences = append(differences, valueMatchJSONComparisonResult.differences...)
			}
			// remove the noisy key from both expected and actual JSON.
			// Viper bindings are case insensitive, so we need convert the key to lowercase.
			if _, ok := CheckStringExist(strings.ToLower(prefix+k), noiseMap); ok {
				delete(copiedExpMap, prefix+k)
				delete(copiedActMap, k)
				continue
			}
		}
		// checks if there is a key which is not present in expMap but present in actMap.
		for k := range actMap {
			_, ok := expMap[k]
			if !ok {
				return matchJSONComparisonResult, nil
			}
		}
		matchJSONComparisonResult.matches = true
		matchJSONComparisonResult.isExact = isExact
		matchJSONComparisonResult.differences = append(matchJSONComparisonResult.differences, differences...)
		return matchJSONComparisonResult, nil
	case reflect.Slice:
		if regexArr, isNoisy := CheckStringExist(key, noiseMap); isNoisy && len(regexArr) != 0 {
			break
		}
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)
		if expSlice.Len() != actSlice.Len() {
			return matchJSONComparisonResult, nil
		}
		isMatched := true
		isExact := true
		for i := 0; i < expSlice.Len(); i++ {
			matched := false
			for j := 0; j < actSlice.Len(); j++ {
				if valMatchJSONComparisonResult, err := matchJSONWithNoiseHandling(key, expSlice.Index(i).Interface(), actSlice.Index(j).Interface(), noiseMap, ignoreOrdering); err == nil && valMatchJSONComparisonResult.matches {
					if !valMatchJSONComparisonResult.isExact {
						for _, val := range valMatchJSONComparisonResult.differences {
							prefixedVal := key + "[" + fmt.Sprint(j) + "]." + val // Prefix the value
							matchJSONComparisonResult.differences = append(matchJSONComparisonResult.differences, prefixedVal)
						}
					}
					matched = true
					break
				}
			}

			if !matched {
				isMatched = false
				isExact = false
				break
			}
		}
		if !isMatched {
			matchJSONComparisonResult.matches = isMatched
			matchJSONComparisonResult.isExact = isExact
			return matchJSONComparisonResult, nil
		}
		if !ignoreOrdering {
			for i := 0; i < expSlice.Len(); i++ {
				if valMatchJSONComparisonResult, er := matchJSONWithNoiseHandling(key, expSlice.Index(i).Interface(), actSlice.Index(i).Interface(), noiseMap, ignoreOrdering); er != nil || !valMatchJSONComparisonResult.isExact {
					isExact = false
					break
				}
			}
		}
		matchJSONComparisonResult.matches = isMatched
		matchJSONComparisonResult.isExact = isExact

		return matchJSONComparisonResult, nil
	default:
		return matchJSONComparisonResult, errors.New("type not registered for json")
	}
	matchJSONComparisonResult.matches = true
	matchJSONComparisonResult.isExact = true
	return matchJSONComparisonResult, nil
}

// MAX_LINE_LENGTH is chars PER expected/actual string. Can be changed no problem
const MAX_LINE_LENGTH = 50

type DiffsPrinter struct {
	testCase              string
	statusExp             string
	statusAct             string
	headerExp             map[string]string
	headerAct             map[string]string
	bodyExp               string
	bodyAct               string
	bodyNoise             map[string][]string
	headNoise             map[string][]string
	hasarrayIndexMismatch bool
	text                  string
}

func NewDiffsPrinter(testCase string) DiffsPrinter {
	return DiffsPrinter{testCase, "", "", map[string]string{}, map[string]string{}, "", "", map[string][]string{}, map[string][]string{}, false, ""}
}

func (d *DiffsPrinter) PushStatusDiff(exp, act string) {
	d.statusExp, d.statusAct = exp, act
}

func (d *DiffsPrinter) PushFooterDiff(key string) {
	d.hasarrayIndexMismatch = true
	d.text = key
}

func (d *DiffsPrinter) PushHeaderDiff(exp, act, key string, noise map[string][]string) {
	d.headerExp[key], d.headerAct[key], d.headNoise = exp, act, noise
}

func (d *DiffsPrinter) PushBodyDiff(exp, act string, noise map[string][]string) {
	d.bodyExp, d.bodyAct, d.bodyNoise = exp, act, noise
}

// Render will display and colorize diffs side-by-side
func (d *DiffsPrinter) Render() error {
	diffs := []string{}

	if d.statusExp != d.statusAct {
		diffs = append(diffs, sprintDiff(d.statusExp, d.statusAct, "status"))
	}

	diffs = append(diffs, sprintDiffHeader(d.headerExp, d.headerAct))

	if len(d.bodyExp) != 0 || len(d.bodyAct) != 0 {
		bE, bA := []byte(d.bodyExp), []byte(d.bodyAct)
		if json.Valid(bE) && json.Valid(bA) {
			difference, err := sprintJSONDiff(bE, bA, "body", d.bodyNoise)
			if err != nil {
				// difference = sprintDiff(d.bodyExp, d.bodyAct, "body")
			}
			diffs = append(diffs, difference)
		} else {
			diffs = append(diffs, sprintDiff(d.bodyExp, d.bodyAct, "body"))
		}

	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{fmt.Sprintf("Diffs %v", d.testCase)})
	table.SetHeaderColor(tablewriter.Colors{tablewriter.FgHiRedColor})
	table.SetAlignment(tablewriter.ALIGN_CENTER)

	for _, e := range diffs {
		table.Append([]string{e})
	}
	if d.hasarrayIndexMismatch {
		yellowPaint := color.New(color.FgYellow).SprintFunc()
		redPaint := color.New(color.FgRed).SprintFunc()
		startPart := " Expected and actual value"
		var midPartpaint string
		if len(d.text) > 0 {
			midPartpaint = redPaint(d.text)
			startPart += " of "
		}
		initalPart := yellowPaint(utils.WarningSign + startPart)

		endPaint := yellowPaint(" are in different order but have the same objects")
		table.SetHeader([]string{initalPart + midPartpaint + endPaint})
		table.SetAlignment(tablewriter.ALIGN_CENTER)
		table.Append([]string{initalPart + midPartpaint + endPaint})
	}
	table.Render()
	return nil
}

/*
 * Returns a nice diff table where the left is the expect and the right
 * is the actual. each entry in expect and actual will contain the key
 * and the corresponding value.
 */
func sprintDiffHeader(expect, actual map[string]string) string {

	expectAll := ""
	actualAll := ""
	for key, expValue := range expect {
		actValue := key + ": " + actual[key]
		expValue = key + ": " + expValue
		// Offset will be where the string start to unmatch
		offsets, _ := diffIndexRange(expValue, actValue)

		// Color of the unmatch, can be changed
		cE, cA := color.FgHiRed, color.FgHiGreen

		expectAll += breakWithColor(expValue, &cE, offsets)

		actualAll += breakWithColor(actValue, &cA, offsets)

	}
	if len(expect) > MAX_LINE_LENGTH || len(actual) > MAX_LINE_LENGTH {
		return expectActualTable(expectAll, actualAll, "header", false) // Don't centerize
	}
	return expectActualTable(expectAll, actualAll, "header", true)
}

/*
 * Returns a nice diff table where the left is the expect and the right
 * is the actual. For JSON-based diffs use SprintJSONDiff
 * field: body, status...
 */
func sprintDiff(expect, actual, field string) string {

	// Offset will be where the string start to unmatch
	offset, _ := diffIndexRange(expect, actual)
	// Color of the unmatch, can be changed
	cE, cA := color.FgHiRed, color.FgHiGreen

	var exp, act string

	exp += breakWithColor(expect, &cE, offset)

	act += breakWithColor(actual, &cA, offset)

	if len(expect) > MAX_LINE_LENGTH || len(actual) > MAX_LINE_LENGTH {
		return expectActualTable(exp, act, field, false) // Don't centerize
	}
	return expectActualTable(exp, act, field, true)
}

/* This will return the json diffs in a beautifull way. It will in fact
 * create a colorized table-based expect-response string and return it.
 * on the left-side there'll be the expect and on the right the actual
 * response. Its important to mention the inputs must to be a json. If
 * the body isnt in the rest-api formats (what means it is not json-based)
 * its better to use a generic diff output as the SprintDiff.
 */
func sprintJSONDiff(json1 []byte, json2 []byte, field string, noise map[string][]string) (string, error) {
	diffString, err := calculateJSONDiffs(json1, json2)
	if err != nil {
		return "", err
	}
	expect, actual := separateAndColorize(diffString, noise)
	result := expectActualTable(expect, actual, field, false)

	return result, nil
}

// Find the diff between two strings returning index where
// the difference begin
func diffIndexRange(s1, s2 string) ([]Range, bool) {
	var ranges []Range
	diff := false

	maxLen := len(s1)
	if len(s2) > maxLen {
		maxLen = len(s2)
	}

	var startDiff = -1
	for i := 0; i < maxLen; i++ {
		char1, char2 := byte(0), byte(0)
		if i < len(s1) {
			char1 = s1[i]
		}
		if i < len(s2) {
			char2 = s2[i]
		}

		if char1 != char2 {
			if startDiff == -1 {
				startDiff = i
			}
			diff = true
		} else {
			if startDiff != -1 {
				ranges = append(ranges, Range{Start: startDiff, End: i - 1})
				startDiff = -1
			}
		}
	}

	if startDiff != -1 {
		ranges = append(ranges, Range{Start: startDiff, End: maxLen - 1})
	}

	return ranges, diff
}

/* Will perform the calculation of the diffs, returning a string that
 * containes the lines that does not match represented by either a
 * minus or add symbol followed by the respective line.
 */
func calculateJSONDiffs(json1 []byte, json2 []byte) (string, error) {
	result1 := gjson.ParseBytes(json1)
	result2 := gjson.ParseBytes(json2)

	var diffStrings []string
	result1.ForEach(func(key, value gjson.Result) bool {
		value2 := result2.Get(key.String())
		if !value2.Exists() {
			diffStrings = append(diffStrings, fmt.Sprintf("- \"%s\": %v", key, value))
		} else if value.String() != value2.String() {
			diffStrings = append(diffStrings, fmt.Sprintf("- \"%s\": %v", key, value))
			diffStrings = append(diffStrings, fmt.Sprintf("+ \"%s\": %v", key, value2))
		}
		return true
	})

	result2.ForEach(func(key, value gjson.Result) bool {
		if !result1.Get(key.String()).Exists() {
			diffStrings = append(diffStrings, fmt.Sprintf("+ \"%s\": %v", key, value))
		}
		return true
	})

	return strings.Join(diffStrings, "\n"), nil
}
func writeKeyValuePair(builder *strings.Builder, key string, value interface{}, indent string, colorFunc func(a ...interface{}) string) {
	serializedValue, _ := json.MarshalIndent(value, "", "  ")
	valueStr := string(serializedValue)
	if colorFunc != nil && !reflect.DeepEqual(value, "") {
		// Apply color only to the value, not the key
		builder.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", indent, key, colorFunc(valueStr)))
	} else {
		// No color function provided, or the value is an empty string (which should not be colorized)
		builder.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", indent, key, valueStr))
	}
}

// compareAndColorizeSlices compares two slices of interfaces and outputs color-coded differences.
// compareAndColorizeSlices compares two slices of interfaces and outputs color-coded differences.
func compareAndColorizeSlices(a, b []interface{}, indent string, red, green func(a ...interface{}) string) (string, string) {
	var expectedOutput strings.Builder
	var actualOutput strings.Builder

	maxLength := len(a)
	if len(b) > maxLength {
		maxLength = len(b)
	}

	for i := 0; i < maxLength; i++ {
		var aValue, bValue interface{}
		var aExists, bExists bool

		if i < len(a) {
			aValue = a[i]
			aExists = true
		}

		if i < len(b) {
			bValue = b[i]
			bExists = true
		}

		if aExists && bExists {
			// If both slices have this index, compare values
			switch v1 := aValue.(type) {
			case map[string]interface{}:
				if v2, ok := bValue.(map[string]interface{}); ok {
					// When both values are maps, compare and colorize the maps
					expectedText, actualText := compareAndColorizeMaps(v1, v2, indent+"  ", red, green)
					expectedOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, expectedText))
					actualOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, actualText))
				} else {
					// Fallback to simple comparison if types differ
					expectedOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, red("%v", aValue)))
					actualOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, green("%v", bValue)))
				}
			case []interface{}:

				// Handle nested slices
				if v2, ok := bValue.([]interface{}); ok {
					// Handle nested slices
					expectedText, actualText := compareAndColorizeSlices(v1, v2, indent+"  ", red, green)
					expectedOutput.WriteString(fmt.Sprintf("%s[%d]: [\n%s%s]\n", indent, i, expectedText, indent))
					actualOutput.WriteString(fmt.Sprintf("%s[%d]: [\n%s%s]\n", indent, i, actualText, indent))
				} else {
					// Fallback to simple comparison if types differ
					expectedOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, red("%v", aValue)))
					actualOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, green("%v", bValue)))
				}

			default:
				// For non-map types, highlight differences
				if !reflect.DeepEqual(aValue, bValue) {
					expectedOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, red("%v", aValue)))
					actualOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, green("%v", bValue)))
				} else {
					// Write without highlighting if values are the same
					expectedOutput.WriteString(fmt.Sprintf("%s[%d]: %v\n", indent, i, aValue))
					actualOutput.WriteString(fmt.Sprintf("%s[%d]: %v\n", indent, i, bValue))
				}
			}
		} else if aExists {

			// Only a has this index, it's a deletion
			expectedOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, red(serialize(aValue))))
		} else if bExists {
			// Only b has this index, it's an addition
			actualOutput.WriteString(fmt.Sprintf("%s[%d]: %s\n", indent, i, green(serialize(bValue))))
		}
	}

	return expectedOutput.String(), actualOutput.String()
}

// compare compares the values and decides whether to call compareAndColorizeMap or compareAndColorizeSlices.
func compare(key string, val1, val2 interface{}, indent string, expect, actual *strings.Builder, red, green func(a ...interface{}) string) {
	switch v1 := val1.(type) {
	case map[string]interface{}:
		if v2, ok := val2.(map[string]interface{}); ok {
			expectedText, actualText := compareAndColorizeMaps(v1, v2, indent+"  ", red, green)
			expect.WriteString(fmt.Sprintf("%s\"%s\": %s\n", indent, key, expectedText))
			actual.WriteString(fmt.Sprintf("%s\"%s\": %s\n", indent, key, actualText))
		} else {
			writeKeyValuePair(expect, key, val1, indent, red)
			writeKeyValuePair(actual, key, val2, indent, green)
		}
	case []interface{}:
		if v2, ok := val2.([]interface{}); ok {
			expectedText, actualText := compareAndColorizeSlices(v1, v2, indent+"  ", red, green)
			expect.WriteString(fmt.Sprintf("%s\"%s\": [\n%s\n%s]\n", indent, key, expectedText, indent))
			actual.WriteString(fmt.Sprintf("%s\"%s\": [\n%s\n%s]\n", indent, key, actualText, indent))
		} else {
			writeKeyValuePair(expect, key, val1, indent, red)
			writeKeyValuePair(actual, key, val2, indent, green)
		}
	default:
		if !reflect.DeepEqual(val1, val2) {
			// Serialize only if the values are different to prevent unnecessary serialization
			val1Str, err := json.MarshalIndent(val1, "", "  ")
			if err != nil {
				// Handle error
				return
			}
			val2Str, err := json.MarshalIndent(val2, "", "  ")
			if err != nil {
				// Handle error
				return
			}
			expect.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", indent, key, red(string(val1Str))))
			actual.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", indent, key, green(string(val2Str))))
		} else {
			// Serialize the value since they are the same and do not require color
			valStr, err := json.MarshalIndent(val1, "", "  ")
			if err != nil {
				// Handle error
				return
			}
			expect.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", indent, key, string(valStr)))
			actual.WriteString(fmt.Sprintf("%s\"%s\": %s,\n", indent, key, string(valStr)))
		}
	}
}

func compareAndColorizeMaps(a, b map[string]interface{}, indent string, red, green func(a ...interface{}) string) (string, string) {
	var expectedOutput, actualOutput strings.Builder

	// Initialize the outputs with an opening curly brace
	expectedOutput.WriteString("{\n")
	actualOutput.WriteString("{\n")

	// Iterate through all keys in map 'a' and compare with 'b'
	for key, aValue := range a {
		bValue, bHasKey := b[key]
		if !bHasKey {
			// Key is missing in map 'b', so the whole line is red
			writeKeyValuePair(&expectedOutput, key, aValue, indent+"  ", red)
			continue
		}
		compare(key, aValue, bValue, indent+"  ", &expectedOutput, &actualOutput, red, green)
	}

	// Now check for any keys that are in 'b' but not in 'a'
	for key, bValue := range b {
		if _, aHasKey := a[key]; !aHasKey {
			// Key is missing in map 'a', so the whole line is green
			writeKeyValuePair(&actualOutput, key, bValue, indent+"  ", green)
		}
	}

	// Close the curly braces
	expectedOutput.WriteString(indent + "}")
	actualOutput.WriteString(indent + "}")

	return expectedOutput.String(), actualOutput.String()
}
func serialize(value interface{}) string {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "error" // Handle error properly in production code
	}
	return string(bytes)
}

// writeKeyValuePair writes a key-value pair with optional color.
// writeKeyValuePair writes a key-value pair with optional color.

// Will receive a string that has the differences represented
// by a plus or a minus sign and separate it. Just works with json
// Modified separateAndColorize function to handle nested JSON paths for noise keys
// Updated separateAndColorize function
func separateAndColorize(diffStr string, noise map[string][]string) (string, string) {
	lines := strings.Split(diffStr, "\n")
	expects := make(map[string]interface{}, 0)
	actuals := make(map[string]interface{}, 0)
	expect, actual := "", ""

	for i := 0; i < len(lines)-1; i++ {
		line := lines[i]
		nextLine := lines[i+1]

		if len(line) > 0 && line[0] == '-' && i != len(lines)-1 {

			// Remove the leading  "+ "
			expectedTrimmedLine := nextLine[3:] // Assuming lines[i+1] starts with "+ "
			expectedKeyValue := strings.SplitN(expectedTrimmedLine, ":", 2)
			if len(expectedKeyValue) == 2 {
				key := strings.TrimSpace(expectedKeyValue[0])
				value := strings.TrimSpace(expectedKeyValue[1])

				var jsonObj map[string]interface{}
				err := json.Unmarshal([]byte(value), &jsonObj)
				if err != nil {
					continue
				}

				expects = map[string]interface{}{key[:len(key)-1]: jsonObj}
			}

			// Remove the leading "- "
			actualTrimmedLine := line[3:]
			actualkeyValue := strings.SplitN(actualTrimmedLine, ":", 2)
			if len(actualkeyValue) == 2 {
				key := strings.TrimSpace(actualkeyValue[0])
				value := strings.TrimSpace(actualkeyValue[1])

				var jsonObj map[string]interface{}
				err := json.Unmarshal([]byte(value), &jsonObj)
				if err != nil {
					continue
				}

				actuals = map[string]interface{}{key[:len(key)-1]: jsonObj}

			}
			red := color.New(color.FgRed).SprintFunc()
			green := color.New(color.FgGreen).SprintFunc()

			expectedText, actualText := compareAndColorizeMaps(expects, actuals, " ", red, green)

			dsa := breakLines(expectedText)
			dsb := breakLines(actualText)
			expect += dsa
			actual += dsb
			expects = make(map[string]interface{}, 0)
			actuals = make(map[string]interface{}, 0)

			// Remove current line
			diffStr = strings.Replace(diffStr, line, "", 1)
			diffStr = strings.Replace(diffStr, nextLine, "", 1)
		}

	}
	if diffStr == "" {
		return expect, actual
	}

	diffLines := strings.Split(diffStr, "\n")
	jsonPath := []string{} // Stack to keep track of nested paths
	for i, line := range diffLines {
		if len(line) > 0 {
			noised := false
			lineContent := line[1:] // Remove the diff indicator (+/-)
			trimmedLine := strings.TrimSpace(lineContent)

			// Update the JSON path stack based on the line content
			if strings.HasSuffix(trimmedLine, "{") {
				key := strings.TrimSpace(trimmedLine[:len(trimmedLine)-1]) // Remove '{'
				jsonPath = append(jsonPath, key)                           // Push to stack
			} else if trimmedLine == "}," || trimmedLine == "}" {
				jsonPath = jsonPath[:len(jsonPath)-1] // Pop from stack
			}

			currentPath := strings.Join(jsonPath, ".")

			// Check for noise based on the current JSON path
			for noisePath := range noise {
				if strings.HasPrefix(currentPath, noisePath) {
					line = " " + lineContent
					if line[0] == '-' {
						line = " " + line[1:]
						expect += breakWithColor(line, nil, []Range{})
					} else if line[0] == '+' {
						line = " " + line[1:]
						actual += breakWithColor(line, nil, []Range{})
					}
					noised = true
					break
				}
			}

			if noised {
				continue
			}

			// Process lines without noise
			if line[0] == '-' {
				c := color.FgRed
				// Workaround to get the exact index where the diff begins
				if i < len(diffLines)-1 && len(line) > 1 && diffLines[i+1] != "" && diffLines[i+1][0] == '+' {

					/* As we want to get the exact difference where the line's
					 * diff begin we must to, first, get the expect (this) and
					 * the actual (next) line. Then we must to espace the first
					 * char that is an "+" or "-" symbol so we end up having
					 * just the contents of the line we want to compare */
					offsets, _ := diffIndexRange(line[1:], diffLines[i+1][1:])
					expect += breakWithColor(line, &c, offsets)
				} else {
					// In the case where there isn't in fact an actual
					// version to compare, it was just expect to have this
					expect += breakWithColor(line, &c, []Range{{Start: 0, End: len(line) - 1}})
				}
			} else if line[0] == '+' {
				c := color.FgGreen
				// Here we do the same thing as above, just inverted
				if i > 0 && len(line) > 1 && diffLines[i-1] != "" && diffLines[i-1][0] == '-' {

					offsets, _ := diffIndexRange(line[1:], diffLines[i-1][1:])
					actual += breakWithColor(line, &c, offsets)
				} else {
					actual += breakWithColor(line, &c, []Range{{Start: 0, End: len(line) - 1}})
				}
			} else {
				expect += breakWithColor(line, nil, []Range{})
				actual += breakWithColor(line, nil, []Range{})
			}
		}
	}
	return expect, actual
}

// Will colorize the strubg and do the job of break it if it pass MAX_LINE_LENGTH,
// always respecting the reset of ascii colors before the break line to dont
func breakWithColor(input string, c *color.Attribute, highlightRanges []Range) string {
	paint := func(a ...interface{}) string { return "" }
	if c != nil {
		paint = color.New(*c).SprintFunc()
	}
	var output strings.Builder
	var isColorRange bool
	lineLen := 0

	for i, char := range input {
		isColorRange = false // Reset for each character

		// Determine if this character is within any of the color ranges
		for _, r := range highlightRanges {
			if i >= r.Start+1 && i < r.End+2 {
				isColorRange = true
				break
			}
		}

		if isColorRange {
			output.WriteString(paint(string(char)))
		} else {
			output.WriteString(string(char))
		}

		// Increment line length, wrap line if necessary
		lineLen++
		if lineLen == MAX_LINE_LENGTH {
			output.WriteString("\n")
			lineLen = 0 // Reset line length after a newline
		}
	}

	// Catch any case where the input does not end with a newline
	if lineLen > 0 {
		output.WriteString("\n")
	}

	return output.String()
}

func isControlCharacter(char rune) bool {
	return char < ' '
}

func breakLines(input string) string {
	const MAX_LINE_LENGTH = 50 // Set your desired max line length
	var output strings.Builder
	var currentLine strings.Builder
	inANSISequence := false
	lineLength := 0

	// We'll collect ANSI sequences here and then reapply them as needed
	var ansiSequenceBuilder strings.Builder

	for _, char := range input {
		// Check for the beginning of an ANSI sequence
		if char == '\x1b' {
			inANSISequence = true
		}
		if inANSISequence {
			// Continue collecting the ANSI sequence
			ansiSequenceBuilder.WriteRune(char)
			if char == 'm' {
				// End of the ANSI sequence
				inANSISequence = false
				currentLine.WriteString(ansiSequenceBuilder.String())
				ansiSequenceBuilder.Reset()
			}
		} else {
			if isControlCharacter(char) && char != '\n' {
				currentLine.WriteRune(char)
			} else {
				// Handle word wrapping
				if char == ' ' && lineLength >= MAX_LINE_LENGTH {
					output.WriteString(currentLine.String())
					output.WriteRune('\n')
					currentLine.Reset()
					lineLength = 0
				} else if char == '\n' {
					output.WriteString(currentLine.String())
					output.WriteRune(char)
					currentLine.Reset()
					lineLength = 0
				} else {
					currentLine.WriteRune(char)
					lineLength++
				}
			}
		}
	}

	// Write any remaining content in currentLine to output
	if currentLine.Len() > 0 {
		output.WriteString(currentLine.String())
	}
	return output.String()
}

// Will return a string in a two columns table where the left
// side is the expected string and the right is the actual
// field: body, header, status...
func expectActualTable(exp string, act string, field string, centerize bool) string {
	buf := &bytes.Buffer{}
	table := tablewriter.NewWriter(buf)

	if centerize {
		table.SetAlignment(tablewriter.ALIGN_CENTER)
	}

	table.SetHeader([]string{fmt.Sprintf("Expect %v", field), fmt.Sprintf("Actual %v", field)})
	table.SetAutoWrapText(false)
	table.SetBorder(false)
	table.SetColMinWidth(0, MAX_LINE_LENGTH)
	table.SetColMinWidth(1, MAX_LINE_LENGTH)
	table.Append([]string{exp, act})
	table.Render()
	return buf.String()
}

func Contains(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}

func checkKey(res *[]models.HeaderResult, key string) bool {
	for _, v := range *res {
		if key == v.Expected.Key {
			return false
		}
	}
	return true
}

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]models.HeaderResult, noise map[string][]string) bool {
	if res == nil {
		return false
	}
	match := true
	_, isHeaderNoisy := noise["header"]
	for k, v := range h1 {
		regexArr, isNoisy := CheckStringExist(strings.ToLower(k), noise)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(v[0], regexArr)
		}
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h2[k]
		if !isNoisy {
			if !ok {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: nil,
						},
					})
				}

				match = false
				continue
			}
			if len(v) != len(val) {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: val,
						},
					})
				}
				match = false
				continue
			}
			for i, e := range v {
				if val[i] != e {
					if checkKey(res, k) {
						*res = append(*res, models.HeaderResult{
							Normal: false,
							Expected: models.Header{
								Key:   k,
								Value: v,
							},
							Actual: models.Header{
								Key:   k,
								Value: val,
							},
						})
					}
					match = false
					continue
				}
			}
		}
		if checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: v,
				},
				Actual: models.Header{
					Key:   k,
					Value: val,
				},
			})
		}
	}
	for k, v := range h2 {
		regexArr, isNoisy := CheckStringExist(strings.ToLower(k), noise)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(v[0], regexArr)
		}
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h1[k]
		if isNoisy && checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: val,
				},
				Actual: models.Header{
					Key:   k,
					Value: v,
				},
			})
			continue
		}
		if !ok {
			if checkKey(res, k) {
				*res = append(*res, models.HeaderResult{
					Normal: false,
					Expected: models.Header{
						Key:   k,
						Value: nil,
					},
					Actual: models.Header{
						Key:   k,
						Value: v,
					},
				})
			}

			match = false
		}
	}
	return match
}

func MapToArray(mp map[string][]string) []string {
	var result []string
	for k := range mp {
		result = append(result, k)
	}
	return result
}

func CheckStringExist(s string, mp map[string][]string) ([]string, bool) {
	if val, ok := mp[s]; ok {
		return val, ok
	}
	return []string{}, false
}

func MatchesAnyRegex(str string, regexArray []string) (bool, string) {
	for _, pattern := range regexArray {
		re := regexp.MustCompile(pattern)
		if re.MatchString(str) {
			return true, pattern
		}
	}
	return false, ""
}

func AddHTTPBodyToMap(body string, m map[string][]string) error {
	// add body
	if json.Valid([]byte(body)) {
		var result interface{}

		err := json.Unmarshal([]byte(body), &result)
		if err != nil {
			return err
		}
		j := Flatten(result)
		for k, v := range j {
			nk := "body"
			if k != "" {
				nk = nk + "." + k
			}
			m[nk] = v
		}
	} else {
		// add it as raw text
		m["body"] = []string{body}
	}
	return nil
}

// Flatten takes a map and returns a new one where nested maps are replaced
// by dot-delimited keys.
// examples of valid jsons - https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/JSON/parse#examples
func Flatten(j interface{}) map[string][]string {
	if j == nil {
		return map[string][]string{"": {""}}
	}
	o := make(map[string][]string)
	x := reflect.ValueOf(j)
	switch x.Kind() {
	case reflect.Map:
		m, ok := j.(map[string]interface{})
		if !ok {
			return map[string][]string{}
		}
		for k, v := range m {
			nm := Flatten(v)
			for nk, nv := range nm {
				fk := k
				if nk != "" {
					fk = fk + "." + nk
				}
				o[fk] = nv
			}
		}
	case reflect.Bool:
		o[""] = []string{strconv.FormatBool(x.Bool())}
	case reflect.Float64:
		o[""] = []string{strconv.FormatFloat(x.Float(), 'E', -1, 64)}
	case reflect.String:
		o[""] = []string{x.String()}
	case reflect.Slice:
		child, ok := j.([]interface{})
		if !ok {
			return map[string][]string{}
		}
		for _, av := range child {
			nm := Flatten(av)
			for nk, nv := range nm {
				if ov, exists := o[nk]; exists {
					o[nk] = append(ov, nv...)
				} else {
					o[nk] = nv
				}
			}
		}
	}
	return o
}
