package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
)

// GenerateTableDiff creates a human-readable key-value diff for two JSON strings.
// (JSON-only, compact "Path / Old / New" style.)
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
// If it isn't valid JSON, return the original string so callers can still diff safely.
func parseJSONLoose(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	if err != nil {
		return s, nil
	}

	// Check if there's any trailing content that would make it invalid JSON
	var trailing any
	if dec.Decode(&trailing) == nil {
		// There was trailing content, so return the original string
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

// -------------------- Non-JSON (gRPC) compact diff --------------------

// GeneratePlainOldNewDiff emits the old compact "Path / Old / New" diff for non-JSON bodies.
// For large payloads it prints short previews around the first difference, plus lengths,
// so we avoid spewing megabytes while keeping the exact original lines/labels.
func GeneratePlainOldNewDiff(expected, actual string, bodyType models.BodyType) string {
	if expected == actual {
		return "No differences found in body."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Path: %s\n", bodyType))
	sb.WriteString(fmt.Sprintf("  Old: %s\n", escapeOneLine(expected)))
	sb.WriteString(fmt.Sprintf("  New: %s\n", escapeOneLine(actual)))
	// write an empty line after the diff
	sb.WriteString("\n")
	return strings.TrimSpace(sb.String())
}

// firstDiff returns the first byte offset where a and b differ (or min(len(a),len(b)) if only length differs).
func firstDiff(a, b string) int {
	max := len(a)
	if len(b) < max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return max
}

// previewAt returns a short escaped window around pos.
func previewAt(s string, pos, ctx int) string {
	if pos < 0 {
		pos = 0
	}
	start := pos - ctx
	if start < 0 {
		start = 0
	}
	end := pos + ctx
	if end > len(s) {
		end = len(s)
	}
	head := ""
	tail := ""
	if start > 0 {
		head = "…"
	}
	if end < len(s) {
		tail = "…"
	}
	return head + escapeOneLine(s[start:end]) + tail
}

// escapeOneLine keeps output single-line and safe for terminals.
func escapeOneLine(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c >= 32 && c < 127 {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "\\x%02X", c)
			}
		}
	}
	return b.String()
}

func quote(s string) string { return strconv.Quote(s) }

// GeneratePlainBodyChangeSummary emits a terse, old-style summary for non-JSON bodies.
// It never prints body contents; it only reports whether the body was added/removed/modified
// plus lengths and the first differing offset (when applicable).
func GeneratePlainBodyChangeSummary(expected, actual string) string {
	const header = "=== CHANGES WITHIN THE RESPONSE BODY ===\n"
	const path = "body"

	if expected == actual {
		return "No differences found in body."
	}

	expLen, actLen := len(expected), len(actual)

	change := "modified"
	switch {
	case expLen == 0 && actLen > 0:
		change = "added"
	case expLen > 0 && actLen == 0:
		change = "removed"
	}

	// Only compute first-diff when both sides are non-empty.
	first := 0
	if expLen > 0 && actLen > 0 {
		first = firstDiff(expected, actual)
	}

	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteString(fmt.Sprintf("Path: %s\n", path))

	if expLen > 0 && actLen > 0 {
		// modified (both present)
		sb.WriteString(fmt.Sprintf("  Change: %s (len=%d -> %d, firstDiff@%d)\n", change, expLen, actLen, first))
	} else {
		// added / removed
		sb.WriteString(fmt.Sprintf("  Change: %s (len=%d -> %d)\n", change, expLen, actLen))
	}

	return strings.TrimSpace(sb.String())
}
