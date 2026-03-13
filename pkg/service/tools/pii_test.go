package tools

import (
	"testing"
)

func TestValidateLuhn(t *testing.T) {
	tests := []struct {
		name   string
		number string
		want   bool
	}{
		// Valid card numbers (test cards)
		{"visa_valid", "4111111111111111", true},
		{"visa_valid_13", "4012888888881881", true},
		{"mastercard_valid", "5500000000000004", true},
		{"mastercard_2series", "2221000000000009", true},
		{"amex_valid", "378282246310005", true},
		{"discover_valid", "6011111111111117", true},
		{"diners_valid", "30569309025904", true},

		// Invalid card numbers
		{"visa_invalid_luhn", "4111111111111112", false},
		{"mastercard_invalid", "5500000000000005", false},
		{"too_short", "411111111111", false},
		{"too_long", "41111111111111111111", false},
		{"all_zeros", "0000000000000000", true}, // Luhn passes for all zeros mathematically
		{"non_numeric", "4111-1111-1111-1111", true}, // Cleaned to valid
		{"with_spaces", "4111 1111 1111 1111", true}, // Cleaned to valid

		// Edge cases
		{"empty", "", false},
		{"single_digit", "4", false},
		{"letters", "abcdefghijklmnop", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateLuhn(tt.number); got != tt.want {
				t.Errorf("ValidateLuhn(%q) = %v, want %v", tt.number, got, tt.want)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// Private IPs
		{"10.x.x.x", "10.0.0.1", true},
		{"172.16.x.x", "172.16.0.1", true},
		{"172.31.x.x", "172.31.255.255", true},
		{"192.168.x.x", "192.168.1.1", true},

		// Loopback
		{"localhost_ipv4", "127.0.0.1", true},
		{"localhost_ipv4_alt", "127.0.0.2", true},

		// Public IPs
		{"google_dns", "8.8.8.8", false},
		{"cloudflare", "1.1.1.1", false},
		{"random_public", "203.0.113.50", false},

		// Special
		{"unspecified", "0.0.0.0", true},
		{"broadcast", "255.255.255.255", false},

		// Invalid
		{"invalid", "not.an.ip", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPrivateIP(tt.ip); got != tt.want {
				t.Errorf("IsPrivateIP(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestPIIDetector_Detect_Email(t *testing.T) {
	detector := NewDefaultPIIDetector()

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"simple", "user@example.com", []string{"user@example.com"}},
		{"complex", "john.doe+test@sub.example.co.uk", []string{"john.doe+test@sub.example.co.uk"}},
		{"multiple", "a@b.com and c@d.org", []string{"a@b.com", "c@d.org"}},
		{"in_json", `{"email":"test@test.com"}`, []string{"test@test.com"}},
		{"none", "not an email at all", nil},
		{"partial", "missing@", nil},
		{"partial2", "@domain.com", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.Detect(tt.input)
			var found []string
			for _, f := range findings {
				if f.Type == PIIEmail {
					found = append(found, f.Value)
				}
			}

			if len(found) != len(tt.expected) {
				t.Errorf("Detect(%q) found %d emails, want %d", tt.input, len(found), len(tt.expected))
				return
			}

			for i, exp := range tt.expected {
				if i < len(found) && found[i] != exp {
					t.Errorf("Detect(%q)[%d] = %q, want %q", tt.input, i, found[i], exp)
				}
			}
		})
	}
}

func TestPIIDetector_Detect_CreditCard(t *testing.T) {
	// Test with ValidateCreditCards = true (Luhn validation enabled)
	detector := NewPIIDetector(PIIConfig{
		ValidateCreditCards: true,
	})

	tests := []struct {
		name     string
		input    string
		wantLen  int
		validate bool
	}{
		{"visa_valid", "Card: 4111111111111111", 1, true},
		{"visa_invalid_luhn", "Card: 4111111111111112", 0, false}, // Fails Luhn
		{"mastercard_valid", "MC: 5500000000000004", 1, true},
		{"amex_valid", "Amex: 378282246310005", 1, true},
		{"multiple", "Cards: 4111111111111111 and 5500000000000004", 2, true},
		{"no_cards", "No credit cards here", 0, false},
		{"partial", "4111111111", 0, false}, // Too short
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.Detect(tt.input)
			var cardFindings []PIIFinding
			for _, f := range findings {
				if f.Type == PIICreditCard {
					cardFindings = append(cardFindings, f)
				}
			}

			if len(cardFindings) != tt.wantLen {
				t.Errorf("Detect(%q) found %d cards, want %d", tt.input, len(cardFindings), tt.wantLen)
			}

			for _, f := range cardFindings {
				if tt.validate && !f.Validated {
					t.Errorf("Expected card %q to be validated", f.Value)
				}
			}
		})
	}
}

func TestPIIDetector_Detect_CreditCard_NoValidation(t *testing.T) {
	// Test with ValidateCreditCards = false (no Luhn validation)
	// All credit card patterns should be returned, even those failing Luhn
	detector := NewPIIDetector(PIIConfig{
		ValidateCreditCards: false,
	})

	tests := []struct {
		name      string
		input     string
		wantLen   int
		validated bool
	}{
		{"visa_valid", "Card: 4111111111111111", 1, false},
		{"visa_invalid_luhn", "Card: 4111111111111112", 1, false}, // Would fail Luhn but still detected
		{"no_cards", "No credit cards here", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.Detect(tt.input)
			var cardFindings []PIIFinding
			for _, f := range findings {
				if f.Type == PIICreditCard {
					cardFindings = append(cardFindings, f)
				}
			}

			if len(cardFindings) != tt.wantLen {
				t.Errorf("Detect(%q) found %d cards, want %d", tt.input, len(cardFindings), tt.wantLen)
			}

			// When validation is disabled, findings should have Validated=false
			for _, f := range cardFindings {
				if f.Validated != tt.validated {
					t.Errorf("Expected card %q Validated=%v, got %v", f.Value, tt.validated, f.Validated)
				}
			}
		})
	}
}

