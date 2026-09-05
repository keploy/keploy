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

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// buildHTTPSchema assembles the HTTPSchema that will live in a doc's Spec,
// plus performs the noisy-field detection that mutates tc.Noise. This is the
// "meaningful work" shared between the YAML and JSON encode paths; it does
// not touch any yaml.Node or produce any serialized bytes.
func buildHTTPSchema(tc models.TestCase, logger *zap.Logger) models.HTTPSchema {
	m, err := FlattenHTTPResponse(pkg.ToHTTPHeader(tc.HTTPResp.Header), tc.HTTPResp.Body)
	if err != nil {
		utils.LogError(logger, err, "error in flattening http response")
	}
	noise := tc.Noise
	noiseFieldsFound := FindNoisyFields(m, func(_ string, vals []string) bool {
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}
		return pkg.IsTime(strings.Join(vals, ", "))
	})
	for _, v := range noiseFieldsFound {
		noise[v] = []string{}
	}

	httpSchema := models.HTTPSchema{
		Request:  tc.HTTPReq,
		Response: tc.HTTPResp,
		Created:  tc.Created,
		AppPort:  tc.AppPort,
		Assertions: func() map[models.AssertionType]interface{} {
			a := map[models.AssertionType]interface{}{}
			for k, v := range tc.Assertions {
				a[k] = v
			}
			if len(noise) > 0 {
				a[models.NoiseAssertion] = noise
			}
			return a
		}(),
	}
	httpSchema.Metadata = specMetadata(tc.Description)
	return httpSchema
}

// specMetadata builds the spec-level Metadata map carrying the fields that
// persist through the spec rather than the doc: the user-facing description.
// Returns nil when there is nothing to carry, so specs without metadata keep
// encoding byte-identically to before. The gRPC encoders pass description="" —
// gRPC specs have never persisted a description and that stays unchanged.
func specMetadata(description string) map[string]string {
	if description == "" {
		return nil
	}
	return map[string]string{"description": description}
}

// buildGrpcSpec is the JSON-path counterpart of the gRPC branch in EncodeTestcase.
func buildGrpcSpec(tc models.TestCase) models.GrpcSpec {
	noise := tc.Noise
	return models.GrpcSpec{
		Metadata: specMetadata(""),
		GrpcReq:  tc.GrpcReq,
		GrpcResp: tc.GrpcResp,
		Created:  tc.Created,
		AppPort:  tc.AppPort,
		Assertions: func() map[models.AssertionType]interface{} {
			a := map[models.AssertionType]interface{}{}
			if len(noise) > 0 {
				a[models.NoiseAssertion] = noise
			}
			return a
		}(),
	}
}

// buildConsumerSpec assembles the ConsumerSpec that will live in a doc's Spec.
// It is the CONSUMER counterpart of buildHTTPSchema / buildGrpcSpec, shared by
// the YAML and the JSON encode paths so the two cannot drift.
//
// Unlike the HTTP path it performs NO noise detection: a consumer payload's
// fields are the assertion, and auto-noising anything that merely looks like a
// timestamp is exactly the silent-green failure mode this contract exists to
// remove. Consumer noise is explicit, path-scoped and user-authored (it is
// carried on tc.Noise and round-trips through the same NoiseAssertion key the
// other kinds use).
//
// Returns an error rather than an empty spec when tc.ConsumerSpec is nil: a
// Kind: Consumer test case with no spec has no trigger, no effects and no
// completion rule, so writing it would produce a file that can only ever
// replay as a vacuous pass.
func buildConsumerSpec(tc models.TestCase) (models.ConsumerSpec, error) {
	if tc.ConsumerSpec == nil {
		return models.ConsumerSpec{}, errors.New("consumer test case has no ConsumerSpec")
	}
	spec := *tc.ConsumerSpec
	// Protocol routes everything downstream — which Deliverer is asked to
	// deliver the trigger, which projector decodes the payloads, how the
	// judge groups ordered comparisons. A spec with none is unusable
	// everywhere, so repair it from the trigger if we can and refuse if we
	// cannot, rather than writing a file that can only ever fail later with a
	// less specific message.
	if spec.Protocol == "" {
		spec.Protocol = spec.Trigger.Protocol
	}
	if spec.Protocol == "" {
		return models.ConsumerSpec{}, errors.New("consumer test case has no protocol")
	}
	// TestCase.Created and TestCase.AppPort are the source of truth, exactly
	// as they are for HTTPSchema and GrpcSpec. Carrying a second independent
	// copy on the spec is what lets the two drift; this makes the spec's copy
	// a projection instead.
	spec.Created = tc.Created
	spec.AppPort = tc.AppPort
	// Rebuild Assertions from scratch so encoding is a pure function of the
	// test case: re-encoding a decoded test case must not accumulate a
	// second copy of the noise map.
	assertions := map[models.AssertionType]interface{}{}
	for k, v := range tc.Assertions {
		if k == models.NoiseAssertion {
			continue
		}
		assertions[k] = v
	}
	if len(tc.Noise) > 0 {
		assertions[models.NoiseAssertion] = tc.Noise
	}
	if len(assertions) > 0 {
		spec.Assertions = assertions
	} else {
		spec.Assertions = nil
	}
	if tc.Description != "" {
		spec.Metadata = map[string]string{"description": tc.Description}
	}
	return spec, nil
}

