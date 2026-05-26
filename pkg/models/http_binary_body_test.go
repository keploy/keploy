package models

import (
	"testing"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

// zipMagic is the leading bytes of a ZIP archive. These bytes are not valid
// UTF-8 (note 0x03, 0x04 and the high-bit bytes), which is exactly what
// breaks a naive yaml.Marshal of a string field containing them.
var zipMagic = []byte{
	0x50, 0x4b, 0x03, 0x04, 0x14, 0x00, 0x08, 0x00,
	0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0xff, 0xfe, 0xfd, 0xfc, // high-bit bytes that fail utf8.ValidString
}

func TestHTTPResp_YAML_RoundTrip_BinaryBody(t *testing.T) {
	// Reproduces the failure mode from keploy/enterprise#1902:
	// an application/zip response body contains non-UTF-8 bytes, which
	// yaml.v3 refuses to marshal as !!str.
	original := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type":        "application/zip",
			"Content-Disposition": `attachment; filename="mocks.zip"`,
		},
		Body:      string(zipMagic),
		Timestamp: time.Date(2026, 4, 21, 19, 56, 52, 0, time.UTC),
	}

	out, err := yamlLib.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal non-UTF-8 body failed: %v", err)
	}

	var decoded HTTPResp
	if err := yamlLib.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Body != original.Body {
		t.Fatalf("body mismatch after round-trip: want %d bytes, got %d bytes",
			len(original.Body), len(decoded.Body))
	}
}

func TestHTTPResp_YAML_UTF8BodyStaysPlain(t *testing.T) {
	// Regression guard: valid UTF-8 bodies must continue to serialize as a
	// plain scalar under `body:` without a body_base64 sibling.
	original := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json"},
		Body:       `{"hello":"world"}`,
		Timestamp:  time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
	}

	out, err := yamlLib.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if got := string(out); containsLine(got, "body_base64:") {
		t.Fatalf("UTF-8 body should not emit body_base64, got:\n%s", got)
	}

	var decoded HTTPResp
	if err := yamlLib.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Body != original.Body {
		t.Fatalf("body mismatch: want %q, got %q", original.Body, decoded.Body)
	}
}

func TestHTTPReq_YAML_RoundTrip_BinaryBody(t *testing.T) {
	// The same failure can happen on the request side when an app uploads
	// binary content (e.g. a PUT with application/octet-stream).
	original := HTTPReq{
		Method:     "PUT",
		ProtoMajor: 1, ProtoMinor: 1,
		URL:       "http://example.test/upload",
		Header:    map[string]string{"Content-Type": "application/octet-stream"},
		Body:      string(zipMagic),
		Timestamp: time.Date(2026, 4, 21, 19, 56, 52, 0, time.UTC),
	}

	out, err := yamlLib.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal non-UTF-8 body failed: %v", err)
	}

	var decoded HTTPReq
	if err := yamlLib.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Body != original.Body {
		t.Fatalf("body mismatch after round-trip: want %d bytes, got %d bytes",
			len(original.Body), len(decoded.Body))
	}
}

// containsLine reports whether s contains a line that begins with prefix
// (ignoring leading whitespace). Used to assert the absence of a YAML key.
func containsLine(s, prefix string) bool {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if lineHasPrefix(s[start:i], prefix) {
				return true
			}
			start = i + 1
		}
	}
	return lineHasPrefix(s[start:], prefix)
}

func lineHasPrefix(line, prefix string) bool {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i+len(prefix) > len(line) {
		return false
	}
	return line[i:i+len(prefix)] == prefix
}
