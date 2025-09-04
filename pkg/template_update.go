package pkg

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// updateTemplatesFromJSON attempts to update global utils.TemplatizedValues from the
// given HTTP response body. Returns true if any value changed. It is intentionally
// lightweight and avoids any replay specific propagation (handled elsewhere).
// It supports:
//  1. JSON bodies (exact key match, numeric suffix base-key match, normalized key match)
//  2. Raw JWT tokens when body isn't JSON (updates keys containing 'token')
//
// allowedKeys, if non-nil and non-empty, restricts updates to only those template keys
// referenced by the request that produced this response. This prevents unrelated
// template keys from being overwritten during re-record.
func updateTemplatesFromJSON(logger *zap.Logger, body []byte, allowedKeys map[string]struct{}) bool {
	if len(utils.TemplatizedValues) == 0 || len(body) == 0 {
		return false
	}

	// Try JSON first with UseNumber to preserve fidelity for ints vs floats.
	var parsed map[string]interface{}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		// Fallback to JWT token extraction if not JSON.
		jwtRe := regexp.MustCompile(`eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`)
		token := jwtRe.FindString(string(body))
		if token == "" {
			return false
		}
		changed := false
		for k, v := range utils.TemplatizedValues {
			if strings.Contains(normalizeKey(k), "token") && fmt.Sprintf("%v", v) != token {
				logger.Debug("updating template from non-JSON response (JWT)", zap.String("key", k), zap.String("new_value", token))
				utils.TemplatizedValues[k] = token
				changed = true
			}
		}
		return changed
	}

	changed := false
	for tKey, oldVal := range utils.TemplatizedValues {
		// Determine whether this template key should be considered for update.
		// If allowedKeys is empty, all keys are considered. Otherwise we allow
		// the key if any of the following are true:
		//  - the exact templated key is present in allowedKeys
		//  - the numeric-suffix base key is present in allowedKeys
		//  - the exact key appears in the parsed JSON response
		//  - the numeric-suffix base key appears in the parsed JSON response
		// This lets producer-only keys (e.g. id115 -> base "id") update when
		// the response contains the produced value even if the request didn't
		// reference that template key.
		consider := false
		if len(allowedKeys) == 0 {
			consider = true
		} else {
			if _, ok := allowedKeys[tKey]; ok {
				consider = true
			}
			if !consider {
				if base, has := stripNumericSuffix(tKey); has {
					if _, ok2 := allowedKeys[base]; ok2 {
						consider = true
					}
				}
			}
			// Also allow if the key (or its numeric-suffix base) actually appears
			// in the parsed response JSON -- this covers produced values present
			// only in the response.
			if !consider {
				if _, inParsed := parsed[tKey]; inParsed {
					consider = true
				} else if base, has := stripNumericSuffix(tKey); has {
					if _, inParsedBase := parsed[base]; inParsedBase {
						consider = true
					}
				}
			}
			if !consider {
				continue
			}
		}
		// Exact key
		if rVal, ok := parsed[tKey]; ok {
			if applyTemplateValue(logger, tKey, oldVal, rVal) {
				changed = true
			}
			continue
		}
		// Numeric suffix base key (e.g. id1 -> id)
		if base, has := stripNumericSuffix(tKey); has {
			if rVal, ok := parsed[base]; ok {
				if applyTemplateValue(logger, tKey, oldVal, rVal) {
					changed = true
				}
				continue
			}
		}
		// Normalized key comparison
		normT := normalizeKey(tKey)
		for rKey, rVal := range parsed {
			if normT == normalizeKey(rKey) {
				if applyTemplateValue(logger, tKey, oldVal, rVal) {
					changed = true
				}
				break
			}
		}
	}
	if changed {
		logger.Debug("updated template values from HTTP response", zap.Any("templates", utils.TemplatizedValues))
	}
	return changed
}

// applyTemplateValue converts json.Number when possible and updates the map if changed.
func applyTemplateValue(logger *zap.Logger, key string, oldVal, newVal interface{}) bool {
	currentStr := fmt.Sprintf("%v", oldVal)
	var final interface{} = newVal
	if num, ok := newVal.(json.Number); ok {
		if i, err := num.Int64(); err == nil {
			final = i
		} else if f, err := num.Float64(); err == nil {
			final = f
		}
	}
	newStr := fmt.Sprintf("%v", final)
	if currentStr == newStr {
		return false
	}
	logger.Info("updating template value", zap.String("key", key), zap.Any("old_value", oldVal), zap.Any("new_value", final))
	utils.TemplatizedValues[key] = final
	return true
}

// Helpers duplicated locally (kept small) to avoid depending on orchestrator code.
func normalizeKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "-", "")
	k = strings.ReplaceAll(k, "_", "")
	return k
}

func stripNumericSuffix(s string) (string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] < '0' || s[i] > '9' {
			if i < len(s)-1 {
				return s[:i+1], true
			}
			return s, false
		}
	}
	return "", false
}
