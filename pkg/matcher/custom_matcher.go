package matcher

import (
	"encoding/json"
	"fmt"
	"strings"
)

// valueByPath extracts value from nested JSON using dot path
func valueByPath(data []byte, path string) (interface{}, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}

	parts := strings.Split(path, ".")
	var current interface{} = obj

	for _, p := range parts {
		if m, ok := current.(map[string]interface{}); ok {
			current = m[p]
		} else {
			return nil, fmt.Errorf("invalid path")
		}
	}
	return current, nil
}

// buildMatcher creates matcher function based on rule
func buildMatcher(rule string) func(interface{}, interface{}) bool {
	switch rule {
	case "ignore":
		return func(_, _ interface{}) bool { return true }
	case "exact":
		return func(a, b interface{}) bool { return a == b }
	default:
		return func(a, b interface{}) bool { return a == b }
	}
}
