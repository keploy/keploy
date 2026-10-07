package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The time is optional on the wire: a fixture that never heard of it, and an old agent, must not notice it.
func TestScopeReqTimeIsOptionalOnTheWire(t *testing.T) {
	b, err := json.Marshal(ScopeReq{Name: "t", Pid: 7})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"at"`) {
		t.Fatalf("a zero time was written: %s", b)
	}

	var old ScopeReq
	if err := json.Unmarshal([]byte(`{"name":"t","pid":7}`), &old); err != nil {
		t.Fatal(err)
	}
	if !old.At.IsZero() {
		t.Fatalf("a body without at decoded to %v", old.At)
	}

	var timed ScopeReq
	if err := json.Unmarshal([]byte(`{"name":"t","at":"2026-09-24T10:00:00.000123456Z"}`), &timed); err != nil {
		t.Fatal(err)
	}
	if !timed.At.Equal(time.Date(2026, 9, 24, 10, 0, 0, 123456, time.UTC)) {
		t.Fatalf("at decoded to %v", timed.At)
	}
}
