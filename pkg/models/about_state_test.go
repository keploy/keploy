package models

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// The sum is the first 8 hex characters of the sha256 of the JSON array
// [kind, key, fields, run], so another tool (keploy mock check) can make it
// again from the decision as it reads on disk. Pinned byte for byte.
func TestAboutSumRecipe(t *testing.T) {
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:8] }
	run := "2026-10-27T05:00:00Z"
	if got, want := NoiseSum("body.eventId", run), sum(`["noise","body.eventId",[],"2026-10-27T05:00:00Z"]`); got != want {
		t.Fatalf("noise sum %s, want %s", got, want)
	}
	v := MockVariable{Recorded: "6adf", Bind: "body.id", Use: []string{"b", "a"}, Echo: true,
		Clock: []ClockRead{{At: time.Date(2026, 10, 27, 5, 0, 0, 0, time.FixedZone("x", 3600)), Res: time.Millisecond}}}
	if got, want := VariableSum("_id", v, run), sum(`["variable","_id",["6adf","body.id",["a","b"],[],true,[["2026-10-27T04:00:00Z",1000000,false]]],"2026-10-27T05:00:00Z"]`); got != want {
		t.Fatalf("variable sum %s, want %s", got, want)
	}
	o := ClockOffset{Min: -time.Second, Res: time.Millisecond, Zone: "UTC", Layout: "rfc3339"}
	if got, want := ClockSum("body.at", o, run), sum(`["clock","body.at",[-1000000000,0,1000000,"UTC","rfc3339"],"2026-10-27T05:00:00Z"]`); got != want {
		t.Fatalf("clock sum %s, want %s", got, want)
	}
}

// Row 8: an entry reads as one of four states. Learned with a matching sum;
// learned with a missing or wrong one (edited since); hand (or a by keploy
// does not write); no about (unknown origin). Reordering a variable's places
// is no edit; adding one is.
func TestAboutReadsAsOneOfFourStates(t *testing.T) {
	run := "2026-10-27T05:00:00Z"
	v := MockVariable{Recorded: "6adf", Bind: "body.id", Use: []string{"body.a", "body.b"}}
	learned := About{Run: run}.Stamped(AboutKindVariable, "_id", false, func(r string) string { return VariableSum("_id", v, r) })
	cases := []struct {
		name  string
		about *About
		edit  func(*MockVariable)
		want  AboutState
	}{
		{"no about", nil, nil, AboutUnknown},
		{"learned", &learned, nil, AboutLearned},
		{"learned, places reordered", &learned, func(v *MockVariable) { v.Use = []string{"body.b", "body.a"} }, AboutLearned},
		{"learned, a use place added", &learned, func(v *MockVariable) { v.Use = append(v.Use, "body.c") }, AboutEdited},
		{"learned, recorded edited", &learned, func(v *MockVariable) { v.Recorded = "7" }, AboutEdited},
		{"learned, sum removed", &About{By: AboutByLearned, Run: run}, nil, AboutEdited},
		{"learned, run edited", &About{By: AboutByLearned, Run: "2026-10-28T05:00:00Z", Sum: learned.Sum}, nil, AboutEdited},
		{"hand", &About{By: AboutByHand}, nil, AboutHand},
		{"a by keploy does not write", &About{By: "person", Sum: learned.Sum, Run: run}, nil, AboutHand},
	}
	for _, c := range cases {
		w := v
		w.Use = append([]string(nil), v.Use...)
		w.About = c.about
		if c.edit != nil {
			c.edit(&w)
		}
		if got := VariableAboutState("_id", w); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if got := VariableAboutState("_id", w).Hand(); got != (c.want == AboutEdited || c.want == AboutHand) {
			t.Errorf("%s: Hand() %v", c.name, got)
		}
	}

	spec := &MockSpec{
		Clocks:     map[string]ClockOffset{"body.at": {Res: time.Millisecond}},
		NoiseAbout: map[string]About{"body.x": About{Run: run}.Stamped(AboutKindNoise, "body.x", false, func(r string) string { return NoiseSum("body.x", r) })},
	}
	spec.ClockAbout = map[string]About{"body.at": About{Run: run}.Stamped(AboutKindClock, "body.at", false, func(r string) string { return ClockSum("body.at", spec.Clocks["body.at"], r) })}
	if NoiseAboutState(spec, "body.x") != AboutLearned || NoiseAboutState(spec, "body.y") != AboutUnknown || ClockAboutState(spec, "body.at") != AboutLearned {
		t.Fatal("learned noise or clock read does not read as learned")
	}
	spec.Clocks["body.at"] = ClockOffset{Res: time.Second}
	if ClockAboutState(spec, "body.at") != AboutEdited {
		t.Fatal("a clock read edited by hand reads as learned")
	}
}

// The fixed sentences hold no value and name no mock, and each undo names
// the place it undoes.
func TestTheFixedSentences(t *testing.T) {
	for _, kind := range []string{AboutKindNoise, AboutKindVariable, AboutKindClock} {
		for _, round := range []string{AboutRoundFull, AboutRoundKeployTest} {
			for _, echo := range []bool{false, true} {
				why, undo := AboutWhy(kind, echo, round), AboutUndo(kind, "body.eventId", round)
				if why == "" || !strings.Contains(undo, "body.eventId") || strings.Contains(why+undo, "mock-") {
					t.Errorf("%s %s echo=%v: why %q undo %q", kind, round, echo, why, undo)
				}
			}
		}
	}
}
