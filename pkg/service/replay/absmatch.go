package replay

import (
	"encoding/json"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// AbsMatch (Absolute Match) compares two test cases and returns a boolean value indicating whether they are equal or not.
// It also returns a AbsResult object which contains the results of the comparison.
// Parameters: tcs1, tcs2, noiseConfig, ignoreOrdering, logger
// Returns: bool, *models.AbsResult
func AbsMatch(tcs1, tcs2 *models.TestCase, noiseConfig map[string]map[string][]string, ignoreOrdering bool, logger *zap.Logger) (bool, bool, bool, *models.AbsResult) {
	if tcs1 == nil || tcs2 == nil {
		logger.Error("test case is nil", zap.Any("tcs1", tcs1), zap.Any("tcs2", tcs2))
		return false, false, false, nil
	}

	pass := true
	absResult := &models.AbsResult{}

	kindResult := models.StringResult{
		Normal:   true,
		Expected: string(tcs1.Kind),
		Actual:   string(tcs2.Kind),
	}

	nameResult := models.StringResult{
		Normal:   true,
		Expected: tcs1.Name,
		Actual:   tcs2.Name,
	}

	curlResult := models.StringResult{
		Normal:   true,
		Expected: tcs1.Curl,
		Actual:   tcs2.Curl,
	}

	//compare kind
	if tcs1.Kind != tcs2.Kind {
		kindResult.Normal = false
		logger.Debug("test case kind is not equal", zap.Any("tcs1Kind", tcs1.Kind), zap.Any("tcs2Kind", tcs2.Kind))
		pass = false
	}

	//compare name (just for debugging)
	if tcs1.Name != tcs2.Name {
		nameResult.Normal = false
		logger.Debug("test case name is not equal", zap.Any("tcs1Name", tcs1.Name), zap.Any("tcs2Name", tcs2.Name))
	}

	//compare curl
	ok := CompareCurl(tcs1.Curl, tcs2.Curl, logger)
	if !ok {
		curlResult.Normal = false
		logger.Debug("test case curl is not equal", zap.Any("tcs1Curl", tcs1.Curl), zap.Any("tcs2Curl", tcs2.Curl))
		pass = false
	}

	//compare http req
	reqPass, reqCompare := CompareHTTPReq(tcs1, tcs2, noiseConfig, ignoreOrdering, logger)
	if !reqPass {
		logger.Debug("test case http req is not equal", zap.Any("tcs1HttpReq", tcs1.HTTPReq), zap.Any("tcs2HttpReq", tcs2.HTTPReq))
		pass = false
	}

	//compare http resp
	respPass, respCompare := CompareHTTPResp(tcs1, tcs2, noiseConfig, ignoreOrdering, logger)
	if !respPass {
		logger.Debug("test case http resp is not equal", zap.Any("tcs1HttpResp", tcs1.HTTPResp), zap.Any("tcs2HttpResp", tcs2.HTTPResp))
		pass = false
	}

	absResult.Name = nameResult
	absResult.Kind = kindResult
	absResult.Req = reqCompare
	absResult.Resp = respCompare
	absResult.CurlResult = curlResult

	return pass, reqPass, respPass, absResult
}

// CompareHTTPReq compares two http requests and returns a boolean value indicating whether they are equal or not.
func CompareHTTPReq(tcs1, tcs2 *models.TestCase, _ models.GlobalNoise, ignoreOrdering bool, logger *zap.Logger) (bool, models.ReqCompare) {
	pass := true
	//compare http req
	reqCompare := models.ReqCompare{
		MethodResult: models.StringResult{
			Normal:   true,
			Expected: string(tcs1.HTTPReq.Method),
			Actual:   string(tcs2.HTTPReq.Method),
		},
		URLResult: models.StringResult{
			Normal:   true,
			Expected: tcs1.HTTPReq.URL,
			Actual:   tcs2.HTTPReq.URL,
		},
		URLParamsResult: []models.URLParamsResult{},
		ProtoMajor: models.IntResult{
			Normal:   true,
			Expected: tcs1.HTTPReq.ProtoMajor,
			Actual:   tcs2.HTTPReq.ProtoMajor,
		},
		ProtoMinor: models.IntResult{
			Normal:   true,
			Expected: tcs1.HTTPReq.ProtoMinor,
			Actual:   tcs2.HTTPReq.ProtoMinor,
		},
		HeaderResult: []models.HeaderResult{},
		BodyResult: models.BodyResult{
			Normal:   true,
			Expected: tcs1.HTTPReq.Body,
			Actual:   tcs2.HTTPReq.Body,
		},
	}

	if tcs1.HTTPReq.Method != tcs2.HTTPReq.Method {
		reqCompare.MethodResult.Normal = false
		logger.Debug("test case http req method is not equal", zap.Any("tcs1HttpReqMethod", tcs1.HTTPReq.Method), zap.Any("tcs2HttpReqMethod", tcs2.HTTPReq.Method))
		pass = false
	}

	if tcs1.HTTPReq.URL != tcs2.HTTPReq.URL {
		reqCompare.URLResult.Normal = false
		logger.Debug("test case http req url is not equal", zap.Any("tcs1HttpReqURL", tcs1.HTTPReq.URL), zap.Any("tcs2HttpReqURL", tcs2.HTTPReq.URL))
		pass = false
	}

	if tcs1.HTTPReq.ProtoMajor != tcs2.HTTPReq.ProtoMajor {
		reqCompare.ProtoMajor.Normal = false
		logger.Debug("test case http req proto major is not equal", zap.Any("tcs1HttpReqProtoMajor", tcs1.HTTPReq.ProtoMajor), zap.Any("tcs2HttpReqProtoMajor", tcs2.HTTPReq.ProtoMajor))
		pass = false
	}

	if tcs1.HTTPReq.ProtoMinor != tcs2.HTTPReq.ProtoMinor {
		reqCompare.ProtoMinor.Normal = false
		logger.Debug("test case http req proto minor is not equal", zap.Any("tcs1HttpReqProtoMinor", tcs1.HTTPReq.ProtoMinor), zap.Any("tcs2HttpReqProtoMinor", tcs2.HTTPReq.ProtoMinor))
		pass = false
	}

	//compare url params
	urlParams1 := tcs1.HTTPReq.URLParams
	urlParams2 := tcs2.HTTPReq.URLParams
	if len(urlParams1) == len(urlParams2) {
		ok := CompareURLParams(urlParams1, urlParams2, &reqCompare.URLParamsResult)
		if !ok {
			logger.Debug("test case http req url params are not equal", zap.Any("tcs1HttpReqURLParams", tcs1.HTTPReq.URLParams), zap.Any("tcs2HttpReqURLParams", tcs2.HTTPReq.URLParams))
			pass = false
		}
	} else {
		logger.Debug("test case http req url params are not equal", zap.Any("tcs1HttpReqURLParams", tcs1.HTTPReq.URLParams), zap.Any("tcs2HttpReqURLParams", tcs2.HTTPReq.URLParams))
		pass = false
	}

	reqHeaderNoise := map[string][]string{}
	reqHeaderNoise["Keploy-Test-Id"] = []string{}

	// compare http req headers
	ok := CompareHeaders(pkg.ToHTTPHeader(tcs1.HTTPReq.Header), pkg.ToHTTPHeader(tcs2.HTTPReq.Header), &reqCompare.HeaderResult, reqHeaderNoise)
	if !ok {
		logger.Debug("test case http req headers are not equal", zap.Any("tcs1HttpReqHeaders", tcs1.HTTPReq.Header), zap.Any("tcs2HttpReqHeaders", tcs2.HTTPReq.Header))
		pass = false
	}

	reqBodyNoise := map[string][]string{}

	// compare http req body
	bodyType1 := models.BodyTypePlain
	if json.Valid([]byte(tcs1.HTTPReq.Body)) {
		bodyType1 = models.BodyTypeJSON
	}

	bodyType2 := models.BodyTypePlain
	if json.Valid([]byte(tcs2.HTTPReq.Body)) {
		bodyType2 = models.BodyTypeJSON
	}

	if bodyType1 != bodyType2 {
		logger.Debug("test case http req body type is not equal", zap.Any("tcs1HttpReqBodyType", bodyType1), zap.Any("tcs2HttpReqBodyType", bodyType2))
		pass = false
		reqCompare.BodyResult.Normal = false
		return pass, reqCompare
	}

	bodyRes := true
	// stores the json body after removing the noise
	cleanExp, cleanAct := tcs1.HTTPReq.Body, tcs2.HTTPReq.Body
	var jsonComparisonResult JSONComparisonResult
	if !Contains(MapToArray(reqBodyNoise), "body") && bodyType1 == models.BodyTypeJSON {
		//validate the stored json
		validatedJSON, err := ValidateAndMarshalJSON(logger, &cleanExp, &cleanAct)
		if err != nil {
			logger.Error("failed to validate and marshal json", zap.Error(err))
			reqCompare.BodyResult.Normal = false
			return false, reqCompare
		}
		if validatedJSON.isIdentical {
			jsonComparisonResult, err = JSONDiffWithNoiseControl(validatedJSON, reqBodyNoise, ignoreOrdering)
			exact := jsonComparisonResult.isExact
			if err != nil {
				logger.Error("failed to compare json", zap.Error(err))
				reqCompare.BodyResult.Normal = false
				return false, reqCompare
			}

			if !exact {
				pass = false
				bodyRes = false
			}
		} else {
			pass = false
			bodyRes = false
		}

		// debug log for cleanExp and cleanAct
		logger.Debug("cleanExp", zap.Any("", cleanExp))
		logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !Contains(MapToArray(reqBodyNoise), "body") && tcs1.HTTPReq.Body != tcs2.HTTPReq.Body {
			pass = false
			bodyRes = false
		}
	}

	if !bodyRes {
		reqCompare.BodyResult.Normal = false
	}

	return pass, reqCompare
}

// CompareHTTPResp compares two http responses and returns a boolean value indicating whether they are equal or not.
func CompareHTTPResp(tcs1, tcs2 *models.TestCase, noiseConfig models.GlobalNoise, ignoreOrdering bool, logger *zap.Logger) (bool, models.RespCompare) {
	pass := true
	//compare http resp
	respCompare := models.RespCompare{
		StatusCode: models.IntResult{
			Normal:   true,
			Expected: tcs1.HTTPResp.StatusCode,
			Actual:   tcs2.HTTPResp.StatusCode,
		},
		HeadersResult: []models.HeaderResult{},
		BodyResult: models.BodyResult{
			Normal:   true,
			Expected: tcs1.HTTPResp.Body,
			Actual:   tcs2.HTTPResp.Body,
		},
	}

	if tcs1.HTTPResp.StatusCode != tcs2.HTTPResp.StatusCode {
		respCompare.StatusCode.Normal = false
		logger.Debug("test case http resp status code is not equal", zap.Any("tcs1HttpRespStatusCode", tcs1.HTTPResp.StatusCode), zap.Any("tcs2HttpRespStatusCode", tcs2.HTTPResp.StatusCode))
		pass = false
	}

	//compare the auto added noise in test case
	noise1 := tcs1.Noise
	noise2 := tcs2.Noise
	ok := CompareNoise(noise1, noise2)
	if !ok {
		logger.Debug("test case noise is not equal", zap.Any("tcs1Noise", tcs1.Noise), zap.Any("tcs2Noise", tcs2.Noise))
		logger.Debug("response body and headers can not be compared because noise is not equal")
		pass = false
		respCompare.BodyResult.Normal = false
		return pass, respCompare
	}

	noise := noise1

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
			bodyNoise[x] = regexArr
		} else if a[0] == "header" {
			headerNoise[a[len(a)-1]] = regexArr
		}
	}

	// compare http resp headers
	ok = CompareHeaders(pkg.ToHTTPHeader(tcs1.HTTPResp.Header), pkg.ToHTTPHeader(tcs2.HTTPResp.Header), &respCompare.HeadersResult, headerNoise)
	if !ok {
		logger.Debug("test case http resp headers are not equal", zap.Any("tcs1HttpRespHeaders", tcs1.HTTPResp.Header), zap.Any("tcs2HttpRespHeaders", tcs2.HTTPResp.Header))
		pass = false
	}

	// compare http resp body
	bodyType1 := models.BodyTypePlain
	if json.Valid([]byte(tcs1.HTTPResp.Body)) {
		bodyType1 = models.BodyTypeJSON
	}

	bodyType2 := models.BodyTypePlain
	if json.Valid([]byte(tcs2.HTTPResp.Body)) {
		bodyType2 = models.BodyTypeJSON
	}

	if bodyType1 != bodyType2 {
		logger.Debug("test case http resp body type is not equal", zap.Any("tcs1HttpRespBodyType", bodyType1), zap.Any("tcs2HttpRespBodyType", bodyType2))
		pass = false
		respCompare.BodyResult.Normal = false
		return pass, respCompare
	}

	bodyRes := true

	// stores the json body after removing the noise
	cleanExp, cleanAct := tcs1.HTTPResp.Body, tcs2.HTTPResp.Body
	var jsonComparisonResult JSONComparisonResult
	if !Contains(MapToArray(noise), "body") && bodyType1 == models.BodyTypeJSON {
		//validate the stored json
		validatedJSON, err := ValidateAndMarshalJSON(logger, &cleanExp, &cleanAct)
		if err != nil {
			logger.Error("failed to validate and marshal json", zap.Error(err))
			respCompare.BodyResult.Normal = false
			return false, respCompare
		}
		if validatedJSON.isIdentical {
			jsonComparisonResult, err = JSONDiffWithNoiseControl(validatedJSON, bodyNoise, ignoreOrdering)
			exact := jsonComparisonResult.isExact
			if err != nil {
				logger.Error("failed to compare json", zap.Error(err))
				respCompare.BodyResult.Normal = false
				return false, respCompare
			}
			if !exact {
				pass = false
				bodyRes = false
			}
		} else {
			pass = false
			bodyRes = false
		}

		// debug log for cleanExp and cleanAct
		logger.Debug("cleanExp", zap.Any("", cleanExp))
		logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !Contains(MapToArray(noise), "body") && tcs1.HTTPResp.Body != tcs2.HTTPResp.Body {
			pass = false
			bodyRes = false
		}
	}

	if !bodyRes {
		respCompare.BodyResult.Normal = false
	}

	return pass, respCompare
}

