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

// MatchOption tunes Match without changing its signature — enterprise and
// k8s-proxy both call Match, so a new parameter would break them on their next
// OSS bump.
type MatchOption func(*matchOptions)

type matchOptions struct{ autoHeaderNoise bool }

// WithAutoHeaderNoise forgives a VALUE difference on response headers the server
// mints fresh on every call — the HTTP date, request/correlation ids and
// distributed-trace ids (models.IsVolatileResponseHeader).
//
// Presence is still asserted: a header that only one side carries stays a
// failure. Off unless the caller asks.
func WithAutoHeaderNoise(enabled bool) MatchOption {
	return func(o *matchOptions) { o.autoHeaderNoise = enabled }
}

func Match(tc *models.TestCase, actualResponse *models.HTTPResp, noiseConfig map[string]map[string][]string, ignoreOrdering bool, compareAll bool, logger *zap.Logger, emitFailureLogs bool, opts ...MatchOption) (bool, *models.Result) {
	var mo matchOptions
	for _, opt := range opts {
		opt(&mo)
	}
	// If the response body was skipped during recording (>1MB), compute body size comparison
	// and clear the actual body so the normal comparison runs (empty vs empty).
	var bodySizeResult models.IntResult
	if tc.HTTPResp.BodySkipped {
		actualBodySize := int64(len(actualResponse.Body))
		bodySizeMatch := tc.HTTPResp.BodySize == actualBodySize

		logger.Info("response body was greater than 1MB during recording, comparing body size",
			zap.String("testcase", tc.Name),
			zap.Int64("expected_size", tc.HTTPResp.BodySize),
			zap.Int64("actual_size", actualBodySize),
			zap.Bool("size_match", bodySizeMatch))

		// Log actual response body as debug before clearing
		logger.Debug("actual response body (skipped during recording)",
			zap.String("testcase", tc.Name),
			zap.String("body", actualResponse.Body))

		bodySizeResult = models.IntResult{
			Normal:   bodySizeMatch,
			Expected: int(tc.HTTPResp.BodySize),
			Actual:   int(actualBodySize),
		}

		// Clear actual body so body comparison below runs as empty vs empty
		actualResponse.Body = ""
	}

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
		BodySizeResult: bodySizeResult,
	}

	// If body size comparison failed, mark pass as false
	if tc.HTTPResp.BodySkipped && !bodySizeResult.Normal {
		pass = false
	}

	noise := tc.Noise
	// Copy the shared config maps before merging this test case's noise into
	// them, otherwise each test case permanently widens the noise applied to
	// every later one.
	var (
		bodyNoise    = matcherUtils.CloneNoiseMap(noiseConfig["body"])
		headerNoise  = matcherUtils.CloneNoiseMap(noiseConfig["header"])
		wildcardBody bool
	)
	// wildcardBody carries this on its own. Writing the sentinel back into
	// tc.Noise used to be how it reached the body-skip check; that mutated the
	// caller's test case, panicked when tc.Noise was nil, and could be persisted
	// back to disk by a later re-encode.
	ignoreFields, hasWildcard := bodyNoise["*"]
	wildcardBody = hasWildcard && len(ignoreFields) > 0 && ignoreFields[0] == "*"

	tcBodyNoise, tcHeaderNoise, skipBody := matcherUtils.SplitNoise(noise, logger)
	// The global wildcard means "ignore every response body" on its own; it
	// must not depend on the test case also carrying a bare "body" key, which
	// stops being the skip sentinel once that key lists field paths.
	skipBody = skipBody || wildcardBody
	for field, regexArr := range tcBodyNoise {
		bodyNoise[field] = regexArr
	}
	for field, regexArr := range tcHeaderNoise {
		headerNoise[field] = regexArr
	}

	// stores the json body after removing the noise
	cleanExp, cleanAct := tc.HTTPResp.Body, actualResponse.Body

	var jsonComparisonResult matcherUtils.JSONComparisonResult
	if !skipBody && bodyType == models.JSON && jsonValid234([]byte(tc.HTTPResp.Body)) {
		//validate the stored json
		validatedJSON, err := matcherUtils.ValidateAndMarshalJSON(logger, &cleanExp, &cleanAct)
		if err != nil {
			return false, res
		}
		if validatedJSON.IsIdentical() {
			jsonComparisonResult, err = matcherUtils.JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering, logger)
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
		} else if !skipBody && tc.HTTPResp.Body != actualResponse.Body {
			pass = false
		}
	}

	res.BodyResult[0].Normal = pass

	// A body that failed while carrying noise is the one moment the user needs to
	// know that some of that noise is inert: the report names a field they
	// believe they already excluded. Only on a JSON failure — noise paths address
	// JSON fields, so on any other body every entry would read as dead and the
	// advice would be nonsense. Only on failure, so the happy path pays nothing
	// for the extra walk.
	//
	// tcBodyNoise, not the merged map: a globalNoise entry applies to every
	// endpoint by design, so naming it dead on each case that happens not to have
	// that field would emit a line per failing case and bury the real failures.
	// Only the test case's own recorded noise is a claim about THIS response.
	if !pass && !skipBody && bodyType == models.JSON {
		matcherUtils.WarnUnmatchableBodyNoise(logger, tc.Name, tcBodyNoise, tc.HTTPResp.Body, actualResponse.Body)
	}

	if !matcherUtils.CompareHeaders(pkg.ToHTTPHeader(tc.HTTPResp.Header), pkg.ToHTTPHeader(actualResponse.Header), hRes, headerNoise) {
		res.HeadersResult = *hRes

		// Forgive a VALUE difference on a header the server mints fresh per call.
		//
		// Applied to the comparison RESULT, deliberately not by seeding
		// headerNoise: that map is resolved by matcher.SubstringKeyMatch, which
		// uses strings.Contains, so a "date" entry would also swallow
		// "X-Candidate-Id" and "X-Validate-Token" — headers that merely contain
		// the word. Matching the result's key exactly confines the forgiveness to
		// the header it was meant for.
		//
		// Only when BOTH sides carried the header. CompareHeaders reports a
		// missing or unexpected header with a nil Value on the absent side, and
		// those stay failures: a server that stops emitting X-Request-Id, or
		// starts emitting one it never did, is a real change and must not be
		// hidden by a rule about values.
		if mo.autoHeaderNoise {
			for i := range res.HeadersResult {
				hr := &res.HeadersResult[i]
				if hr.Normal || hr.Expected.Value == nil || hr.Actual.Value == nil {
					continue
				}
				name := hr.Expected.Key
				if name == "" {
					name = hr.Actual.Key
				}
				// An explicit user pattern outranks this. If the user wrote a
				// regex for this header they narrowed it on purpose, and
				// CompareHeaders already judged the replayed value against it —
				// widening that into a blanket ignore would silently discard the
				// constraint. Resolved through SubstringKeyMatch so the lookup
				// sees the entry exactly as CompareHeaders did, whatever the
				// user's casing. (An UNCONDITIONAL user entry never reaches here:
				// CompareHeaders would already have marked the header normal.)
				if patterns, constrained := matcherUtils.SubstringKeyMatch(name, headerNoise); constrained && len(patterns) > 0 {
					continue
				}
				if models.IsVolatileResponseHeader(name) {
					logger.Debug("ignoring value drift on a per-request-volatile response header",
						zap.String("header", name))
					hr.Normal = true
				}
			}
		}

		// If body matches but content-length differs, ignore the content-length difference
		if res.BodyResult[0].Normal {
			for i := range res.HeadersResult {
				if strings.ToLower(res.HeadersResult[i].Expected.Key) == "content-length" && !res.HeadersResult[i].Normal {
					logger.Debug("Ignoring Content-Length mismatch since body content is identical",
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
					logger.Debug("failed to compute json diff", zap.Error(err))
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
					jsonComparisonResult, err = matcherUtils.JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering, logger)
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
					logger.Debug("failed to compute json diff", zap.Error(err))
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
		var bodyAssessment *models.FailureAssessment
		if isBodyMismatch {
			if actRespBodyType == models.JSON && expRespBodyType == models.JSON {
				if assess, err := matcherUtils.ComputeFailureAssessmentJSON(cleanExp, cleanAct, bodyNoise, ignoreOrdering); err == nil && assess != nil {
					currentRisk = matcherUtils.MaxRisk(currentRisk, assess.Risk)
					currentCategories = append(currentCategories, assess.Category...)
					bodyAssessment = assess
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
			Risk:       currentRisk,
			Category:   uniqueCategories,
			Assessment: bodyAssessment,
		}

		isBodySizeMismatch := tc.HTTPResp.BodySkipped && !bodySizeResult.Normal

		if isStatusMismatch || isHeaderMismatch || isBodyMismatch || isBodySizeMismatch {
			skipSuccessMsg = true

			if isBodySizeMismatch {
				logDiffs.PushBodyDiff(
					fmtSprint234(fmt.Sprintf("body_size: %d bytes", bodySizeResult.Expected)),
					fmtSprint234(fmt.Sprintf("body_size: %d bytes", bodySizeResult.Actual)),
					nil,
				)
			}

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

	// When emitFailureLogs is false, caller is handling logging themselves (e.g., streaming comparison)
	// so we skip the success message as well
	if !skipSuccessMsg && emitFailureLogs {
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
			actualClass := fmtSprintf234("%dxx", actualResponse.StatusCode/100)
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
				logger.Debug("unhandled assertion type", zap.String("name", string(assertionName)))
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
