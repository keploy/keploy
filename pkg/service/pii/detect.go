package pii

import (
	"encoding/json"
	"fmt"
	"io"
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

// maxPIIFieldNameWords is the longest entry in piiFieldNames, in words.
const maxPIIFieldNameWords = 3

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

	// UseNumber keeps a JSON number's digits as sent. Decoded to a float64, a
	// 16-digit card number prints as 4.111111111111111e+15 and no pattern
	// matches it.
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()

	var payload interface{}
	if err := decoder.Decode(&payload); err != nil {
		return DetectValue(source, body)
	}
	// Decode stops after the first value, so text after it means the body is
	// not a JSON document.
	if _, err := decoder.Token(); err != io.EOF {
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
		// Non-string JSON values (json.Number, booleans, etc.) are converted
		// to strings so that regex-based patterns (phone, credit card, etc.)
		// can still match their textual representation.
		detections = append(detections, DetectValue(path, fmt.Sprint(v))...)
	}
	return dedupeDetections(detections)
}

// isPIIFieldName reports whether field, a dotted path such as
// "request.body.user.creditCard", holds a word or run of adjacent words that
// is listed in piiFieldNames.
func isPIIFieldName(field string) bool {
	parts := splitFieldName(field)
	for i := range parts {
		for n := 1; n <= maxPIIFieldNameWords && i+n <= len(parts); n++ {
			if _, ok := piiFieldNames[strings.Join(parts[i:i+n], "_")]; ok {
				return true
			}
		}
	}
	return false
}

// splitFieldName splits field into lowercase words at path and word
// delimiters and at camelCase boundaries, keeping an acronym in one piece:
// "userSSNValue" becomes "user", "ssn", "value".
func splitFieldName(field string) []string {
	isLower := func(r rune) bool { return r >= 'a' && r <= 'z' }
	isUpper := func(r rune) bool { return r >= 'A' && r <= 'Z' }
	isDigit := func(r rune) bool { return r >= '0' && r <= '9' }

	runes := []rune(field)
	parts := make([]string, 0, 4)
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			parts = append(parts, strings.ToLower(word.String()))
			word.Reset()
		}
	}

	for i, r := range runes {
		switch r {
		case '.', '_', '-', '[', ']':
			flush()
			continue
		}
		if word.Len() > 0 && isUpper(r) {
			prev := runes[i-1]
			nextIsLower := i+1 < len(runes) && isLower(runes[i+1])
			if isLower(prev) || isDigit(prev) || (isUpper(prev) && nextIsLower) {
				flush()
			}
		}
		word.WriteRune(r)
	}
	flush()
	return parts
}

func hasPhone(value string) bool {
	matches := phoneRegex.FindAllString(value, -1)
	for _, match := range matches {
		digits := onlyDigits(match)
		if len(digits) < 10 || len(digits) > 15 {
			continue
		}
		// Avoid plain numeric IDs and Unix timestamps in seconds or
		// milliseconds as phone numbers.
		if !strings.ContainsAny(match, "+-(). ") && (len(digits) == 10 || len(digits) == 13) {
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
