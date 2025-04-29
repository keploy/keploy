package testdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func EncodeTestcase(tc models.TestCase, logger *zap.Logger) (*yaml.NetworkTrafficDoc, error) {
	logger.Debug("Starting test case encoding",
		zap.String("kind", string(tc.Kind)),
		zap.String("name", tc.Name))

	doc := &yaml.NetworkTrafficDoc{
		Version: tc.Version,
		Kind:    tc.Kind,
		Name:    tc.Name,
	}

	var noise map[string][]string
	switch tc.Kind {
	case models.HTTP:
		respType := models.HTTPResponseJSON
		isXML := utils.IsXMLResponse(&tc.HTTPResp)
		if isXML {
			respType = models.HTTPResponseXML
		}
		doc.RespType = respType

		logger.Debug("Encoding HTTP test case")
		doc.Curl = pkg.MakeCurlCommand(tc.HTTPReq)

		// find noisy fields only for HTTP responses
		m, err := FlattenHTTPResponse(pkg.ToHTTPHeader(tc.HTTPResp.Header), tc.HTTPResp.Body)
		respHeaders, headerErr := flattenHTTPResponseHeaders(pkg.ToHTTPHeader(tc.HTTPResp.Header))
		if err != nil || headerErr != nil {
			msg := "error in flattening http response"
			utils.LogError(logger, err, msg)
		}
		noise := tc.Noise

		if tc.Name == "" {
			// noise detection for Time, UUID, JWT
			noiseFieldsFound := FindNoisyFields(m, func(k string, vals []string) bool {
				// check for time, UUID, or JWT in values
				for _, v := range vals {
					if pkg.IsTime(v) || pkg.IsUUID(v) || pkg.IsJWT(v) {
						return true
					}
				}
				// maybe we need to concatenate the values
				return pkg.IsTime(strings.Join(vals, ", "))
			})

			// Add dynamic header detection
			dynamicHeaders := map[string]bool{
				"header.etag":         true,
				"header.x-request-id": true,
				"header.x-csrf-token": true,
			}

			noiseFieldsFound = append(noiseFieldsFound, FindNoisyFields(respHeaders, func(k string, vals []string) bool {
				lowerK := strings.ToLower(k)
				if _, found := dynamicHeaders[lowerK]; found {
					return true
				}
				// handle if the set-cookie has expires field
				if lowerK == "header.set-cookie" {
					for _, cookie := range vals {
						lowerCookie := strings.ToLower(cookie)
						if strings.Contains(lowerCookie, "expires") {
							return true
						}
					}
				}
				return false
			})...)

			for _, v := range noiseFieldsFound {
				noise[v] = []string{}
			}
		}

		switch respType {
		case models.HTTPResponseXML:
			m, err := utils.XMLToMap(tc.HTTPResp.Body)
			if err != nil {
				utils.LogError(logger, err, "failed to convert xml to map")
				return nil, err
			}
			xmlSchema := models.XMLSchema{
				Request: tc.HTTPReq,
				Response: models.XMLResp{
					Body:          m,
					StatusCode:    tc.HTTPResp.StatusCode,
					Header:        tc.HTTPResp.Header,
					StatusMessage: tc.HTTPResp.StatusMessage,
					ProtoMajor:    tc.HTTPResp.ProtoMajor,
					ProtoMinor:    tc.HTTPResp.ProtoMinor,
					Binary:        tc.HTTPResp.Binary,
					Timestamp:     tc.HTTPResp.Timestamp,
				},
				Created: tc.Created,
				// need to check here for type here as well as push in other custom assertions
				Assertions: func() map[models.AssertionType]interface{} {
					a := map[models.AssertionType]interface{}{}
					if len(noise) > 0 {
						a[models.NoiseAssertion] = noise
					}
					return a
				}(),
			}
			if tc.Description != "" {
				xmlSchema.Metadata = map[string]string{
					"description": tc.Description,
				}
			}
			err = doc.Spec.Encode(xmlSchema)
			if err != nil {
				utils.LogError(logger, err, "failed to encode testcase into a yaml doc")
				return nil, err
			}
		case models.HTTPResponseJSON:
			httpSchema := models.HTTPSchema{
				Request:  tc.HTTPReq,
				Response: tc.HTTPResp,
				Created:  tc.Created,
				// need to check here for type here as well as push in other custom assertions
				Assertions: func() map[models.AssertionType]interface{} {
					a := map[models.AssertionType]interface{}{}
					if len(noise) > 0 {
						a[models.NoiseAssertion] = noise
					}
					for k, v := range tc.Assertions {
						a[k] = v
					}

					// Optionally add other custom assertions if needed here
					// Example:
					// a[models.StatusCode] = tc.HTTPResp.StatusCode

					return a
				}(),
			}
			if tc.Description != "" {
				httpSchema.Metadata = map[string]string{
					"description": tc.Description,
				}
			}
			err := doc.Spec.Encode(httpSchema)
			if err != nil {
				utils.LogError(logger, err, "failed to encode testcase into a yaml doc")
				return nil, err
			}
		}
	case models.GRPC_EXPORT:
		logger.Debug("Encoding gRPC test case")
		// For gRPC, use the noise directly from the test case
		noise = tc.Noise

		// Create a YAML node for the gRPC schema
		grpcSpec := models.GrpcSpec{
			GrpcReq:  tc.GrpcReq,
			GrpcResp: tc.GrpcResp,
			Created:  tc.Created,
			// need to check here for type here as well as push in other custom assertions
			Assertions: func() map[models.AssertionType]interface{} {
				a := map[models.AssertionType]interface{}{}
				if len(noise) > 0 {
					a[models.NoiseAssertion] = noise
				}
				// Optionally add other custom assertions if needed here
				// Example:
				// a[models.StatusCode] = tc.HTTPResp.StatusCode

				return a
			}(),
		}

		logger.Debug("gRPC schema created",
			zap.Any("request_headers", grpcSpec.GrpcReq.Headers),
			zap.Any("response_headers", grpcSpec.GrpcResp.Headers),
			zap.Int("request_body_length", len(grpcSpec.GrpcReq.Body.DecodedData)),
			zap.Int("response_body_length", len(grpcSpec.GrpcResp.Body.DecodedData)))

		// Create a new YAML node and encode the gRPC schema
		var node yamlLib.Node
		err := node.Encode(grpcSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to encode gRPC schema to YAML node")
			return nil, err
		}

		// Set the node as the spec
		doc.Spec = node
		logger.Debug("Successfully encoded gRPC test case")
	default:
		utils.LogError(logger, nil, "failed to marshal the testcase into yaml due to invalid kind of testcase")
		return nil, errors.New("type of testcases is invalid")
	}
	return doc, nil
}

