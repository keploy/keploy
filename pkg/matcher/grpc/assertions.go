package grpc

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

const supportedGrpcAssertions = "status_code, status_code_in, header_equal, header_contains, header_exists, header_matches, json_equal, json_contains (and noise)"

// AssertionsReplaceComparison reports whether tc carries at least one
// assertion other than noise, in which case AssertionMatch judges the response
// in place of the ordinary recorded-response comparison. Noise (including an
// empty or nil noise entry) judges nothing and stays on the comparison path.
func AssertionsReplaceComparison(tc *models.TestCase) bool {
	if tc == nil {
		return false
	}
	for name := range tc.Assertions {
		if name != models.NoiseAssertion {
			return true
		}
	}
	return false
}

// AssertionMatch evaluates the assertions declared on a gRPC test case against
// the actual response. If every assertion holds it returns true; unlike the
// ordinary comparison it does not care whether the rest of the response matches
// the recording. It is the gRPC counterpart of http.AssertionMatch.
//
// gRPC-specific mapping of the shared assertion types:
//   - status_code / status_code_in target the grpc-status trailer, which is the
//     canonical gRPC status. The HTTP/2 :status pseudo-header is transport
//     framing and is deliberately not used. Under KEPLOY_PROVEN=1 or on an
//     empty/unreached response, an absent grpc-status trailer fails closed.
//   - json_equal / json_contains run against unary message 0's decoded payload.
//     Streaming responses fail closed on json_equal rather than silently
//     ignoring tail messages.
//   - header_* assertions look up metadata case-insensitively across both
//     headers and trailers (with trailers winning on duplicate keys).
//   - status_code_class is an HTTP notion (2xx/4xx/5xx) with no gRPC equivalent,
//     so it fails closed at Error level.
func AssertionMatch(tc *models.TestCase, actualResp *models.GrpcResp, logger *zap.Logger) (bool, *models.Result) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if tc == nil || actualResp == nil {
		logger.Error("gRPC assertion match cannot run with nil test case or response")
		return false, &models.Result{}
	}

	pass := true
	statusPassed := false
	statusFailed := false

	expectedStatus, _ := resolveGrpcAssertionStatus(&tc.GrpcResp)
	actualStatus, actualStatusOK := resolveGrpcAssertionStatus(actualResp)

	expectedBody := ""
	if !tc.GrpcResp.NoMessages {
		expectedBody = tc.GrpcResp.Body.DecodedData
	}
	actualBody := ""
	if !actualResp.NoMessages {
		actualBody = actualResp.Body.DecodedData
	}

	metadata := mergeGrpcMetadata(actualResp)

	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: expectedStatus,
			Actual:   actualStatus,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Type:     models.GrpcData,
			Expected: expectedBody,
			Actual:   actualBody,
		}},
	}

	for assertionName, value := range tc.Assertions {
		switch assertionName {

		case models.StatusCode:
			expected, err := toInt(value)
			if err != nil {
				pass = false
				statusFailed = true
				logUnreadableAssertion(logger, assertionName, value, "an integer gRPC status code")
				continue
			}
			if !actualStatusOK || expected != actualStatus {
				pass = false
				statusFailed = true
				logger.Error("status_code assertion failed",
					zap.Int("expected", expected),
					zap.Int("actual", actualStatus),
					zap.Bool("actual_status_present", actualStatusOK),
				)
			} else {
				statusPassed = true
			}

		case models.StatusCodeIn:
			ints, ok := toIntSlice(value)
			if !ok || len(ints) == 0 {
				pass = false
				statusFailed = true
				logUnreadableAssertion(logger, assertionName, value, "a list of integer gRPC status codes")
				continue
			}
			found := false
			if actualStatusOK {
				for _, c := range ints {
					if c == actualStatus {
						found = true
						break
					}
				}
			}
			if !found {
				pass = false
				statusFailed = true
				logger.Error("status_code_in assertion failed",
					zap.Ints("expected_codes", ints),
					zap.Int("actual", actualStatus),
					zap.Bool("actual_status_present", actualStatusOK),
				)
			} else {
				statusPassed = true
			}

		case models.StatusCodeClass:
			// gRPC status codes (0-16) have no 2xx/4xx/5xx class. Fail closed
			// at Error level rather than silently passing an unsupported rule.
			pass = false
			statusFailed = true
			logger.Error("status_code_class assertion is not supported for gRPC test cases; use status_code or status_code_in instead",
				zap.String("value_type", fmt.Sprintf("%T", value)),
			)

		case models.HeaderEqual:
			hm, ok := toStringMap(value)
			if !ok {
				pass = false
				logUnreadableAssertion(logger, assertionName, value, "a map of header names to values")
				continue
			}
			for header, exp := range hm {
				act, exists := metadata[strings.ToLower(header)]
				if !exists || act != exp {
					pass = false
					logger.Error("header_equal assertion failed",
						zap.String("header", header),
						zap.Int("expected_bytes", len(exp)),
						zap.Bool("present", exists),
						zap.Int("actual_bytes", len(act)),
					)
				}
			}

		case models.HeaderContains:
			hm, ok := toStringMap(value)
			if !ok {
				pass = false
				logUnreadableAssertion(logger, assertionName, value, "a map of header names to substrings")
				continue
			}
			for header, exp := range hm {
				act, exists := metadata[strings.ToLower(header)]
				if !exists || !strings.Contains(act, exp) {
					pass = false
					logger.Error("header_contains assertion failed",
						zap.String("header", header),
						zap.Int("expected_substr_bytes", len(exp)),
						zap.Bool("present", exists),
						zap.Int("actual_bytes", len(act)),
					)
				}
			}

		case models.HeaderExists:
			names, ok := assertionHeaderNames(value)
			if !ok {
				pass = false
				logUnreadableAssertion(logger, assertionName, value, "a list or map of header names")
				continue
			}
			for _, hdr := range names {
				if _, exists := metadata[strings.ToLower(hdr)]; !exists {
					pass = false
					logger.Error("header_exists assertion failed", zap.String("header", hdr))
				}
			}

		case models.HeaderMatches:
			hm, ok := toStringMap(value)
			if !ok {
				pass = false
				logUnreadableAssertion(logger, assertionName, value, "a map of header names to patterns")
				continue
			}
			for header, pattern := range hm {
				act, exists := metadata[strings.ToLower(header)]
				if !exists {
					pass = false
					logger.Error("header_matches: header not found", zap.String("header", header))
					continue
				}
				if matched, err := regexp.MatchString(pattern, act); err != nil || !matched {
					pass = false
					logger.Error("header_matches assertion failed",
						zap.String("header", header),
						zap.Int("pattern_bytes", len(pattern)),
						zap.Int("actual_bytes", len(act)),
						zap.Bool("pattern_compiles", err == nil),
					)
				}
			}

		case models.JsonEqual:
			if tc.GrpcResp.IsStream() || actualResp.IsStream() {
				pass = false
				logger.Error("json_equal assertion is not supported for streaming gRPC responses")
				continue
			}
			if tc.GrpcResp.NoMessages != actualResp.NoMessages {
				pass = false
				logger.Error("json_equal assertion failed: message presence mismatch",
					zap.Bool("expected_no_messages", tc.GrpcResp.NoMessages),
					zap.Bool("actual_no_messages", actualResp.NoMessages),
				)
				continue
			}
			if !grpcBodiesEqual(expectedBody, actualBody) {
				pass = false
				logger.Error("json_equal assertion failed",
					zap.Int("expected_bytes", len(expectedBody)),
					zap.Int("actual_bytes", len(actualBody)),
				)
			}

		case models.JsonContains:
			expectedMap, ok := toJSONObject(value)
			if !ok {
				pass = false
				logUnreadableAssertion(logger, assertionName, value, "a JSON object, as a map or a string")
				continue
			}
			if actualResp.NoMessages || !json.Valid([]byte(actualBody)) {
				pass = false
				logger.Error("json_contains assertion failed: actual payload is not valid JSON",
					zap.Bool("no_messages", actualResp.NoMessages),
					zap.Int("actual_bytes", len(actualBody)),
				)
				continue
			}
			if ok, _ := matcher.JsonContains(actualBody, expectedMap); !ok {
				pass = false
				logger.Error("json_contains assertion failed",
					zap.Strings("expected_keys", models.FieldShapes(slices.Sorted(maps.Keys(expectedMap)))),
				)
			}

		case models.NoiseAssertion:
			// Handled by the comparison path, not evaluated as an assertion here.

		default:
			pass = false
			logger.Error("this keploy does not support the gRPC test case's assertion "+strconv.Quote(string(assertionName))+", so it fails the test case",
				zap.String("assertion", string(assertionName)),
				zap.String("supported", supportedGrpcAssertions),
			)
		}
	}

	if pass {
		res.StatusCode.Normal = true
		res.BodyResult[0].Normal = true
	} else if statusPassed && !statusFailed {
		res.StatusCode.Normal = true
	}

	return pass, res
}

