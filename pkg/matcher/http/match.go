// Package http for http matching
package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
)

// Assignable global variables for system and utility functions
var jsonValid234 = json.Valid
var fmtSprint234 = fmt.Sprint
var ppNew234 = pp.New
var jsonMarshal234 = json.Marshal
var jsonUnmarshal234 = json.Unmarshal

func Match(tc *models.TestCase, actualResponse *models.HTTPResp, noiseConfig map[string]map[string][]string, ignoreOrdering bool, compareAll bool, logger *zap.Logger, emitFailureLogs bool) (bool, *models.Result) {
	bodyType := models.Plain
	if jsonValid234([]byte(actualResponse.Body)) {
		bodyType = models.JSON
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
	if bodyNoise != nil {
		if ignoreFields, ok := bodyNoise["*"]; ok && len(ignoreFields) > 0 && ignoreFields[0] == "*" {
			if noise["body"] == nil {
				noise["body"] = make([]string, 0)
			}
		}
	} else {
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

	var jsonComparisonResult matcherUtils.JSONComparisonResult
	if !matcherUtils.Contains(matcherUtils.MapToArray(noise), "body") && bodyType == models.JSON && jsonValid234([]byte(tc.HTTPResp.Body)) {
		//validate the stored json
		validatedJSON, err := matcherUtils.ValidateAndMarshalJSON(logger, &cleanExp, &cleanAct)
		if err != nil {
			return false, res
		}
		if validatedJSON.IsIdentical() {
			jsonComparisonResult, err = matcherUtils.JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering)
			pass = jsonComparisonResult.IsExact()
			if err != nil {
				return false, res
			}
		} else {
			pass = false
		}

		// debug log for cleanExp and cleanAct
		logger.Debug("cleanExp", zap.Any("cleanExp", cleanExp))
		logger.Debug("cleanAct", zap.Any("cleanAct", cleanAct))
	} else {
		// Skip body comparison for non-JSON responses unless compareAll is enabled
		if !compareAll && bodyType != models.JSON {
			logger.Debug("Skipping body comparison for non-JSON response", zap.String("bodyType", string(bodyType)))
			// Mark body as passing when compareAll is false and body is not JSON
		} else if !matcherUtils.Contains(matcherUtils.MapToArray(noise), "body") && tc.HTTPResp.Body != actualResponse.Body {
			pass = false
		}
	}

	res.BodyResult[0].Normal = pass

	if !matcherUtils.CompareHeaders(pkg.ToHTTPHeader(tc.HTTPResp.Header), pkg.ToHTTPHeader(actualResponse.Header), hRes, headerNoise) {
		res.HeadersResult = *hRes

		// If body matches but content-length differs, ignore the content-length difference
		if res.BodyResult[0].Normal {
			for i := range res.HeadersResult {
				if strings.ToLower(res.HeadersResult[i].Expected.Key) == "content-length" && !res.HeadersResult[i].Normal {
					logger.Warn("Ignoring Content-Length mismatch since body content is identical",
						zap.String("expected", strings.Join(res.HeadersResult[i].Expected.Value, ",")),
						zap.String("actual", strings.Join(res.HeadersResult[i].Actual.Value, ",")))
					res.HeadersResult[i].Normal = true
				}
			}
		}

		// Check if there are still any header mismatches after ignoring content-length
		hasHeaderMismatch := false
		for _, hr := range res.HeadersResult {
			if !hr.Normal {
				hasHeaderMismatch = true
				break
			}
		}
		if hasHeaderMismatch {
			pass = false
		}
	} else {
		res.HeadersResult = *hRes
	}
	if tc.HTTPResp.StatusCode == actualResponse.StatusCode {
		res.StatusCode.Normal = true
	} else {
		pass = false
	}

	skipSuccessMsg := false
	if !pass {
		isStatusMismatch := false
		isHeaderMismatch := false
		isBodyMismatch := false

		logDiffs := matcherUtils.NewDiffsPrinter(tc.Name)
		newLogger := ppNew234()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetFailingColorScheme())
		var logs = ""

		logs = logs + newLogger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)

		// ------------ DIFFS RELATED CODE -----------
		if !res.StatusCode.Normal {
			logDiffs.PushStatusDiff(fmtSprint234(res.StatusCode.Expected), fmtSprint234(res.StatusCode.Actual))
			isStatusMismatch = true
		} else {
			isStatusMismatch = false
		}

		var (
			actualHeader   = map[string][]string{}
			expectedHeader = map[string][]string{}
		)

		for _, j := range res.HeadersResult {
			var actualValue []string
			var expectedValue []string
			if !j.Normal {
				for _, v := range j.Actual.Value {
					_, temp, err := tools.RenderIfTemplatized(v)
					if err != nil {
						utils.LogError(logger, err, "failed to render the actual header")
						return false, nil
					}
					val, ok := temp.(string)
					if !ok {
						utils.LogError(logger, fmt.Errorf("failed to convert the actual header value to string while templatizing"), "")
						return false, nil
					}
					actualValue = append(actualValue, val)
				}
				for _, v := range j.Expected.Value {
					_, temp, err := tools.RenderIfTemplatized(v)
					if err != nil {
						utils.LogError(logger, err, "failed to render the expected header")
						return false, nil
					}
					val, ok := temp.(string)
					if !ok {
						utils.LogError(logger, fmt.Errorf("failed to convert the expected header value to string while templatizing"), "")
						return false, nil
					}
					expectedValue = append(expectedValue, val)
				}
			}
			if len(actualValue) != len(expectedValue) {
				isHeaderMismatch = true
				actualHeader[j.Actual.Key] = actualValue
				expectedHeader[j.Expected.Key] = expectedValue
			} else {
				for i, v := range actualValue {
					if v != expectedValue[i] {
						isHeaderMismatch = true
						actualHeader[j.Actual.Key] = actualValue
						expectedHeader[j.Expected.Key] = expectedValue
						break
					}
				}
			}
		}

		if isHeaderMismatch {
			for i, j := range expectedHeader {
				logDiffs.PushHeaderDiff(fmtSprint234(j), fmtSprint234(actualHeader[i]), i, headerNoise)
			}
		}

		actRespBodyType := pkg.GuessContentType([]byte(actualResponse.Body))
		expRespBodyType := pkg.GuessContentType([]byte(tc.HTTPResp.Body))

		if !res.BodyResult[0].Normal {
			if actRespBodyType != expRespBodyType {
				actRespBodyType = models.UnknownType
			}

			switch actRespBodyType {
			case models.JSON:
				patch, err := jsondiff.Compare(cleanExp, cleanAct)
				if err != nil {
					logger.Warn("failed to compute json diff", zap.Error(err))
				}

				// Checking for templatized values.
				for _, val := range patch {
					// Parse the value in map.
					expStringVal, ok := val.OldValue.(string)
					if !ok {
						continue
					}
					// Parse the body into json.
					expResponse, err := matcherUtils.ParseIntoJSON(expStringVal)
					if err != nil {
						utils.LogError(logger, err, "failed to parse the exp response into json")
						break
					}

					actStringVal, ok := val.Value.(string)
					if !ok {
						continue
					}

					actResponse, err := matcherUtils.ParseIntoJSON(actStringVal)
					if err != nil {
						utils.LogError(logger, err, "failed to parse the act response into json")
						break
					}
					matcherUtils.CompareResponses(&expResponse, &actResponse, "")
					jsonBytes, err := jsonMarshal234(expResponse)
					if err != nil {
						return false, nil
					}
					actJSONBytes, err := jsonMarshal234(actResponse)
					if err != nil {
						return false, nil
					}
					cleanExp = string(jsonBytes)
					cleanAct = string(actJSONBytes)
				}
				validatedJSON, err := matcherUtils.ValidateAndMarshalJSON(logger, &cleanExp, &cleanAct)
				if err != nil {
					return false, res
				}
				isBodyMismatch = false
				if validatedJSON.IsIdentical() {
					jsonComparisonResult, err = matcherUtils.JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering)
					if err != nil {
						return false, res
					}
					if !jsonComparisonResult.IsExact() {
						isBodyMismatch = true
					}
				} else {
					isBodyMismatch = true
				}
				// Comparing the body again after updating the expected
				patch, err = jsondiff.Compare(cleanExp, cleanAct)
				if err != nil {
					logger.Warn("failed to compute json diff", zap.Error(err))
				}
				for _, op := range patch {
					if jsonComparisonResult.Matches() {
						logDiffs.SetHasarrayIndexMismatch(true)
						logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.Differences(), ", "))
					}
					logDiffs.PushBodyDiff(fmtSprint234(op.OldValue), fmtSprint234(op.Value), bodyNoise)
				}
			default: // right now for every other type we would do a simple comparison, till we don't have dedicated logic for other types.
				if tc.HTTPResp.Body != actualResponse.Body {
					isBodyMismatch = true
				}
				logDiffs.PushBodyDiff(fmtSprint234(tc.HTTPResp.Body), fmtSprint234(actualResponse.Body), bodyNoise)
			}
		}

		currentRisk := models.None
		var currentCategories []models.FailureCategory

		// 1) Status code mismatch => HIGH & Broken (contract-level)
		if isStatusMismatch {
			currentRisk = models.High
			currentCategories = append(currentCategories, models.StatusCodeChanged)
		}

		//  2. Header mismatches => MEDIUM normally (schema unchanged: value-only),
		//     but Content-Type change => HIGH & Broken
		if isHeaderMismatch {
			currentCategories = append(currentCategories, models.HeaderChanged)

			headerRisk := models.Medium // default for header diffs

			if expVals, ok := expectedHeader["Content-Type"]; ok {
				actVals := actualHeader["Content-Type"]
				if !matcherUtils.CompareSlicesIgnoreOrder(expVals, actVals) {
					headerRisk = models.High
				}
			}

			currentRisk = matcherUtils.MaxRisk(currentRisk, headerRisk)

			// keep your logging of header diffs as-is
			for k, v := range expectedHeader {
				logDiffs.PushHeaderDiff(fmtSprint234(v), fmtSprint234(actualHeader[k]), k, headerNoise)
			}
		}

		// 3) Body mismatches
		if isBodyMismatch {
			if actRespBodyType == models.JSON && expRespBodyType == models.JSON {
				if assess, err := matcherUtils.ComputeFailureAssessmentJSON(cleanExp, cleanAct, bodyNoise, ignoreOrdering); err == nil && assess != nil {
					currentRisk = matcherUtils.MaxRisk(currentRisk, assess.Risk)
					currentCategories = append(currentCategories, assess.Category...)
				} else {
					// couldn't classify → conservative
					currentRisk = models.High
					currentCategories = append(currentCategories, models.InternalFailure)
				}
			} else {
				// Non-JSON body mismatch: cannot noise-mask or classify precisely → treat as Broken
				currentRisk = models.High
				currentCategories = append(currentCategories, models.SchemaBroken)
			}
		}

		// Remove duplicates
		catMap := make(map[models.FailureCategory]bool)
		uniqueCategories := []models.FailureCategory{}
		for _, cat := range currentCategories {
			if !catMap[cat] {
				catMap[cat] = true
				uniqueCategories = append(uniqueCategories, cat)
			}
		}

		res.FailureInfo = models.FailureInfo{
			Risk:     currentRisk,
			Category: uniqueCategories,
		}

		if isStatusMismatch || isHeaderMismatch || isBodyMismatch {
			skipSuccessMsg = true
			if emitFailureLogs {
				_, err := newLogger.Printf(logs)
				if err != nil {
					utils.LogError(logger, err, "failed to print the logs")
				}

				err = logDiffs.Render()
				if err != nil {
					utils.LogError(logger, err, "failed to render the diffs")
				}
			}
		} else {
			pass = true
		}
	}

	if !skipSuccessMsg {
		newLogger := ppNew234()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetPassingColorScheme())
		var log2 = ""
		log2 += newLogger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)
		_, err := newLogger.Printf(log2)
		if err != nil {
			utils.LogError(logger, err, "failed to print the logs")
		}
	}

	if len(tc.Assertions) > 1 || (len(tc.Assertions) == 1 && tc.Assertions[models.NoiseAssertion] == nil) {
		return AssertionMatch(tc, actualResponse, logger)
	}

	return pass, res
}