func FindNoisyFields(m map[string][]string, comparator func(string, []string) bool) []string {
	var noise []string
	for k, v := range m {
		if comparator(k, v) {
			noise = append(noise, k)
		}
	}
	return noise
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

func flattenHTTPResponseHeaders(h http.Header) (map[string][]string, error) {
	m := map[string][]string{}
	for k, v := range h {
		m["header."+k] = []string{strings.Join(v, "")}
	}
	return m, nil
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
	default:
		fmt.Println(utils.Emoji, "found invalid value in json", j, x.Kind())
	}
	return o
}

func ContainsMatchingURL(urlMethods []string, urlStr string, requestURL string, requestMethod models.Method) (bool, error) {
	urlMatched := false
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return false, err
	}

	// Check for URL path and method
	regex, err := regexp.Compile(urlStr)
	if err != nil {
		return false, err
	}

	urlMatch := regex.MatchString(parsedURL.Path)

	if urlMatch && len(urlStr) != 0 {
		urlMatched = true
	}

	if len(urlMethods) != 0 && urlMatched {
		urlMatched = false
		for _, method := range urlMethods {
			if string(method) == string(requestMethod) {
				urlMatched = true
			}
		}
	}

	return urlMatched, nil
}

func HasBannedHeaders(object map[string]string, bannedHeaders map[string]string) (bool, error) {
	for headerName, headerNameValue := range object {
		for bannedHeaderName, bannedHeaderValue := range bannedHeaders {
			regex, err := regexp.Compile(headerName)
			if err != nil {
				return false, err
			}

			headerNameMatch := regex.MatchString(bannedHeaderName)
			regex, err = regexp.Compile(bannedHeaderValue)
			if err != nil {
				return false, err
			}
			headerValueMatch := regex.MatchString(headerNameValue)
			if headerNameMatch && headerValueMatch {
				return true, nil
			}
		}
	}
	return false, nil
}