func TestPIIDetector_Detect_IP(t *testing.T) {
	// Test with private IPs excluded
	detector := NewPIIDetector(PIIConfig{
		ExcludePrivateIPs: true,
	})

	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		{"public_ip", "Client IP: 8.8.8.8", 1},
		{"private_ip", "Internal: 192.168.1.1", 0}, // Excluded
		{"localhost", "Localhost: 127.0.0.1", 0},   // Excluded
		{"multiple", "Public: 1.1.1.1, Private: 10.0.0.1", 1},
		{"no_ip", "No IPs here", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.Detect(tt.input)
			var ipFindings []PIIFinding
			for _, f := range findings {
				if f.Type == PIIIPv4 || f.Type == PIIIPv6 {
					ipFindings = append(ipFindings, f)
				}
			}

			if len(ipFindings) != tt.wantLen {
				t.Errorf("Detect(%q) found %d IPs, want %d", tt.input, len(ipFindings), tt.wantLen)
			}
		})
	}

	// Test with private IPs included
	detectorIncludePrivate := NewPIIDetector(PIIConfig{
		ExcludePrivateIPs: false,
	})

	findingsIncluded := detectorIncludePrivate.Detect("Private: 192.168.1.1")
	found := false
	for _, f := range findingsIncluded {
		if f.Type == PIIIPv4 && f.Value == "192.168.1.1" {
			found = true
		}
	}
	if !found {
		t.Error("Expected to find private IP when ExcludePrivateIPs is false")
	}
}

func TestPIIDetector_Detect_Phone(t *testing.T) {
	detector := NewDefaultPIIDetector()

	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		{"us_format", "Call: (555) 123-4567", 1},
		{"us_dashes", "Phone: 555-123-4567", 1},
		{"us_dots", "Tel: 555.123.4567", 1},
		{"us_with_1", "Number: 1-555-123-4567", 1},
		{"intl", "International: +14155551234", 1},
		{"no_phone", "No phone here", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.Detect(tt.input)
			var phoneFindings []PIIFinding
			for _, f := range findings {
				if f.Type == PIIPhone {
					phoneFindings = append(phoneFindings, f)
				}
			}

			if len(phoneFindings) != tt.wantLen {
				t.Errorf("Detect(%q) found %d phones, want %d", tt.input, len(phoneFindings), tt.wantLen)
			}
		})
	}
}

func TestPIIDetector_Detect_SSN(t *testing.T) {
	detector := NewDefaultPIIDetector()

	// Note: SSN regex is permissive and matches SSN-like 9-digit patterns with separators.
	// IRS-specific invalid patterns (000, 666, 9xx start, etc.) are rejected in validateFinding post-match.
	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		// Valid SSN formats that should be detected
		{"valid_with_dashes", "SSN: 123-45-6789", 1},
		{"valid_with_spaces", "Social: 123 45 6789", 1},
		{"valid_no_separator", "Number: 123456789", 1},
		{"valid_mixed", "ID: 456-78-9012", 1},
		// Invalid SSN formats per IRS rules (rejected post-match)
		{"invalid_start_000", "Invalid: 000-12-3456", 0}, // 000 is invalid per IRS
		{"invalid_start_666", "Invalid: 666-12-3456", 0}, // 666 is invalid per IRS
		{"invalid_start_9xx", "Invalid: 900-12-3456", 0}, // 9xx is invalid per IRS
		{"invalid_group_00", "Invalid: 123-00-4567", 0},  // 00 group is invalid
		{"invalid_serial_0000", "Invalid: 123-45-0000", 0}, // 0000 serial is invalid
		// Edge cases
		{"no_ssn", "No SSN here", 0},
		{"too_short", "Short: 12-34-567", 0}, // Only 8 digits
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.Detect(tt.input)
			var ssnFindings []PIIFinding
			for _, f := range findings {
				if f.Type == PIISSN {
					ssnFindings = append(ssnFindings, f)
				}
			}

			if len(ssnFindings) != tt.wantLen {
				t.Errorf("Detect(%q) found %d SSNs, want %d", tt.input, len(ssnFindings), tt.wantLen)
			}
		})
	}
}

