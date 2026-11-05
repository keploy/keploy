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
			Header: map[string]string{"X-Decline-Code": "insufficient_funds"}, DropHeader: []string{"Etag"},
			Fault: &Fault{Kind: FaultTruncate, BodyBytes: 4}},
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
	c.Spec.Authored.Delta.Fault.BodyBytes = 9

	if m.Spec.Authored.Reason != want.Reason || m.Spec.Authored.Delta.Status != wantDelta.Status {
		t.Fatalf("writing the copy wrote the original: %+v", m.Spec.Authored)
	}
	if *m.Spec.Authored.Delta.Body != wantBody {
		t.Fatalf("writing the copy's body wrote the original's: %q", *m.Spec.Authored.Delta.Body)
	}
	if !reflect.DeepEqual(m.Spec.Authored.Delta.Header, wantDelta.Header) || !reflect.DeepEqual(m.Spec.Authored.Delta.DropHeader, wantDelta.DropHeader) {
		t.Fatalf("writing the copy's headers wrote the original's: %+v", m.Spec.Authored.Delta)
	}

	if m.Spec.Authored.Delta.Fault.BodyBytes != 4 {
		t.Fatalf("writing the copy's fault wrote the original's: %+v", m.Spec.Authored.Delta.Fault)
	}

	if (&Mock{Kind: HTTP}).DeepCopy().Spec.Authored != nil {
		t.Fatal("a recorded mock's copy has an authored block")
	}
	var nilAuthored *Authored
	if nilAuthored.Clone() != nil {
		t.Fatal("Clone of nil is not nil")
	}
}

// A replay summary names a fault an authored mock served "as authored", and
// one a recorded mock served "as recorded", also once the mock's payload is
// left on disk (LeavePayloadOnDisk), as keploy test reads a set's mocks: a
// reader never takes an answer someone wrote for one the upstream gave.
func TestAFaultAnAuthoredMockServedIsNamedAsAuthored(t *testing.T) {
	authored := &Authored{How: AuthoredGenerated, Reason: "provider times out"}
	var none *Mock
	if none.IsAuthored() {
		t.Fatal("a nil mock is authored")
	}
	for _, c := range []struct {
		authored *Authored
		want     string
	}{
		{nil, "mock-4: no answer until the application closed, as recorded"},
		{authored, "mock-4: no answer until the application closed, as authored"},
	} {
		m := &Mock{Name: "mock-4", Kind: HTTP, Spec: MockSpec{Fault: &Fault{Kind: FaultTimeout}, Authored: c.authored}}
		if got := m.IsAuthored(); got != (c.authored != nil) {
			t.Errorf("IsAuthored() = %v with block %+v", got, c.authored)
		}
		if got := m.FaultServed(); got != c.want {
			t.Errorf("FaultServed() = %q, want %q", got, c.want)
		}

		recorded := payloadMocks()
		d := recorded["http"]()
		d.Name = "mock-4"
		d.Spec.Fault = &Fault{Kind: FaultTimeout}
		d.Spec.Authored = c.authored.Clone()
		d.DeriveLifetime()
		d.LeavePayloadOnDisk(&fileSource{recorded: recorded}, PayloadAt{Name: "http"})
		if !d.PayloadOnDisk() || d.Spec.Authored != nil {
			t.Fatalf("the payload stayed in memory: on disk %v, Spec.Authored %+v", d.PayloadOnDisk(), d.Spec.Authored)
		}
		if got := d.IsAuthored(); got != (c.authored != nil) {
			t.Errorf("on disk: IsAuthored() = %v with block %+v", got, c.authored)
		}
		if got := d.FaultServed(); got != c.want {
			t.Errorf("on disk: FaultServed() = %q, want %q", got, c.want)
		}
	}
}

// A derived mock replaces, in its own test, the recording its Source names:
// the mock part of "<test>/<mock>", whatever the test part holds, also once
// its payload is left on disk. A generated or a recorded mock, and a Source
// naming no mock, replace nothing; ReplacedBy collects what a test's own
// replace, and WithoutReplaced drops exactly those, in order.
func TestADerivedMockReplacesTheRecordingItsSourceNames(t *testing.T) {
	for _, c := range []struct {
		authored *Authored
		want     string
	}{
		{nil, ""},
		{&Authored{How: AuthoredDerived, Source: "pkg/a::TestCheckout/mock-7"}, "mock-7"},
		{&Authored{How: AuthoredDerived, Source: "startup:/mock-0"}, "mock-0"},
		{&Authored{How: AuthoredDerived, Source: "mock-7"}, ""},
		{&Authored{How: AuthoredGenerated, Source: "TestCheckout/mock-7"}, ""},
	} {
		m := &Mock{Name: "mock-9", Kind: HTTP, Spec: MockSpec{Authored: c.authored}}
		if got := m.Replaces(); got != c.want {
			t.Errorf("Replaces() = %q with block %+v, want %q", got, c.authored, c.want)
		}
		recorded := payloadMocks()
		d := recorded["http"]()
		d.Name = "mock-9"
		d.Spec.Authored = c.authored.Clone()
		d.DeriveLifetime()
		d.LeavePayloadOnDisk(&fileSource{recorded: recorded}, PayloadAt{Name: "http"})
		if !d.PayloadOnDisk() || d.Spec.Authored != nil {
			t.Fatalf("the payload stayed in memory: on disk %v, Spec.Authored %+v", d.PayloadOnDisk(), d.Spec.Authored)
		}
		if got := d.Replaces(); got != c.want {
			t.Errorf("on disk: Replaces() = %q with block %+v, want %q", got, c.authored, c.want)
		}
	}
	var none *Mock
	if none.Replaces() != "" {
		t.Fatal("a nil mock replaces a recording")
	}

	src, other, kept := &Mock{Name: "mock-1"}, &Mock{Name: "mock-2"}, &Mock{Name: "mock-3"}
	derived := &Mock{Name: "mock-5", Spec: MockSpec{Authored: &Authored{How: AuthoredDerived, Source: "TestA/mock-1"}}}
	if got := ReplacedBy([]*Mock{kept, nil}); got != nil {
		t.Fatalf("ReplacedBy of no derived mock = %v, want nil", got)
	}
	replaced := ReplacedBy([]*Mock{kept, derived})
	if len(replaced) != 1 {
		t.Fatalf("ReplacedBy = %v, want mock-1 alone", replaced)
	}
	if _, ok := replaced["mock-1"]; !ok {
		t.Fatalf("ReplacedBy = %v, want mock-1", replaced)
	}
	got := withoutReplacedOrFail(t, []*Mock{other, src, nil, kept}, replaced)
	if len(got) != 3 || got[0] != other || got[1] != nil || got[2] != kept {
		t.Fatalf("WithoutReplaced = %v, want mock-2, nil, mock-3", got)
	}
	pool := []*Mock{src, kept}
	if got := WithoutReplaced(pool, nil); len(got) != 2 || &got[0] != &pool[0] {
		t.Fatal("WithoutReplaced with nothing replaced is not the pool itself")
	}
}

// withoutReplacedOrFail is WithoutReplaced, failing t, not the run, when it
// panics (a nil entry of mocks must be kept, not read).
func withoutReplacedOrFail(t *testing.T, mocks []*Mock, replaced map[string]struct{}) (out []*Mock) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithoutReplaced did not keep a nil entry: %v", r)
		}
	}()
	return WithoutReplaced(mocks, replaced)
}