func Decode(yamlTestcase *yaml.NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
	tc := &models.TestCase{
		Version:    yamlTestcase.Version,
		Kind:       yamlTestcase.Kind,
		Name:       yamlTestcase.Name,
		Curl:       yamlTestcase.Curl,
		Noise:      make(map[string][]string),
		Assertions: make(map[models.AssertionType]interface{}),
	}

	switch tc.Kind {
	case models.HTTP:
		if yamlTestcase.RespType == "" {
			yamlTestcase.RespType = models.HTTPResponseJSON
		}
		switch yamlTestcase.RespType {

		case models.HTTPResponseJSON:
			var httpSpec models.HTTPSchema
			if err := yamlTestcase.Spec.Decode(&httpSpec); err != nil {
				utils.LogError(logger, err, "failed to decode HTTP JSON spec")
				return nil, err
			}
			tc.Created = httpSpec.Created
			tc.HTTPReq = httpSpec.Request
			tc.HTTPResp = httpSpec.Response
			tc.Description = httpSpec.Metadata["description"]

			// single map-based loop for all assertions
			for key, raw := range httpSpec.Assertions {
				tc.Assertions[key] = raw
				if key == models.NoiseAssertion {
					noiseMap, ok := raw.(map[models.AssertionType]interface{})
					if !ok {
						logger.Warn("noise assertion not in expected map[AssertionType]interface{}", zap.Any("raw", raw))
						continue
					}
					for kt, inner := range noiseMap {
						field := string(kt)
						// initialize slice
						tc.Noise[field] = []string{}
						arr, ok := inner.([]interface{})
						if !ok {
							continue
						}
						for _, item := range arr {
							if s, ok2 := item.(string); ok2 && s != "" {
								tc.Noise[field] = append(tc.Noise[field], s)
							}
						}
					}
				}
			}

		case models.HTTPResponseXML:
			var xmlSpec models.XMLSchema
			if err := yamlTestcase.Spec.Decode(&xmlSpec); err != nil {
				utils.LogError(logger, err, "failed to decode HTTP XML spec")
				return nil, err
			}
			tc.Created = xmlSpec.Created
			tc.HTTPReq = xmlSpec.Request
			tc.XMLResp = xmlSpec.Response
			tc.Description = xmlSpec.Metadata["description"]

			for key, raw := range xmlSpec.Assertions {
				tc.Assertions[key] = raw
				if key == models.NoiseAssertion {
					noiseMap, ok := raw.(map[models.AssertionType]interface{})
					if !ok {
						logger.Warn("noise assertion not in expected map[AssertionType]interface{}", zap.Any("raw", raw))
						continue
					}
					for kt, inner := range noiseMap {
						field := string(kt)
						tc.Noise[field] = []string{}
						arr, ok := inner.([]interface{})
						if !ok {
							continue
						}
						for _, item := range arr {
							if s, ok2 := item.(string); ok2 && s != "" {
								tc.Noise[field] = append(tc.Noise[field], s)
							}
						}
					}
				}
			}

		}

	case models.GRPC_EXPORT:
		var grpcSpec models.GrpcSpec
		if err := yamlTestcase.Spec.Decode(&grpcSpec); err != nil {
			utils.LogError(logger, err, "failed to decode gRPC spec")
			return nil, err
		}
		tc.Created = grpcSpec.Created
		tc.GrpcReq = grpcSpec.GrpcReq
		tc.GrpcResp = grpcSpec.GrpcResp

		for key, raw := range grpcSpec.Assertions {
			tc.Assertions[key] = raw
			if key == models.NoiseAssertion {
				noiseMap, ok := raw.(map[models.AssertionType]interface{})
				if !ok {
					logger.Warn("noise assertion not in expected map[AssertionType]interface{}", zap.Any("raw", raw))
					continue
				}
				for kt, inner := range noiseMap {
					field := string(kt)
					tc.Noise[field] = []string{}
					arr, ok := inner.([]interface{})
					if !ok {
						continue
					}
					for _, item := range arr {
						if s, ok2 := item.(string); ok2 && s != "" {
							tc.Noise[field] = append(tc.Noise[field], s)
						}
					}
				}
			}
		}

	default:
		utils.LogError(logger, nil, "invalid testcase kind", zap.String("kind", string(tc.Kind)))
		return nil, errors.New("invalid testcase kind")
	}

	return tc, nil
}
