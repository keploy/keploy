// Package matcherutils for matching utilities
package matcherutils

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/7sDream/geko"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/yudai/gojsondiff"
	"github.com/yudai/gojsondiff/formatter"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type ValidatedJSON struct {
	expected    interface{} // The expected JSON
	actual      interface{} // The actual JSON
	isIdentical bool
}

func (v *ValidatedJSON) IsIdentical() bool {
	return v.isIdentical
}
func (v *ValidatedJSON) Expected() interface{} {
	return v.expected
}
func (v *ValidatedJSON) Actual() interface{} {
	return v.actual
}

type JSONComparisonResult struct {
	matches     bool     // Indicates if the JSON strings match according to the criteria
	isExact     bool     // Indicates if the match is exact, considering ordering and noise
	differences []string // Lists the keys or indices of values that are not the same
}

func (v *JSONComparisonResult) IsExact() bool {
	return v.isExact
}
func (v *JSONComparisonResult) Matches() bool {
	return v.matches
}
func (v *JSONComparisonResult) Differences() []string {
	return v.differences
}
func MarshalRequestBodies(mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockRequestBody []byte
	var testRequestBody []byte
	var err error
	if mockOperation.RequestBody != nil {
		mockRequestBody, err = json.Marshal(mockOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock RequestBody: %v", err)
		}
	}
	if testOperation.RequestBody != nil {
		testRequestBody, err = json.Marshal(testOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test RequestBody: %v", err)
		}
	}
	return string(mockRequestBody), string(testRequestBody), nil
}

func MarshalResponseBodies(status string, mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockResponseBody []byte
	var testResponseBody []byte
	var err error
	if mockOperation.Responses[status].Content != nil {
		mockResponseBody, err = json.Marshal(mockOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock ResponseBody: %v", err)
		}
	}
	if testOperation.Responses[status].Content != nil {
		testResponseBody, err = json.Marshal(testOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test ResponseBody: %v", err)
		}
	}
	return string(mockResponseBody), string(testResponseBody), nil
}
func FindOperation(item models.PathItem) (*models.Operation, string) {
	if item.Get != nil {
		return item.Get, "GET"
	}
	if item.Post != nil {
		return item.Post, "POST"
	}
	if item.Put != nil {
		return item.Put, "PUT"
	}
	if item.Delete != nil {
		return item.Delete, "DELETE"
	}
	if item.Patch != nil {
		return item.Patch, "PATCH"
	}
	return nil, ""
}

