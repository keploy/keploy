// Package http for http matching
package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/v2/pkg"
	matcherUtils "go.keploy.io/server/v2/pkg/matcher"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
)

func Match(tc *models.TestCase, actualResponse *models.HTTPResp, noiseConfig map[string]map[string][]string, ignoreOrdering bool, logger *zap.Logger) (bool, *models.Result) {
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
	var jsonComparisonResult matcherUtils.JSONComparisonResult
	if !matcherUtils.Contains(matcherUtils.MapToArray(noise), "body") && bodyType == models.BodyTypeJSON {
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
		logger.Debug("cleanExp", zap.Any("", cleanExp))
		logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !matcherUtils.Contains(matcherUtils.MapToArray(noise), "body") && tc.HTTPResp.Body != actualResponse.Body {
			pass = false
		}
	}

	res.BodyResult[0].Normal = pass

	if !matcherUtils.CompareHeaders(pkg.ToHTTPHeader(tc.HTTPResp.Header), pkg.ToHTTPHeader(actualResponse.Header), hRes, headerNoise) {
		pass = false
	}

	res.HeadersResult = *hRes
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
		newLogger := pp.New()
		newLogger.WithLineInfo = false
		newLogger.SetColorScheme(models.GetFailingColorScheme())
		var logs = ""

		logs = logs + newLogger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)

		// ------------ DIFFS RELATED CODE -----------
		if !res.StatusCode.Normal {
			logDiffs.PushStatusDiff(fmt.Sprint(res.StatusCode.Expected), fmt.Sprint(res.StatusCode.Actual))
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
			for i, v := range actualValue {
				if v != expectedValue[i] {
					fmt.Println(v, expectedValue[i])
					isHeaderMismatch = true
					actualHeader[j.Actual.Key] = actualValue
					expectedHeader[j.Expected.Key] = expectedValue
					break
				}
			}
		}

		if isHeaderMismatch {
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
					jsonBytes, err := json.Marshal(expResponse)
					if err != nil {
						return false, nil
					}
					actJsonBytes, err := json.Marshal(actResponse)
					if err != nil {
						return false, nil
					}
					tc.HTTPResp.Body = string(jsonBytes)
					actualResponse.Body = string(actJsonBytes)
				}

				validatedJSON, err := matcherUtils.ValidateAndMarshalJSON(logger, &tc.HTTPResp.Body, &actualResponse.Body)
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
					isBodyMismatch = true

					// Comparing the body again after updating the expected
					patch, err = jsondiff.Compare(tc.HTTPResp.Body, actualResponse.Body)
					if err != nil {
						logger.Warn("failed to compute json diff", zap.Error(err))
					}
					for _, op := range patch {
						if jsonComparisonResult.Matches() {
							logDiffs.SetHasarrayIndexMismatch(true)
							logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.Differences(), ", "))
						}
						logDiffs.PushBodyDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value), bodyNoise)
					}
				}
			} else {
				logDiffs.PushBodyDiff(fmt.Sprint(tc.HTTPResp.Body), fmt.Sprint(actualResponse.Body), bodyNoise)
			}
		}

		fmt.Println(isStatusMismatch, isHeaderMismatch, isBodyMismatch)

		if isStatusMismatch || isHeaderMismatch || isBodyMismatch {
			skipSuccessMsg = true
			_, err := newLogger.Printf(logs)
			if err != nil {
				utils.LogError(logger, err, "failed to print the logs")
			}

			err = logDiffs.Render()
			if err != nil {
				utils.LogError(logger, err, "failed to render the diffs")
			}
		}

	}

	fmt.Println(skipSuccessMsg)

	if !skipSuccessMsg {
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
	err := matcherUtils.AddHTTPBodyToMap(body, m)
	if err != nil {
		return m, err
	}
	return m, nil
}
