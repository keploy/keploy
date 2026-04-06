package secrets

import (
	"regexp"
	"strings"
	"sync"
)

// xmlRegexCache caches compiled regexes for XML tag/attribute patterns per key.
var (
	xmlRegexCache   = make(map[string]*regexp.Regexp)
	xmlAttrCache    = make(map[string]*regexp.Regexp)
	xmlRegexCacheMu sync.RWMutex
)

func getXMLTagRegex(key string) *regexp.Regexp {
	cacheKey := "tag:" + key
	xmlRegexCacheMu.RLock()
	if re, ok := xmlRegexCache[cacheKey]; ok {
		xmlRegexCacheMu.RUnlock()
		return re
	}
	xmlRegexCacheMu.RUnlock()

	pattern := `(?i)(<` + regexp.QuoteMeta(key) + `[^>]*>)([^<]*)(</` + regexp.QuoteMeta(key) + `>)`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	xmlRegexCacheMu.Lock()
	xmlRegexCache[cacheKey] = re
	xmlRegexCacheMu.Unlock()
	return re
}

func getXMLAttrRegex(key string) *regexp.Regexp {
	cacheKey := "attr:" + key
	xmlRegexCacheMu.RLock()
	if re, ok := xmlAttrCache[cacheKey]; ok {
		xmlRegexCacheMu.RUnlock()
		return re
	}
	xmlRegexCacheMu.RUnlock()

	pattern := `(?i)(` + regexp.QuoteMeta(key) + `\s*=\s*")([^"]*)`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	xmlRegexCacheMu.Lock()
	xmlAttrCache[cacheKey] = re
	xmlRegexCacheMu.Unlock()
	return re
}

// ProcessXMLBody detects and replaces secrets in an XML body string using
// regex-based tag content replacement with cached compiled patterns.
// Note: currently detects by tag/attribute name only (like JSON field-name matching).
// Value-pattern scanning (JWTs, API keys in arbitrary tags) requires full XML
// parsing and is not yet implemented — use JSON bodies or custom rules for those.
func ProcessXMLBody(body string, detector *Detector, engine SecretEngine, tracker *FieldTracker) string {
	if body == "" {
		return body
	}
	trimmed := strings.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return body
	}

	result := body

	for key := range detector.nameBody {
		fieldPath := "body." + key
		if detector.IsAllowed(fieldPath) {
			continue
		}

		// Process element content: <fieldName>value</fieldName>
		if re := getXMLTagRegex(key); re != nil {
			result = re.ReplaceAllStringFunc(result, func(match string) string {
				sub := re.FindStringSubmatch(match)
				if len(sub) < 4 || sub[2] == "" {
					return match
				}
				processed, err := engine.Process(sub[2])
				if err != nil {
					processed = "[KEPLOY_REDACTED:process_error]"
				}
				tracker.AddBody(key)
				return sub[1] + processed + sub[3]
			})
		}

		// Process attribute values: fieldName="value"
		if re := getXMLAttrRegex(key); re != nil {
			result = re.ReplaceAllStringFunc(result, func(match string) string {
				sub := re.FindStringSubmatch(match)
				if len(sub) < 3 || sub[2] == "" {
					return match
				}
				processed, err := engine.Process(sub[2])
				if err != nil {
					processed = "[KEPLOY_REDACTED:process_error]"
				}
				tracker.AddBody(key)
				return sub[1] + processed
			})
		}
	}

	return result
}
