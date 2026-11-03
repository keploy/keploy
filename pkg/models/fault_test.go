package models

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// Each fault the format has, spelled the one way it has for it, validates;
// every other spelling is refused with the reason named.
func TestFaultValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Fault
		want string // "" valid; else a part of the error
	}{
		{"reset before any byte", Fault{Kind: FaultReset}, ""},
		{"reset after the head", Fault{Kind: FaultReset, AfterHead: true, BodyBytes: 12}, ""},
		{"reset after the head, no body", Fault{Kind: FaultReset, AfterHead: true}, ""},
		{"eof", Fault{Kind: FaultEOF}, ""},
		{"timeout", Fault{Kind: FaultTimeout}, ""},
		{"timeout after the head", Fault{Kind: FaultTimeout, AfterHead: true, BodyBytes: 3}, ""},
		{"truncate", Fault{Kind: FaultTruncate, BodyBytes: 10}, ""},
		{"truncate after no body bytes", Fault{Kind: FaultTruncate}, ""},
		{"delay", Fault{Kind: FaultDelay, Delay: 3 * time.Second}, ""},

		{"no kind", Fault{}, "no kind"},
		{"unknown kind", Fault{Kind: "tls_alert"}, "upgrade keploy"},
		{"eof after the head", Fault{Kind: FaultEOF, AfterHead: true}, "write kind truncate"},
		{"eof with body bytes", Fault{Kind: FaultEOF, BodyBytes: 4}, "set after_head"},
		{"truncate with after_head", Fault{Kind: FaultTruncate, AfterHead: true, BodyBytes: 1}, "leave after_head out"},
		{"delay of zero", Fault{Kind: FaultDelay}, "positive duration"},
		{"negative delay", Fault{Kind: FaultDelay, Delay: -time.Second}, "positive duration"},
		{"delay after the head", Fault{Kind: FaultDelay, Delay: time.Second, AfterHead: true}, "whole answer"},
		{"delay with body bytes", Fault{Kind: FaultDelay, Delay: time.Second, BodyBytes: 2}, "whole answer"},
		{"reset with a delay", Fault{Kind: FaultReset, Delay: time.Second}, "only kind delay"},
		{"timeout with a delay", Fault{Kind: FaultTimeout, Delay: time.Second}, "only kind delay"},
		{"negative body bytes", Fault{Kind: FaultTruncate, BodyBytes: -1}, "not negative"},
		{"reset with body bytes and no head", Fault{Kind: FaultReset, BodyBytes: 5}, "set after_head"},
		{"timeout with body bytes and no head", Fault{Kind: FaultTimeout, BodyBytes: 5}, "set after_head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.f, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate(%+v) = %v, want an error naming %q", tc.f, err, tc.want)
			}
		})
	}
	if err := (*Fault)(nil).Validate(); err != nil {
		t.Fatalf("a call with no fault: %v", err)
	}
	if err := (&Fault{Kind: "tls_alert"}).Validate(); !errors.Is(err, ErrFaultUnsupported) {
		t.Fatalf("a kind of a newer format: %v, want ErrFaultUnsupported", err)
	}
}

