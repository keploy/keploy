package test

import (
	"encoding/json"
	"strings"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

// AbsMatch (Absolute Match) compares two test cases and returns a boolean value indicating whether they are equal or not.
// It also returns a AbsResult object which contains the results of the comparison.
// Parameters: tcs1, tcs2, noiseConfig, log
// Returns: bool, *models.AbsResult
func AbsMatch(tcs1, tcs2 *models.TestCase, noiseConfig models.GlobalNoise, logger *zap.Logger) (bool, *models.AbsResult) {
	if tcs1 == nil || tcs2 == nil {
		logger.Error("test case is nil", zap.Any("tcs1", tcs1), zap.Any("tcs2", tcs2))
		return false, nil
	}

	pass := true
	absResult := &models.AbsResult{}

	kindResult := models.StringResult{
		Normal:   false,
		Expected: string(tcs1.Kind),
		Actual:   string(tcs2.Kind),
	}

	nameResult := models.StringResult{
		Normal:   false,
		Expected: tcs1.Name,
		Actual:   tcs2.Name,
	}

	curlResult := models.StringResult{
		Normal:   false,
		Expected: tcs1.Curl,
		Actual:   tcs2.Curl,
	}

	//compare kind
	if tcs1.Kind == tcs2.Kind {
		kindResult.Normal = true
	} else {
		logger.Debug("test case kind is not equal", zap.Any("tcs1Kind", tcs1.Kind), zap.Any("tcs2Kind", tcs2.Kind))
		pass = false
	}

	//compare name
	if tcs1.Name == tcs2.Name {
		nameResult.Normal = true
	} else {
		logger.Debug("test case name is not equal", zap.Any("tcs1Name", tcs1.Name), zap.Any("tcs2Name", tcs2.Name))
		pass = false
	}

	//compare curl
	ok := CompareCurl(tcs1.Curl, tcs2.Curl, logger)
	if ok {
		curlResult.Normal = true
	} else {
		logger.Debug("test case curl is not equal", zap.Any("tcs1Curl", tcs1.Curl), zap.Any("tcs2Curl", tcs2.Curl))
		pass = false
	}

	//compare http req
	reqPass, reqResult := CompareHTTPReq(tcs1, tcs2, noiseConfig, logger)
	if !reqPass {
		logger.Debug("test case http req is not equal", zap.Any("tcs1HttpReq", tcs1.HttpReq), zap.Any("tcs2HttpReq", tcs2.HttpReq))
		pass = false
	}

	//compare http resp
	respPass, respResult := CompareHTTPResp(tcs1, tcs2, noiseConfig, logger)
	if !respPass {
		logger.Debug("test case http resp is not equal", zap.Any("tcs1HttpResp", tcs1.HttpResp), zap.Any("tcs2HttpResp", tcs2.HttpResp))
		pass = false
	}

	absResult.Kind = kindResult
	absResult.ReqResult = reqResult
	absResult.RespResult = respResult
	absResult.CurlResult = curlResult

	return pass, absResult
}

// CompareHTTPReq compares two http requests and returns a boolean value indicating whether they are equal or not.
func CompareHTTPReq(tcs1, tcs2 *models.TestCase, noiseConfig models.GlobalNoise, logger *zap.Logger) (bool, models.ReqResult) {
	pass := true
	//compare http req
	reqResult := models.ReqResult{
		MethodResult: models.StringResult{
			Normal:   false,
			Expected: string(tcs1.HttpReq.Method),
			Actual:   string(tcs2.HttpReq.Method),
		},
		UrlResult: models.StringResult{
			Normal:   false,
			Expected: tcs1.HttpReq.URL,
			Actual:   tcs2.HttpReq.URL,
		},
		UrlParamsResult: []models.URLParamsResult{},
		ProtoMajor: models.IntResult{
			Normal:   false,
			Expected: tcs1.HttpReq.ProtoMajor,
			Actual:   tcs2.HttpReq.ProtoMajor,
		},
		ProtoMinor: models.IntResult{
			Normal:   false,
			Expected: tcs1.HttpReq.ProtoMinor,
			Actual:   tcs2.HttpReq.ProtoMinor,
		},
		HeaderResult: []models.HeaderResult{},
		BodyResult: models.BodyResult{
			Normal:   false,
			Expected: tcs1.HttpReq.Body,
			Actual:   tcs2.HttpReq.Body,
		},
		HostResult: models.StringResult{
			Normal:   false,
			Expected: tcs1.HttpReq.Host,
			Actual:   tcs2.HttpReq.Host,
		},
	}

	if tcs1.HttpReq.Method == tcs2.HttpReq.Method {
		reqResult.MethodResult.Normal = true
	} else {
		logger.Debug("test case http req method is not equal", zap.Any("tcs1HttpReqMethod", tcs1.HttpReq.Method), zap.Any("tcs2HttpReqMethod", tcs2.HttpReq.Method))
		pass = false
	}

	if tcs1.HttpReq.URL == tcs2.HttpReq.URL {
		reqResult.UrlResult.Normal = true
	} else {
		logger.Debug("test case http req url is not equal", zap.Any("tcs1HttpReqURL", tcs1.HttpReq.URL), zap.Any("tcs2HttpReqURL", tcs2.HttpReq.URL))
		pass = false
	}

	if tcs1.HttpReq.ProtoMajor == tcs2.HttpReq.ProtoMajor {
		reqResult.ProtoMajor.Normal = true
	} else {
		logger.Debug("test case http req proto major is not equal", zap.Any("tcs1HttpReqProtoMajor", tcs1.HttpReq.ProtoMajor), zap.Any("tcs2HttpReqProtoMajor", tcs2.HttpReq.ProtoMajor))
		pass = false
	}

	if tcs1.HttpReq.ProtoMinor == tcs2.HttpReq.ProtoMinor {
		reqResult.ProtoMinor.Normal = true
	} else {
		logger.Debug("test case http req proto minor is not equal", zap.Any("tcs1HttpReqProtoMinor", tcs1.HttpReq.ProtoMinor), zap.Any("tcs2HttpReqProtoMinor", tcs2.HttpReq.ProtoMinor))
		pass = false
	}

	//compare url params
	urlParams1 := tcs1.HttpReq.URLParams
	urlParams2 := tcs2.HttpReq.URLParams
	if len(urlParams1) == len(urlParams2) {
		ok := CompareURLParams(urlParams1, urlParams2, &reqResult.UrlParamsResult)
		if !ok {
			logger.Debug("test case http req url params are not equal", zap.Any("tcs1HttpReqURLParams", tcs1.HttpReq.URLParams), zap.Any("tcs2HttpReqURLParams", tcs2.HttpReq.URLParams))
			pass = false
		}
	} else {
		logger.Debug("test case http req url params are not equal", zap.Any("tcs1HttpReqURLParams", tcs1.HttpReq.URLParams), zap.Any("tcs2HttpReqURLParams", tcs2.HttpReq.URLParams))
		pass = false
	}

	reqHeaderNoise := map[string][]string{}
	reqHeaderNoise["Keploy-Test-Id"] = []string{}
	reqHeaderNoise["Accept-Encoding"] = []string{}

	// compare http req headers
	ok := CompareHeaders(pkg.ToHttpHeader(tcs1.HttpReq.Header), pkg.ToHttpHeader(tcs2.HttpReq.Header), &reqResult.HeaderResult, reqHeaderNoise)
	if !ok {
		logger.Debug("test case http req headers are not equal", zap.Any("tcs1HttpReqHeaders", tcs1.HttpReq.Header), zap.Any("tcs2HttpReqHeaders", tcs2.HttpReq.Header))
		pass = false
	}

	reqBodyNoise := map[string][]string{}

	// compare http req body
	bodyType1 := models.BodyTypePlain
	if json.Valid([]byte(tcs1.HttpReq.Body)) {
		bodyType1 = models.BodyTypeJSON
	}

	bodyType2 := models.BodyTypePlain
	if json.Valid([]byte(tcs2.HttpReq.Body)) {
		bodyType2 = models.BodyTypeJSON
	}

	if bodyType1 != bodyType2 {
		logger.Debug("test case http req body type is not equal", zap.Any("tcs1HttpReqBodyType", bodyType1), zap.Any("tcs2HttpReqBodyType", bodyType2))
		pass = false
	} else {
		if bodyType1 == models.BodyTypeJSON {
			cleanExp, cleanAct, match, _, err := Match(tcs1.HttpReq.Body, tcs2.HttpReq.Body, reqBodyNoise, logger, false)
			if err != nil || !match {
				logger.Debug("test case http req body is not equal", zap.Any("tcs1HttpReqBody", tcs1.HttpReq.Body), zap.Any("tcs2HttpReqBody", tcs2.HttpReq.Body))
				pass = false
			} else {
				reqResult.BodyResult.Normal = true
			}
			logger.Debug("ExpReq", zap.Any("", cleanExp))
			logger.Debug("ActReq", zap.Any("", cleanAct))
		} else {
			if tcs1.HttpReq.Body != tcs2.HttpReq.Body {
				logger.Debug("test case http req body is not equal", zap.Any("tcs1HttpReqBody", tcs1.HttpReq.Body), zap.Any("tcs2HttpReqBody", tcs2.HttpReq.Body))
				pass = false
			} else {
				reqResult.BodyResult.Normal = true
			}
		}
	}

	//compare host
	if tcs1.HttpReq.Host == tcs2.HttpReq.Host {
		reqResult.HostResult.Normal = true
	} else {
		logger.Debug("test case http req host is not equal", zap.Any("tcs1HttpReqHost", tcs1.HttpReq.Host), zap.Any("tcs2HttpReqHost", tcs2.HttpReq.Host))
		pass = false
	}

	return pass, reqResult
}

// CompareHTTPResp compares two http responses and returns a boolean value indicating whether they are equal or not.
func CompareHTTPResp(tcs1, tcs2 *models.TestCase, noiseConfig models.GlobalNoise, logger *zap.Logger) (bool, models.RespResult) {
	pass := true
	//compare http resp
	respResult := models.RespResult{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tcs1.HttpResp.StatusCode,
			Actual:   tcs2.HttpResp.StatusCode,
		},
		HeadersResult: []models.HeaderResult{},
		BodyResult: models.BodyResult{
			Normal:   false,
			Expected: tcs1.HttpResp.Body,
			Actual:   tcs2.HttpResp.Body,
		},
	}

	if tcs1.HttpResp.StatusCode == tcs2.HttpResp.StatusCode {
		respResult.StatusCode.Normal = true
	} else {
		logger.Debug("test case http resp status code is not equal", zap.Any("tcs1HttpRespStatusCode", tcs1.HttpResp.StatusCode), zap.Any("tcs2HttpRespStatusCode", tcs2.HttpResp.StatusCode))
		pass = false
	}

	//compare the auto added noise in test case
	noise1 := tcs1.Noise
	noise2 := tcs2.Noise
	ok := CompareNoise(noise1, noise2, logger)
	if !ok {
		logger.Debug("test case noise is not equal", zap.Any("tcs1Noise", tcs1.Noise), zap.Any("tcs2Noise", tcs2.Noise))
		logger.Debug("response body and headers can not be compared because noise is not equal")
		pass = false
		return pass, respResult
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
	ok = CompareHeaders(pkg.ToHttpHeader(tcs1.HttpResp.Header), pkg.ToHttpHeader(tcs2.HttpResp.Header), &respResult.HeadersResult, headerNoise)
	if !ok {
		logger.Debug("test case http resp headers are not equal", zap.Any("tcs1HttpRespHeaders", tcs1.HttpResp.Header), zap.Any("tcs2HttpRespHeaders", tcs2.HttpResp.Header))
		pass = false
	}

	// compare http resp body
	bodyType1 := models.BodyTypePlain
	if json.Valid([]byte(tcs1.HttpResp.Body)) {
		bodyType1 = models.BodyTypeJSON
	}

	bodyType2 := models.BodyTypePlain
	if json.Valid([]byte(tcs2.HttpResp.Body)) {
		bodyType2 = models.BodyTypeJSON
	}

	if bodyType1 != bodyType2 {
		logger.Debug("test case http resp body type is not equal", zap.Any("tcs1HttpRespBodyType", bodyType1), zap.Any("tcs2HttpRespBodyType", bodyType2))
		pass = false
	} else {
		if !Contains(MapToArray(noise), "body") && bodyType1 == models.BodyTypeJSON {
			cleanExp, cleanAct, match, isSame, err :=
				Match(tcs1.HttpResp.Body, tcs2.HttpResp.Body, bodyNoise, logger, true)
			if err != nil || !match {
				logger.Debug("test case http resp body is not equal", zap.Any("tcs1HttpRespBody", tcs1.HttpResp.Body), zap.Any("tcs2HttpRespBody", tcs2.HttpResp.Body))
				pass = false
			} else {
				respResult.BodyResult.Normal = true
			}
			logger.Debug("ExpResp", zap.Any("", cleanExp))
			logger.Debug("ActResp", zap.Any("", cleanAct))
			logger.Debug("SameOrder", zap.Any("", isSame))
		} else {
			if !Contains(MapToArray(noise), "body") && tcs1.HttpResp.Body != tcs2.HttpResp.Body {
				logger.Debug("test case http resp body is not equal", zap.Any("tcs1HttpRespBody", tcs1.HttpResp.Body), zap.Any("tcs2HttpRespBody", tcs2.HttpResp.Body))
				pass = false
			} else {
				respResult.BodyResult.Normal = true
			}
		}
	}
	return pass, respResult
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

func CompareNoise(noise1, noise2 map[string][]string, logger *zap.Logger) bool {
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
	curlHeaderNoise["Accept-Encoding"] = []string{}

	hres := []models.HeaderResult{}
	ok := CompareHeaders(pkg.ToHttpHeader(headers1), pkg.ToHttpHeader(headers2), &hres, curlHeaderNoise)
	if !ok {
		logger.Debug("test case curl headers are not equal", zap.Any("curlHeaderResult", hres))
		return false
	} else {
		return true
	}
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
