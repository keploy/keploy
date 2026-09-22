package grpc

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// AssertionMatch evaluates the assertions declared on a gRPC test case against
// the actual response. If every assertion holds it returns true; unlike the
// ordinary comparison it does not care whether the rest of the response matches
// the recording. It is the gRPC counterpart of http.AssertionMatch.
//
// gRPC-specific mapping of the shared assertion types:
//   - status_code / status_code_in target the grpc-status trailer, which is the
//     canonical gRPC status (see parseGrpcStatus). The HTTP/2 :status
//     pseudo-header is always 200 for gRPC and is deliberately not used.
//   - json_equal / json_contains run against message 0's decoded payload. A
//     streaming response carries further messages via AllMessages(); asserting
//     on the tail is a follow-up, not part of this fix.
//   - header_* assertions look in both headers and trailers, since gRPC metadata
//     is split across the two.
//   - status_code_class is an HTTP notion (2xx/4xx/5xx) with no gRPC equivalent,
//     so it is rejected loudly rather than silently passed.
func AssertionMatch(tc *models.TestCase, actualResp *models.GrpcResp, logger *zap.Logger) (bool, *models.Result) {
	pass := true

	actualStatus := parseGrpcStatus(actualResp.Trailers.OrdinaryHeaders["grpc-status"])

	// message 0's decoded payload; a direction that carried no messages has no
	// body to assert against, so treat it as empty rather than the zero value.
	actualBody := ""
	if !actualResp.NoMessages {
		actualBody = actualResp.Body.DecodedData
	}

	metadata := mergeGrpcMetadata(actualResp)

	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: parseGrpcStatus(tc.GrpcResp.Trailers.OrdinaryHeaders["grpc-status"]),
			Actual:   actualStatus,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Type:     models.GrpcData,
			Expected: tc.GrpcResp.Body.DecodedData,
			Actual:   actualBody,
		}},
	}

	for assertionName, value := range tc.Assertions {
		switch assertionName {

		case models.StatusCode:
			expected, err := toInt(value)
			if err != nil || expected != actualStatus {
				pass = false
				logger.Error("status_code assertion failed", zap.Int("expected", expected), zap.Int("actual", actualStatus), zap.Error(err))
			} else {
				res.StatusCode.Normal = true
			}

		case models.StatusCodeIn:
			codes := toStringSlice(value)
			found := false
			for _, s := range codes {
				if i, err := strconv.Atoi(s); err == nil && i == actualStatus {
					found = true
					break
				}
			}
			if !found {
				pass = false
				logger.Error("status_code_in assertion failed", zap.Strings("expected", codes), zap.Int("actual", actualStatus))
			}

		case models.StatusCodeClass:
			// gRPC status codes (0-16) have no 2xx/4xx/5xx class. Fail rather
			// than silently pass an assertion we cannot evaluate — a silent pass
			// is the very bug this path fixes.
			pass = false
			logger.Warn("status_code_class assertion is not supported for gRPC test cases; use status_code or status_code_in instead", zap.Any("value", value))

		case models.HeaderEqual:
			hm := toStringMap(value)
			for header, exp := range hm {
				act, ok := metadata[header]
				if !ok || act != exp {
					pass = false
					logger.Error("header_equal assertion failed", zap.String("header", header), zap.String("expected", exp), zap.String("actual", act))
				}
			}

		case models.HeaderContains:
			hm := toStringMap(value)
			for header, exp := range hm {
				act, ok := metadata[header]
				if !ok || !strings.Contains(act, exp) {
					pass = false
					logger.Error("header_contains assertion failed", zap.String("header", header), zap.String("expected_substr", exp), zap.String("actual", act))
				}
			}

		case models.HeaderExists:
			for _, hdr := range assertionHeaderNames(value) {
				if _, ok := metadata[hdr]; !ok {
					pass = false
					logger.Error("header_exists assertion failed", zap.String("header", hdr))
				}
			}

		case models.HeaderMatches:
			hm := toStringMap(value)
			for header, pattern := range hm {
				act, ok := metadata[header]
				if !ok {
					pass = false
					logger.Error("header_matches: header not found", zap.String("header", header))
					continue
				}
				if matched, err := regexp.MatchString(pattern, act); err != nil || !matched {
					pass = false
					logger.Error("header_matches assertion failed", zap.String("header", header), zap.String("pattern", pattern), zap.String("actual", act), zap.Error(err))
				}
			}

		case models.JsonEqual:
			if tc.GrpcResp.Body.DecodedData != actualBody {
				pass = false
				logger.Error("json_equal assertion failed", zap.String("expected", tc.GrpcResp.Body.DecodedData), zap.String("actual", actualBody))
			}

		case models.JsonContains:
			var expectedMap map[string]interface{}
			switch v := value.(type) {
			case map[string]interface{}:
				expectedMap = v
			case string:
				_ = json.Unmarshal([]byte(v), &expectedMap)
			default:
				pass = false
				logger.Error("json_contains: unexpected format", zap.Any("value", value))
				continue
			}
			if ok, _ := matcher.JsonContains(actualBody, expectedMap); !ok {
				pass = false
				logger.Error("json_contains assertion failed", zap.Any("expected", expectedMap))
			}

		case models.NoiseAssertion:
			// handled by the noise path, not evaluated as an assertion here.

		default:
			logger.Debug("unhandled assertion type", zap.String("name", string(assertionName)))
		}
	}

	if pass {
		res.StatusCode.Normal = true
		res.BodyResult[0].Normal = true
	}

	return pass, res
}