// AssertionMatch checks the assertions in the test case against the actual response, if all of the assertions pass, it returns true, it doesn't care about other parameters of the response,
// and make the test case pass.

// Assignable global variables for system and utility functions
var fmtSprintf234 = fmt.Sprintf
var strconvAtoi234 = strconv.Atoi

func AssertionMatch(tc *models.TestCase, actualResponse *models.HTTPResp, logger *zap.Logger) (bool, *models.Result) {
	pass := true
	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tc.HTTPResp.StatusCode,
			Actual:   actualResponse.StatusCode,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Expected: tc.HTTPResp.Body,
			Actual:   actualResponse.Body,
		}},
	}

	for assertionName, value := range tc.Assertions {
		switch assertionName {

		case models.StatusCode:
			expected, err := toInt(value)
			if err != nil || expected != actualResponse.StatusCode {
				pass = false
				logger.Error("status_code assertion failed", zap.Int("expected", expected), zap.Int("actual", actualResponse.StatusCode))
			} else {
				res.StatusCode.Normal = true
			}

		case models.StatusCodeClass:
			class := toString(value)
			var classStr string
			if len(class) == 3 {
				// handle if class given is status code without xx, e.g. 200
				if class[1:] != "xx" {
					classStr = fmtSprintf234("%cxx", class[0])
				} else {
					classStr = class
				}
			} else {
				classStr = class
			}
			actualClass := fmtSprintf234("%dxx", 200/100)
			if classStr != actualClass {
				pass = false
				logger.Error("status_code_class assertion failed", zap.String("expected", class), zap.String("actual", actualClass))
			}

		case models.StatusCodeIn:
			codes := toStringSlice(value)
			var ints []int
			for _, s := range codes {
				if i, err := strconvAtoi234(s); err == nil {
					ints = append(ints, i)
				}
			}
			found := false
			for _, c := range ints {
				if c == actualResponse.StatusCode {
					found = true
					break
				}
			}
			if !found {
				pass = false
				logger.Error("status_code_in assertion failed", zap.Ints("expectedCodes", ints), zap.Int("actual", actualResponse.StatusCode))
			}

		case models.HeaderEqual:
			// value should be a map[string]interface{} → we convert to map[string]string
			hm := toStringMap(value)
			for header, exp := range hm {
				act, ok := actualResponse.Header[header]
				if !ok || act != exp {
					pass = false
					logger.Error("header_equal assertion failed",
						zap.String("header", header),
						zap.String("expected", exp),
						zap.String("actual", act),
					)
				}
				logger.Info("header_equal assertion failed",
					zap.String("header", header),
					zap.String("expected", exp),
					zap.String("actual", act),
				)
			}

		case models.HeaderContains:
			hm := toStringMap(value)
			for header, exp := range hm {
				act, ok := actualResponse.Header[header]
				if !ok || !strings.Contains(act, exp) {
					pass = false
					logger.Error("header_contains assertion failed",
						zap.String("header", header),
						zap.String("expected_substr", exp),
						zap.String("actual", act),
					)
				}
			}

		case models.HeaderExists:
			switch v := value.(type) {

			// a flat slice of header names
			case []interface{}:
				for _, item := range v {
					hdr := fmtSprint234(item)
					if _, ok := actualResponse.Header[hdr]; !ok {
						pass = false
						logger.Error("header_exists assertion failed", zap.String("header", hdr))
					}
				}

			// a map[string]… where the keys are header names
			case map[string]interface{}:
				for hdr := range v {
					if _, ok := actualResponse.Header[hdr]; !ok {
						pass = false
						logger.Error("header_exists assertion failed", zap.String("header", hdr))
					}
				}

			case map[models.AssertionType]interface{}:
				for kt := range v {
					hdr := string(kt)
					if _, ok := actualResponse.Header[hdr]; !ok {
						pass = false
						logger.Error("header_exists assertion failed", zap.String("header", hdr))
					}
				}

			default:
				pass = false
				logger.Error("header_exists: unsupported format, expected slice or map", zap.Any("value", value))
			}

		case models.HeaderMatches:
			// value should be a map[string]interface{} → convert to map[string]string
			hm := toStringMap(value)
			for header, pattern := range hm {
				act, ok := actualResponse.Header[header]
				if !ok {
					pass = false
					logger.Error("header_matches: header not found", zap.String("header", header))
					continue
				}
				if matched, err := regexp.MatchString(pattern, act); err != nil || !matched {
					pass = false
					logger.Error("header_matches assertion failed",
						zap.String("header", header),
						zap.String("pattern", pattern),
						zap.String("actual", act),
						zap.Error(err),
					)
				}
			}

		case models.JsonEqual:
			expJSON := tc.HTTPResp.Body
			actJSON := actualResponse.Body
			if expJSON != actJSON {
				pass = false
				logger.Error("json_equal assertion failed", zap.String("expected", expJSON), zap.String("actual", actJSON))
			}

		case models.JsonContains:
			var expectedMap map[string]interface{}
			switch v := value.(type) {
			case map[string]interface{}:
				expectedMap = v
			case string:
				_ = jsonUnmarshal234([]byte(v), &expectedMap)
			default:
				pass = false
				logger.Error("json_contains: unexpected format", zap.Any("value", value))
				continue
			}
			if ok, _ := matcherUtils.JsonContains(actualResponse.Body, expectedMap); !ok {
				pass = false
				logger.Error("json_contains assertion failed", zap.Any("expected", expectedMap))
			}

		default:
			if assertionName != models.NoiseAssertion {
				logger.Warn("unhandled assertion type", zap.String("name", string(assertionName)))
			}
		}
	}

	if pass {
		res.StatusCode.Normal = true
		res.BodyResult[0].Normal = true
	}

	return pass, res
}

func FlattenHTTPResponse(h http.Header, body string) (map[string][]string, error) {
	m := map[string][]string{}
	for k, v := range h {
		m["header."+k] = []string{strings.Join(v, "")}
	}
	err := matcherUtils.AddHTTPBodyToMap(body, m)
	if err != nil {
		return m, err
	}
	return m, nil
}
