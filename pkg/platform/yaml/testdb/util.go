package testdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func EncodeTestcase(tc models.TestCase, logger *zap.Logger) (*yaml.NetworkTrafficDoc, error) {
	respType := models.HTTPResponseJSON
	var idempotentoperations = []string{"GET", "PUT", "DELETE"}
	isXML := utils.IsXMLResponse(&tc.HTTPResp)
	if isXML {
		respType = models.HTTPResponseXML
	}
	curl := pkg.MakeCurlCommand(tc.HTTPReq)
	doc := &yaml.NetworkTrafficDoc{
		Version:  tc.Version,
		Kind:     tc.Kind,
		Name:     tc.Name,
		Curl:     curl,
		RespType: respType,
	}
	// find noisy fields
	m, err := FlattenHTTPResponse(pkg.ToHTTPHeader(tc.HTTPResp.Header), tc.HTTPResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		utils.LogError(logger, err, msg)
	}
	noise := tc.Noise

	if tc.Name == "" {
		noiseFieldsFound := FindNoisyFields(m, func(_ string, vals []string) bool {
			// check if k is date
			for _, v := range vals {
				if pkg.IsTime(v) {
					return true
				}
			}

			// maybe we need to concatenate the values
			return pkg.IsTime(strings.Join(vals, ", "))
		})

		for _, v := range noiseFieldsFound {
			noise[v] = []string{}
		}
	}
	if slices.Contains(idempotentoperations, string(tc.HTTPReq.Method)) {
		var responses []map[string][]string
		client := &http.Client{}
		req, err := http.NewRequest(string(tc.HTTPReq.Method), string(tc.HTTPReq.URL), strings.NewReader(tc.HTTPReq.Body))
		if err != nil {
			utils.LogError(logger, err, "failed to create new http request")
			return nil, err
		}
		//inheriting the headers to new request
		for k, v := range tc.HTTPReq.Header {
			req.Header.Set(k, v)
		}
		// Add special header to prevent recording these replay requests
		req.Header.Set("Idempotency-Check", "true")
		replays := 8
		for i := 0; i < replays; i++ {
			resp, err := client.Do(req)
			if err != nil {
				utils.LogError(logger, err, "failed to execute http req")
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				utils.LogError(logger, err, "failed to read response body")
				continue
			}
			resMap, err := FlattenHTTPResponse(resp.Header, string(body))
			if err != nil {
				utils.LogError(logger, err, "failed to flatten http response")
				continue
			}
			responses = append(responses, resMap)
		}
		dynamicfield := DynamicFields(responses, m)
		missingfield := Missingfields(responses, m)
		fmt.Println("Missing fields are : \n")
		for k, v := range missingfield {
			if v == true {
				fmt.Println(k)
			}
		}
		for field := range dynamicfield {
			noise[field] = []string{}
		}
		if !Verifyresponses(responses, m, dynamicfield, logger) {
			utils.LogError(logger, nil, "request failed idempotency check")
		}

	}
	switch tc.Kind {
	case models.HTTP:
		switch respType {
		case models.HTTPResponseXML:
			m, err := utils.XMLToMap(tc.HTTPResp.Body)
			if err != nil {
				utils.LogError(logger, err, "failed to convert xml to map")
				return nil, err
			}
			err = doc.Spec.Encode(models.XMLSchema{
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
				Assertions: map[string]interface{}{
					"noise": noise,
				},
			})
			if err != nil {
				utils.LogError(logger, err, "failed to encode testcase into a yaml doc")
				return nil, err
			}
		case models.HTTPResponseJSON:
			err := doc.Spec.Encode(models.HTTPSchema{
				Request:  tc.HTTPReq,
				Response: tc.HTTPResp,
				Created:  tc.Created,
				Assertions: map[string]interface{}{
					"noise": noise,
				},
			})
			if err != nil {
				utils.LogError(logger, err, "failed to encode testcase into a yaml doc")
				return nil, err
			}
		}

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
func Missingfields(responses []map[string][]string, m map[string][]string) map[string]bool {
	missingfields := make(map[string]bool)
	if len(responses) <= 1 {
		return missingfields
	}
	fieldcount := []string{}
	fieldcountresp := make(map[string]int)
	for field := range m {
		fieldcount = append(fieldcount, field)

		//count the no of times each field occurs in the replays to find any missing fields

		for _, resp := range responses {
			for k := range resp {
				fieldcountresp[k]++
			}
		}
	}
	for field := range m {
		if fieldcountresp[field] == 0 {
			missingfields[field] = true
		} else {
			missingfields[field] = false
		}
	}
	return missingfields
}
func DynamicFields(responses []map[string][]string, m map[string][]string) map[string]bool {
	dynamicfields := make(map[string]bool)
	if len(responses) <= 1 {
		return dynamicfields
	}
	for field := range m {
		uniquevals := make(map[string]bool)
		validfield := true
		// check if field exists for each response
		for _, resp := range responses {
			val, exists := resp[field]
			if !exists {
				validfield = false
				break
			}
			b, err := json.Marshal(val)
			if err != nil {
				validfield = false
				break
			}
			uniquevals[string(b)] = true
		}
		if validfield && len(uniquevals) == len(responses) {
			dynamicfields[field] = true
		}
	}
	return dynamicfields
}

func Decode(yamlTestcase *yaml.NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
	tc := models.TestCase{
		Version: yamlTestcase.Version,
		Kind:    yamlTestcase.Kind,
		Name:    yamlTestcase.Name,
		Curl:    yamlTestcase.Curl,
	}

	switch tc.Kind {
	case models.HTTP:

		// added this condition for backward compatibility
		if yamlTestcase.RespType == "" {
			yamlTestcase.RespType = models.HTTPResponseJSON
		}

		switch yamlTestcase.RespType {
		case models.HTTPResponseJSON:

			httpSpec := models.HTTPSchema{}
			err := yamlTestcase.Spec.Decode(&httpSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into the http testcase")
				return nil, err
			}
			tc.Created = httpSpec.Created
			tc.HTTPReq = httpSpec.Request
			tc.HTTPResp = httpSpec.Response
			tc.Noise = map[string][]string{}
			switch reflect.ValueOf(httpSpec.Assertions["noise"]).Kind() {
			case reflect.Map:
				for k, v := range httpSpec.Assertions["noise"].(map[string]interface{}) {
					tc.Noise[k] = []string{}
					for _, val := range v.([]interface{}) {
						tc.Noise[k] = append(tc.Noise[k], val.(string))
					}
				}
			case reflect.Slice:
				for _, v := range httpSpec.Assertions["noise"].([]interface{}) {
					tc.Noise[v.(string)] = []string{}
				}
			}
		case models.HTTPResponseXML:
			xmlSpec := models.XMLSchema{}
			err := yamlTestcase.Spec.Decode(&xmlSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into the xml testcase")
				return nil, err
			}
			tc.Created = xmlSpec.Created
			tc.HTTPReq = xmlSpec.Request
			tc.XMLResp = xmlSpec.Response
			tc.Noise = map[string][]string{}
			switch reflect.ValueOf(xmlSpec.Assertions["noise"]).Kind() {
			case reflect.Map:
				for k, v := range xmlSpec.Assertions["noise"].(map[string]interface{}) {
					tc.Noise[k] = []string{}
					for _, val := range v.([]interface{}) {
						tc.Noise[k] = append(tc.Noise[k], val.(string))
					}
				}
			case reflect.Slice:
				for _, v := range xmlSpec.Assertions["noise"].([]interface{}) {
					tc.Noise[v.(string)] = []string{}
				}
			}
		}
	// unmarshal its mocks from yaml docs to go struct
	case models.GRPC_EXPORT:
		grpcSpec := models.GrpcSpec{}
		err := yamlTestcase.Spec.Decode(&grpcSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to unmarshal a yaml doc into the gRPC testcase")
			return nil, err
		}
		tc.GrpcReq = grpcSpec.GrpcReq
		tc.GrpcResp = grpcSpec.GrpcResp
	default:
		utils.LogError(logger, nil, "failed to unmarshal yaml doc of unknown type", zap.Any("type of yaml doc", tc.Kind))
		return nil, errors.New("yaml doc of unknown type")
	}
	return &tc, nil
}
func Verifyresponses(responses []map[string][]string, m map[string][]string, dynamicfield map[string]bool, logger *zap.Logger) bool {
	if len(responses) <= 1 {
		return true
	}
	consistent := true
	// Extract only non-dynamic fields from the baseline response
	nonDynamicFields := make(map[string][]string)
	for field, value := range m {
		if !dynamicfield[field] && !strings.HasPrefix(field, "header.") {
			nonDynamicFields[field] = value
		}
	}
	// Validate consistency of non-dynamic fields
	for _, resp := range responses {
		for field, expectedValue := range nonDynamicFields {
			val, exists := resp[field]

			// Ignore missing fields
			if !exists {
				continue
			}

			if !reflect.DeepEqual(expectedValue, val) {
				utils.LogError(logger, nil, "Inconsistent response body detected",
					zap.String("field", field),
					zap.Any("expected", expectedValue),
					zap.Any("got", val))
				consistent = false
			}
		}
	}

	return consistent
}
