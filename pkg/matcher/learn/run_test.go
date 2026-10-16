package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func sent(mock string, at time.Time, path, place, rec, live string) Earlier {
	return Earlier{Mock: mock, At: at, Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: path, Type: String, Value: rec}, Live: live, Place: place, Written: true}}}
}

// What the start-up sends, while no test is open, is an earlier place of
// every later test of the set, and its variables keep one name across them.
func TestTrackerKeepsTheRunsExchangesForEveryTest(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body.auth_time", "body.auth_time", "1792000000", "1792000777"))
	tr.Served(0, sent("mock-2", t0, "body.status", "body.status", "ok", "ok"))
	run := tr.RunName("mock-1", "body.auth_time", nil)

	for i := 0; i < 2; i++ {
		tr.Begin()
		n, earlier, _, _ := tr.Test()
		if len(earlier) != 1 || !earlier[0].Run || earlier[0].Mock != "mock-1" {
			t.Fatalf("test %d: the run's exchanges are %+v, want the start-up call that carried a value the run made", i, earlier)
		}
		if name, ok := tr.Name(n, "mock-1", "body.auth_time", nil); !ok || name != run {
			t.Fatalf("test %d: the run's variable is named %q here, %q at the start-up", i, name, run)
		}
		if name, _ := tr.Name(n, "mock-9", "body.auth_time", nil); name == run {
			t.Fatalf("test %d: a test's own variable took the run's name %q", i, run)
		}
		tr.End()
	}
	tr.NewSet()
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("a new set reads the previous set's start-up: %+v", earlier)
	}
	tr.End()
	tr.NewSet()
	n, _, _, _ := tr.Test()
	tr.Served(n, sent("mock-1", t0, "body.auth_time", "body.auth_time", "1792000000", "1792000999"))
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || !earlier[0].Run {
		t.Fatalf("the next set's start-up is not the run's: %+v", earlier)
	}
	if again := tr.RunName("mock-1", "body.auth_time", nil); again != run {
		t.Fatalf("the next set's run's variable is named %q, want %q again", again, run)
	}
}

// A run's variable is named apart from a test case's template keys: a test
// that reserves the key the run's value sits under uses the run's name as it
// is.
func TestTheRunsVariableTakesNoTemplateKey(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body._id", "body._id", "rec-id", "live-id"))
	tr.Begin()
	run := tr.RunName("mock-1", "body._id", nil)
	tr.End()
	tr.Begin()
	n, _, _, _ := tr.Test()
	reserved := map[string]bool{"_id": true}
	if again := tr.RunName("mock-1", "body._id", reserved); again != run || reserved[run] {
		t.Fatalf("the run's variable is %q in a test that reserves _id, %q before", again, run)
	}
	if own, _ := tr.Name(n, "mock-5", "body._id", reserved); own == run || reserved[own] {
		t.Fatalf("the test's own variable is %q, next to the run's %q and its reserved _id", own, run)
	}
	if name := (*Tracker)(nil).RunName("mock-1", "body._id", map[string]bool{"run-_id": true}); name != "run-_id-2" {
		t.Fatalf("a nil tracker names the run's variable %q, want run-_id-2", name)
	}
}

// A run's variable is named when a test first uses it, after earlier tests
// named their own: it takes none of their names, and later tests take none
// of its (two tests may share a name: variables are named per test).
func TestTheRunsVariableTakesNoTestsName(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id"))
	tr.Begin()
	n1, _, _, _ := tr.Test()
	own, _ := tr.Name(n1, "mock-5", "body.docs.msgs.0._id", nil)
	tr.End()
	tr.Begin()
	run := tr.RunName("mock-1", "body.docs.msgs.0._id", nil)
	n2, _, _, _ := tr.Test()
	later, _ := tr.Name(n2, "mock-7", "body.docs.msgs.0._id", nil)
	if own != "_id" || run == own || later == run {
		t.Fatalf("test 1 named its own %q, the run's is %q, test 2's own is %q; want the run's apart from both", own, run, later)
	}
}

// Only what the start-up served, before the first test began, is the run's.
// A request of a test answered after it ended, or between it and the next,
// is that test's, forgotten by the next Begin; a start-up exchange that did
// not drift is kept until the first test begins.
func TestOnlyTheStartUpsExchangesAreTheRuns(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, sent("mock-1", t0, "body.status", "body.status", "ok", "ok"))
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Run {
		t.Fatalf("a start-up exchange that did not drift is not kept until the first test: %+v", earlier)
	}
	tr.Begin()
	n, earlier, _, _ := tr.Test()
	if len(earlier) != 0 {
		t.Fatalf("the first test sees a start-up exchange that carried nothing the run made: %+v", earlier)
	}
	tr.End()
	tr.Served(n, sent("mock-4", t0.Add(time.Second), "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id"))
	if _, earlier, _, _ := tr.Test(); len(earlier) != 1 || earlier[0].Run {
		t.Fatalf("a late exchange of the ended test is not its own: %+v", earlier)
	}
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("the next test sees the previous test's late exchange: %+v", earlier)
	}
	tr.End()
	if !tr.Begun() {
		t.Fatal("a test has begun in this set, and the tracker says none has")
	}
	tr.NewSet()
	if tr.Begun() {
		t.Fatal("a new set begins with no test begun, and the tracker says one has")
	}
	tr.Served(7, sent("mock-8", t0, "body.docs.msgs.0._id", "body.docs.msgs.0._id", "rec-id", "live-id"))
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("an exchange of a test that never ran is kept: %+v", earlier)
	}
}

