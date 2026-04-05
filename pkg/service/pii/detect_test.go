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

func hasPattern(detections []Detection, pattern string) bool {
	for _, d := range detections {
		if d.PatternType == pattern {
			return true
		}
	}
	return false
}
