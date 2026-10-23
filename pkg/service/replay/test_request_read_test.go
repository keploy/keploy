package replay

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// readsInstr is the harness's agent, which answers every filter saying its
// learn step read the test case's request it carried, or not
// (pkg.TestRequestReader).
type readsInstr struct {
	*prInstr
	reads bool
}

func (a *readsInstr) AgentReadTestRequest() bool { return a.reads }

// What an agent learned on the mocks a test consumed is kept only where the
// agent said it read the test case's request the opening filter carried: an
// agent older than the replay ignores the request, and a value the
// application took from it may have been learned as noise, an echo, or a
// variable whose bind takes any value. None of it is kept otherwise. An
// instrumentation that cannot say is an agent that did not.
func TestRunTestSet_TheFullRoundKeepsNoNoiseAnAgentLearnedWithoutTheRequest(t *testing.T) {
	learned := models.MockState{Name: "mock-1", Kind: models.HTTP, ReqBodyNoise: map[string][]string{"body.at": {}},
		Variables: models.MockVariables{"echo_1": {Recorded: "r-1", Bind: "body.rid", Echo: true}, "id_1": {Recorded: "u-1", Use: []string{"body.id"}}},
		Binds:     map[string]models.MockVariables{"mock-0": {"id_1": {Recorded: "u-1", Bind: "body.id"}}}}
	for _, tc := range []struct {
		name  string
		reads *bool
	}{{"an agent that read it", ptr(true)}, {"an agent that did not", ptr(false)}, {"an instrumentation that cannot say", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.Learn = true
			store := &learnedStore{prMockDB: h.mocks}
			h.replayer.mockDB = store
			if tc.reads != nil {
				h.replayer.instrumentation = &readsInstr{prInstr: h.instr, reads: *tc.reads}
			}
			h.replayer.hookImpl = prHooks{consumed: []models.MockState{learned}}
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s, want PASSED", status)
			}
			got, ok := store.persisted["mock-1"]
			if !ok {
				t.Fatalf("persisted %v lacks mock-1", store.persisted)
			}
			if tc.reads != nil && *tc.reads {
				if !reflect.DeepEqual(got.ReqBodyNoise, learned.ReqBodyNoise) || !reflect.DeepEqual(got.Variables, learned.Variables) || !reflect.DeepEqual(got.Binds, learned.Binds) {
					t.Fatalf("kept %+v; want what was learned", got)
				}
				return
			}
			if got.ReqBodyNoise != nil || got.Variables != nil || got.Binds != nil {
				t.Fatalf("kept %+v; want nothing the agent learned", got)
			}
			if len(learned.Variables) != 2 || len(learned.ReqBodyNoise) != 1 || len(learned.Binds) != 1 {
				t.Fatal("the consumed state itself was changed")
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// withoutUnreadLearning takes everything learned off the mocks a test whose
// request was not read consumed, and nothing off the others, and says what
// it took, sorted, as one text for one input.
func TestWithoutUnreadLearning(t *testing.T) {
	consumed := map[string]models.MockState{
		"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.b": {}, "body.a": {}}, Variables: models.MockVariables{"e": {Echo: true}, "v": {Use: []string{"body.v"}}},
			Binds: map[string]models.MockVariables{"mock-0": {"v": {Bind: "body.v"}}}},
		"mock-2": {Name: "mock-2", ReqBodyNoise: map[string][]string{"body.c": {}}},
		"boot":   {Name: "boot", ReqBodyNoise: map[string][]string{"body.d": {}}},
	}
	consumers := map[string][]string{"mock-1": {"test-1", "test-2"}, "mock-2": {"test-2"}}
	for i := 0; i < 50; i++ {
		kept, dropped := withoutUnreadLearning(consumed, consumers, map[string]bool{"test-1": true})
		if want := "mock-1 body.a (noise),mock-1 body.b (noise),mock-1 e (echo),mock-1 v (bound at mock-0),mock-1 v (variable)"; strings.Join(dropped, ",") != want {
			t.Fatalf("dropped %v; want %s", dropped, want)
		}
		if kept["mock-1"].ReqBodyNoise != nil || kept["mock-1"].Variables != nil || kept["mock-1"].Binds != nil || len(kept["mock-2"].ReqBodyNoise) != 1 || len(kept["boot"].ReqBodyNoise) != 1 {
			t.Fatalf("kept %+v", kept)
		}
	}
	if len(consumed["mock-1"].ReqBodyNoise) != 2 {
		t.Fatal("the consumed states were changed")
	}
	if kept, dropped := withoutUnreadLearning(consumed, consumers, nil); dropped != nil || !reflect.DeepEqual(kept, consumed) {
		t.Fatalf("with every request read: kept %v, dropped %v", kept, dropped)
	}
}
