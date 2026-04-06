package secrets

import (
	"encoding/json"
	"strings"
	"testing"

	ossModels "go.keploy.io/server/v3/pkg/models"
)

// --- Detection Tests ---

func TestDetector_Headers(t *testing.T) {
	d := NewDetector(nil, nil, nil, nil, nil)
	results := d.DetectInHeaders(map[string]string{
		"Authorization": "Bearer xyz",
		"Content-Type":  "application/json",
		"X-API-Key":     "secret123",
		"X-Request-ID":  "abc",
	})
	found := map[string]bool{}
	for _, r := range results {
		found[r.Field] = true
	}
	if !found["header.Authorization"] {
		t.Error("should detect Authorization")
	}
	if !found["header.X-API-Key"] {
		t.Error("should detect X-API-Key")
	}
	if found["header.Content-Type"] {
		t.Error("should NOT detect Content-Type")
	}
	if found["header.X-Request-ID"] {
		t.Error("should NOT detect X-Request-ID")
	}
}

func TestDetector_ValuePatterns(t *testing.T) {
	d := NewDetector(nil, nil, nil, nil, nil)

	tests := []struct {
		name     string
		field    string
		value    string
		detected bool
	}{
		// Note: test values are crafted to match regex patterns without triggering
		// GitHub push protection (which blocks real-looking Stripe, Slack, AWS keys).
		{"JWT", "data", "eyJhbGciOiJIUzI1NiJ9.eyJ0ZXN0IjoiZmFrZSJ9.dGVzdHNpZ25hdHVyZQ", true},
		{"Bearer", "val", "Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9", true},
		{"Basic Auth", "val", "Basic dXNlcjpwYXNzd29yZDEyMzQ1Njc4OTA=", true},
		{"Private Key", "val", "-----BEGIN PRIVATE KEY-----\nMIIE...", true},
		{"Short value", "x", "abc", false},
		{"Normal value", "user", "alice@example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := d.ScanValue(tt.field, tt.value)
			if tt.detected && reason == "" {
				t.Errorf("expected detection for %q", tt.value)
			}
			if !tt.detected && reason != "" {
				t.Errorf("unexpected detection %q for %q", reason, tt.value)
			}
		})
	}
}

func TestDetector_Allowlist(t *testing.T) {
	d := NewDetector(nil, nil, nil, []string{"header.Authorization"}, nil)
	results := d.DetectInHeaders(map[string]string{
		"Authorization": "Bearer xyz",
		"Cookie":        "session=abc",
	})
	for _, r := range results {
		if r.Field == "header.Authorization" {
			t.Error("Authorization should be allowlisted")
		}
	}
	found := false
	for _, r := range results {
		if r.Field == "header.Cookie" {
			found = true
		}
	}
	if !found {
		t.Error("Cookie should still be detected")
	}
}

func TestDetector_URLParams(t *testing.T) {
	d := NewDetector(nil, nil, nil, nil, nil)
	results := d.DetectInURLParams(map[string]string{
		"api_key": "secret123",
		"page":    "1",
	})
	if len(results) != 1 || results[0].Field != "url_param.api_key" {
		t.Errorf("expected api_key detection, got %v", results)
	}
}

func TestDetector_CustomRules(t *testing.T) {
	d := NewDetector(
		[]string{"X-Custom-Secret"},
		[]string{"my_private_field"},
		[]string{"custom_token"},
		nil,
		nil,
	)
	// Custom header
	results := d.DetectInHeaders(map[string]string{"X-Custom-Secret": "val"})
	if len(results) == 0 {
		t.Error("should detect custom header")
	}
	// Built-in should still work
	results = d.DetectInHeaders(map[string]string{"Authorization": "Bearer x"})
	if len(results) == 0 {
		t.Error("built-in headers should still be detected with custom additions")
	}
	// Custom body key
	if !d.IsBodyKeySensitive("my_private_field") {
		t.Error("custom body key should be sensitive")
	}
	// Built-in body key should still work
	if !d.IsBodyKeySensitive("password") {
		t.Error("built-in body key should still work")
	}
}

// --- Obfuscation Tests ---

