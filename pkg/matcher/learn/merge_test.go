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
		t.Fatal("a state with only a clock read holds no learning")
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
