package pii

import "testing"

func TestDetectValueEmail(t *testing.T) {
	detections := DetectValue("request.body.email", "user@example.com")
	if !hasPattern(detections, PatternEmail) {
		t.Fatalf("expected %q detection, got %+v", PatternEmail, detections)
	}
}

func TestDetectValueInvalidEmail(t *testing.T) {
	detections := DetectValue("request.body.message", "user@@example")
	if hasPattern(detections, PatternEmail) {
		t.Fatalf("did not expect %q detection, got %+v", PatternEmail, detections)
	}
}

func TestDetectValuePhone(t *testing.T) {
	detections := DetectValue("request.body.phone", "+1 (415) 555-2671")
	if !hasPattern(detections, PatternPhone) {
		t.Fatalf("expected %q detection, got %+v", PatternPhone, detections)
	}
}

func TestDetectValueCreditCardLuhn(t *testing.T) {
	valid := DetectValue("request.body.card", "4111 1111 1111 1111")
	if !hasPattern(valid, PatternCreditCard) {
		t.Fatalf("expected %q detection for valid card, got %+v", PatternCreditCard, valid)
	}

	invalid := DetectValue("request.body.card", "4111 1111 1111 1112")
	if hasPattern(invalid, PatternCreditCard) {
		t.Fatalf("did not expect %q detection for invalid card, got %+v", PatternCreditCard, invalid)
	}
}

func TestDetectValueSSN(t *testing.T) {
	detections := DetectValue("request.body.identity", "123-45-6789")
	if !hasPattern(detections, PatternSSN) {
		t.Fatalf("expected %q detection, got %+v", PatternSSN, detections)
	}
}

func TestDetectValueFieldName(t *testing.T) {
	detections := DetectValue("request.body.password", "not-a-secret")
	if !hasPattern(detections, PatternFieldName) {
		t.Fatalf("expected %q detection, got %+v", PatternFieldName, detections)
	}
}

func TestDetectNoFalsePositives(t *testing.T) {
	detections := DetectBody(`{"username":"john_doe_123","note":"hello world","amount":"42"}`, "request.body")
	if len(detections) != 0 {
		t.Fatalf("expected no detections, got %+v", detections)
	}
}

func TestDetectBodyJSONNumbers(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		pattern string
	}{
		{"card", `{"payment":{"pan":4111111111111111}}`, PatternCreditCard},
		{"phone with country code", `{"contact":14155552671}`, PatternPhone},
		{"card beyond float64 precision", `{"pan":5555555555554444}`, PatternCreditCard},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detections := DetectBody(tt.body, "response.body")
			if !hasPattern(detections, tt.pattern) {
				t.Fatalf("expected %q detection, got %+v", tt.pattern, detections)
			}
		})
	}
}

func TestDetectBodyPlainNumbersStayQuiet(t *testing.T) {
	detections := DetectBody(`{"amount":42,"price":19.99,"id":4111111111,"createdAt":1793476229000}`, "response.body")
	if len(detections) != 0 {
		t.Fatalf("expected no detections, got %+v", detections)
	}
}

func TestDetectBodyTextAfterJSONIsScanned(t *testing.T) {
	detections := DetectBody(`{"ok":true} contact user@example.com`, "request.body")
	if !hasPattern(detections, PatternEmail) {
		t.Fatalf("expected %q detection, got %+v", PatternEmail, detections)
	}
}

func TestIsPIIFieldName(t *testing.T) {
	tests := []struct {
		field string
		want  bool
	}{
		{"request.body.creditCard", true},
		{"request.body.credit_card", true},
		{"request.body.credit-card", true},
		{"request.body.user.socialSecurity", true},
		{"request.body.dateOfBirth", true},
		{"request.body.items[0].date_of_birth", true},
		{"request.header.X-SSN", true},
		{"request.body.userSSNValue", true},
		{"request.body.SSN", true},
		{"response.body.CREDIT_CARD", true},
		{"request.body.password", true},
		{"request.body.creditLimit", false},
		{"request.body.cardType", false},
		{"request.body.dateOfPurchase", false},
		{"request.body.tokens", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if got := isPIIFieldName(tt.field); got != tt.want {
				t.Fatalf("isPIIFieldName(%q) = %v, want %v (parts %v)", tt.field, got, tt.want, splitFieldName(tt.field))
			}
		})
	}
}

func hasPattern(detections []Detection, pattern string) bool {
	for _, d := range detections {
		if d.PatternType == pattern {
			return true
		}
	}
	return false
}
