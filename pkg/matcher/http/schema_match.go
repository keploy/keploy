package http

import (
	"encoding/json"
	"fmt"
	"reflect"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// MatchSchema checks if the actual response matches the expected response schema.
func MatchSchema(tc *models.TestCase, actualResponse *models.HTTPResp, logger *zap.Logger) (bool, *models.Result) {
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

	// 1. Status Code Match
	if tc.HTTPResp.StatusCode == actualResponse.StatusCode {
		res.StatusCode.Normal = true
	} else {
		pass = false
	}

	// 2. Body Schema Match
	// Try to unmarshal both as JSON
	var expObj, actObj interface{}
	errExp := json.Unmarshal([]byte(tc.HTTPResp.Body), &expObj)
	errAct := json.Unmarshal([]byte(actualResponse.Body), &actObj)

	if errExp == nil && errAct == nil {
		res.BodyResult[0].Type = models.JSON
		// Both are JSON, perform schema match
		match, msg := schemaMatchRecursive(expObj, actObj, "body", logger)
		if !match {
			pass = false
			logger.Error("Schema match FAIL", zap.String("reason", msg))
		} else {
			logger.Info("Schema match PASS", zap.String("note", "Extra fields in actual are ignored (Superset)"))
		}
		// Populate body result with schema match outcome.
		res.BodyResult[0].Normal = match
	} else {
		// Non-JSON body handling.
		// If one is JSON and other is not, that's a type mismatch -> Fail.
		if (errExp == nil) != (errAct == nil) {
			pass = false
			res.BodyResult[0].Normal = false
		} else {
			// Both non-JSON: fallback to strict equality.
			pass = tc.HTTPResp.Body == actualResponse.Body
			res.BodyResult[0].Normal = pass
		}
	}

	// Check headers existence (schema match checks key presence, not values).
	for k := range tc.HTTPResp.Header {
		if _, ok := actualResponse.Header[k]; !ok {
			// Missing header
			pass = false
		}
	}

	return pass, res
}

func schemaMatchRecursive(expected, actual interface{}, path string, logger *zap.Logger) (bool, string) {
	// 1. Handle nil cases
	if expected == nil {
		// If expected is nil, we generally accept anything in actual,
		// unless strictly we want actual to be nil too.
		// For looser schema matching, nil expected usually means "any structure" or "optional".
		// But here, if expected has a specific structure (e.g. key: nil),
		// we probably just check path existence which is done by caller for maps.
		// If the value itself is nil, it matches anything or nothing?
		// Let's assume strict type matching: nil matches nil.
		// But in JSON, null is a value.
		if actual == nil {
			return true, ""
		}
		// If expected is nil but actual is not, it's a mismatch if we treat nil as a specific "null" type.
		// However, in Go unmarshaling, nil interface{} could be anything.
		// Let's print a warning and allow it for now, or fail?
		// Failure message:
		return false, fmt.Sprintf("mismatch at %s: expected nil, got %T", path, actual)
	}

	if actual == nil {
		return false, fmt.Sprintf("mismatch at %s: expected %T, got nil", path, expected)
	}

	expType := reflect.TypeOf(expected)
	actType := reflect.TypeOf(actual)

	// 2. Type Check
	// Note: JSON numbers are float64 by default in Go unmarshal.
	// If types are different, check if they are compatible (e.g. both numeric? float64 vs int?)
	if expType != actType {
		// Handle numeric cases if necessary, though json.Unmarshal usually gives float64 for all numbers
		// unless UseNumber is used. Standard Keploy seems to use standard unmarshal.
		return false, fmt.Sprintf("type mismatch at %s: expected %T, got %T", path, expected, actual)
	}

	// 3. Recursive Check based on Kind
	switch expType.Kind() {
	case reflect.Map:
		expMap, ok := expected.(map[string]interface{})
		if !ok {
			// Should be map[string]interface{} for JSON objects
			// If not, it might be map[interface{}]interface{} or custom type.
			// Attempt to cast or handle generic map.
			// for simplicity assuming map[string]interface{} as typical from json.Unmarshal
			logger.Warn("SchemaMatch: expected value matches map kind but not map[string]interface{}", zap.String("path", path))
			// strict match for now
			if !reflect.DeepEqual(expected, actual) {
				return false, fmt.Sprintf("non-standard map mismatch at %s", path)
			}
			return true, ""
		}
		actMap, ok := actual.(map[string]interface{})
		if !ok {
			return false, fmt.Sprintf("type mismatch at %s: expected map, got %T", path, actual)
		}

		for k, vExp := range expMap {
			vAct, exists := actMap[k]
			if !exists {
				return false, fmt.Sprintf("missing key at %s: %s", path, k)
			}
			newPath := k
			if path != "" {
				newPath = path + "." + k
			}
			if match, msg := schemaMatchRecursive(vExp, vAct, newPath, logger); !match {
				return false, msg
			}
		}
		// Extra keys in actMap are allowed (superset)

	case reflect.Slice, reflect.Array:
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)

		// For schema matching, the user requested that array length differences should be ignored.
		// We will only check the elements that exist in both arrays.
		// If actual has fewer elements, it's a pass.
		// If actual has more elements, it's a pass (superset).
		// We only check type/structure for the common indices.

		minLen := expSlice.Len()
		if actSlice.Len() < minLen {
			minLen = actSlice.Len()
		}

		for i := 0; i < minLen; i++ {
			vExp := expSlice.Index(i).Interface()
			vAct := actSlice.Index(i).Interface()
			newPath := fmt.Sprintf("%s[%d]", path, i)
			if match, msg := schemaMatchRecursive(vExp, vAct, newPath, logger); !match {
				return false, msg
			}
		}

	default:
		// Primitives (string, float64, bool)
		// We already checked types above. Value doesn't matter for schema match.
		return true, ""
	}

	return true, ""
}
