package models

import "testing"

func TestStampRecordStartIsReadBackFromAnyPool(t *testing.T) {
	unstamped := &Mock{}
	stamped := &Mock{Spec: MockSpec{Metadata: map[string]string{"type": "http-client"}}}
	StampRecordStart(stamped, "2026-10-12T12:29:14.123Z")
	if got := stamped.Spec.Metadata["type"]; got != "http-client" {
		t.Fatalf("stamping replaced the mock's other metadata: type=%q", got)
	}
	if got := RecordStartOf([]*Mock{nil, unstamped}, []*Mock{stamped}); got != "2026-10-12T12:29:14.123Z" {
		t.Fatalf("RecordStartOf = %q, want the stamped start", got)
	}
}

func TestRecordStartOfAnUnstampedRecordingIsEmpty(t *testing.T) {
	m := &Mock{}
	StampRecordStart(m, "")
	if m.Spec.Metadata != nil {
		t.Fatalf("an empty start must leave the mock unchanged, got %v", m.Spec.Metadata)
	}
	if got := RecordStartOf([]*Mock{m}, nil); got != "" {
		t.Fatalf("RecordStartOf = %q, want \"\"", got)
	}
}
