package learn

import (
	"strings"
	"testing"
)

// A page that read the same call under two sessions the test switched between
// (a login the test signed itself, given to keploy by its harness) recorded
// calls that differ only in the session. PairGiven tells them apart by the
// session the request carries; Pair cannot, and takes the first in recorded
// order.
func TestPairGivenTellsSessionsApart(t *testing.T) {
	spent := func(s Shape) Candidate { return Candidate{Shape: s, Known: None, Spent: true} }
	// The harness gave tok-A (session A) and tok-B (session B); this run's are tok-A2 and tok-B2.
	given := func(rec, live Leaf) bool {
		return rec.Value != "" && map[string]string{"tok-A": "tok-A2", "tok-B": "tok-B2"}[rec.Value] == live.Value
	}
	read := func(session string) Shape {
		return body(t, "k", `{"op":"GetBranches","session":"`+session+`"}`)
	}

	t.Run("a read once more under a session is one call more than recorded", func(t *testing.T) {
		cands := []Candidate{spent(read("tok-A")), cand(read("tok-B"), true)}
		if r := Pair(read("tok-A2"), cands); r.Outcome != Paired || r.Index != 1 {
			t.Fatalf("Pair: %+v; want the other session's recording paired, as before", r)
		}
		r := PairGiven(read("tok-A2"), cands, given)
		if r.Outcome != Miss || r.Index != 0 || r.Reason != "" || !strings.Contains(r.Why, "one call more") || !r.SpentExact {
			t.Fatalf("PairGiven: %+v; want one call more than recorded, on session A's recording", r)
		}
	})
	t.Run("only an exact repeat of a spent recording is one call more by SpentExact", func(t *testing.T) {
		if r := Pair(body(t, "k", `{"id":"a"}`), []Candidate{spent(body(t, "k", `{"id":"a"}`)), cand(body(t, "k", `{"id":"b"}`), true)}); !r.SpentExact {
			t.Fatalf("an exact repeat of a spent recording: %+v; want SpentExact", r)
		}
		r := Pair(body(t, "k", `{"id":"z","x":1}`), []Candidate{spent(body(t, "k", `{"id":"a","x":1}`)), cand(body(t, "k", `{"id":"a","x":2}`), true)})
		if r.Outcome != Miss || r.SpentExact {
			t.Fatalf("a spent recording that is only the closest: %+v; want a miss without SpentExact", r)
		}
	})
	t.Run("a read is paired with its own session's recording", func(t *testing.T) {
		cands := []Candidate{cand(read("tok-A"), true), cand(read("tok-B"), true)}
		if r := Pair(read("tok-B2"), cands); r.Outcome != Paired || r.Index != 0 {
			t.Fatalf("Pair: %+v; want the first recording paired, as before", r)
		}
		r := PairGiven(read("tok-B2"), cands, given)
		if r.Outcome != Paired || r.Index != 1 {
			t.Fatalf("PairGiven: %+v; want session B's recording paired", r)
		}
	})
	t.Run("a value no harness gave is paired as before", func(t *testing.T) {
		cands := []Candidate{cand(read("tok-A"), true), cand(read("tok-B"), true)}
		if r, want := PairGiven(read("tok-Z"), cands, given), Pair(read("tok-Z"), cands); r.Outcome != want.Outcome || r.Index != want.Index {
			t.Fatalf("PairGiven %+v; want Pair's %+v", r, want)
		}
	})
	t.Run("a recording of the own session that drifted elsewhere too is compared as before", func(t *testing.T) {
		live := body(t, "k", `{"op":"GetBranch","session":"tok-B2"}`)
		cands := []Candidate{cand(read("tok-A"), true), cand(read("tok-B"), true)}
		if r, want := PairGiven(live, cands, given), Pair(live, cands); r.Outcome != want.Outcome || r.Index != want.Index {
			t.Fatalf("PairGiven %+v; want Pair's %+v", r, want)
		}
	})
	t.Run("a recording the learn step may not learn onto is never paired for its session", func(t *testing.T) {
		cands := []Candidate{cand(read("tok-B"), false)}
		r := PairGiven(read("tok-B2"), cands, given)
		if want := Pair(read("tok-B2"), cands); r.Outcome != want.Outcome || r.Index != want.Index || r.Outcome == Paired {
			t.Fatalf("PairGiven %+v; want Pair's %+v, not paired", r, want)
		}
	})
	t.Run("nothing given is Pair", func(t *testing.T) {
		cands := []Candidate{spent(read("tok-A")), cand(read("tok-B"), true)}
		if r, want := PairGiven(read("tok-A2"), cands, nil), Pair(read("tok-A2"), cands); r.Outcome != want.Outcome || r.Index != want.Index {
			t.Fatalf("PairGiven %+v; want Pair's %+v", r, want)
		}
	})
}