func TestObfuscationEngine_CharsetPreservation(t *testing.T) {
	eng, _ := NewObfuscationEngine()

	tests := []string{
		"Bearer eyJhbGciOiJIUzI1NiJ9.abc123",
		"4111111111111111",
		"sk-live_A1b2C3d4",
		"",
	}
	for _, input := range tests {
		result, err := eng.Process(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		inputRunes := []rune(input)
		resultRunes := []rune(result)
		if len(resultRunes) != len(inputRunes) {
			t.Errorf("rune length mismatch for %q: %d vs %d", input, len(resultRunes), len(inputRunes))
		}
		// Verify each rune's charset is preserved.
		for i, r := range inputRunes {
			out := resultRunes[i]
			switch {
			case r >= 'A' && r <= 'Z' && (out < 'A' || out > 'Z'):
				t.Errorf("pos %d: expected upper, got %c", i, out)
			case r >= 'a' && r <= 'z' && (out < 'a' || out > 'z'):
				t.Errorf("pos %d: expected lower, got %c", i, out)
			case r >= '0' && r <= '9' && (out < '0' || out > '9'):
				t.Errorf("pos %d: expected digit, got %c", i, out)
			}
		}
		// Deterministic
		result2, _ := eng.Process(input)
		if result != result2 {
			t.Error("not deterministic")
		}
	}
}

// --- Encryption Tests ---

func TestEncryptionEngine_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	eng, err := NewEncryptionEngine(key, "wrapped-key", 1, "test-app")
	if err != nil {
		t.Fatal(err)
	}

	values := []string{
		"Bearer my-secret-token",
		"password123",
		"eyJhbGciOiJIUzI1NiJ9.eyJ0ZXN0IjoxfQ.sig",
		"",
		"short",
	}
	for _, v := range values {
		encrypted, err := eng.Process(v)
		if err != nil {
			t.Fatalf("encrypt %q: %v", v, err)
		}
		if v != "" && !strings.HasPrefix(encrypted, EncryptedPrefix) {
			t.Errorf("missing sentinel prefix for %q", v)
		}
		decrypted, err := eng.Decrypt(encrypted)
		if err != nil {
			t.Fatalf("decrypt %q: %v", encrypted, err)
		}
		if decrypted != v {
			t.Errorf("round-trip failed: %q -> %q -> %q", v, encrypted, decrypted)
		}
	}
}

func TestEncryptionEngine_BadKey(t *testing.T) {
	_, err := NewEncryptionEngine([]byte("too-short"), "", 0, "")
	if err == nil {
		t.Error("should reject non-32-byte key")
	}
}

func TestDecryptor_AllOrNothing(t *testing.T) {
	key := make([]byte, 32)
	eng, _ := NewEncryptionEngine(key, "wrapped", 1, "app")

	tc := &ossModels.TestCase{
		HTTPReq: ossModels.HTTPReq{
			Header: map[string]string{
				"Authorization": mustEncrypt(t, eng, "Bearer token"),
			},
			Body: `{"password": "` + mustEncrypt(t, eng, "secret") + `"}`,
		},
		HTTPResp: ossModels.HTTPResp{
			Header: map[string]string{},
		},
	}

	dec, _ := NewDecryptor(key, &EncryptionMetadata{WrappedDEK: "w", KeyVersion: 1, AppID: "app"})
	if err := dec.DecryptTestCase(tc); err != nil {
		t.Fatalf("decryption should succeed: %v", err)
	}
	if tc.HTTPReq.Header["Authorization"] != "Bearer token" {
		t.Error("header not decrypted")
	}
	if !strings.Contains(tc.HTTPReq.Body, "secret") {
		t.Error("body not decrypted")
	}
}

