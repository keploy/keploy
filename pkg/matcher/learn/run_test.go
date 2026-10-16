package learn

import (
	"testing"
	"time"
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
	}
	for _, c := range []struct {
		value, mock, live string
	}{
		{"BJBM-NZBS", "mock-1", "FRST-XKCD"},
		{"1792000000000", "mock-2", "1792000555000"},
		{"12", "", ""},
		{"pending", "", ""},
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
