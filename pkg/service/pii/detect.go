package pii

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	PatternEmail      = "email"
	PatternPhone      = "phone"
	PatternCreditCard = "credit_card"
	PatternSSN        = "ssn"
	PatternFieldName  = "field_name"
)

var (
	emailRegex = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	phoneRegex = regexp.MustCompile(`(?:\+?\d{1,3}[\s.\-]?)?(?:\(?\d{2,4}\)?[\s.\-]?)\d{3,4}[\s.\-]?\d{4}`)
	ccRegex    = regexp.MustCompile(`(?:\d[ -]*?){13,19}`)
	ssnRegex   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
)

var piiFieldNames = map[string]struct{}{
	"password":        {},
	"secret":          {},
	"token":           {},
	"ssn":             {},
	"credit_card":     {},
	"social_security": {},
	"date_of_birth":   {},
	"passport":        {},
}

type Detection struct {
	Field       string
	PatternType string
}

func DetectHeaders(headers map[string]string, source string) []Detection {
	detections := make([]Detection, 0)
	for key, value := range headers {
		field := key
		if source != "" {
			field = source + "." + key
		}
		detections = append(detections, DetectValue(field, value)...)
	}
	return dedupeDetections(detections)
}

func DetectBody(body string, source string) []Detection {
	if strings.TrimSpace(body) == "" {
		return nil
	}

	var payload interface{}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return DetectValue(source, body)
	}

	detections := make([]Detection, 0)
	detections = append(detections, detectJSON(payload, source)...)
	return dedupeDetections(detections)
}

func DetectValue(field, value string) []Detection {
	detections := make([]Detection, 0)
	if isPIIFieldName(field) {
		detections = append(detections, Detection{Field: field, PatternType: PatternFieldName})
	}

	if emailRegex.FindString(value) != "" {
		detections = append(detections, Detection{Field: field, PatternType: PatternEmail})
	}

	if hasPhone(value) {
		detections = append(detections, Detection{Field: field, PatternType: PatternPhone})
	}

	if hasCreditCard(value) {
		detections = append(detections, Detection{Field: field, PatternType: PatternCreditCard})
	}

	if ssnRegex.FindString(value) != "" {
		detections = append(detections, Detection{Field: field, PatternType: PatternSSN})
	}

	return dedupeDetections(detections)
}

func detectJSON(value interface{}, path string) []Detection {
	detections := make([]Detection, 0)
	switch v := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			nextPath := key
			if path != "" {
				nextPath = path + "." + key
			}
			detections = append(detections, detectJSON(v[key], nextPath)...)
		}
	case []interface{}:
		for i, item := range v {
			nextPath := path + "[" + strconv.Itoa(i) + "]"
			detections = append(detections, detectJSON(item, nextPath)...)
		}
	case string:
		detections = append(detections, DetectValue(path, v)...)
	default:
		// Non-string JSON values (numbers, booleans, etc.) are converted to
		// strings so that regex-based patterns (SSN, credit card, etc.) can
		// still match their textual representation.
		detections = append(detections, DetectValue(path, fmt.Sprint(v))...)
	}
	return dedupeDetections(detections)
}

func isPIIFieldName(field string) bool {
	// Insert a separator before each uppercase letter so that camelCase keys
	// like "creditCard" or "socialSecurity" are split into their component
	// words (e.g. "credit_card", "social_security") before lookup.
	var expanded strings.Builder
	for i, r := range field {
		if r >= 'A' && r <= 'Z' && i > 0 {
			expanded.WriteByte('_')
		}
		expanded.WriteRune(r)
	}
	parts := strings.FieldsFunc(strings.ToLower(expanded.String()), func(r rune) bool {
		switch r {
		case '.', '_', '-', '[', ']':
			return true
		default:
			return false
		}
	})
	for _, part := range parts {
		if _, ok := piiFieldNames[part]; ok {
			return true
		}
	}
	return false
}

func hasPhone(value string) bool {
	matches := phoneRegex.FindAllString(value, -1)
	for _, match := range matches {
		digits := onlyDigits(match)
		if len(digits) < 10 || len(digits) > 15 {
			continue
		}
		// Avoid plain numeric IDs as phone numbers.
		if !strings.ContainsAny(match, "+-(). ") && len(digits) == 10 {
			continue
		}
		return true
	}
	return false
}

func hasCreditCard(value string) bool {
	matches := ccRegex.FindAllString(value, -1)
	for _, match := range matches {
		digits := onlyDigits(match)
		if len(digits) < 13 || len(digits) > 19 {
			continue
		}
		if isLuhnValid(digits) {
			return true
		}
	}
	return false
}

func isLuhnValid(number string) bool {
	sum := 0
	double := false
	for i := len(number) - 1; i >= 0; i-- {
		d := int(number[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum > 0 && sum%10 == 0
}

func onlyDigits(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, ch := range value {
		if ch >= '0' && ch <= '9' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func dedupeDetections(detections []Detection) []Detection {
	if len(detections) == 0 {
		return nil
	}
	uniq := make(map[string]Detection, len(detections))
	for _, d := range detections {
		key := d.Field + "|" + d.PatternType
		uniq[key] = d
	}
	keys := make([]string, 0, len(uniq))
	for key := range uniq {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Detection, 0, len(keys))
	for _, key := range keys {
		out = append(out, uniq[key])
	}
	return out
}