// EncodeTestcaseJSON builds a NetworkTrafficDocJSON directly from a TestCase,
// skipping the expensive yaml.Node intermediate that EncodeTestcase goes
// through. Use this on the JSON storage path so the full
//
//	HTTPSchema → yaml bytes → parse → yaml.Node → decode → map[string]any → JSON bytes
//
// chain collapses to:
//
//	HTTPSchema → JSON bytes
//
// which eliminates ~90% of the allocations previously dominated by
// yaml_emitter_emit under load.
func EncodeTestcaseJSON(tc models.TestCase, logger *zap.Logger) (*yaml.NetworkTrafficDocJSON, error) {
	doc := &yaml.NetworkTrafficDocJSON{
		Version:     tc.Version,
		Kind:        tc.Kind,
		Name:        tc.Name,
		LastUpdated: tc.LastUpdated,
	}
	switch tc.Kind {
	case models.HTTP:
		doc.Curl = tc.Curl
		specBytes, err := json.Marshal(buildHTTPSchema(tc, logger))
		if err != nil {
			utils.LogError(logger, err, "failed to marshal HTTPSchema to JSON")
			return nil, err
		}
		doc.Spec = specBytes
	case models.GRPC_EXPORT:
		specBytes, err := json.Marshal(buildGrpcSpec(tc))
		if err != nil {
			utils.LogError(logger, err, "failed to marshal GrpcSpec to JSON")
			return nil, err
		}
		doc.Spec = specBytes
	case models.CONSUMER:
		// NOTE: no Curl. A consumer test case has no HTTP request, and
		// stamping one would ship `curl --request  --url ` on every test.
		spec, err := buildConsumerSpec(tc)
		if err != nil {
			utils.LogError(logger, err, "failed to build ConsumerSpec")
			return nil, err
		}
		specBytes, err := json.Marshal(spec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal ConsumerSpec to JSON")
			return nil, err
		}
		doc.Spec = specBytes
	default:
		utils.LogError(logger, nil, "invalid testcase kind for JSON encoding")
		return nil, errors.New("type of testcases is invalid")
	}
	return doc, nil
}

