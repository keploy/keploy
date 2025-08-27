package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GenerateTableDiff creates a human-readable key-value diff for two JSON strings.
func GenerateTableDiff(expected, actual string) (string, error) {
	exp, err1 := parseJSONLoose(expected)
	act, err2 := parseJSONLoose(actual)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("cannot parse JSON (expectedErr=%v, actualErr=%v)", err1, err2)
	}

	aMap := map[string]string{}
	bMap := map[string]string{}
	flattenToMap(exp, "", aMap)
	flattenToMap(act, "", bMap)

	keysSet := map[string]struct{}{}
	for k := range aMap {
		keysSet[k] = struct{}{}
	}
	for k := range bMap {
		keysSet[k] = struct{}{}
	}

	keys := make([]string, 0, len(keysSet))
	for k := range keysSet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("=== CHANGES WITHIN THE RESPONSE BODY ===\n")

	hasDiffs := false
	for _, k := range keys {
		av, aok := aMap[k]
		bv, bok := bMap[k]

		// Only report on differences
		if aok && bok && av == bv {
			continue
		}

		hasDiffs = true
		path := strings.TrimPrefix(k, "$.")

		sb.WriteString(fmt.Sprintf("Path: %s\n", path))
		if aok && bok { // Modified
			sb.WriteString(fmt.Sprintf("  Old: %s\n", av))
			sb.WriteString(fmt.Sprintf("  New: %s\n\n", bv))
		} else if aok { // Removed
			sb.WriteString(fmt.Sprintf("  Old: %s\n", av))
			sb.WriteString("  New: <removed>\n\n")
		} else { // Added
			sb.WriteString("  Old: <added>\n")
			sb.WriteString(fmt.Sprintf("  New: %s\n\n", bv))
		}
	}

	if !hasDiffs {
		return "No differences found in JSON body after flattening.", nil
	}

	return strings.TrimSpace(sb.String()), nil
}

// parseJSONLoose parses a JSON string into an interface{}, using UseNumber to preserve number precision.
func parseJSONLoose(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	if err != nil {
		return s, nil
	}
	return v, nil
}

// flattenToMap recursively flattens a JSON-like structure into a map of path -> value strings.
func flattenToMap(v any, base string, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			next := k
			if base != "" {
				next = base + "." + k
			}
			flattenToMap(x[k], next, out)
		}
	case []any:
		for i, it := range x {
			next := fmt.Sprintf("%s[%d]", base, i)
			if base == "" {
				// Handle root-level arrays
				next = fmt.Sprintf("$[%d]", i)
			}
			flattenToMap(it, next, out)
		}
	default:
		js, err := json.Marshal(x)
		if err != nil {
			// Fallback to Sprintf for non-marshallable types
			js = []byte(fmt.Sprintf("%v", x))
		}
		out[pathWithDollar(base)] = string(js)
	}
}

// pathWithDollar ensures the path starts with a '$' for consistency.
func pathWithDollar(base string) string {
	if base == "" {
		return "$"
	}
	if strings.HasPrefix(base, "$") {
		return base
	}
	return "$." + base
}
