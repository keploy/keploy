package models

// The defect these tests exist for: AgentResp.Error used to be an `error`,
// which is an interface, so no failure reply ever survived the wire. The type
// made the bug UNTESTABLE — you cannot write "the message survives" against a
// field that cannot hold a message after a round trip — which is why it lived
// through several attempted fixes in the handlers.
//
// TestAgentResp_ErrorMessageSurvivesRoundTrip is the test that would have
// caught it. TestAgentResp_WireCompatAcrossVersions pins the CLI/agent skew
// behaviour that dictated the chosen shape (JSON key "error" + omitempty, Go
// field renamed to ErrorMsg).

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const wantMsg = "proxy failed to start: address already in use"

// TestAgentResp_ErrorMessageSurvivesRoundTrip is the regression test: a non-nil
// error must survive marshal+unmarshal with its MESSAGE intact, on BOTH wire
// formats this type travels over (JSON for /mock and /updatemockparams, gob for
// /storemocks).
func TestAgentResp_ErrorMessageSurvivesRoundTrip(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		b, err := json.Marshal(AgentResp{ErrorMsg: wantMsg})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(b), wantMsg) {
			t.Fatalf("message destroyed by marshal: %s", b)
		}
		var got AgentResp
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.ErrorMsg != wantMsg {
			t.Fatalf("ErrorMsg = %q, want %q", got.ErrorMsg, wantMsg)
		}
		if got.IsSuccess {
			t.Fatal("IsSuccess must be false when an error is carried")
		}
		if got.Err() == nil || got.Err().Error() != wantMsg {
			t.Fatalf("Err() = %v, want an error reading %q", got.Err(), wantMsg)
		}
	})

	t.Run("gob", func(t *testing.T) {
		var buf bytes.Buffer
		// The old type failed HERE, not at decode: gob refuses to encode an
		// unregistered interface, the handler discarded that error, and the
		// body went out empty.
		if err := gob.NewEncoder(&buf).Encode(AgentResp{ErrorMsg: wantMsg}); err != nil {
			t.Fatalf("gob encode: %v", err)
		}
		var got AgentResp
		if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
			t.Fatalf("gob decode: %v", err)
		}
		if got.ErrorMsg != wantMsg {
			t.Fatalf("ErrorMsg = %q, want %q", got.ErrorMsg, wantMsg)
		}
	})

	t.Run("success round-trips as success", func(t *testing.T) {
		b, _ := json.Marshal(AgentResp{IsSuccess: true})
		var got AgentResp
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !got.IsSuccess || got.Err() != nil {
			t.Fatalf("got %+v, want success with no error", got)
		}
	})
}

// legacyAgentResp is the shape shipped in keploy <= v3.6.60. Kept verbatim so
// the compatibility claims below are tested rather than asserted in a comment.
type legacyAgentResp struct {
	Error     error `json:"error"`
	IsSuccess bool  `json:"isSuccess"`
}

// TestAgentResp_WireCompatAcrossVersions pins the CLI/agent version-skew
// matrix. The CLI and the agent are released separately and are routinely
// mismatched during a rolling upgrade, so each direction is a real deployment.
func TestAgentResp_WireCompatAcrossVersions(t *testing.T) {
	// --- JSON: new agent -> OLD cli ---
	// The success path MUST still decode, or upgrading the agent breaks every
	// older CLI. This is what `omitempty` buys: the "error" key is absent, so
	// the old interface field is simply left nil. Drop omitempty and this test
	// fails with "cannot unmarshal string into ... of type error".
	t.Run("json new agent success decodes on old CLI", func(t *testing.T) {
		b, _ := json.Marshal(AgentResp{IsSuccess: true})
		if strings.Contains(string(b), `"error"`) {
			t.Fatalf("success reply must omit the error key entirely, got %s", b)
		}
		var old legacyAgentResp
		if err := json.Unmarshal(b, &old); err != nil {
			t.Fatalf("old CLI cannot decode a new agent's success reply: %v", err)
		}
		if !old.IsSuccess {
			t.Fatal("old CLI lost IsSuccess")
		}
	})

	// --- JSON: OLD agent -> new cli ---
	// The direction that actually happens: the CLI ships first and the agent
	// image trails it. The legacy encoded-interface forms must not fail the
	// decode, or the new CLI reports a JSON error in place of the agent's
	// failure — the very bug being fixed, just moved.
	t.Run("new CLI tolerates legacy error encodings", func(t *testing.T) {
		for _, body := range []string{
			`{"error":null,"isSuccess":true}`,          // legacy success
			`{"error":{},"isSuccess":false}`,           // legacy failure, message already gone
			`{"error":{"Offset":2},"isSuccess":false}`, // legacy *json.SyntaxError
		} {
			var got AgentResp
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("new CLI failed to decode legacy body %s: %v", body, err)
			}
			if want := strings.Contains(body, "true"); got.IsSuccess != want {
				t.Fatalf("body %s: IsSuccess = %v, want %v", body, got.IsSuccess, want)
			}
		}
	})

	// --- gob: new agent -> OLD cli ---
	// This is why the Go field is ErrorMsg and not Error. gob records field
	// NAMES and TYPES in its type descriptor and rejects a field whose type
	// changed, so reusing the name `Error` would break /storemocks for every
	// mismatched pair — on success as well as failure. A field the old decoder
	// does not know is ignored instead.
	t.Run("gob new agent decodes on old CLI", func(t *testing.T) {
		for _, v := range []AgentResp{{IsSuccess: true}, {ErrorMsg: wantMsg}} {
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(v); err != nil {
				t.Fatalf("encode %+v: %v", v, err)
			}
			var old legacyAgentResp
			if err := gob.NewDecoder(&buf).Decode(&old); err != nil {
				t.Fatalf("old CLI cannot gob-decode a new agent's %+v reply: %v", v, err)
			}
			if old.IsSuccess != v.IsSuccess {
				t.Fatalf("old CLI lost IsSuccess for %+v", v)
			}
		}
	})

	// --- gob: OLD agent -> new cli ---
	// A new CLI must still read an old agent's success reply, so a CLI upgrade
	// alone cannot break /storemocks.
	t.Run("new CLI gob-decodes an old agent success", func(t *testing.T) {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(legacyAgentResp{IsSuccess: true}); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var got AgentResp
		if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
			t.Fatalf("new CLI cannot decode an old agent's success reply: %v", err)
		}
		if !got.IsSuccess {
			t.Fatal("new CLI lost IsSuccess")
		}
	})

	// Documents, rather than laments, the one cell that cannot be fixed from
	// here: an OLD agent's FAILURE never carried a message in the first place
	// (its gob encode aborted on the interface field and wrote nothing), so a
	// new CLI has nothing to recover and must fall back to the HTTP status.
	t.Run("old agent failure carried no message to recover", func(t *testing.T) {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(legacyAgentResp{Error: errors.New(wantMsg)}); err == nil {
			t.Fatal("expected the legacy encode to fail; if it now succeeds the premise changed")
		}
		if buf.Len() > 0 {
			var got AgentResp
			if err := gob.NewDecoder(&buf).Decode(&got); err == nil && got.ErrorMsg != "" {
				t.Fatalf("unexpectedly recovered %q from a legacy failure", got.ErrorMsg)
			}
		}
	})
}
