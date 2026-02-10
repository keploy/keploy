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

	// Status Code Match
	if tc.HTTPResp.StatusCode == actualResponse.StatusCode {
		res.StatusCode.Normal = true
	} else {
		pass = false
	}

	// Body Schema Match
	var expObj, actObj interface{}
	errExp := json.Unmarshal([]byte(tc.HTTPResp.Body), &expObj)
	errAct := json.Unmarshal([]byte(actualResponse.Body), &actObj)

	if errExp == nil && errAct == nil {
		res.BodyResult[0].Type = models.JSON
		match, msg := schemaMatchRecursive(expObj, actObj, "body", logger)
		if !match {
			pass = false
			logger.Error("Schema match FAIL", zap.String("reason", msg))
		} else {
			logger.Info("Schema match PASS")
		}
		res.BodyResult[0].Normal = match
	} else {
		if (errExp == nil) != (errAct == nil) {
			pass = false
			res.BodyResult[0].Normal = false
		} else {
			// Both non-JSON: fallback to strict equality.
			bodyMatch := tc.HTTPResp.Body == actualResponse.Body
			res.BodyResult[0].Normal = bodyMatch
			if !bodyMatch {
				pass = false
			}
		}
	}

	// Check headers existence
	for k := range tc.HTTPResp.Header {
		if _, ok := actualResponse.Header[k]; !ok {
			pass = false
		}
	}

	return pass, res
}

func schemaMatchRecursive(expected, actual interface{}, path string, logger *zap.Logger) (bool, string) {
	// Handle Nil Cases
	if expected == nil {
		// Strict: if we expect nil/null, actual must be nil/null
		if actual == nil {
			return true, ""
		}
		return false, fmt.Sprintf("mismatch at %s: expected nil, got %T", path, actual)
	}

	if actual == nil {
		return false, fmt.Sprintf("mismatch at %s: expected %T, got nil", path, expected)
	}

	expType := reflect.TypeOf(expected)
	actType := reflect.TypeOf(actual)

	// Type Check with Numeric Compatibility
	if expType != actType {
		// Handle the specific case where one is int and the other is float (common in Go JSON)
		if isNumeric(expType.Kind()) && isNumeric(actType.Kind()) {
			// Compatible numeric types, proceed
		} else {
			return false, fmt.Sprintf("type mismatch at %s: expected %T, got %T", path, expected, actual)
		}
	}

	// Recursive Check
	expKind := expType.Kind()

	// If expected was an interface, get the underlying kind
	if expKind == reflect.Interface {
		expKind = reflect.ValueOf(expected).Elem().Kind()
	}

	switch expKind {
	case reflect.Map:
		// Convert both to reflect.Value to handle any map type (not just map[string]interface{})
		expVal := reflect.ValueOf(expected)
		actVal := reflect.ValueOf(actual)

		if actVal.Kind() != reflect.Map {
			return false, fmt.Sprintf("type mismatch at %s: expected Map, got %v", path, actVal.Kind())
		}

		// Iterate over EXPECTED keys (because Field Deletion is NOT tolerable)
		for _, key := range expVal.MapKeys() {
			// Check if key exists in actual
			actValue := actVal.MapIndex(key)

			if !actValue.IsValid() {
				// Key missing in actual -> FAILURE
				return false, fmt.Sprintf("missing key at %s: %v", path, key)
			}

			// Construct new path
			newPath := fmt.Sprintf("%s.%v", path, key)
			if path == "" {
				newPath = fmt.Sprintf("%v", key)
			}

			// Recursion
			match, msg := schemaMatchRecursive(expVal.MapIndex(key).Interface(), actValue.Interface(), newPath, logger)
			if !match {
				return false, msg
			}
		}
		// Extra keys in Actual are ignored (Superset allowed)

	case reflect.Slice, reflect.Array:
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)

		// Rule Update: If Actual has FEWER elements than Expected, it's a structural mismatch
		// (missing fields/items defined in expected schema).
		// If Actual has MORE elements, it is tolerable.
		if actSlice.Len() < expSlice.Len() {
			return false, fmt.Sprintf("array length mismatch at %s: expected at least %d items, got %d", path, expSlice.Len(), actSlice.Len())
		}

		// Iterate only up to Expected Length
		// We validate that the first N items of Actual match the N items of Expected.
		// Any extra items in Actual are ignored.
		for i := 0; i < expSlice.Len(); i++ {
			vExp := expSlice.Index(i).Interface()
			vAct := actSlice.Index(i).Interface()
			newPath := fmt.Sprintf("%s[%d]", path, i)

			match, msg := schemaMatchRecursive(vExp, vAct, newPath, logger)
			if !match {
				return false, msg
			}
		}

	default:
		// Primitives (String, Bool, Float, Int)
		// We already checked Types (or numeric compatibility) above.
		// Values are ignored.
		return true, ""
	}

	return true, ""
}

// Helper to handle Go's strict types vs JSON loose numbers
func isNumeric(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
