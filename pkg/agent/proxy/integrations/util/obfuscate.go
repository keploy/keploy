package util

import (
	"encoding/json"
	"regexp"
	"sync"
)

// NoiseChecker holds compiled regex patterns from Mock.Noise for efficient
// repeated checking during mock matching. Safe for concurrent use.
type NoiseChecker struct {
	patterns []*regexp.Regexp
}

// Global cache for compiled regexes to avoid recompiling the same patterns
// across multiple mock comparisons.
var (
	noiseCacheMu sync.RWMutex
	noiseCache   = make(map[string]*regexp.Regexp)
)

func getCachedRegexp(pattern string) *regexp.Regexp {
	noiseCacheMu.RLock()
	re := noiseCache[pattern]
	noiseCacheMu.RUnlock()
	if re != nil {
		return re
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil // invalid pattern — silently skipped; not user-actionable
	}
	noiseCacheMu.Lock()
	if old := noiseCache[pattern]; old == nil {
		noiseCache[pattern] = compiled
	} else {
		compiled = old
	}
	noiseCacheMu.Unlock()
	return compiled
}

// NewNoiseChecker compiles the regex patterns from Mock.Noise into a checker.
// Returns nil if patterns is nil or empty — callers should nil-check before use,
// or use the methods directly which handle nil receivers.
func NewNoiseChecker(patterns []string) *NoiseChecker {
	if len(patterns) == 0 {
		return nil
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if re := getCachedRegexp(p); re != nil {
			compiled = append(compiled, re)
		}
	}
	if len(compiled) == 0 {
		return nil
	}
	return &NoiseChecker{patterns: compiled}
}

// IsNoisy reports whether value matches any of the obfuscation noise patterns,
// indicating it was redacted and should be skipped during match scoring.
func (nc *NoiseChecker) IsNoisy(value string) bool {
	if nc == nil {
		return false
	}
	for _, re := range nc.patterns {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

// IsNoisyValue reports whether value (as interface{}) matches any noise pattern.
// Returns false for non-string values or if nc is nil.
func (nc *NoiseChecker) IsNoisyValue(value interface{}) bool {
	s, ok := value.(string)
	if !ok || nc == nil {
		return false
	}
	return nc.IsNoisy(s)
}

// StripNoisyJSON removes values matching noise patterns from a JSON body string.
// Returns the body as-is if it's not JSON or nc is nil.
func StripNoisyJSON(body string, nc *NoiseChecker) string {
	if nc == nil {
		return body
	}
	var data interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return body
	}
	stripped := StripNoisyFields(data, nc)
	b, err := json.Marshal(stripped)
	if err != nil {
		return body
	}
	return string(b)
}

// StripNoisyFields recursively removes noisy values from parsed JSON.
func StripNoisyFields(data interface{}, nc *NoiseChecker) interface{} {
	if nc == nil {
		return data
	}
	switch v := data.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, val := range v {
			if nc.IsNoisyValue(val) {
				continue
			}
			result[key] = StripNoisyFields(val, nc)
		}
		return result
	case []interface{}:
		result := make([]interface{}, 0, len(v))
		for _, item := range v {
			if nc.IsNoisyValue(item) {
				continue
			}
			result = append(result, StripNoisyFields(item, nc))
		}
		return result
	default:
		return data
	}
}

// HasExtraNonNoisyKeys checks whether reqVal contains keys not present in
// mockVal (excluding keys whose mock value is noisy). Returns true if extra
// non-noisy keys exist, meaning the request is not an exact match.
func HasExtraNonNoisyKeys(mockVal, reqVal interface{}, nc *NoiseChecker) bool {
	switch mv := mockVal.(type) {
	case map[string]interface{}:
		rv, ok := reqVal.(map[string]interface{})
		if !ok {
			return false
		}
		// Build set of all mock keys — noisy keys are still valid keys,
		// we only skip their value comparison, not their presence.
		mockKeys := make(map[string]struct{}, len(mv))
		for k := range mv {
			mockKeys[k] = struct{}{}
		}
		for k := range rv {
			if _, exists := mockKeys[k]; !exists {
				return true
			}
		}
		// Recurse into shared non-noisy keys
		for k, mockField := range mv {
			if nc.IsNoisyValue(mockField) {
				continue
			}
			if reqField, exists := rv[k]; exists {
				if HasExtraNonNoisyKeys(mockField, reqField, nc) {
					return true
				}
			}
		}
		return false
	case []interface{}:
		rv, ok := reqVal.([]interface{})
		if !ok {
			return false
		}
		// Request array has more elements than mock array — extra data
		if len(rv) > len(mv) {
			return true
		}
		for i := 0; i < len(mv) && i < len(rv); i++ {
			if nc.IsNoisyValue(mv[i]) {
				continue
			}
			if HasExtraNonNoisyKeys(mv[i], rv[i], nc) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// JSONBodyMatchScore recursively compares mock and request JSON values,
// excluding noisy (obfuscated) values from scoring. Noisy values don't
// count towards matched or total — they are tracked separately.
func JSONBodyMatchScore(mockVal, reqVal interface{}, nc *NoiseChecker) (matched, total, noisy int) {
	if nc.IsNoisyValue(mockVal) {
		return 0, 0, 1
	}

	switch mv := mockVal.(type) {
	case map[string]interface{}:
		rv, ok := reqVal.(map[string]interface{})
		if !ok {
			return 0, 1, 0
		}
		for key, mockField := range mv {
			if nc.IsNoisyValue(mockField) {
				noisy++
				continue
			}
			reqField, exists := rv[key]
			if !exists {
				total++
				continue
			}
			m, t, n := JSONBodyMatchScore(mockField, reqField, nc)
			matched += m
			total += t
			noisy += n
		}
		return

	case []interface{}:
		rv, ok := reqVal.([]interface{})
		if !ok {
			return 0, 1, 0
		}
		for i := 0; i < len(mv); i++ {
			if nc.IsNoisyValue(mv[i]) {
				noisy++
				continue
			}
			if i >= len(rv) {
				total++
				continue
			}
			m, t, n := JSONBodyMatchScore(mv[i], rv[i], nc)
			matched += m
			total += t
			noisy += n
		}
		return

	default:
		total = 1
		if mockVal == reqVal {
			matched = 1
		}
		return
	}
}