func mustEncrypt(t *testing.T, eng *EncryptionEngine, val string) string {
	t.Helper()
	e, err := eng.Process(val)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// --- JSON Body Tests (sjson byte-fidelity) ---

func TestProcessJSONBody_ByteFidelity(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	eng, _ := NewObfuscationEngine()
	tracker := NewFieldTracker()

	// Carefully crafted JSON with specific whitespace and key order.
	body := `{
  "username": "alice",
  "password": "super-secret-123",
  "count": 42,
  "nested": {
    "token": "jwt-value-here",
    "safe": "not-a-secret"
  }
}`

	result := ProcessJSONBody(body, det, eng, tracker)

	// Username should be byte-identical.
	if !strings.Contains(result, `"username": "alice"`) {
		t.Error("username should be preserved exactly")
	}
	// Count should be byte-identical (number formatting).
	if !strings.Contains(result, `"count": 42`) {
		t.Error("count should be preserved exactly (number format)")
	}
	// safe should be byte-identical.
	if !strings.Contains(result, `"safe": "not-a-secret"`) {
		t.Error("safe should be preserved exactly")
	}
	// password should be changed.
	if strings.Contains(result, "super-secret-123") {
		t.Error("password should be obfuscated")
	}
	// token should be changed.
	if strings.Contains(result, "jwt-value-here") {
		t.Error("token should be obfuscated")
	}
	// Verify JSON is still valid.
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Errorf("result is not valid JSON: %v", err)
	}
	// Tracker should have recorded paths.
	if _, ok := tracker.BodyPaths["password"]; !ok {
		t.Error("tracker should record password path")
	}
}

func TestProcessJSONBody_NonJSON(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	eng, _ := NewObfuscationEngine()
	tracker := NewFieldTracker()

	body := "plain text body"
	result := ProcessJSONBody(body, det, eng, tracker)
	if result != body {
		t.Error("non-JSON body should be unchanged")
	}
}

// --- XML Body Tests ---

func TestProcessXMLBody(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	eng, _ := NewObfuscationEngine()
	tracker := NewFieldTracker()

	body := `<request><password>my-secret</password><name>alice</name></request>`
	result := ProcessXMLBody(body, det, eng, tracker)

	if strings.Contains(result, "my-secret") {
		t.Error("password should be obfuscated in XML")
	}
	if !strings.Contains(result, "<name>alice</name>") {
		t.Error("name should be preserved")
	}
}

// --- URL Processing Tests ---

func TestProcessURL(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	eng, _ := NewObfuscationEngine()
	tracker := NewFieldTracker()

	url := "https://api.example.com/v1/users?api_key=secret123&page=1"
	result := ProcessURL(url, det, eng, tracker)

	if strings.Contains(result, "secret123") {
		t.Error("api_key value should be replaced")
	}
	if !strings.Contains(result, "page=1") {
		t.Error("page param should be unchanged")
	}
	if _, ok := tracker.URLParams["api_key"]; !ok {
		t.Error("tracker should record api_key")
	}
}

// --- Processor Integration Tests ---

func TestProcessor_ObfuscateMode_InjectsNoise(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	p := NewProcessor("obfuscate", det, nil, nil)

	tc := &ossModels.TestCase{
		HTTPReq: ossModels.HTTPReq{
			Header: map[string]string{"Authorization": "Bearer token"},
			Body:   `{"password": "secret"}`,
		},
		HTTPResp: ossModels.HTTPResp{
			Header: map[string]string{"Set-Cookie": "session=abc"},
			Body:   `{"ok": true}`,
		},
	}

	p.ProcessTestCase(tc)

	// Authorization should be obfuscated.
	if tc.HTTPReq.Header["Authorization"] == "Bearer token" {
		t.Error("Authorization not obfuscated")
	}
	// Noise should contain header and body entries.
	if tc.Noise == nil {
		t.Fatal("noise should be set")
	}
	headerNoise := tc.Noise["header"]
	hasAuth := false
	for _, h := range headerNoise {
		if h == "Authorization" {
			hasAuth = true
		}
	}
	if !hasAuth {
		t.Error("Authorization should be in header noise")
	}
}

