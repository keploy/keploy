package secrets

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ProcessJSONBody detects and replaces secrets in a JSON body string using
// byte-level sjson replacement (no re-serialization, preserves whitespace,
// key order, number formatting, and unicode escapes for untouched content).
func ProcessJSONBody(body string, detector *Detector, engine SecretEngine, tracker *FieldTracker) string {
	if body == "" {
		return body
	}
	trimmed := strings.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return body
	}
	if !gjson.Valid(body) {
		return body
	}

	// Collect all leaf paths that need replacement.
	type replacement struct {
		path  string
		value string // the new (processed) value
	}
	var replacements []replacement

	walkJSONLeaves(body, "", func(path, key, value string) {
		fieldPath := "body." + path
		if detector.IsAllowed(fieldPath) {
			return
		}

		// Check field name against sensitive body keys, then value patterns.
		detected := detector.IsBodyKeySensitive(key)
		if !detected {
			detected = detector.ScanValue(key, value) != ""
		}

		if detected {
			processed, err := engine.Process(value)
			if err != nil {
				// Fail-closed: redact the value rather than leaving the secret in place.
				processed = "[KEPLOY_REDACTED:process_error]"
			}
			replacements = append(replacements, replacement{path: path, value: processed})
			tracker.AddBody(path)
		}
	})

	if len(replacements) == 0 {
		return body
	}

	// Sort replacements by path length descending so deeper paths are replaced first.
	// This prevents sjson from invalidating byte offsets of parent paths.
	sort.Slice(replacements, func(i, j int) bool {
		return len(replacements[i].path) > len(replacements[j].path)
	})

	result := body
	for _, r := range replacements {
		// sjson.Set wraps the value with quotes for strings. Use SetRaw with explicit quoting
		// to ensure we produce a valid JSON string.
		quotedVal := quoteJSONString(r.value)
		updated, err := sjson.SetRaw(result, r.path, quotedVal)
		if err != nil {
			continue
		}
		result = updated
	}
	return result
}

// walkJSONLeaves recursively walks a JSON document and calls fn for each string leaf.
// path is the dot-separated sjson path, key is the immediate field name.
func walkJSONLeaves(json string, prefix string, fn func(path, key, value string)) {
	parsed := gjson.Parse(json)
	walkResult(parsed, prefix, fn)
}

func walkResult(result gjson.Result, prefix string, fn func(path, key, value string)) {
	if result.IsObject() {
		result.ForEach(func(key, value gjson.Result) bool {
			keyStr := key.String()
			// Escape dots in key names for sjson compatibility.
			escapedKey := escapeKey(keyStr)
			escapedPath := escapedKey
			if prefix != "" {
				escapedPath = prefix + "." + escapedKey
			}

			if value.IsObject() || value.IsArray() {
				walkResult(value, escapedPath, fn)
			} else if value.Type == gjson.String {
				fn(escapedPath, keyStr, value.Str)
			}
			return true
		})
	} else if result.IsArray() {
		result.ForEach(func(key, value gjson.Result) bool {
			idx := key.String()
			path := idx
			if prefix != "" {
				path = prefix + "." + idx
			}
			if value.IsObject() || value.IsArray() {
				walkResult(value, path, fn)
			} else if value.Type == gjson.String {
				fn(path, "", value.Str)
			}
			return true
		})
	}
}

// escapeKey escapes sjson special characters in JSON keys for path compatibility.
func escapeKey(key string) string {
	if !strings.ContainsAny(key, `.*?#\`) {
		return key
	}
	// Escape backslash first (sjson's escape char), then special chars.
	r := strings.NewReplacer(
		`\`, `\\`,
		".", `\.`,
		"*", `\*`,
		"?", `\?`,
		"#", `\#`,
	)
	return r.Replace(key)
}

// quoteJSONString produces a JSON-encoded string literal (with surrounding quotes).
func quoteJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[byte(r)>>4])
				b.WriteByte("0123456789abcdef"[byte(r)&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
