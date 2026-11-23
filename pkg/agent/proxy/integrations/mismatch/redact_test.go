package mismatch

import (
	"strings"
	"testing"
)

func TestRedactorSharedHooks(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"

	t.Run("IsSensitiveKey", func(t *testing.T) {
		for _, k := range []string{"Authorization", "body.token", "body.attributes.api_key", "body.client_secret", "body.password", "body.user_pwd"} {
			if !IsSensitiveKey(k) {
				t.Errorf("IsSensitiveKey(%q) = false, want true", k)
			}
		}
		for _, k := range []string{"body.orderId", "body.status", "body.updatedAt", "body.row"} {
			if IsSensitiveKey(k) {
				t.Errorf("IsSensitiveKey(%q) = true, want false", k)
			}
		}
	})

	t.Run("LooksLikeSecretValue and ScrubSecretsInText", func(t *testing.T) {
		longJWT := "eyJhbGciOiJIUzI1NiJ9.eyJ" + strings.Repeat("a", 300) + ".c2lnbmF0dXJl"
		for _, s := range []string{
			jwt,
			longJWT,
			"Bearer " + jwt,
			"Basic dXNlcjpwYXNzd29yZA==",
			"AKIAIOSFODNN7EXAMPLE",
			"ghp_1234567890abcdefghijklmnopqrstuvwx",
			"kep_xyHzkNV34tG9ec9Yii9SardU1JO9udmld8",
			"sk_live_abcdefghijklmnop1234",
			"xoxb-123456789012-abcdefXYZ",
			"glpat-abcdefghijklmnop1234",
			"AIzaSyDaGmWKa4JsXZ-HjGw7ISLn_3namBGewQe",
			"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQ...",
		} {
			if !LooksLikeSecretValue(s) {
				t.Errorf("LooksLikeSecretValue(%q) = false, want true", s)
			}
		}
		for _, benign := range []string{"short", "550e8400-e29b-41d4-a716-446655440000", "deadbeefcafebabe1234567890abcdef", "ok-value"} {
			if LooksLikeSecretValue(benign) {
				t.Errorf("LooksLikeSecretValue(%q) = true, want false", benign)
			}
		}
		scrubbed := ScrubSecretsInText("auth=Bearer " + jwt + " ok=1")
		if strings.Contains(scrubbed, jwt) || !strings.Contains(scrubbed, ValueRedacted) || !strings.Contains(scrubbed, "ok=1") {
			t.Errorf("ScrubSecretsInText unexpected output: %q", scrubbed)
		}
	})

	t.Run("SplitTypedDiffValue", func(t *testing.T) {
		for _, tc := range []struct {
			in      string
			wantP   string
			wantRaw string
			wantOK  bool
		}{
			{"string: hello", "string: ", "hello", true},
			{"number: 42", "number: ", "42", true},
			{"boolean: true", "boolean: ", "true", true},
			{"null: null", "null: ", "null", true},
			{"object", "", "", false},
			{"array", "", "", false},
			{"plain", "", "", false},
		} {
			p, raw, ok := SplitTypedDiffValue(tc.in)
			if p != tc.wantP || raw != tc.wantRaw || ok != tc.wantOK {
				t.Errorf("SplitTypedDiffValue(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, p, raw, ok, tc.wantP, tc.wantRaw, tc.wantOK)
			}
		}
	})

	t.Run("ValueRedactor and JSONBodyDiffsWithTypeStrictRedacted pre-cut JWT", func(t *testing.T) {
		longJWT := "eyJhbGciOiJIUzI1NiJ9.eyJ" + strings.Repeat("a", 300) + ".c2lnbmF0dXJl"
		nilNoisy := ValueRedactor(nil)
		if !nilNoisy(longJWT) || nilNoisy("plain-value") {
			t.Fatalf("ValueRedactor(nil) unexpected result")
		}
		withNoisy := ValueRedactor(func(s string) bool { return s == "PH-1" })
		if !withNoisy("PH-1") || !withNoisy(longJWT) || withNoisy("plain-value") {
			t.Fatalf("ValueRedactor(fn) unexpected result")
		}
		diffs := JSONBodyDiffsWithTypeStrictRedacted(`{"x":1}`, `{"x":"`+longJWT+`"}`, nil, nil, nilNoisy)
		if len(diffs) != 1 || diffs[0].Actual != "string: "+ValueRedacted {
			t.Fatalf("expected pre-cut redacted long JWT, got %+v", diffs)
		}
	})

	t.Run("RedactDiffValue", func(t *testing.T) {
		red := ValueRedactor(func(s string) bool { return s == "ph-1" })
		if got := RedactDiffValue("", false, true, red); got != "" {
			t.Errorf("empty value under sensitive key must stay empty, got %q", got)
		}
		if got := RedactDiffValue("s3cr3t", false, true, red); got != ValueRedacted {
			t.Errorf("non-empty sensitive key got %q, want %q", got, ValueRedacted)
		}
		if got := RedactDiffValue("string: ph-1", true, false, red); got != "string: "+ValueRedacted {
			t.Errorf("type_changed noisy got %q", got)
		}
		if got := RedactDiffValue("string: "+jwt, true, false, red); got != "string: "+ValueRedacted {
			t.Errorf("type_changed JWT got %q", got)
		}
		if got := RedactDiffValue("object", true, false, red); got != "object" {
			t.Errorf("type_changed container got %q", got)
		}
		if got := RedactDiffValue("plain", false, false, red); got != "plain" {
			t.Errorf("benign got %q", got)
		}
	})
}
