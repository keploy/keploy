package learn

import (
	"reflect"
	"testing"

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
