// Package matcher provides configurable custom matchers for replay comparisons.
package matcher

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// Custom matcher type constants.
const (
	MatcherTypeRegex            = "regex"
	MatcherTypeNumericTolerance = "numeric_tolerance"
	MatcherTypePresence         = "presence"
	MatcherTypeType             = "type"
)

// ApplyCustomMatcher evaluates the given custom matcher against expected and actual values.
// Returns true if the matcher passes, false otherwise.
// An error is returned only for invalid matcher configurations (not for match failures).
func ApplyCustomMatcher(matcherDef models.CustomMatcher, expected, actual interface{}) (bool, error) {
	switch strings.ToLower(matcherDef.Type) {
	case MatcherTypeRegex:
		return matchRegex(matcherDef.Value, actual), nil
	case MatcherTypeNumericTolerance:
		return matchNumericTolerance(matcherDef.Value, expected, actual)
	case MatcherTypePresence:
		return matchPresence(actual), nil
	case MatcherTypeType:
		return matchTypeCheck(matcherDef.Value, actual), nil
	default:
		return false, fmt.Errorf("unsupported custom matcher type: %q", matcherDef.Type)
	}
}

// matchRegex checks if the actual value (converted to string) matches the regex pattern.
func matchRegex(pattern string, actual interface{}) bool {
	if actual == nil {
		return false
	}
	s := fmt.Sprintf("%v", actual)
	return getCompiled(pattern).MatchString(s)
}

// matchNumericTolerance checks if expected and actual numeric values are within ±tolerance.
func matchNumericTolerance(toleranceStr string, expected, actual interface{}) (bool, error) {
	tolerance, err := strconv.ParseFloat(toleranceStr, 64)
	if err != nil {
		return false, fmt.Errorf("invalid numeric_tolerance value %q: %w", toleranceStr, err)
	}
	expFloat, ok := toFloat64(expected)
	if !ok {
		return false, nil
	}
	actFloat, ok := toFloat64(actual)
	if !ok {
		return false, nil
	}
	return math.Abs(expFloat-actFloat) <= tolerance, nil
}

// matchPresence returns true if the actual value is non-nil (i.e. the field exists).
func matchPresence(actual interface{}) bool {
	return actual != nil
}

// matchTypeCheck checks if the actual value's JSON type matches the expected type string.
// Supported types: "string", "number", "boolean", "array", "object", "null".
func matchTypeCheck(expectedType string, actual interface{}) bool {
	actualType := jsonType(actual)
	return strings.EqualFold(actualType, expectedType)
}

// jsonType returns the JSON type name for a Go interface value.
func jsonType(v interface{}) string {
	if v == nil {
		return "null"
	}
	rv := reflect.TypeOf(v)
	switch rv.Kind() {
	case reflect.String:
		return "string"
	case reflect.Float64, reflect.Float32, reflect.Int, reflect.Int64, reflect.Int32:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice:
		return "array"
	case reflect.Map:
		return "object"
	default:
		return "unknown"
	}
}

// toFloat64 attempts to convert an interface{} value to float64.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// ResolveCustomMatchers merges global and test-set-specific custom matchers.
// Test-set matchers override global matchers for the same field path.
func ResolveCustomMatchers(
	global map[string]map[string]models.CustomMatcher,
	testsets map[string]map[string]map[string]models.CustomMatcher,
	testSetID string,
) map[string]models.CustomMatcher {
	result := make(map[string]models.CustomMatcher)

	// Copy global body matchers.
	if bodyMatchers, ok := global["body"]; ok {
		for path, m := range bodyMatchers {
			result[path] = m
		}
	}

	// Override with test-set-specific body matchers.
	if tsMatchers, ok := testsets[testSetID]; ok {
		if bodyMatchers, ok := tsMatchers["body"]; ok {
			for path, m := range bodyMatchers {
				result[path] = m
			}
		}
	}

	return result
}