func CompareURLParams(urlParams1, urlParams2 map[string]string, urlParamsResult *[]models.URLParamsResult) bool {
	pass := true
	for k, v := range urlParams1 {
		if v2, ok := urlParams2[k]; ok {

			if v != v2 {
				pass = false
			}

			*urlParamsResult = append(*urlParamsResult, models.URLParamsResult{
				Normal: v == v2,
				Expected: models.Params{
					Key:   k,
					Value: v,
				},
				Actual: models.Params{
					Key:   k,
					Value: v2,
				},
			})
		} else {
			pass = false
		}
	}
	return pass
}

func CompareNoise(noise1, noise2 map[string][]string) bool {
	pass := true
	for k, v := range noise1 {
		if v2, ok := noise2[k]; ok {
			if len(v) != len(v2) {
				pass = false
			} else {
				for i := 0; i < len(v); i++ {
					if v[i] != v2[i] {
						pass = false
					}
				}
			}
		} else {
			pass = false
		}
	}
	return pass
}

func CompareCurl(curl1, curl2 string, logger *zap.Logger) bool {
	// Parse the values into method, URL, headers, and data
	method1, url1, headers1, data1 := parseCurlString(curl1)
	method2, url2, headers2, data2 := parseCurlString(curl2)

	// Compare method, URL, and data
	if method1 != method2 || url1 != url2 || data1 != data2 {
		return false
	}

	curlHeaderNoise := map[string][]string{}
	curlHeaderNoise["Keploy-Test-Id"] = []string{}

	hres := []models.HeaderResult{}
	ok := CompareHeaders(pkg.ToHTTPHeader(headers1), pkg.ToHTTPHeader(headers2), &hres, curlHeaderNoise)
	if !ok {
		logger.Debug("test case curl headers are not equal", zap.Any("curlHeaderResult", hres))
		return false
	}
	return true
}

func parseCurlString(curlString string) (method, url string, headers map[string]string, data string) {
	lines := strings.Split(curlString, "\\")
	headers = make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--request") {
			method = strings.TrimSpace(strings.Split(line, " ")[1])
		} else if strings.HasPrefix(line, "--url") {
			url = strings.TrimSpace(strings.Split(line, " ")[1])
		} else if strings.HasPrefix(line, "--header") {
			headerParts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, "--header")), ":", 2)
			if len(headerParts) == 2 {
				headers[strings.TrimSpace(headerParts[0])] = strings.TrimSpace(headerParts[1])
			}
		} else if strings.HasPrefix(line, "--data") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "--data"))
		}
	}
	return
}