// mergeGrpcMetadata flattens a response's headers and trailers into a single
// name->value map for header_* assertions. Trailers win on the rare key that
// appears in both.
func mergeGrpcMetadata(resp *models.GrpcResp) map[string]string {
	md := make(map[string]string)
	for k, v := range resp.Headers.PseudoHeaders {
		md[k] = v
	}
	for k, v := range resp.Headers.OrdinaryHeaders {
		md[k] = v
	}
	for k, v := range resp.Trailers.PseudoHeaders {
		md[k] = v
	}
	for k, v := range resp.Trailers.OrdinaryHeaders {
		md[k] = v
	}
	return md
}

// assertionHeaderNames extracts the set of header names carried by a
// header_exists assertion value, which may arrive as a flat list or as a map
// keyed by header name depending on the YAML vs JSON decode path.
func assertionHeaderNames(value interface{}) []string {
	var names []string
	switch v := value.(type) {
	case []interface{}:
		for _, item := range v {
			names = append(names, fmt.Sprint(item))
		}
	case []string:
		names = append(names, v...)
	case map[string]interface{}:
		for hdr := range v {
			names = append(names, hdr)
		}
	case map[models.AssertionType]interface{}:
		for hdr := range v {
			names = append(names, string(hdr))
		}
	}
	return names
}

// The type-coercion helpers below mirror the ones in the http package. They are
// duplicated rather than shared to keep this fix contained to the grpc matcher;
// consolidating them into pkg/matcher is a reasonable follow-up.

func toInt(v interface{}) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case float64:
		return int(x), nil
	case json.Number:
		i64, err := x.Int64()
		return int(i64), err
	case string:
		i64, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, err
		}
		const maxInt = int64(^uint(0) >> 1)
		const minInt = -maxInt - 1
		if i64 > maxInt || i64 < minInt {
			return 0, fmt.Errorf("value out of range for int: %d", i64)
		}
		return int(i64), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

func toString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func toStringSlice(v interface{}) []string {
	var out []string
	switch x := v.(type) {
	case []interface{}:
		for _, e := range x {
			out = append(out, toString(e))
		}
	case string:
		for _, part := range strings.Split(x, ",") {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}

func toStringMap(val interface{}) map[string]string {
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
		// YAML v3 sometimes decodes to this shape.
		for ki, vi := range m {
			out[fmt.Sprint(ki)] = fmt.Sprint(vi)
		}
	}
	return out
}