func TestPIIDetector_DetectInField(t *testing.T) {
	detector := NewPIIDetector(PIIConfig{
		SensitiveFields: []string{"password", "secret_data"},
		ExcludeFields:   []string{"public_id"},
	})

	tests := []struct {
		name      string
		fieldName string
		value     string
		wantLen   int
		wantType  PIIType
	}{
		{"sensitive_custom", "password", "my_secret_value", 1, PIICustom},
		{"sensitive_custom2", "Secret_Data", "any value here", 1, PIICustom}, // Case insensitive
		{"excluded_field", "public_id", "test@example.com", 0, ""},           // Excluded
		{"normal_field", "email", "test@example.com", 1, PIIEmail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := detector.DetectInField(tt.fieldName, tt.value)

			if len(findings) != tt.wantLen {
				t.Errorf("DetectInField(%q, %q) found %d, want %d", tt.fieldName, tt.value, len(findings), tt.wantLen)
				return
			}

			if tt.wantLen > 0 && findings[0].Type != tt.wantType {
				t.Errorf("DetectInField(%q, %q) type = %v, want %v", tt.fieldName, tt.value, findings[0].Type, tt.wantType)
			}
		})
	}
}

func TestPIIDetector_IsSensitiveField(t *testing.T) {
	detector := NewPIIDetector(PIIConfig{
		SensitiveFields: []string{"my_custom_field"},
	})

	tests := []struct {
		name      string
		fieldName string
		want      bool
	}{
		{"password", "password", true},
		{"Password_upper", "PASSWORD", true},
		{"password_confirm", "password_confirm", true},
		{"credit_card", "credit_card_number", true},
		{"token", "auth_token", true},
		{"ssn", "ssn", true},
		{"custom", "my_custom_field", true},
		{"not_sensitive", "username", false},
		{"not_sensitive2", "id", false},
		{"not_sensitive3", "created_at", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detector.IsSensitiveField(tt.fieldName); got != tt.want {
				t.Errorf("IsSensitiveField(%q) = %v, want %v", tt.fieldName, got, tt.want)
			}
		})
	}
}

func TestGetPIITypeFromRuleID(t *testing.T) {
	tests := []struct {
		ruleID string
		want   PIIType
	}{
		{"pii-email", PIIEmail},
		{"pii-phone-us", PIIPhone},
		{"pii-phone-intl", PIIPhone},
		{"pii-ipv4", PIIIPv4},
		{"pii-ipv6", PIIIPv6},
		{"pii-credit-card-visa", PIICreditCard},
		{"pii-ssn", PIISSN},
		{"pii-passport", PIIPassport},
		{"pii-dob-labeled", PIIDOB},
		{"pii-name-field", PIIName},
		{"pii-us-zipcode", PIIZipCode},
		{"some-other-rule", ""},
		{"jwt", ""},
	}

	for _, tt := range tests {
		t.Run(tt.ruleID, func(t *testing.T) {
			if got := GetPIITypeFromRuleID(tt.ruleID); got != tt.want {
				t.Errorf("GetPIITypeFromRuleID(%q) = %v, want %v", tt.ruleID, got, tt.want)
			}
		})
	}
}

func TestIsPIIRule(t *testing.T) {
	tests := []struct {
		ruleID string
		want   bool
	}{
		{"pii-email", true},
		{"pii-credit-card-visa", true},
		{"PII-SSN", true}, // Case insensitive
		{"jwt", false},
		{"aws-access-key", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.ruleID, func(t *testing.T) {
			if got := IsPIIRule(tt.ruleID); got != tt.want {
				t.Errorf("IsPIIRule(%q) = %v, want %v", tt.ruleID, got, tt.want)
			}
		})
	}
}

func TestNewPIIDetector_Categories(t *testing.T) {
	// Only enable email detection
	detector := NewPIIDetector(PIIConfig{
		EnabledCategories: []string{"email"},
	})

	input := "Email: test@test.com, Phone: 555-123-4567, IP: 8.8.8.8"
	findings := detector.Detect(input)

	// Should only find email
	for _, f := range findings {
		if f.Type != PIIEmail {
			t.Errorf("Unexpected PII type %v found when only email enabled", f.Type)
		}
	}

	hasEmail := false
	for _, f := range findings {
		if f.Type == PIIEmail {
			hasEmail = true
		}
	}
	if !hasEmail {
		t.Error("Expected to find email when email category is enabled")
	}
}

// Benchmark tests
func BenchmarkPIIDetector_Detect(b *testing.B) {
	detector := NewDefaultPIIDetector()
	input := `{
		"email": "john.doe@example.com",
		"phone": "+1-555-123-4567",
		"ip": "192.168.1.100",
		"card": "4111111111111111",
		"ssn": "123-45-6789"
	}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = detector.Detect(input)
	}
}

func BenchmarkValidateLuhn(b *testing.B) {
	number := "4111111111111111"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateLuhn(number)
	}
}
