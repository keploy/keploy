package matcher

import (
	"fmt"
	"regexp"
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
	var parts []string
	for _, p := range regexp.MustCompile(`\.`).Split(path, -1) {
		parts = append(parts, p)
	}
	return parts
}

type Matcher interface {
	Match(expected, actual interface{}) error
}

type exactMatcher struct{}

func (e exactMatcher) Match(expected, actual interface{}) error {
	if expected != actual {
		return fmt.Errorf("values not equal")
	}
	return nil
}

func BuildMatcher(mType, pattern string, delta float64) (Matcher, error) {
	switch mType {
	case "exact":
		return exactMatcher{}, nil
	default:
		return nil, fmt.Errorf("unsupported matcher type: %s", mType)
	}
}