// What a summary says the application got.
func TestFaultString(t *testing.T) {
	for _, tc := range []struct {
		f    *Fault
		want string
	}{
		{&Fault{Kind: FaultReset}, "upstream reset after 0 bytes"},
		{&Fault{Kind: FaultReset, AfterHead: true, BodyBytes: 12}, "upstream reset after the head and 12 body bytes"},
		{&Fault{Kind: FaultEOF}, "upstream closed with no answer"},
		{&Fault{Kind: FaultTimeout}, "no answer until the application closed"},
		{&Fault{Kind: FaultTimeout, AfterHead: true, BodyBytes: 3}, "upstream sent the head and 3 body bytes, then nothing until the application closed"},
		{&Fault{Kind: FaultTruncate, BodyBytes: 10}, "upstream sent the head and 10 body bytes, then closed"},
		{&Fault{Kind: FaultDelay, Delay: 1500 * time.Millisecond}, "answer held 1.5s"},
		{&Fault{Kind: "tls_alert"}, `fault "tls_alert"`},
		{nil, ""},
	} {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}

// A fault is enacted on HTTP calls only: on any other call it is refused,
// never served as the recorded answer; on an HTTP call it is validated.
func TestMockValidateFault(t *testing.T) {
	if err := (&Mock{Kind: Postgres}).ValidateFault(); err != nil {
		t.Fatalf("a mock with no fault: %v", err)
	}
	if err := (*Mock)(nil).ValidateFault(); err != nil {
		t.Fatalf("no mock: %v", err)
	}
	pg := &Mock{Name: "mock-3", Kind: Postgres, Spec: MockSpec{Fault: &Fault{Kind: FaultReset}}}
	err := pg.ValidateFault()
	if !errors.Is(err, ErrFaultUnsupported) || !strings.Contains(err.Error(), "mock-3") || !strings.Contains(err.Error(), "HTTP calls only") {
		t.Fatalf("a fault on a Postgres call: %v, want it refused, naming the mock and why", err)
	}
	h := &Mock{Name: "mock-4", Kind: HTTP, Spec: MockSpec{Fault: &Fault{Kind: FaultTruncate, BodyBytes: 2}}}
	if err := h.ValidateFault(); err != nil {
		t.Fatalf("a truncate on an HTTP call: %v", err)
	}
	h.Spec.Fault.AfterHead = true
	if err := h.ValidateFault(); err == nil || !strings.Contains(err.Error(), "mock-4") {
		t.Fatalf("a misspelt fault on an HTTP call: %v, want it refused, naming the mock", err)
	}
}

// A copy of a mock has a fault of its own.
func TestDeepCopyCopiesTheFault(t *testing.T) {
	m := &Mock{Kind: HTTP, Spec: MockSpec{Fault: &Fault{Kind: FaultTruncate, BodyBytes: 4}}}
	c := m.DeepCopy()
	if c.Spec.Fault == nil || *c.Spec.Fault != *m.Spec.Fault {
		t.Fatalf("copy has fault %+v, want %+v", c.Spec.Fault, m.Spec.Fault)
	}
	c.Spec.Fault.BodyBytes = 9
	if m.Spec.Fault.BodyBytes != 4 {
		t.Fatal("the copy shares its fault with the mock it was copied from")
	}
}

// FaultServed names the mock and its fault as the application met it, for a
// replay summary; a mock with a whole answer, or none, names nothing.
func TestFaultServedNamesTheMockAndItsFault(t *testing.T) {
	var none *Mock
	for _, c := range []struct {
		mock *Mock
		want string
	}{
		{none, ""},
		{&Mock{Name: "mock-1", Kind: HTTP}, ""},
		{&Mock{Name: "mock-12", Kind: HTTP, Spec: MockSpec{Fault: &Fault{Kind: FaultReset}}}, "mock-12: upstream reset after 0 bytes, as recorded"},
		{&Mock{Name: "mock-7", Kind: HTTP, Spec: MockSpec{Fault: &Fault{Kind: FaultTimeout}}}, "mock-7: no answer until the application closed, as recorded"},
	} {
		if got := c.mock.FaultServed(); got != c.want {
			t.Errorf("FaultServed() = %q, want %q", got, c.want)
		}
	}
}

// A mock whose payload is left on disk (LeavePayloadOnDisk), as keploy test
// reads a set's mocks, still says the fault its answer ends in, for the
// replay summary: its Spec.Fault is back only once its payload is read back.
// It keeps a copy of its own, counted in what its index entry costs; a mock
// whose answer ends in no fault keeps none.
func TestFaultOfAMockWhosePayloadIsOnDisk(t *testing.T) {
	recorded := payloadMocks()
	src := &fileSource{recorded: recorded}
	onDisk := func(fault *Fault) *Mock {
		m := recorded["http"]()
		m.Name = "mock-4"
		m.Spec.Fault = fault
		m.DeriveLifetime()
		m.LeavePayloadOnDisk(src, PayloadAt{Name: "http"})
		return m
	}
	fault := &Fault{Kind: FaultTruncate, BodyBytes: 3, Observed: "the upstream closed the connection partway through the answer"}
	m := onDisk(fault)
	if !m.PayloadOnDisk() || m.Spec.Fault != nil {
		t.Fatalf("the payload stayed in memory: on disk %v, Spec.Fault %+v", m.PayloadOnDisk(), m.Spec.Fault)
	}
	if got := m.FaultOf(); got == nil || *got != *fault {
		t.Fatalf("FaultOf() = %+v, want %+v", got, fault)
	}
	if got, want := m.FaultServed(), "mock-4: upstream sent the head and 3 body bytes, then closed, as recorded"; got != want {
		t.Fatalf("FaultServed() = %q, want %q", got, want)
	}
	plain := onDisk(nil)
	if plain.FaultOf() != nil || plain.FaultServed() != "" {
		t.Fatalf("a mock with no fault: FaultOf() %+v, FaultServed() %q", plain.FaultOf(), plain.FaultServed())
	}
	if got, least := m.IndexEntryBytes()-plain.IndexEntryBytes(), int64(unsafe.Sizeof(*fault))+int64(len(fault.Observed)); got < least {
		t.Fatalf("the kept fault adds %d bytes to the index entry's cost, want at least %d", got, least)
	}
}

// The faults a replay served are named in the order it first served them,
// by name between mocks first served at one time; a consumed mock whose
// answer ends in no fault is not named.
func TestFaultsServedInTheOrderTheyWereServed(t *testing.T) {
	lines := map[string]string{"mock-9": "nine", "mock-2": "two", "mock-5": "five", "mock-1": "one"}
	consumed := []MockState{
		{Name: "mock-9", Timestamp: 30},
		{Name: "mock-3", Timestamp: 5},
		{Name: "mock-5", Timestamp: 10},
		{Name: "mock-2", Timestamp: 10},
		{Name: "mock-1", Timestamp: 20},
	}
	got := FaultsServed(consumed, func(name string) string { return lines[name] })
	if want := []string{"two", "five", "one", "nine"}; !slices.Equal(got, want) {
		t.Fatalf("FaultsServed() = %q, want %q", got, want)
	}
	if got := FaultsServed(nil, func(string) string { return "x" }); len(got) != 0 {
		t.Fatalf("no mock consumed: %q", got)
	}
}

// The kind a fault mock is written under has no "-": an open-source keploy
// skips a "-" kind as an enterprise one without a word, and the kind is there
// to make a keploy that knows no faults say it skipped the mock.
func TestFaultMockKindHasNoHyphen(t *testing.T) {
	if strings.Contains(string(HTTPFault), "-") || HTTPFault == HTTP {
		t.Fatalf("HTTPFault = %q", HTTPFault)
	}
}

// An HTTP mock with a fault is written as HttpFault, and only it; a document's
// kind and its fault must go together.
func TestDiskKindAndFaultKindMismatch(t *testing.T) {
	f := &Fault{Kind: FaultEOF}
	for _, tc := range []struct {
		m    Mock
		want Kind
	}{
		{Mock{Kind: HTTP, Spec: MockSpec{Fault: f}}, HTTPFault},
		{Mock{Kind: HTTP}, HTTP},
		{Mock{Kind: DNS, Spec: MockSpec{Fault: f}}, DNS},
	} {
		if got := tc.m.DiskKind(); got != tc.want {
			t.Errorf("DiskKind(%s, fault %v) = %s, want %s", tc.m.Kind, tc.m.Spec.Fault, got, tc.want)
		}
	}
	for _, tc := range []struct {
		kind Kind
		f    *Fault
		bad  bool
	}{
		{HTTP, f, true}, {HTTPFault, nil, true}, {HTTPFault, f, false}, {HTTP, nil, false}, {DNS, f, false},
	} {
		if got := FaultKindMismatch(tc.kind, tc.f) != ""; got != tc.bad {
			t.Errorf("FaultKindMismatch(%s, %v) refuses: %v, want %v", tc.kind, tc.f, got, tc.bad)
		}
	}
}