// ParseIntoJSON Parse the json string into a geko type variable, it will maintain the order of the keys in the json.
func ParseIntoJSON(response string) (interface{}, error) {
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

// CompareResponses compares the two responses, if there is any difference in the values,
// It checks in the templatized values map if the value is already present, it will update the value in the map.
// It also changes the expected value to the actual value in the response1 (expected body)
func CompareResponses(response1, response2 *interface{}, key string) {
	switch v1 := (*response1).(type) {
	case geko.Array:
		for _, val1 := range v1.List {
			CompareResponses(&val1, response2, "")
		}
	case geko.ObjectItems:
		keys := v1.Keys()
		vals := v1.Values()
		for i := range keys {
			CompareResponses(&vals[i], response2, keys[i])
			v1.SetValueByIndex(i, vals[i]) // in order to change the expected value if required
		}
	case map[string]interface{}:
		for key, val := range v1 {
			CompareResponses(&val, response2, key)
			v1[key] = val // in order to change the expected value if required
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
func reverseMap(m map[string]interface{}) map[interface{}]string {
	var reverseMap = make(map[interface{}]string)
	for key, val := range m {
		reverseMap[val] = key
	}
	return reverseMap
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

func ValidateAndMarshalJSON(log *zap.Logger, exp, act *string) (ValidatedJSON, error) {
	var validatedJSON ValidatedJSON
	var expected interface{}
	var actual interface{}
	var err error
	if *exp != "" {
		expected, err = UnmarshallJSON(*exp, log)
		if err != nil {
			return validatedJSON, err
		}
	}
	if *act != "" {
		actual, err = UnmarshallJSON(*act, log)
		if err != nil {
			return validatedJSON, err
		}
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

// UnmarshallJSON returns unmarshalled JSON object.
func UnmarshallJSON(s string, log *zap.Logger) (interface{}, error) {
	var result interface{}
	if s == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		utils.LogError(log, err, "cannot convert json string into json object", zap.String("json", s))
		return nil, err
	}
	return result, nil
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
	typeExp               string
	typeAct               string
}

func (d *DiffsPrinter) HasarrayIndexMismatch(has bool) {
	d.hasarrayIndexMismatch = has
}

func NewDiffsPrinter(testCase string) DiffsPrinter {
	return DiffsPrinter{testCase, "", "", map[string]string{}, map[string]string{}, "", "", map[string][]string{}, map[string][]string{}, false, "", "", ""}
}
func (d *DiffsPrinter) PushTypeDiff(exp, act string) {
	d.typeExp, d.typeAct = exp, act
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
				difference = sprintDiff(d.bodyExp, d.bodyAct, "body")
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
func (d *DiffsPrinter) TableWriter(diffs []string) error {

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
func (d *DiffsPrinter) RenderAppender() error {
	//Only show difference for the response body
	diffs := []string{}
	pass := true

	if d.typeExp != d.typeAct {
		diffs = append(diffs, sprintDiff(d.typeExp, d.typeAct, "request body type"))
		pass = false
	}
	if !pass {
		err := d.TableWriter(diffs)
		if err != nil {
			return err
		}
		return nil
	}

	if len(d.bodyExp) != 0 || len(d.bodyAct) != 0 {
		pass = false
		bE, bA := []byte(d.bodyExp), []byte(d.bodyAct)
		if json.Valid(bE) && json.Valid(bA) {
			difference, err := sprintJSONDiff(bE, bA, "response", d.bodyNoise)
			if err != nil {
				difference = sprintDiff(d.bodyExp, d.bodyAct, "response")
			}
			diffs = append(diffs, difference)
		} else {
			diffs = append(diffs, sprintDiff(d.bodyExp, d.bodyAct, "response"))
		}
	}
	if !pass {
		err := d.TableWriter(diffs)
		if err != nil {
			return err
		}

	}

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
		offset, _ := diffIndex(expValue, actValue)

		// Color of the unmatch, can be changed
		cE, cA := color.FgHiRed, color.FgHiGreen

		expectAll += breakWithColor(expValue, &cE, offset)
		actualAll += breakWithColor(actValue, &cA, offset)
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
	offset, _ := diffIndex(expect, actual)

	// Color of the unmatch, can be changed
	cE, cA := color.FgHiRed, color.FgHiGreen

	exp := breakWithColor(expect, &cE, offset)
	act := breakWithColor(actual, &cA, offset)
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
func diffIndex(s1, s2 string) (int, bool) {
	diff := false
	i := -1

	// Check if one string is smaller than another, if so theres a diff
	if len(s1) < len(s2) {
		i = len(s1)
		diff = true
	} else if len(s2) < len(s1) {
		diff = true
		i = len(s2)
	}

	// Check for unmatched characters
	for i := 0; i < len(s1) && i < len(s2); i++ {
		if s1[i] != s2[i] {
			return i, true
		}
	}

	return i, diff
}

/* Will perform the calculation of the diffs, returning a string that
 * containes the lines that does not match represented by either a
 * minus or add symbol followed by the respective line.
 */
func calculateJSONDiffs(json1 []byte, json2 []byte) (string, error) {
	var diff = gojsondiff.New()
	dObj, err := diff.Compare(json1, json2)
	if err != nil {
		return "", err
	}

	var jsonObject map[string]interface{}
	err = json.Unmarshal([]byte(json1), &jsonObject)
	if err != nil {
		return "", err
	}

	diffString, _ := formatter.NewAsciiFormatter(jsonObject, formatter.AsciiFormatterConfig{
		ShowArrayIndex: true,
		Coloring:       false, // We will color our way
	}).Format(dObj)

	return diffString, nil
}

// Will receive a string that has the differences represented
// by a plus or a minus sign and separate it. Just works with json
// Modified separateAndColorize function to handle nested JSON paths for noise keys
// Updated separateAndColorize function
func separateAndColorize(diffStr string, noise map[string][]string) (string, string) {
	expect, actual := "", ""
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
				key = strings.Trim(key, `"`)                               // Remove surrounding quotes
				jsonPath = append(jsonPath, key)
			} else if trimmedLine == "}," || trimmedLine == "}" {
				jsonPath = jsonPath[:len(jsonPath)-1] // Pop from stack
			} else {
				// For key-value pairs, extract the key
				if colonIndex := strings.Index(trimmedLine, ":"); colonIndex != -1 {
					key := strings.TrimSpace(trimmedLine[:colonIndex])
					key = strings.Trim(key, `"`) // Remove surrounding quotes
					if len(jsonPath) > 0 {
						jsonPath = append(jsonPath[:len(jsonPath)-1], key)
					} else {
						jsonPath = append(jsonPath, key)
					}
				}
			}

			currentPath := strings.Join(jsonPath, ".")

			// Check for noise based on the current JSON path
			for noisePath := range noise {
				if strings.HasPrefix(strings.ToLower(currentPath), noisePath) {
					line = " " + lineContent
					if line[0] == '-' {
						expect += breakWithColor(line, nil, 0)
					} else if line[0] == '+' {
						actual += breakWithColor(line, nil, 0)
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
				if i+1 < len(diffLines) && diffLines[i+1][0] == '+' {
					offset, _ := diffIndex(lineContent, diffLines[i+1][1:])
					expect += breakWithColor(line, &c, offset+1)
				} else {
					expect += breakWithColor(line, &c, 0)
				}
			} else if line[0] == '+' {
				c := color.FgGreen
				if i > 0 && diffLines[i-1][0] == '-' {
					offset, _ := diffIndex(lineContent, diffLines[i-1][1:])
					actual += breakWithColor(line, &c, offset+1)
				} else {
					actual += breakWithColor(line, &c, 0)
				}
			} else {
				expect += breakWithColor(line, nil, 0)
				actual += breakWithColor(line, nil, 0)
			}
		}
	}
	return expect, actual
}

// Will colorize the strubg and do the job of break it if it pass MAX_LINE_LENGTH,
// always respecting the reset of ascii colors before the break line to dont
func breakWithColor(input string, c *color.Attribute, offset int) string {
	var output []string
	var paint func(a ...interface{}) string
	colorize := false

	if c != nil {
		colorize = true
		paint = color.New(*c).SprintFunc()
	}

	for i := 0; i < len(input); i += MAX_LINE_LENGTH {
		end := i + MAX_LINE_LENGTH

		if end > len(input) {
			end = len(input)
		}

		// This conditions joins if we are at line where the offset begins
		if colorize && i+MAX_LINE_LENGTH > offset {
			paintedStart := i
			if paintedStart < offset {
				paintedStart = offset
			}

			// Will basically concatenated the non-painted string with the
			// painted
			prePaint := input[i:paintedStart]           // Start at i ends at offset
			postPaint := paint(input[paintedStart:end]) // Starts at offset (diff begins), goes til maxLength
			substr := prePaint + postPaint + "\n"       // Concatenate
			output = append(output, substr)
		} else {
			substr := input[i:end] + "\n"
			output = append(output, substr)
		}
	}
	return strings.Join(output, "")
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

func JSONDiffWithNoiseControl(validatedJSON ValidatedJSON, noise map[string][]string, ignoreOrdering bool) (JSONComparisonResult, error) {
	var matchJSONComparisonResult JSONComparisonResult
	matchJSONComparisonResult, err := matchJSONWithNoiseHandling("", validatedJSON.expected, validatedJSON.actual, noise, ignoreOrdering)
	if err != nil {
		return matchJSONComparisonResult, err
	}

	return matchJSONComparisonResult, nil
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
