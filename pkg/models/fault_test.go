package models

import (
	"errors"
	"strings"
	"testing"
	"time"
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
