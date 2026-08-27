package models

import (
	"encoding/json"
	"testing"
)

// A runner that predates the attempt field sends no "attempt" key. It must
// decode to 0 — the first-run value — so an old fixture keeps the exact
// behaviour it had before the field existed.
func TestScopeReq_AbsentAttemptIsFirstRun(t *testing.T) {
	var req ScopeReq
	if err := json.Unmarshal([]byte(`{"name":"t1"}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Attempt != 0 {
		t.Fatalf("an absent attempt must decode to 0 (first run), got %d", req.Attempt)
	}

	// And a runner that does send it round-trips.
	if err := json.Unmarshal([]byte(`{"name":"t1","attempt":2}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Attempt != 2 {
		t.Fatalf("attempt must decode verbatim, got %d", req.Attempt)
	}
}

// The compatibility gate for the acknowledgement: on every non-retry call the
// three retry fields are zero, and all three are omitempty, so the JSON is
// byte-identical to the pre-retry contract. A fixture asserting on the ack of a
// normal test sees no change at all.
func TestScopeAck_NonRetryJSONIsUnchanged(t *testing.T) {
	got, err := json.Marshal(ScopeAck{Status: "ok", Scoped: true, Mocks: 3, Reason: ScopeReasonPoolRestricted})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"status":"ok","scoped":true,"mocks":3,"reason":"pool_restricted"}`
	if string(got) != want {
		t.Fatalf("non-retry ack must be byte-identical to the old contract:\n got: %s\nwant: %s", got, want)
	}
}

// A retry ack carries the three new fields so a fixture can log that the reset
// happened and how many mocks came back.
func TestScopeAck_RetryJSONCarriesTheReset(t *testing.T) {
	got, err := json.Marshal(ScopeAck{
		Status: "ok", Scoped: true, Mocks: 2, Reason: ScopeReasonPoolRestricted,
		Attempt: 1, RetryReset: true, RestoredMocks: 2,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"status":"ok","scoped":true,"mocks":2,"reason":"pool_restricted","attempt":1,"retry_reset":true,"restored_mocks":2}`
	if string(got) != want {
		t.Fatalf("retry ack shape changed:\n got: %s\nwant: %s", got, want)
	}
}
