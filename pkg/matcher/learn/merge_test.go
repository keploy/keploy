package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// What a set's first cycle learned is put on a copy of each recording it was
// learned for, as the recording carries it once persisted: noise, the
// mock's own variables, and the variables other mocks bind at it. A
// recording nothing was learned for is handed out as it is.
func TestKeptLearningPutsWhatWasLearnedOnACopyOfTheRecording(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	kept := NewKeptLearning([]models.MockState{
		{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.at": nil}},
		{Name: "mock-2", Binds: map[string]models.MockVariables{"mock-3": {"id": {Recorded: "42", Bind: "body.id"}}}},
		{Name: "mock-4"},
	})
	if kept == nil {
		t.Fatal("learning that carries noise and binds was read as none")
	}
	one := &models.Mock{Name: "mock-1"}
	got := kept.Onto(one)
	if got == one {
		t.Fatal("the learned noise was put on the recording itself, not a copy")
	}
	if one.Spec.ReqBodyNoise != nil {
		t.Fatalf("the recording itself was changed: %v", one.Spec.ReqBodyNoise)
	}
	if _, ok := got.Spec.ReqBodyNoise["body.at"]; !ok {
		t.Fatalf("the learned noise is not on the copy: %v", got.Spec.ReqBodyNoise)
	}
	three := &models.Mock{Name: "mock-3"}
	kept.Into(three)
	if v, ok := three.Spec.Variables["id"]; !ok || v.Bind != "body.id" || v.Recorded != "42" {
		t.Fatalf("the variable another mock binds at mock-3 is not on it: %+v", three.Spec.Variables)
	}
	for _, name := range []string{"mock-2", "mock-4", "mock-9"} {
		m := &models.Mock{Name: name}
		if kept.Onto(m) != m || kept.Has(name) {
			t.Fatalf("%s, which nothing was learned on, was not handed out as it is", name)
		}
	}
	if NewKeptLearning([]models.MockState{{Name: "mock-4"}}) != nil {
		t.Fatal("states that carry no learning were read as learning")
	}
	var none *KeptLearning
	m := &models.Mock{Name: "mock-1"}
	if none.Onto(m) != m {
		t.Fatal("no learning changed a recording")
	}
	none.Into(m)
	if !reflect.DeepEqual(m, &models.Mock{Name: "mock-1"}) {
		t.Fatal("no learning changed a recording")
	}
}

// A set's learning that holds only clock reads is learning (FoldLearned
// keeps its state, KeptLearning puts it on the recording), and noise at a
// clock read's place, learned or on the recording, is dropped: the place
// stays compared. Two states' clock reads for one place merge onto a
// recording as their Meet in either order.
func TestKeptLearningPutsAClockReadOnTheRecordingAndNoNoiseThere(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	ms := time.Millisecond
	ttl := models.ClockOffset{Min: 30*time.Minute - 20*ms, Max: 30*time.Minute + 10*ms, Res: ms}
	later := models.ClockOffset{Min: 30*time.Minute - 5*ms, Max: 30*time.Minute + 40*ms, Res: ms}
	kept := NewKeptLearning([]models.MockState{{Name: "mock-1", Clocks: map[string]models.ClockOffset{"body.exp": ttl}, ReqBodyNoise: map[string][]string{"body.exp": nil}}})
	if !kept.Has("mock-1") {
		t.Fatal("a state with a clock read holds no learning")
	}
	if !NewKeptLearning([]models.MockState{{Name: "mock-2", Clocks: map[string]models.ClockOffset{"body.at": ttl}}}).Has("mock-2") {
		t.Fatal("a state with only a clock read, no noise or variable, holds no learning")
	}
	// Noise at a clock read's place, learned with it or with one on the
	// recording, is not listed as added: the place stays compared.
	onDisk := &models.Mock{Name: "m", Spec: models.MockSpec{Clocks: map[string]models.ClockOffset{"body.exp": ttl}}}
	added := MergeLearned(onDisk, models.MockState{ReqBodyNoise: map[string][]string{"body.exp": nil, "body.at": nil}, Clocks: map[string]models.ClockOffset{"body.at": ttl}}, nil)
	if want := []string{"body.at (" + ttl.String() + ")"}; !reflect.DeepEqual(added, want) || len(onDisk.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("added %v, noise %v; want %v and no noise at either clock read's place", added, onDisk.Spec.ReqBodyNoise, want)
	}
	rec := &models.Mock{Name: "mock-1", Spec: models.MockSpec{ReqBodyNoise: map[string][]string{"body.exp": nil, "body.trace": nil}}}
	got := kept.Onto(rec)
	if !reflect.DeepEqual(got.Spec.Clocks, map[string]models.ClockOffset{"body.exp": ttl}) || len(got.Spec.ReqBodyNoise) != 1 || !hasKey(got.Spec.ReqBodyNoise, "body.trace") {
		t.Fatalf("onto the recording: clocks %+v, noise %v; want body.exp a clock read and no noise there", got.Spec.Clocks, got.Spec.ReqBodyNoise)
	}
	if len(rec.Spec.Clocks) != 0 || len(rec.Spec.ReqBodyNoise) != 2 {
		t.Fatal("Onto changed the recording")
	}
	for _, order := range [][]models.ClockOffset{{ttl, later}, {later, ttl}} {
		m := &models.Mock{Name: "m"}
		var added []string
		for _, o := range order {
			added = append(added, MergeLearned(m, models.MockState{Clocks: map[string]models.ClockOffset{"body.exp": o}}, nil)...)
		}
		want := ttl.Meet(later)
		if m.Spec.Clocks["body.exp"] != want || len(added) != 2 || added[1] != "body.exp ("+want.String()+")" {
			t.Fatalf("%v: clock %+v, added %v; want %+v", order, m.Spec.Clocks, added, want)
		}
		if again := MergeLearned(m, models.MockState{Clocks: map[string]models.ClockOffset{"body.exp": order[0]}}, nil); len(again) != 0 {
			t.Fatalf("a clock read already held is added again: %v", again)
		}
	}
	t.Setenv("KEPLOY_PROVEN", "")
	m := &models.Mock{Name: "m", Spec: models.MockSpec{Clocks: map[string]models.ClockOffset{"body.exp": ttl}}}
	if added := MergeLearned(m, models.MockState{ReqBodyNoise: map[string][]string{"body.exp": nil}, Clocks: map[string]models.ClockOffset{"body.at": ttl}}, nil); !reflect.DeepEqual(added, []string{"body.exp (noise)"}) || !reflect.DeepEqual(m.Spec.Clocks, map[string]models.ClockOffset{"body.exp": ttl}) {
		t.Fatalf("switch off: added %v, clocks %+v; want noise merged as always and the clock reads as they were", added, m.Spec.Clocks)
	}
}

func hasKey(m map[string][]string, k string) bool { _, ok := m[k]; return ok }

// A value test B both binds for itself (create, then read it back) and a
// later test C reuses is one variable: the run's, which C binds at B's
// create, takes the place, and B's own read reads it too. A recording place
// so never carries two names (a replay refuses one that does). Test D's own
// variable of the same name is D's and stays so; nothing is changed without
// a run's bind, and the states handed in are left as they were.
func TestTheRunsVariableTakesTheEarlierTestsOwnPlace(t *testing.T) {
	v := func(bind string, use ...string) models.MockVariable {
		return models.MockVariable{Recorded: "o-1", Bind: bind, Use: use}
	}
	states := map[string]models.MockState{
		"mock-0": {Name: "mock-0"},
		"mock-1": {Name: "mock-1", Variables: models.MockVariables{"order": v("", "path1")},
			Binds: map[string]models.MockVariables{"mock-0": {"order": v("body.order")}}},
		"mock-2": {Name: "mock-2", Variables: models.MockVariables{"run-order": v("", "path1")},
			Binds: map[string]models.MockVariables{"mock-0": {"run-order": v("body.order")}}},
		"mock-5": {Name: "mock-5", Variables: models.MockVariables{"order": {Recorded: "o-3", Bind: "body.order", Echo: true, Answer: []string{"body.id"}}}},
		"mock-6": {Name: "mock-6", Variables: models.MockVariables{"order": {Recorded: "o-3", Use: []string{"path1"}}},
			Binds: map[string]models.MockVariables{"mock-5": {"order": {Recorded: "o-3", Bind: "body.order"}}}},
	}
	owner := map[string]string{"mock-0": "TestB", "mock-1": "TestB", "mock-2": "TestC", "mock-5": "TestD", "mock-6": "TestD"}
	testOf := func(m string) string { return owner[m] }
	before := map[string]models.MockState{}
	for k, st := range states {
		before[k] = st
	}
	out, taken := RunTakesPlaces(states, testOf)
	if want := []Taken{{Test: "TestB", Own: "order", Run: "run-order", Mock: "mock-0", Place: "body.order"}}; !reflect.DeepEqual(taken, want) {
		t.Fatalf("taken %+v; want %+v", taken, want)
	}
	if got := out["mock-1"]; !reflect.DeepEqual(got.Variables, models.MockVariables{"run-order": v("", "path1")}) ||
		!reflect.DeepEqual(got.Binds, map[string]models.MockVariables{"mock-0": {"run-order": v("body.order")}}) {
		t.Fatalf("test B's read: %+v; want it to read the run's variable, bound at its create", got)
	}
	for _, m := range []string{"mock-0", "mock-2", "mock-5", "mock-6"} {
		if !reflect.DeepEqual(out[m], states[m]) {
			t.Fatalf("%s became %+v; want it as it was (%+v)", m, out[m], states[m])
		}
	}
	if !reflect.DeepEqual(states, before) || states["mock-1"].Variables["order"].Use[0] != "path1" {
		t.Fatalf("the states handed in changed: %+v", states)
	}
	if _, binds := FoldLearned(out); len(binds["mock-0"]) != 1 {
		t.Fatalf("test B's create binds %+v; want one variable at its place", binds["mock-0"])
	}

	// A run's variable bound where test B's own one is used (its read's
	// path) takes it too; the lesser run's name takes it where two are
	// bound at its places, whatever the order the states are read in.
	states["mock-3"] = models.MockState{Name: "mock-3", Variables: models.MockVariables{"run-a": v("", "path1")},
		Binds: map[string]models.MockVariables{"mock-1": {"run-a": v("path1")}}}
	owner["mock-3"] = "TestE"
	for i := 0; i < 20; i++ {
		_, taken = RunTakesPlaces(states, testOf)
		if want := []Taken{{Test: "TestB", Own: "order", Run: "run-a", Mock: "mock-1", Place: "path1"}}; !reflect.DeepEqual(taken, want) {
			t.Fatalf("taken %+v; want %+v", taken, want)
		}
	}

	// With no run's bind, or no test's own name at its place, nothing.
	alone := map[string]models.MockState{"mock-5": states["mock-5"], "mock-6": states["mock-6"]}
	if out, taken := RunTakesPlaces(alone, testOf); taken != nil || !reflect.DeepEqual(out, alone) {
		t.Fatalf("no run's variable: %+v %+v; want nothing taken", out, taken)
	}
	delete(owner, "mock-1")
	delete(owner, "mock-0")
	if _, taken := RunTakesPlaces(states, testOf); taken != nil {
		t.Fatalf("a state of no test: taken %+v; want none (the start-up's carry no test's variable)", taken)
	}
}