// resolveGrpcAssertionStatus reads grpc-status as a gRPC client reads it
// (including Trailers-Only responses via TrailersAsRead). It reports the
// status code and whether the response carried a valid status that can satisfy
// status_code / status_code_in assertions.
//
// Absent status fails closed (ok=false, code=-1) under KEPLOY_PROVEN=1 and on
// an empty/unreached synthetic GrpcResp (no headers or trailers at all).
// Non-numeric grpc-status always fails closed (ok=false, code=-1) so it cannot
// match a synthetic -1 assertion.
func resolveGrpcAssertionStatus(resp *models.GrpcResp) (int, bool) {
	if resp == nil {
		return -1, false
	}
	statusText := resp.TrailersAsRead().OrdinaryHeaders["grpc-status"]
	if statusText == "" {
		if provenswitch.On() {
			return provenGrpcStatusCode(statusText), false
		}
		if len(resp.Headers.PseudoHeaders) == 0 &&
			len(resp.Headers.OrdinaryHeaders) == 0 &&
			len(resp.Trailers.PseudoHeaders) == 0 &&
			len(resp.Trailers.OrdinaryHeaders) == 0 {
			return -1, false
		}
		return 0, true
	}
	n, err := strconv.Atoi(statusText)
	if err != nil {
		return -1, false
	}
	return n, true
}

