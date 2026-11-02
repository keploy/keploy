package models

import (
	"reflect"
	"testing"
)

// A deep copy of an authored mock shares nothing of its `authored:` block:
// writing the copy's reason, delta body, headers or dropped headers leaves
// the original as it was. A recorded mock's copy has no block.
func TestDeepCopySharesNoAuthoredBlock(t *testing.T) {
	body := `{"error":"card_declined"}`
	wantBody := body // the original's body points at body: compare with a copy
	m := &Mock{Kind: HTTP, Spec: MockSpec{Authored: &Authored{
		How: AuthoredDerived, Reason: "declined card", Source: "TestCheckout/mock-7", Request: "sha256:0f1e",
		Delta: &AuthoredDelta{Status: 402, Body: &body, JSONPatch: `[]`,
			Header: map[string]string{"X-Decline-Code": "insufficient_funds"}, DropHeader: []string{"Etag"}},
	}}}
	want := *m.Spec.Authored
	wantDelta := *m.Spec.Authored.Delta
	wantDelta.Header = map[string]string{"X-Decline-Code": "insufficient_funds"}
	wantDelta.DropHeader = []string{"Etag"}

	c := m.DeepCopy()
	if !reflect.DeepEqual(c.Spec.Authored, m.Spec.Authored) {
		t.Fatalf("copy %+v, want %+v", c.Spec.Authored, m.Spec.Authored)
	}
	c.Spec.Authored.Reason = "edited"
	c.Spec.Authored.Delta.Status = 500
	*c.Spec.Authored.Delta.Body = "edited"
	c.Spec.Authored.Delta.Header["X-Decline-Code"] = "edited"
	c.Spec.Authored.Delta.DropHeader[0] = "edited"

	if m.Spec.Authored.Reason != want.Reason || m.Spec.Authored.Delta.Status != wantDelta.Status {
		t.Fatalf("writing the copy wrote the original: %+v", m.Spec.Authored)
	}
	if *m.Spec.Authored.Delta.Body != wantBody {
		t.Fatalf("writing the copy's body wrote the original's: %q", *m.Spec.Authored.Delta.Body)
	}
	if !reflect.DeepEqual(m.Spec.Authored.Delta.Header, wantDelta.Header) || !reflect.DeepEqual(m.Spec.Authored.Delta.DropHeader, wantDelta.DropHeader) {
		t.Fatalf("writing the copy's headers wrote the original's: %+v", m.Spec.Authored.Delta)
	}

	if (&Mock{Kind: HTTP}).DeepCopy().Spec.Authored != nil {
		t.Fatal("a recorded mock's copy has an authored block")
	}
	var nilAuthored *Authored
	if nilAuthored.Clone() != nil {
		t.Fatal("Clone of nil is not nil")
	}
}
