package variables

import (
	"mime"
	"net/url"

	"go.keploy.io/server/v3/pkg/templates"
)

// IsFormMediaType reports whether ct (a Content-Type header value) declares
// application/x-www-form-urlencoded, ignoring case and parameters such as
// charset.
func IsFormMediaType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/x-www-form-urlencoded"
}

// ParseFormBody parses a raw wire application/x-www-form-urlencoded body when
// contentType declares a form media type and body is not a JSON document (some
// clients send JSON under the default form Content-Type, which stays JSON).
// ok is false when contentType is not form-urlencoded, body is empty or JSON,
// or url.ParseQuery fails or yields no fields.
func ParseFormBody(body, contentType string) (url.Values, bool) {
	if !IsFormMediaType(contentType) || body == "" || IsJSONDoc(body) {
		return nil, false
	}
	vals, err := url.ParseQuery(body)
	if err != nil || len(vals) == 0 {
		return nil, false
	}
	return vals, true
}

// FormLeafAt returns the single value of key path[0].Key in a raw wire
// application/x-www-form-urlencoded body. ok is false when path is not a
// single non-empty key, body is JSON or fails to parse, the key is absent, or
// the key repeats.
func FormLeafAt(body, contentType string, path []templates.PathSeg) (string, bool) {
	if len(path) != 1 || path[0].IsIndex || path[0].Key == "" {
		return "", false
	}
	vals, ok := ParseFormBody(body, contentType)
	if !ok {
		return "", false
	}
	vs, ok := vals[path[0].Key]
	if !ok || len(vs) != 1 {
		return "", false
	}
	return vs[0], true
}

// SpliceFormAt replaces the single value of key path[0].Key in a raw
// application/x-www-form-urlencoded body with value and re-encodes the form.
// ok is false when path is not a single non-empty key, body is JSON or fails
// to parse, or the key does not hold a single value.
func SpliceFormAt(body string, path []templates.PathSeg, value string) (string, bool) {
	if len(path) != 1 || path[0].IsIndex || path[0].Key == "" || IsJSONDoc(body) {
		return body, false
	}
	vals, err := url.ParseQuery(body)
	if err != nil {
		return body, false
	}
	vs, ok := vals[path[0].Key]
	if !ok || len(vs) != 1 {
		return body, false
	}
	vals.Set(path[0].Key, value)
	return vals.Encode(), true
}
