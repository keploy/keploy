package secrets

import (
	"net/url"
	"strings"

	ossModels "go.keploy.io/server/v3/pkg/models"
)

// ProcessFormData detects and replaces secrets in form data entries.
func ProcessFormData(form []ossModels.FormData, detector *Detector, engine SecretEngine, tracker *FieldTracker) []ossModels.FormData {
	if len(form) == 0 {
		return form
	}
	for i, entry := range form {
		fieldPath := "body." + entry.Key
		if detector.IsAllowed(fieldPath) {
			continue
		}
		isSensitive := detector.IsBodyKeySensitive(entry.Key)
		// Also check value patterns (Fix #12: form values may contain JWTs etc.)
		if !isSensitive {
			for _, val := range entry.Values {
				if reason := detector.ScanValue(entry.Key, val); reason != "" {
					isSensitive = true
					break
				}
			}
		}
		if !isSensitive {
			continue
		}
		for j, val := range entry.Values {
			processed, err := engine.Process(val)
			if err != nil {
				processed = "[KEPLOY_REDACTED:process_error]"
			}
			form[i].Values[j] = processed
		}
		tracker.AddBody(entry.Key)
	}
	return form
}

// ProcessURLParams detects and replaces secrets in a URL parameter map.
func ProcessURLParams(params map[string]string, detector *Detector, engine SecretEngine, tracker *FieldTracker) map[string]string {
	if len(params) == 0 {
		return params
	}
	results := detector.DetectInURLParams(params)
	for _, r := range results {
		processed, err := engine.Process(r.Value)
		if err != nil {
			processed = "[KEPLOY_REDACTED:process_error]"
		}
		// Extract the bare key from "url_param.key".
		key := strings.TrimPrefix(r.Field, "url_param.")
		params[key] = processed
		tracker.AddURLParam(key)
	}
	return params
}

// ProcessURL parses a raw URL string, detects and replaces secrets in query
// parameters, and reassembles the URL.
func ProcessURL(rawURL string, detector *Detector, engine SecretEngine, tracker *FieldTracker) string {
	if rawURL == "" {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	query := parsed.Query()
	if len(query) == 0 {
		return rawURL
	}

	changed := false
	for key, values := range query {
		fieldPath := "url_param." + key
		if detector.IsAllowed(fieldPath) {
			continue
		}
		isSensitive := false
		if _, ok := detector.nameURLParams[strings.ToLower(key)]; ok {
			isSensitive = true
		}
		if !isSensitive {
			// Check value patterns.
			for _, v := range values {
				if reason := detector.ScanValue(key, v); reason != "" {
					isSensitive = true
					break
				}
			}
		}
		if !isSensitive {
			continue
		}
		for i, v := range values {
			processed, err := engine.Process(v)
			if err != nil {
				processed = "[KEPLOY_REDACTED:process_error]"
			}
			query[key][i] = processed
			changed = true
		}
		tracker.AddURLParam(key)
	}

	if !changed {
		return rawURL
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
