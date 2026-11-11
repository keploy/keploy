package models

import (
	"strings"
	"testing"
)

// A test's ID is [project::](file|key)::name, and a name holding "::" is
// escaped so the ID's last "::" always ends its path.
func TestTestID(t *testing.T) {
	for _, tc := range []struct {
		project, file, key, name, want string
	}{
		{"", "", "orders", "TestA", "orders::TestA"},
		{"svc", "orders/x_test.go", "orders", "TestA", "svc::orders/x_test.go::TestA"},
		{"", "", "", "TestA", "TestA"},
		{"", "", "net", "TestDial/[::1]:80", "net::TestDial/[%3A%3A1]:80"},
		{"", "", "net", "TestDial/50%/[::1]", "net::TestDial/50%25/[%3A%3A1]"},
		{"", "", "net", "TestDial/50%", "net::TestDial/50%"},
		// A name that already reads like an escape is escaped, so it never
		// shares an ID with the name it would decode to.
		{"", "", "k", "TestX/a%3A%3Ab", "k::TestX/a%253A%253Ab"},
		{"", "", "k", "TestX/a::b", "k::TestX/a%3A%3Ab"},
		{"", "", "k", "TestX/50%25", "k::TestX/50%2525"},
		// A leading ":" would move the split to before it.
		{"", "", "k", ":x", "k::%3Ax"},
		{"", "", "k", ":::x", "k::%3A%3A:x"},
		{"", "tests/x.py", "k", "TestCls::test_x", "tests/x.py::TestCls%3A%3Atest_x"},
	} {
		if got := TestID(tc.project, tc.file, tc.key, tc.name); got != tc.want {
			t.Errorf("TestID(%q, %q, %q, %q) = %q, want %q", tc.project, tc.file, tc.key, tc.name, got, tc.want)
		}
		path := tc.file
		if path == "" {
			path = tc.key
		}
		if got := TestIDPath(tc.want); got != path {
			t.Errorf("TestIDPath(%q) = %q, want %q", tc.want, got, path)
		}
	}
}

// Every name up to five characters from the characters escaping touches gets
// an ID of its own, and the ID's last "::" ends its path, for keys IDPart
// allows: what enterprise reads back (umproven.IsUMID) is what the agent
// built.
func TestTestIDsAreDistinctAndSplitAtThePath(t *testing.T) {
	const alphabet = ":%3A25x"
	names := []string{""}
	all := []string{}
	for n := 1; n <= 5; n++ {
		var next []string
		for _, p := range names {
			for _, c := range alphabet {
				next = append(next, p+string(c))
			}
		}
		names = next
		all = append(all, next...)
	}
	for _, key := range []string{"k", "k:x", "a/b"} {
		if !IDPart(key) {
			t.Fatalf("IDPart(%q) = false", key)
		}
		seen := make(map[string]string, len(all))
		for _, name := range all {
			id := TestID("", "", key, name)
			if prev, ok := seen[id]; ok {
				t.Fatalf("key %q: names %q and %q share the ID %q", key, prev, name, id)
			}
			seen[id] = name
			i := strings.LastIndex(id, "::")
			if i < 0 || id[:i] != key || id[i+2:] != escapeTestName(name) {
				t.Fatalf("key %q, name %q: ID %q splits at %d", key, name, id, i)
			}
			if p := TestIDPath(id); p != key {
				t.Fatalf("key %q, name %q: TestIDPath(%q) = %q", key, name, id, p)
			}
			if pid := TestID("proj", "", key, name); TestIDPath(pid) != key {
				t.Fatalf("key %q, name %q: TestIDPath(%q) = %q", key, name, pid, TestIDPath(pid))
			}
		}
	}
	for _, bad := range []string{"a::b", ":a", "a:", "::"} {
		if IDPart(bad) {
			t.Errorf("IDPart(%q) = true", bad)
		}
	}
}

