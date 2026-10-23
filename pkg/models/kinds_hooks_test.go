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

// A write kind and an exact key each name a registered kind's mocks, a kind
// nobody registers names none, a nil function panics, and unregister removes
// the registration (and only it). A write kind is one registration per kind:
// a second panics. An exact key may be one of several (the next test).
func TestKindRegistriesNameARegisteredKindsMocks(t *testing.T) {
	for _, reg := range []struct {
		name     string
		register func(Kind, func(*Mock) (string, bool)) func()
		of       func(*Mock) (string, bool)
		one      bool
	}{
		{"RegisterWriteKind", RegisterWriteKind, WriteKindOf, true},
		{"RegisterExactKey", RegisterExactKey, ExactKeyOf, false},
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
			if reg.one {
				mustPanic(t, "registered already", func() { reg.register(Mongo, func(*Mock) (string, bool) { return "x", true }) })
			}
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

// httpOn is an HTTP mock of a URL.
func httpOn(name, url string) *Mock {
	return &Mock{Kind: HTTP, Name: name, Spec: MockSpec{HTTPReq: &HTTPReq{URL: url}}}
}

// A kind may have several exact keys, each declining (ok false) the
// recordings its parser does not own: registered together (enterprise's SQS
// keys its queue calls, another HTTP parser its own), they never panic, and
// each recording takes the key of the one that owns it. Two that both claim
// a recording give it no key, and say so as an error, the same every time: it
// counts as having siblings, and stays gated by its place.
func TestAKindsExactKeysEachKeyTheirOwn(t *testing.T) {
	prefix := func(p string) func(*Mock) (string, bool) {
		return func(m *Mock) (string, bool) {
			if m.Spec.HTTPReq == nil || !strings.HasPrefix(m.Spec.HTTPReq.URL, p) {
				return "", false
			}
			return p + "|" + m.Spec.HTTPReq.URL, true
		}
	}
	queue := RegisterExactKey(HTTP, prefix("/queue/"))
	defer queue()
	var other func()
	mustNotPanic := func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a second exact key of a kind panicked: %v", r)
			}
		}()
		other = RegisterExactKey(HTTP, prefix("/api/"))
	}
	mustNotPanic()
	t.Run("disjoint", func(t *testing.T) {
		for _, tc := range []struct{ url, want string }{
			{"/queue/q1", "/queue/|/queue/q1"},
			{"/api/users", "/api/|/api/users"},
		} {
			m := httpOn("m", tc.url)
			if got, ok := ExactKeyOf(m); !ok || got != tc.want {
				t.Fatalf("%s keyed %q, %v; want %q, the key that owns it", tc.url, got, ok, tc.want)
			}
			if err := ExactKeyErr(m); err != nil {
				t.Fatalf("%s: %v", tc.url, err)
			}
		}
		if got, ok := ExactKeyOf(httpOn("m", "/health")); ok {
			t.Fatalf("a recording no key owns is keyed %q", got)
		}
	})
	t.Run("overlapping", func(t *testing.T) {
		both := RegisterExactKey(HTTP, prefix("/queue/q"))
		m := httpOn("mock-5", "/queue/q1")
		if got, ok := ExactKeyOf(m); ok {
			t.Fatalf("a recording two keys claim is keyed %q", got)
		}
		err := ExactKeyErr(m)
		if err == nil || !strings.Contains(err.Error(), "2 of the Http kind's exact keys key recording mock-5") ||
			strings.Count(err.Error(), "TestAKindsExactKeysEachKeyTheirOwn") != 2 {
			t.Fatalf("the overlap says %v; want it to name the recording and both registrations", err)
		}
		if again := ExactKeyErr(m); again == nil || again.Error() != err.Error() {
			t.Fatalf("the overlap's error changed: %v, then %v", err, again)
		}
		if claims := ExactKeyClaims(m); strings.Join(claims, " ") != "/queue/|/queue/q1 /queue/q|/queue/q1" {
			t.Fatalf("the recording's claims are %q; want both keys', in registration order", claims)
		}
		if claims := ExactKeyClaims(httpOn("m", "/api/users")); strings.Join(claims, " ") != "/api/|/api/users" {
			t.Fatalf("a recording one key owns is claimed %q", claims)
		}
		// The keys' other recordings are as they were.
		if got, ok := ExactKeyOf(httpOn("m", "/api/users")); !ok || got != "/api/|/api/users" {
			t.Fatalf("an overlap elsewhere changed another recording's key: %q, %v", got, ok)
		}
		both()
		if got, ok := ExactKeyOf(m); !ok || got != "/queue/|/queue/q1" {
			t.Fatalf("once one of the two is removed, the recording is keyed %q, %v", got, ok)
		}
	})
	// Two registrations that give equal strings give one key each, as each
	// gave it, and two sibling keys: each named by its registration.
	same := func(p string) func(*Mock) (string, bool) {
		return func(m *Mock) (string, bool) {
			if m.Spec.HTTPReq == nil || !strings.HasPrefix(m.Spec.HTTPReq.URL, p) {
				return "", false
			}
			return "DeleteMessage", true
		}
	}
	s1, s2 := RegisterExactKey(HTTP, same("/s1/")), RegisterExactKey(HTTP, same("/s2/"))
	k1, ok1 := ExactKeyOf(httpOn("m", "/s1/x"))
	k2, ok2 := ExactKeyOf(httpOn("m", "/s2/x"))
	if !ok1 || !ok2 || k1 != "DeleteMessage" || k2 != "DeleteMessage" {
		t.Fatalf("two registrations' equal strings are keys %q, %v and %q, %v; want each as its registration gave it", k1, ok1, k2, ok2)
	}
	s1k, sok1 := ExactSiblingKey(httpOn("m", "/s1/x"))
	s2k, sok2 := ExactSiblingKey(httpOn("m", "/s2/x"))
	if !sok1 || !sok2 || s1k == s2k {
		t.Fatalf("two registrations' equal strings are sibling keys %q, %v and %q, %v; want two", s1k, sok1, s2k, sok2)
	}
	if same, ok := ExactSiblingKey(httpOn("n", "/s1/y")); !ok || same != s1k {
		t.Fatalf("one registration's equal keys are sibling keys %q and %q; want one", s1k, same)
	}
	if c := ExactSiblingClaims(httpOn("m", "/s1/x")); len(c) != 1 || c[0] != s1k {
		t.Fatalf("the sibling claims are %q; want [%q]", c, s1k)
	}
	s1()
	s2()
	other()
	if got, ok := ExactKeyOf(httpOn("m", "/api/users")); ok {
		t.Fatalf("an unregistered key still keys: %q", got)
	}
	if got, ok := ExactKeyOf(httpOn("m", "/queue/q1")); !ok || got != "/queue/|/queue/q1" {
		t.Fatalf("removing one key removed another: %q, %v", got, ok)
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