func EncodeTestcase(tc models.TestCase, logger *zap.Logger) (*yaml.NetworkTrafficDoc, error) {
	logger.Debug("Starting test case encoding",
		zap.String("kind", string(tc.Kind)),
		zap.String("name", tc.Name))

	doc := &yaml.NetworkTrafficDoc{
		Version:     tc.Version,
		Kind:        tc.Kind,
		Name:        tc.Name,
		LastUpdated: tc.LastUpdated,
	}

	switch tc.Kind {
	case models.HTTP:
		logger.Debug("Encoding HTTP test case")
		doc.Curl = tc.Curl

		httpSchema := buildHTTPSchema(tc, logger)
		err := doc.Spec.Encode(httpSchema)
		if err != nil {
			utils.LogError(logger, err, "failed to encode testcase into a yaml doc")
			return nil, err
		}
	case models.GRPC_EXPORT:
		logger.Debug("Encoding gRPC test case")

		grpcSpec := buildGrpcSpec(tc)

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
	case models.CONSUMER:
		logger.Debug("Encoding consumer test case")
		// NOTE: no Curl, and no noisy-field auto-detection — see
		// buildConsumerSpec.
		spec, err := buildConsumerSpec(tc)
		if err != nil {
			utils.LogError(logger, err, "failed to build ConsumerSpec")
			return nil, err
		}
		var node yamlLib.Node
		if err := node.Encode(spec); err != nil {
			utils.LogError(logger, err, "failed to encode consumer spec to YAML node")
			return nil, err
		}
		doc.Spec = node
		logger.Debug("Successfully encoded consumer test case",
			zap.String("protocol", spec.Protocol),
			zap.Int("effects", len(spec.Effects)))
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

func Decode(yamlTestcase *yaml.NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
	tc := &models.TestCase{
		Version:     yamlTestcase.Version,
		Kind:        yamlTestcase.Kind,
		Name:        yamlTestcase.Name,
		Curl:        yamlTestcase.Curl,
		LastUpdated: yamlTestcase.LastUpdated,
		Noise:       make(map[string][]string),
		Assertions:  make(map[models.AssertionType]interface{}),
	}

	switch tc.Kind {
	case models.HTTP:

		var httpSpec models.HTTPSchema
		if err := yamlTestcase.Spec.Decode(&httpSpec); err != nil {
			utils.LogError(logger, err, "failed to decode HTTP JSON spec")
			return nil, err
		}
		tc.Created = httpSpec.Created
		tc.HTTPReq = httpSpec.Request
		tc.HTTPResp = httpSpec.Response
		tc.Description = httpSpec.Metadata["description"]
		tc.AppPort = httpSpec.AppPort

		// single map-based loop for all assertions
		for key, raw := range httpSpec.Assertions {
			tc.Assertions[key] = raw
			if key == models.NoiseAssertion {
				noiseMap, ok := raw.(map[models.AssertionType]interface{})
				if !ok {
					logger.Debug("noise assertion not in expected map[AssertionType]interface{}", zap.Any("raw", raw))
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

	case models.GRPC_EXPORT:
		var grpcSpec models.GrpcSpec
		if err := yamlTestcase.Spec.Decode(&grpcSpec); err != nil {
			utils.LogError(logger, err, "failed to decode gRPC spec")
			return nil, err
		}
		tc.Created = grpcSpec.Created
		tc.GrpcReq = grpcSpec.GrpcReq
		tc.GrpcResp = grpcSpec.GrpcResp
		tc.AppPort = grpcSpec.AppPort

		for key, raw := range grpcSpec.Assertions {
			tc.Assertions[key] = raw
			if key == models.NoiseAssertion {
				noiseMap, ok := raw.(map[models.AssertionType]interface{})
				if !ok {
					logger.Debug("noise assertion not in expected map[AssertionType]interface{}", zap.Any("raw", raw))
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

	case models.CONSUMER:
		var consumerSpec models.ConsumerSpec
		if err := yamlTestcase.Spec.Decode(&consumerSpec); err != nil {
			utils.LogError(logger, err, "failed to decode consumer spec")
			return nil, err
		}
		tc.Created = consumerSpec.Created
		tc.AppPort = consumerSpec.AppPort
		tc.Description = consumerSpec.Metadata["description"]
		// Assertions live on the TestCase, not on the spec copy: keeping a
		// second copy inside tc.ConsumerSpec would let the two diverge the
		// moment anything edits noise, and re-encoding would then emit
		// whichever one it happened to read.
		assertions := consumerSpec.Assertions
		consumerSpec.Assertions = nil
		consumerSpec.Metadata = nil
		tc.ConsumerSpec = &consumerSpec
		expandAssertions(tc, assertions)

	default:
		utils.LogError(logger, nil, "invalid testcase kind", zap.String("kind", string(tc.Kind)))
		return nil, errors.New("invalid testcase kind")
	}

	return tc, nil
}

// DecodeJSON is the JSON-native companion to Decode: it unmarshals a
// NetworkTrafficDocJSON whose Spec is a json.RawMessage directly into a
// TestCase, without going through yaml.Node.Decode.
//
// It reproduces the noise-assertion expansion that Decode does on the YAML
// path so tc.Noise stays populated identically regardless of format.
func DecodeJSON(doc *yaml.NetworkTrafficDocJSON, logger *zap.Logger) (*models.TestCase, error) {
	tc := &models.TestCase{
		Version:     doc.Version,
		Kind:        doc.Kind,
		Name:        doc.Name,
		Curl:        doc.Curl,
		LastUpdated: doc.LastUpdated,
		Noise:       make(map[string][]string),
		Assertions:  make(map[models.AssertionType]interface{}),
	}

	switch tc.Kind {
	case models.HTTP:
		var httpSpec models.HTTPSchema
		if err := json.Unmarshal(doc.Spec, &httpSpec); err != nil {
			utils.LogError(logger, err, "failed to decode HTTP JSON spec")
			return nil, err
		}
		tc.Created = httpSpec.Created
		tc.HTTPReq = httpSpec.Request
		tc.HTTPResp = httpSpec.Response
		tc.Description = httpSpec.Metadata["description"]
		tc.AppPort = httpSpec.AppPort
		expandAssertionsJSON(tc, httpSpec.Assertions)

	case models.GRPC_EXPORT:
		var grpcSpec models.GrpcSpec
		if err := json.Unmarshal(doc.Spec, &grpcSpec); err != nil {
			utils.LogError(logger, err, "failed to decode gRPC JSON spec")
			return nil, err
		}
		tc.Created = grpcSpec.Created
		tc.GrpcReq = grpcSpec.GrpcReq
		tc.GrpcResp = grpcSpec.GrpcResp
		tc.AppPort = grpcSpec.AppPort
		expandAssertionsJSON(tc, grpcSpec.Assertions)

	case models.CONSUMER:
		var consumerSpec models.ConsumerSpec
		if err := json.Unmarshal(doc.Spec, &consumerSpec); err != nil {
			utils.LogError(logger, err, "failed to decode consumer JSON spec")
			return nil, err
		}
		tc.Created = consumerSpec.Created
		tc.AppPort = consumerSpec.AppPort
		tc.Description = consumerSpec.Metadata["description"]
		assertions := consumerSpec.Assertions
		consumerSpec.Assertions = nil
		consumerSpec.Metadata = nil
		tc.ConsumerSpec = &consumerSpec
		expandAssertions(tc, assertions)

	default:
		utils.LogError(logger, nil, "invalid testcase kind", zap.String("kind", string(tc.Kind)))
		return nil, errors.New("invalid testcase kind")
	}

	return tc, nil
}

// expandAssertionsJSON mirrors the nested-noise-map flattening that Decode
// performs on the YAML path, but without relying on yaml.Node decoding.
// The noise assertion on the JSON wire arrives as map[string]interface{}
// (because encoding/json keys are always strings) rather than
// map[models.AssertionType]interface{}; we handle both shapes.
func expandAssertionsJSON(tc *models.TestCase, assertions map[models.AssertionType]interface{}) {
	expandAssertions(tc, assertions)
}

// expandAssertions is the shape-agnostic implementation. The noise assertion
// arrives as map[models.AssertionType]interface{} from yaml.Node.Decode and as
// map[string]interface{} from encoding/json (whose keys are always strings),
// and both shapes are handled here — which is why the YAML consumer decode arm
// calls it too rather than duplicating the nested walk a third time.
func expandAssertions(tc *models.TestCase, assertions map[models.AssertionType]interface{}) {
	for key, raw := range assertions {
		tc.Assertions[key] = raw
		if key != models.NoiseAssertion {
			continue
		}
		// Walk whichever map shape the unmarshaler produced.
		walk := func(field string, inner interface{}) {
			tc.Noise[field] = []string{}
			arr, ok := inner.([]interface{})
			if !ok {
				return
			}
			for _, item := range arr {
				if s, ok2 := item.(string); ok2 && s != "" {
					tc.Noise[field] = append(tc.Noise[field], s)
				}
			}
		}
		switch noiseMap := raw.(type) {
		case map[models.AssertionType]interface{}:
			for kt, inner := range noiseMap {
				walk(string(kt), inner)
			}
		case map[string]interface{}:
			for kt, inner := range noiseMap {
				walk(kt, inner)
			}
		}
	}
}