// A test charged a call from no worker by the agent (a worker's REAL use)
// and again by the record (a mock) says it in one sentence with the summed
// count and both chargers' What, never in a second "Also" sentence; the
// sentence for losses charged before (a Unix socket call) is written again
// in its place with them, and a reason with no such sentence gets it after
// what it says.
func TestChargeNotJudgedWritesOneSentenceWithTheSummedCount(t *testing.T) {
	unattributed := func(n uint64, what string) CaptureLosses {
		return CaptureLosses{{Kind: CaptureLossUnattributed, Count: n, What: what}}
	}
	real, mock := "the latest: a connection to mock.real destination db:5432, from process 7", "the latest: mock m3, from process 9"

	ev := ScopeEvidence{}
	ev.ChargeNotJudged(unattributed(2, real))
	ev.ChargeNotJudged(unattributed(3, mock))
	want := unattributed(5, real+"; "+mock)
	if len(ev.CannotCapture) != 1 || ev.CannotCapture[0] != want[0] {
		t.Fatalf("CannotCapture = %+v, want %+v", ev.CannotCapture, want)
	}
	if wantReason := "keploy: " + want.NotJudgedText() + "."; ev.Reason != wantReason {
		t.Fatalf("Reason = %q, want %q", ev.Reason, wantReason)
	}

	unix := CaptureLosses{{Kind: CaptureLossUnixSocket, Count: 1}}
	ev = ScopeEvidence{CannotCapture: unix, Reason: "keploy: a value it gave is not recorded. Also " + unix.NotJudgedText() + "."}
	ev.ChargeNotJudged(unattributed(1, mock))
	both := unix.Plus(unattributed(1, mock))
	if wantReason := "keploy: a value it gave is not recorded. Also " + both.NotJudgedText() + "."; ev.Reason != wantReason {
		t.Fatalf("Reason = %q, want %q", ev.Reason, wantReason)
	}
	if strings.Count(ev.Reason, "not judged:") != 1 {
		t.Fatalf("Reason %q says not judged more than once", ev.Reason)
	}

	ev = ScopeEvidence{Reason: "keploy: a miss."}
	ev.ChargeNotJudged(unattributed(1, mock))
	if wantReason := "keploy: a miss. Also " + unattributed(1, mock).NotJudgedText() + "."; ev.Reason != wantReason {
		t.Fatalf("Reason = %q, want %q", ev.Reason, wantReason)
	}

	for _, c := range []struct{ before, charged, want string }{
		{mock, mock, mock}, // the same call named once
		{"", mock, mock},   // no What before: the charged one alone
		{real, "", real},   // none charged: the one before stands
		{real + "; " + mock, real, real + "; " + mock}, // each named once
	} {
		ev = ScopeEvidence{}
		ev.ChargeNotJudged(append(unattributed(1, c.before), CaptureLosses{{Kind: CaptureLossUnixSocket, Count: 1, What: "s"}}...))
		ev.ChargeNotJudged(unattributed(1, c.charged))
		if got := ev.CannotCapture[0].What; got != c.want {
			t.Fatalf("What after %q then %q = %q, want %q", c.before, c.charged, got, c.want)
		}
		if got := ev.CannotCapture[1].What; got != "s" {
			t.Fatalf("another kind's What = %q, want it untouched", got)
		}
	}
}

// A test's evidence is Unanswered when it holds a call keploy did not answer
// from a recording, or could not tell it answered: anything but the
// recordings the test did not make. Those alone leave every call answered.
func TestScopeEvidenceUnansweredIsAnythingButTheUnmadeRecordings(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   *ScopeEvidence
		want bool
	}{
		{"no evidence", nil, false},
		{"only unmade recordings", &ScopeEvidence{Unconsumed: []string{"mock-1"}}, false},
		{"a miss beside them", &ScopeEvidence{Unconsumed: []string{"mock-1"}, Misses: []UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit"}}}, true},
		{"a call keploy could not capture", &ScopeEvidence{Unconsumed: []string{"mock-1"}, Incomplete: []CaptureIncomplete{{Class: "e", Why: "x"}}}, true},
		{"a counted loss", &ScopeEvidence{CaptureIncomplete: CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}}, true},
		{"what the test did unread", &ScopeEvidence{Unconsumed: []string{"mock-1"}, Unread: true}, true},
	} {
		if got := c.ev.Unanswered(); got != c.want {
			t.Errorf("%s: Unanswered() = %v, want %v", c.name, got, c.want)
		}
	}
	ev := &ScopeEvidence{Unconsumed: []string{"mock-1"}, Misses: []UnmatchedCall{{Protocol: "HTTP"}}}
	_ = ev.Unanswered()
	if len(ev.Unconsumed) != 1 {
		t.Fatalf("Unanswered changed the evidence it read: %+v", ev)
	}
}