func TestProcessor_EncryptMode_ResponseNoise(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	encEng, _ := NewEncryptionEngine(key, "wrapped", 1, "app")
	det := NewDetector(nil, nil, nil, nil, nil)
	p := NewProcessor("encrypt", det, encEng, nil)

	tc := &ossModels.TestCase{
		HTTPReq: ossModels.HTTPReq{
			Header: map[string]string{"Authorization": "Bearer token"},
		},
		HTTPResp: ossModels.HTTPResp{
			Header: map[string]string{"Set-Cookie": "session=abc123def"},
		},
	}

	p.ProcessTestCase(tc)

	// Request header should be encrypted (not noised — decrypt restores it).
	if !IsEncrypted(tc.HTTPReq.Header["Authorization"]) {
		t.Error("Authorization should be encrypted")
	}
	// Response header should be encrypted AND noised (Fix #4: dynamic response secrets).
	if !IsEncrypted(tc.HTTPResp.Header["Set-Cookie"]) {
		t.Error("Set-Cookie should be encrypted")
	}
	if tc.Noise == nil {
		t.Fatal("noise should be set for response fields")
	}
	hasCookie := false
	for _, h := range tc.Noise["header"] {
		if h == "Set-Cookie" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Error("Set-Cookie should be in noise (response-side, Fix #4)")
	}
	// Request-side Authorization should NOT be in noise (encrypt mode restores it).
	for _, h := range tc.Noise["header"] {
		if h == "Authorization" {
			t.Error("Authorization should NOT be in noise in encrypt mode (decrypt restores it)")
		}
	}
}

func TestProcessor_Mock(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	p := NewProcessor("obfuscate", det, nil, nil)

	mock := &ossModels.Mock{
		Kind: ossModels.HTTP,
		Spec: ossModels.MockSpec{
			HTTPReq: &ossModels.HTTPReq{
				Header: map[string]string{"X-API-Key": "secret-key"},
				Body:   `{"api_key": "my-api-key-123"}`,
			},
			HTTPResp: &ossModels.HTTPResp{
				Header: map[string]string{},
				Body:   `{"data": "ok"}`,
			},
		},
	}

	p.ProcessMock(mock)

	if mock.Spec.HTTPReq.Header["X-API-Key"] == "secret-key" {
		t.Error("X-API-Key should be obfuscated")
	}
	if strings.Contains(mock.Spec.HTTPReq.Body, "my-api-key-123") {
		t.Error("api_key in body should be obfuscated")
	}
}

func TestProcessor_NonHTTPMock(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	p := NewProcessor("obfuscate", det, nil, nil)

	mock := &ossModels.Mock{
		Kind: ossModels.REDIS,
		Spec: ossModels.MockSpec{
			RedisRequests: []ossModels.Payload{
				{Message: []ossModels.OutputBinary{
					{Data: `{"token": "redis-secret-token"}`},
				}},
			},
		},
	}

	p.ProcessMock(mock)

	if strings.Contains(mock.Spec.RedisRequests[0].Message[0].Data, "redis-secret-token") {
		t.Error("token in Redis payload should be obfuscated")
	}
}

// --- Config File Tests ---

func TestParseConfigContent(t *testing.T) {
	content := `
customHeaders:
  - "X-Custom-Auth"
customBodyKeys:
  - "signing_secret"
customURLParams:
  - "auth_code"
allowlist:
  - "header.X-Safe"
`
	cfg, err := ParseConfigContent(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CustomHeaders) != 1 || cfg.CustomHeaders[0] != "X-Custom-Auth" {
		t.Errorf("custom headers not parsed: got %v", cfg.CustomHeaders)
	}
	if len(cfg.Allowlist) != 1 || cfg.Allowlist[0] != "header.X-Safe" {
		t.Errorf("allowlist not parsed: got %v", cfg.Allowlist)
	}
}

func TestParseConfigContent_Empty(t *testing.T) {
	cfg, err := ParseConfigContent("")
	if err != nil || cfg != nil {
		t.Error("empty content should return nil")
	}
}

// --- Detection Report Test ---

func TestDetectionReport(t *testing.T) {
	det := NewDetector(nil, nil, nil, nil, nil)
	p := NewProcessor("obfuscate", det, nil, nil)

	tc := &ossModels.TestCase{
		HTTPReq: ossModels.HTTPReq{
			Header: map[string]string{"Authorization": "Bearer token"},
		},
		HTTPResp: ossModels.HTTPResp{},
	}
	p.ProcessTestCase(tc)

	if len(p.Report.Entries) == 0 {
		t.Error("detection report should have entries")
	}
	found := false
	for _, e := range p.Report.Entries {
		if e.Field == "header.Authorization" {
			found = true
			if e.Action != "obfuscated" {
				t.Errorf("expected action 'obfuscated', got %q", e.Action)
			}
		}
	}
	if !found {
		t.Error("report should contain Authorization entry")
	}
}