func grpcBodiesEqual(expected, actual string) bool {
	if expected == actual {
		return true
	}
	if !json.Valid([]byte(expected)) || !json.Valid([]byte(actual)) {
		return false
	}
	expVal, expOK := decodeJSONValue(expected)
	actVal, actOK := decodeJSONValue(actual)
	if !expOK || !actOK {
		return false
	}
	return matcher.JSONValueEqual(actVal, expVal)
}

func decodeJSONValue(raw string) (interface{}, bool) {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var v interface{}
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

func logUnreadableAssertion(logger *zap.Logger, name models.AssertionType, value interface{}, want string) {
	logger.Error(string(name)+" assertion cannot be read, so it fails the test case: its value is not "+want,
		zap.String("value_type", fmt.Sprintf("%T", value)),
	)
}

// mergeGrpcMetadata flattens a response's headers and trailers into a single
// lowercased name->value map for case-insensitive header_* assertions.
// Trailers win on keys that appear in both headers and trailers.
func mergeGrpcMetadata(resp *models.GrpcResp) map[string]string {
	md := make(map[string]string)
	if resp == nil {
		return md
	}
	copyFolded := func(src map[string]string) {
		for _, k := range slices.Sorted(maps.Keys(src)) {
			md[strings.ToLower(k)] = src[k]
		}
	}
	copyFolded(resp.Headers.PseudoHeaders)
	copyFolded(resp.Headers.OrdinaryHeaders)
	copyFolded(resp.Trailers.PseudoHeaders)
	copyFolded(resp.Trailers.OrdinaryHeaders)
	return md
}

// assertionHeaderNames extracts the header names carried by a header_exists
// assertion value (list or map). Returns ok=false on unsupported shapes.
func assertionHeaderNames(value interface{}) ([]string, bool) {
	var names []string
	switch v := value.(type) {
	case []interface{}:
		for _, item := range v {
			names = append(names, fmt.Sprint(item))
		}
		return names, true
	case []string:
		names = append(names, v...)
		return names, true
	case map[string]interface{}:
		for hdr := range v {
			names = append(names, hdr)
		}
		return names, true
	case map[models.AssertionType]interface{}:
		for hdr := range v {
			names = append(names, string(hdr))
		}
		return names, true
	case map[interface{}]interface{}:
		for hdr := range v {
			names = append(names, fmt.Sprint(hdr))
		}
		return names, true
	default:
		return nil, false
	}
}

func toInt(v interface{}) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		if x > math.MaxInt || x < math.MinInt {
			return 0, fmt.Errorf("value out of range for int: %d", x)
		}
		return int(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x {
			return 0, fmt.Errorf("non-integer float value: %v", x)
		}
		if x < float64(math.MinInt) || x >= -float64(math.MinInt) {
			return 0, fmt.Errorf("value out of range for int: %v", x)
		}
		return int(x), nil
	case json.Number:
		return strconv.Atoi(strings.TrimSpace(string(x)))
	case string:
		return strconv.Atoi(strings.TrimSpace(x))
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

func toIntSlice(v interface{}) ([]int, bool) {
	switch x := v.(type) {
	case []interface{}:
		var out []int
		for _, e := range x {
			i, err := toInt(e)
			if err != nil {
				return nil, false
			}
			out = append(out, i)
		}
		return out, len(out) > 0
	case []int:
		if len(x) == 0 {
			return nil, false
		}
		return append([]int(nil), x...), true
	case []string:
		var out []int
		for _, s := range x {
			i, err := toInt(s)
			if err != nil {
				return nil, false
			}
			out = append(out, i)
		}
		return out, len(out) > 0
	case string:
		parts := strings.Split(x, ",")
		var out []int
		for _, part := range parts {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			i, err := toInt(p)
			if err != nil {
				return nil, false
			}
			out = append(out, i)
		}
		return out, len(out) > 0
	case int, int64, float64, json.Number:
		i, err := toInt(x)
		if err != nil {
			return nil, false
		}
		return []int{i}, true
	default:
		return nil, false
	}
}

func toStringMap(val interface{}) (map[string]string, bool) {
	out := make(map[string]string)
	switch m := val.(type) {
	case map[string]interface{}:
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
	case map[string]string:
		for k, v := range m {
			out[k] = v
		}
	case map[models.AssertionType]interface{}:
		for kType, v := range m {
			out[string(kType)] = fmt.Sprint(v)
		}
	case map[models.AssertionType]string:
		for kType, v := range m {
			out[string(kType)] = v
		}
	case map[interface{}]interface{}:
		for ki, vi := range m {
			out[fmt.Sprint(ki)] = fmt.Sprint(vi)
		}
	default:
		return nil, false
	}
	return out, true
}

// toJSONObject reads json_contains' value as the JSON object it names: a JSON
// string or a map of any shape YAML/JSON decoding produces. Numbers are decoded
// as json.Number so integers above 2^53 keep exact precision.
func toJSONObject(val interface{}) (map[string]interface{}, bool) {
	raw, isString := val.(string)
	if !isString {
		keyed := stringKeyed(val)
		if _, isMap := keyed.(map[string]interface{}); !isMap {
			return nil, false
		}
		b, err := json.Marshal(keyed)
		if err != nil {
			return nil, false
		}
		raw = string(b)
	}
	decoded, ok := decodeJSONValue(raw)
	if !ok {
		return nil, false
	}
	out, ok := decoded.(map[string]interface{})
	if !ok || out == nil {
		return nil, false
	}
	return out, true
}

func stringKeyed(val interface{}) interface{} {
	switch m := val.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, v := range m {
			out[k] = stringKeyed(v)
		}
		return out
	case map[models.AssertionType]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, v := range m {
			out[string(k)] = stringKeyed(v)
		}
		return out
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, v := range m {
			out[fmt.Sprint(k)] = stringKeyed(v)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(m))
		for i, v := range m {
			out[i] = stringKeyed(v)
		}
		return out
	default:
		return val
	}
}