// A request place that holds no variable, or whose live value is unknown,
// does not make a start-up exchange the run's.
func TestAStartUpExchangeDriftsOnlyAtAVariablePlace(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(1_792_000_000, 0)
	tr.Served(0, Earlier{Mock: "mock-1", At: t0, Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "request{names}", Type: Keys, Value: "a,b"}, Live: "a,b,c"},
		{Side: Request, Leaf: Leaf{Path: "body.x", Type: String, Value: "rec"}, Live: "", Place: "body.x"},
	}})
	tr.Begin()
	if _, earlier, _, _ := tr.Test(); len(earlier) != 0 {
		t.Fatalf("an exchange that drifted at no variable place is the run's: %+v", earlier)
	}
}

// Named leaves the exchange it was given as it was, and names a place two
// variables hold after the first by name.
func TestNamedLeavesTheExchangeItWasGiven(t *testing.T) {
	e := Earlier{Mock: "mock-1", Seen: []Seen{{Side: Request, Place: "body.id", Leaf: Leaf{Path: "body.id"}}}}
	named := Named(e, models.MockVariables{"second": {Bind: "body.id"}, "first": {Use: []string{"body.id"}}})
	if e.Seen[0].Name != "" {
		t.Fatalf("Named wrote %q into the exchange it was given", e.Seen[0].Name)
	}
	if named.Seen[0].Name != "first" {
		t.Fatalf("the place two variables hold is named %q, want the first by name", named.Seen[0].Name)
	}
	named.Seen = append(named.Seen, Seen{Side: Response, Name: "stale"})
	again := Named(named, nil)
	if again.Seen[0].Name != "" || again.Seen[1].Name != "" || &again.Seen[0] == &named.Seen[0] {
		t.Fatalf("Named with no variables keeps a stale name or the exchange's own Seen: %+v", again.Seen)
	}
}

// An answer that holds a value an earlier call wrote with another value in
// this run is answered with the value where it was first written; a short
// value, or one no call wrote anew, is not.
func TestReadBack(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	earlier := []Earlier{
		sent("mock-3", t0.Add(time.Second), "body.code", "body.code", "BJBM-NZBS", "QQRT-XKCD"),
		sent("mock-1", t0.Add(-time.Second), "body.code", "body.code", "BJBM-NZBS", "FRST-XKCD"),
		sent("mock-2", t0, "body.created", "body.created", "1792000000000", "1792000555000"),
		sent("mock-4", t0, "body.n", "body.n", "12", "13"),
		sent("mock-5", t0, "body.kind", "body.kind", "pending", "pending"),
		sent("mock-6", t0, "body.name", "body.name", "日本", "韓国"),
	}
	for _, c := range []struct {
		value, mock, live string
	}{
		{"BJBM-NZBS", "mock-1", "FRST-XKCD"},
		{"1792000000000", "mock-2", "1792000555000"},
		{"12", "", ""},
		{"pending", "", ""},
		{"日本", "", ""},
	} {
		p, ok := ReadBack(Leaf{Path: "body.value.x", Type: String, Value: c.value}, earlier)
		if c.mock == "" {
			if ok {
				t.Fatalf("%q was read back from %+v", c.value, p)
			}
			continue
		}
		if !ok || p.Mock != c.mock || p.Live != c.live || p.Side != Request || !p.Whole {
			t.Fatalf("%q read back as %+v (%v), want it from %s with %q", c.value, p, ok, c.mock, c.live)
		}
	}
}

// ReplaceToken replaces a value only where it stands as a token.
func TestReplaceToken(t *testing.T) {
	for _, c := range []struct {
		s, v, with, want string
		found            bool
	}{
		{"USER:42", "42", "7", "USER:7", true},
		{"order-42x", "42", "7", "order-42x", false},
		{"42/42x/42", "42", "7", "7/42x/7", true},
		{"abcdef", "abcdef", "LIVE01", "LIVE01", true},
		{"Xabcdef9", "abcdef", "LIVE01", "Xabcdef9", false},
		{"", "42", "7", "", false},
		{"42", "", "7", "42", false},
	} {
		got, found := ReplaceToken(c.s, c.v, c.with)
		if got != c.want || found != c.found {
			t.Fatalf("ReplaceToken(%q, %q, %q) = %q, %v; want %q, %v", c.s, c.v, c.with, got, found, c.want, c.found)
		}
	}
}
