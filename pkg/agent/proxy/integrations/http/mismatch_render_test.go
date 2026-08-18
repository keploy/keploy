package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// These renders are persisted into the report YAML and uploaded to the platform
// API, so a secret must never survive redaction. The tests below pin every
// redaction entry point (key-name denylist, mock-noise regexes, and the
// value-shape backstop) and also assert the backstop does NOT over-redact
// benign high-entropy values (UUIDs) that users need to read the diff.

const (
	// A real-shaped JWT (header.payload.signature, base64url, eyJ prefix).
	jwtSecret = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	awsKey    = "AKIAIOSFODNN7EXAMPLE"
	ghToken   = "ghp_1234567890abcdefghijklmnopqrstuvwx"
	kepToken  = "kep_xyHzkNV34tG9ec9Yii9SardU1JO9udmld8"
	// A benign high-entropy value that is NOT a credential — must remain visible.
	benignUUID = "550e8400-e29b-41d4-a716-446655440000"
)

func mustNotContain(t *testing.T, out, secret, where string) {
	t.Helper()
	if secret != "" && strings.Contains(out, secret) {
		t.Errorf("%s: secret %q leaked into output:\n%s", where, secret, out)
	}
}

func mustContain(t *testing.T, out, want, where string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("%s: expected output to contain %q, got:\n%s", where, want, out)
	}
}

func TestLooksLikeSecretValue(t *testing.T) {
	secrets := []string{
		jwtSecret,
		"Bearer " + jwtSecret,
		"bearer abcdef1234567890",
		"Basic dXNlcjpwYXNzd29yZA==",
		awsKey,
		ghToken,
		kepToken,
		"sk_live_abcdefghijklmnop1234",
		"xoxb-123456789012-abcdefXYZ",
		"glpat-abcdefghijklmnop1234",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQ...",
	}
	for _, s := range secrets {
		if !looksLikeSecretValue(s) {
			t.Errorf("looksLikeSecretValue(%q) = false, want true", s)
		}
	}

	// Must NOT be flagged — benign high-entropy / common values the user needs
	// in the diff. This guards the deliberate decision to NOT use raw entropy.
	benign := []string{
		benignUUID,
		"d41d8cd98f00b204e9800998ecf8427e", // md5 hex digest
		"deadbeefcafebabe1234567890abcdef", // 32-char hex (e.g. a request id)
		"warehouse-BLR-42",
		"a short string",
		"42",
		"application/json",
	}
	for _, s := range benign {
		if looksLikeSecretValue(s) {
			t.Errorf("looksLikeSecretValue(%q) = true, want false (over-redaction)", s)
		}
	}
}

func TestRedactBody_JSON(t *testing.T) {
	// noise regex covering a per-run token under a NON-keyword key
	nc := util.NewNoiseChecker([]string{"^sess-[a-z0-9]+$"})
	body := `{
	  "password": "hunter2",
	  "session": "sess-deadbeef",
	  "assertion": "` + jwtSecret + `",
	  "nested": {"client_secret": "shh-very-secret"},
	  "items": ["` + jwtSecret + `", "ok-value"],
	  "trace_id": "` + benignUUID + `"
	}`
	out := redactBody(body, "application/json", nc)

	mustNotContain(t, out, "hunter2", "keyword key (password)")
	mustNotContain(t, out, "sess-deadbeef", "noise-value on non-keyword key (session)")
	mustNotContain(t, out, jwtSecret, "value-shape backstop (assertion + array element)")
	mustNotContain(t, out, "shh-very-secret", "keyword key in nested object (client_secret)")
	mustContain(t, out, valueRedacted, "redaction marker present")
	// benign high-entropy value must remain so the user can diff it
	mustContain(t, out, benignUUID, "benign UUID retained")
	mustContain(t, out, "ok-value", "benign array element retained")
}

func TestRedactBody_FormURLEncoded(t *testing.T) {
	nc := util.NewNoiseChecker(nil)
	body := "api_key=supersecretvalue123&token=" + ghToken + "&order=42"
	out := redactBody(body, "application/x-www-form-urlencoded", nc)
	mustNotContain(t, out, "supersecretvalue123", "form keyword key (api_key)")
	mustNotContain(t, out, ghToken, "form value-shape backstop (token)")
	mustContain(t, out, "order=42", "benign form param retained")
}

func TestRedactBody_OpaqueXML(t *testing.T) {
	nc := util.NewNoiseChecker(nil)
	// SOAP / XML: no field structure to walk — the backstop must scrub the
	// embedded credential while leaving the surrounding structure readable.
	body := `<soap:Header><wsse:Password>plaintextpw</wsse:Password>` +
		`<Authorization>Bearer ` + jwtSecret + `</Authorization></soap:Header>`
	out := redactBody(body, "text/xml", nc)
	mustNotContain(t, out, jwtSecret, "opaque-body bearer/JWT scrub")
	mustContain(t, out, "<soap:Header>", "surrounding XML structure retained")
}

func TestRedactQuery(t *testing.T) {
	nc := util.NewNoiseChecker(nil)
	out := redactQuery("access_token="+jwtSecret+"&trace="+benignUUID+"&q=hello", nc)
	mustNotContain(t, out, jwtSecret, "query keyword key + value-shape")
	mustContain(t, out, benignUUID, "benign query param retained")
	mustContain(t, out, "q=hello", "benign query param retained")
}

func TestRenderRequestPreview_RedactsEverywhere(t *testing.T) {
	header := map[string]string{
		"Authorization": "Bearer " + jwtSecret, // keyword key
		"X-Trace-Token": jwtSecret,             // 'token' keyword key too
		"X-Correlation": jwtSecret,             // NON-keyword key -> value-shape backstop
		"Content-Type":  "application/json",
		"X-Request-Id":  benignUUID, // benign, must remain
	}
	body := `{"password":"hunter2","trace_id":"` + benignUUID + `"}`
	out := renderRequestPreview("POST", "/pay", "api_key="+ghToken+"&id="+benignUUID, header, body, nil)

	mustNotContain(t, out, jwtSecret, "header secret (keyword + non-keyword key)")
	mustNotContain(t, out, "hunter2", "body keyword key")
	mustNotContain(t, out, ghToken, "query value-shape")
	mustContain(t, out, benignUUID, "benign correlation/request id retained")
}

func TestRedactFieldDiffs(t *testing.T) {
	nc := []string{"^drift-[0-9]+$"}
	diffs := []models.MockFieldDiff{
		{Path: "header.Authorization", Expected: "Bearer old", Actual: "Bearer new"}, // keyword path
		{Path: "body.note", Expected: "drift-111", Actual: "drift-222"},              // noise value, benign path
		{Path: "body.assertion", Expected: jwtSecret, Actual: "x"},                   // value-shape backstop
		{Path: "body.qty", Expected: "2", Actual: "5"},                               // benign -> must remain
	}
	redactFieldDiffs(diffs, nc)
	if diffs[0].Expected != valueRedacted || diffs[0].Actual != valueRedacted {
		t.Errorf("keyword path not redacted: %+v", diffs[0])
	}
	if diffs[1].Expected != valueRedacted || diffs[1].Actual != valueRedacted {
		t.Errorf("noise value not redacted: %+v", diffs[1])
	}
	if diffs[2].Expected != valueRedacted {
		t.Errorf("value-shape secret not redacted: %+v", diffs[2])
	}
	if diffs[3].Expected != "2" || diffs[3].Actual != "5" {
		t.Errorf("benign qty diff should remain visible: %+v", diffs[3])
	}
}
