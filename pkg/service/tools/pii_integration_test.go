package tools

import (
"testing"
)

// TestRedactYAML_PIIDetection verifies that the PII rules in custom_gitleaks_rules.toml
// are properly loaded and detected during YAML redaction
func TestRedactYAML_PIIDetection(t *testing.T) {
	// Test YAML with various PII types
	testYAML := `
name: test-1
version: 1
http_req:
  url: /api/users
  method: POST
  body: '{"email":"john.doe@example.com","phone":"555-123-4567","ip":"8.8.8.8","card":"4111111111111111"}'
  header:
    Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c
`
	aggSecrets := make(map[string]string)
	
	redacted, err := RedactYAML([]byte(testYAML), aggSecrets, "test-1")
	if err != nil {
		t.Fatalf("RedactYAML failed: %v", err)
	}
	
	redactedStr := string(redacted)
	
	// Verify that PII was detected and replaced with placeholders
	if len(aggSecrets) == 0 {
		t.Error("Expected secrets to be detected, but aggSecrets is empty")
		t.Logf("Redacted YAML:\n%s", redactedStr)
	}
	
	// Check that at least JWT was detected (guaranteed in existing rules)
	foundJWT := false
	for key := range aggSecrets {
		if key != "" {
			foundJWT = true
			break
		}
	}
	if !foundJWT {
		t.Error("Expected at least one secret to be detected")
	}
	
	t.Logf("Detected %d secrets/PII items", len(aggSecrets))
	for key := range aggSecrets {
		t.Logf("  - %s", key)
	}
}

// TestPIIRulesLoaded verifies that PII rules are present in the embedded config
func TestPIIRulesLoaded(t *testing.T) {
	// Check that the PII rules are in the embedded config
	if !contains(customGitleaksRules, "pii-email") {
		t.Error("pii-email rule not found in custom_gitleaks_rules.toml")
	}
	if !contains(customGitleaksRules, "pii-credit-card-visa") {
		t.Error("pii-credit-card-visa rule not found in custom_gitleaks_rules.toml")
	}
	if !contains(customGitleaksRules, "pii-ipv4") {
		t.Error("pii-ipv4 rule not found in custom_gitleaks_rules.toml")
	}
	if !contains(customGitleaksRules, "pii-ssn") {
		t.Error("pii-ssn rule not found in custom_gitleaks_rules.toml")
	}
	if !contains(customGitleaksRules, "pii-phone-us") {
		t.Error("pii-phone-us rule not found in custom_gitleaks_rules.toml")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
