package models

import (
	"strings"
	"testing"
	"time"
)

// mustPanic runs f and fails unless it panics with a message holding want.
func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one saying %q", want)
		}
		if s, _ := r.(string); !strings.Contains(s, want) {
			t.Fatalf("panic %v; want one saying %q", r, want)
		}
	}()
	f()
}

// mongoWrite is a Mongo mock whose metadata names its op.
func mongoWrite(op string) *Mock {
	return &Mock{Kind: Mongo, Name: "mock-" + op, Spec: MockSpec{Metadata: map[string]string{"op": op}}}
}

// A write kind and an exact key are each one registration per kind: a
// registered kind names its mocks, a kind nobody registers names none, a
// second registration and a nil function panic, and unregister removes the
// registration (and only it).
func TestKindRegistriesTakeOneRegistrationPerKind(t *testing.T) {
	for _, reg := range []struct {
		name     string
		register func(Kind, func(*Mock) (string, bool)) func()
		of       func(*Mock) (string, bool)
	}{
		{"RegisterWriteKind", RegisterWriteKind, WriteKindOf},
		{"RegisterExactKey", RegisterExactKey, ExactKeyOf},
	} {
		t.Run(reg.name, func(t *testing.T) {
			if got, ok := reg.of(mongoWrite("insert")); ok || got != "" {
				t.Fatalf("an unregistered kind names %q, %v", got, ok)
			}
			if _, ok := reg.of(nil); ok {
				t.Fatal("a nil mock is named")
			}
			unregister := reg.register(Mongo, func(m *Mock) (string, bool) {
				if op := m.Spec.Metadata["op"]; op == "insert" {
					return "Mongo insert keploy.events", true
				}
				return "", false
			})
			if got, ok := reg.of(mongoWrite("insert")); !ok || got != "Mongo insert keploy.events" {
				t.Fatalf("the registered kind names %q, %v", got, ok)
			}
			if got, ok := reg.of(mongoWrite("find")); ok || got != "" {
				t.Fatalf("a mock the registration does not name is %q, %v", got, ok)
			}
			if _, ok := reg.of(&Mock{Kind: HTTP}); ok {
				t.Fatal("another kind is named by Mongo's registration")
			}
			mustPanic(t, "registered already", func() { reg.register(Mongo, func(*Mock) (string, bool) { return "x", true }) })
			mustPanic(t, "a nil function", func() { reg.register(HTTP, nil) })
			unregister()
			if _, ok := reg.of(mongoWrite("insert")); ok {
				t.Fatal("unregister left the registration")
			}
			// A kind unregistered may be registered again; the old unregister
			// does not remove the new registration.
			again := reg.register(Mongo, func(*Mock) (string, bool) { return "again", true })
			unregister()
			if got, ok := reg.of(mongoWrite("insert")); !ok || got != "again" {
				t.Fatalf("a stale unregister removed the next registration: %q, %v", got, ok)
			}
			again()
		})
	}
	// A function that says ok but names nothing names no write kind, and
	// keys nothing.
	defer RegisterWriteKind(Mongo, func(*Mock) (string, bool) { return "", true })()
	defer RegisterExactKey(Mongo, func(*Mock) (string, bool) { return "", true })()
	if got, ok := WriteKindOf(mongoWrite("insert")); ok || got != "" {
		t.Fatalf("an empty write kind is %q, %v; want none", got, ok)
	}
	if got, ok := ExactKeyOf(mongoWrite("insert")); ok || got != "" {
		t.Fatalf("an empty exact key is %q, %v; want none", got, ok)
	}
}

// The window derivation reads a mock's clock and write kind, and a test
// case's window its clock; none without an origin or a registration, and the
// parser is not asked for the write kind of a mock without an origin (one
// recorded without the proven switch).
func TestWindowsCarryTheirClockAndWriteKind(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	m := mongoWrite("insert")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	if w := NewWindowedMock(m); w.Clock != "" || w.WriteKind != "" {
		t.Fatalf("a mock without an origin or a registration reads clock %q, write kind %q", w.Clock, w.WriteKind)
	}
	asked := 0
	defer RegisterWriteKind(Mongo, func(*Mock) (string, bool) { asked++; return "Mongo insert keploy.events", true })()
	if w := NewWindowedMock(m); w.WriteKind != "" || asked != 0 {
		t.Fatalf("a mock without an origin reads write kind %q (the parser asked %d times)", w.WriteKind, asked)
	}
	m.Origin = &Origin{Comm: "sqs", Exe: "./sqs"} // an untrusted stamp
	if w := NewWindowedMock(m); w.Clock != "" || w.WriteKind != "Mongo insert keploy.events" {
		t.Fatalf("a mock with an untrusted stamp reads clock %q, write kind %q", w.Clock, w.WriteKind)
	}
	m.Origin = &Origin{Clock: "proxy:run-1", Comm: "sqs", Exe: "./sqs"}
	if w := NewWindowedMock(m); w.Clock != "proxy:run-1" || w.WriteKind != "Mongo insert keploy.events" {
		t.Fatalf("the mock reads clock %q, write kind %q", w.Clock, w.WriteKind)
	}
	tc := &TestCase{Name: "post-url-1", Kind: HTTP, HTTPReq: HTTPReq{Timestamp: at}, HTTPResp: HTTPResp{Timestamp: at.Add(time.Second)}}
	if w := RecordedWindow(tc); w.Clock != "" {
		t.Fatalf("a test case without an origin opens a window on clock %q", w.Clock)
	}
	tc.Origin = &Origin{Clock: "proxy:run-1"}
	if w := RecordedWindow(tc); w.Clock != "proxy:run-1" || !w.Start.Equal(at) {
		t.Fatalf("the window is %+v; want its start on proxy:run-1", w)
	}
}

// A mock's copy holds its own origin.
func TestDeepCopyCopiesTheOrigin(t *testing.T) {
	m := &Mock{Kind: Mongo, Origin: &Origin{Clock: "ktime:boot.1", Comm: "sqs", Exe: "./sqs"}}
	c := m.DeepCopy()
	if c.Origin == nil || *c.Origin != *m.Origin {
		t.Fatalf("the copy's origin is %+v; want %+v", c.Origin, m.Origin)
	}
	if c.Origin == m.Origin {
		t.Fatal("the copy shares the origin with the mock")
	}
	if (&Mock{}).DeepCopy().Origin != nil {
		t.Fatal("a mock without an origin copies to one with one")
	}
}
