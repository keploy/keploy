package matcher

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"

	"go.keploy.io/server/v3/config"
)

// GetValueByPath extracts nested field using dot path
func GetValueByPath(data map[string]interface{}, path string) (interface{}, bool) {
	current := interface{}(data)

	for _, p := range splitPath(path) {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func splitPath(path string) []string {
	return strings.Split(path, ".")
}

type Matcher interface {
	Match(expected, actual interface{}) error
}

type exactMatcher struct{}

func (e exactMatcher) Match(expected, actual interface{}) error {
	if !reflect.DeepEqual(expected, actual) {
		return fmt.Errorf("values not equal: expected %v, got %v", expected, actual)
	}
	return nil
}

type regexMatcher struct {
	re *regexp.Regexp
}

func (r regexMatcher) Match(_, actual interface{}) error {
	if !r.re.MatchString(fmt.Sprint(actual)) {
		return fmt.Errorf("value %v does not match regex pattern %q", actual, r.re.String())
	}
	return nil
}

type toleranceMatcher struct {
	delta float64
}

func (t toleranceMatcher) Match(expected, actual interface{}) error {
	exp, ok := toFloat64(expected)
	if !ok {
		return fmt.Errorf("expected value is not numeric")
	}
	act, ok := toFloat64(actual)
	if !ok {
		return fmt.Errorf("actual value is not numeric")
	}
	diff := math.Abs(exp - act)
	if diff > t.delta {
		return fmt.Errorf("values differ by %v which exceeds tolerance %v (expected: %v, actual: %v)", diff, t.delta, expected, actual)
	}
	return nil
}

func BuildMatcher(mType, pattern string, delta float64) (Matcher, error) {
	switch mType {
	case "exact":
		return exactMatcher{}, nil
	case "regex":
		if pattern == "" {
			return nil, fmt.Errorf("regex matcher requires pattern")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return regexMatcher{re: re}, nil
	case "tolerance":
		if delta < 0 {
			return nil, fmt.Errorf("tolerance must be non-negative")
		}
		return toleranceMatcher{delta: delta}, nil
	default:
		return nil, fmt.Errorf("unsupported matcher type %q (valid types: exact, regex, tolerance)", mType)
	}
}

// CompareWithMatchers applies field-level matchers against JSON bodies.
// Matchers require top-level JSON objects; array/scalar roots are rejected.
func CompareWithMatchers(expectedBody []byte, actualBody []byte, matchers map[string]config.FieldMatcher) error {
	var expected interface{}
	var actual interface{}

	if err := json.Unmarshal(expectedBody, &expected); err != nil {
		return err
	}
	if err := json.Unmarshal(actualBody, &actual); err != nil {
		return err
	}

	expMap, ok := expected.(map[string]interface{})
	if !ok {
		return fmt.Errorf("field matchers require JSON object root")
	}
	actMap, ok := actual.(map[string]interface{})
	if !ok {
		return fmt.Errorf("field matchers require JSON object root")
	}

	for path, cfg := range matchers {
		expVal, ok := GetValueByPath(expMap, path)
		if !ok {
			return fmt.Errorf("missing field %q in expected body; verify the field path and expected payload", path)
		}
		actVal, ok := GetValueByPath(actMap, path)
		if !ok {
			return fmt.Errorf("missing field %q in actual body; verify the field path and response payload", path)
		}

		m, err := BuildMatcher(cfg.Type, cfg.Pattern, cfg.Delta)
		if err != nil {
			return err
		}

		if err := m.Match(expVal, actVal); err != nil {
			return fmt.Errorf("field matcher failed at path %q (type: %s): %w", path, cfg.Type, err)
		}
	}
	return nil
}

func toFloat64(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint64:
		return float64(x), true
	case uint32:
		return float64(x), true
	default:
		return 0, false
	}
}
