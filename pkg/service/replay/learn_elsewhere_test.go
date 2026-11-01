package replay

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// keploy record's agent client says what the learn step taught across
// windows: with no such read, keploy record's full round would write a
// timer's recording with nothing learned, every row of the learning green.
var _ learn.LearnedElsewhereReader = (*agenthttp.AgentClient)(nil)

// elsewhereAgent is the harness's agent whose learn step taught recordings
// from calls made in other windows than theirs (GetLearnedElsewhere); asked
// counts the reads.
type elsewhereAgent struct {
	*prInstr
	taught []models.MockState
	err    error
	asked  int
}

func (e *elsewhereAgent) GetLearnedElsewhere(context.Context) ([]models.MockState, error) {
	e.asked++
	return e.taught, e.err
}

// SendTestCaseFilter is UpdateMockParams, answered as an agent whose learn
// step took each test case's request (pkg.TestCaseFilterSender).
func (e *elsewhereAgent) SendTestCaseFilter(ctx context.Context, p models.MockFilterParams) (bool, error) {
	return true, e.UpdateMockParams(ctx, p)
}

// addingStore keeps what the learn step learned, and says it added each
// clock read it was handed, as the yaml store does: "<path> (<clock read>)".
type addingStore struct {
	*prMockDB
	persisted map[string]models.MockState
}

func (a *addingStore) PersistLearned(_ context.Context, _ string, states map[string]models.MockState) (map[string][]string, error) {
	a.persisted = states
	added := map[string][]string{}
	for name, st := range states {
		for path, o := range st.Clocks {
			added[name] = append(added[name], path+" ("+o.String()+")")
		}
	}
	return added, nil
}

// keploy record's full round writes what a call made in another window than
// its recording's taught the recording, as keploy mock record's does: the
// timer's recording no test consumed carries its clock read, and nothing
// else such a call showed (its noise). A recording a call of the round
// consumed keeps what its own pairing learned alone: what another window
// taught it is voided, and the record says which evidence won. The record
// says the taught recording apart, under the test whose recording it is
// (the set's recorded mapping), with the window that taught it.
func TestRunTestSet_KeployRecordWritesWhatACallInAnotherWindowTaughtOntoTheRecording(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.Learn = true
	store := &addingStore{prMockDB: h.mocks}
	h.replayer.mockDB = store
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	agent := &elsewhereAgent{prInstr: h.instr, taught: []models.MockState{
		{Name: "timer-mock", Kind: models.Mongo, Clocks: map[string]models.ClockOffset{"claimed_at": minute}, ReqBodyNoise: map[string][]string{"docs._id": {}}, TaughtIn: []models.TeachingWindow{{Test: "test-2"}}},
		{Name: "own-mock", Kind: models.HTTP, Clocks: map[string]models.ClockOffset{"dirty_at": minute}, TaughtIn: []models.TeachingWindow{{Test: "test-2"}}},
	}}
	h.replayer.instrumentation = agent
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "own-mock", Kind: models.HTTP}}}
	core, logs := observer.New(zap.InfoLevel)
	h.replayer.logger = zap.New(core)

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (%s), want PASSED: learning across windows decides no verdict", status, h.report.report.FailureReason)
	}
	if agent.asked == 0 {
		t.Fatal("keploy record's full round never asked the agent what calls in other windows taught")
	}
	timer, ok := store.persisted["timer-mock"]
	if !ok || !reflect.DeepEqual(timer.Clocks, map[string]models.ClockOffset{"claimed_at": minute}) || timer.ReqBodyNoise != nil {
		t.Fatalf("the timer's recording was written %+v (kept %v); want its clock read alone", timer, ok)
	}
	if own := store.persisted["own-mock"]; own.Clocks != nil {
		t.Fatalf("the consumed recording was written %+v; want what its own pairing learned alone (nothing)", own)
	}
	learned := logs.FilterMessageSnippet("from calls made in other windows").AllUntimed()
	if len(learned) != 1 || learned[0].ContextMap()["mock"] != "timer-mock" || learned[0].ContextMap()["taughtIn"] != "test-2's window" {
		t.Fatalf("the learned lines %+v; want timer-mock's alone, naming test-2's window", learned)
	}
	for _, e := range logs.FilterMessage("learned fields that changed between record and replay").AllUntimed() {
		t.Fatalf("a test's own learned line %+v names what another window taught", e.ContextMap())
	}
	void := logs.FilterMessageSnippet("voided what a call made in another window taught").AllUntimed()
	if len(void) != 1 || void[0].ContextMap()["mock"] != "own-mock" {
		t.Fatalf("the voided lines %+v; want own-mock's", void)
	}
}

// What a call made in another window taught is that window's learning: a
// window whose test's request the agent did not say it read keeps nothing,
// as that test's own recordings keep nothing (readLearning), and neither
// does a window keploy did not know the test of while any such test ran.
// An agent that cannot say, or whose read failed, teaches nothing.
func TestWhatAWindowWhoseRequestWasUnreadTaughtIsNotKept(t *testing.T) {
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	taught := []models.MockState{
		{Name: "in-read", Clocks: map[string]models.ClockOffset{"a": minute}, TaughtIn: []models.TeachingWindow{{Test: "test-1"}}},
		{Name: "in-unread", Clocks: map[string]models.ClockOffset{"a": minute}, TaughtIn: []models.TeachingWindow{{Test: "test-2"}}},
		{Name: "between", Clocks: map[string]models.ClockOffset{"a": minute}, TaughtIn: []models.TeachingWindow{{}}},
	}
	core, logs := observer.New(zap.InfoLevel)
	r := &Replayer{logger: zap.New(core), instrumentation: &elsewhereAgent{taught: taught}}
	if got := r.learnedElsewhere(context.Background(), "s"); len(got) != 3 {
		t.Fatalf("kept %d of 3 with every request read", len(got))
	}
	r.requestsUnread.note("test-2", "v3.0.0")
	got := r.learnedElsewhere(context.Background(), "s")
	if len(got) != 1 || got[0].Name != "in-read" {
		t.Fatalf("kept %+v; want in-read alone", got)
	}
	if n := logs.FilterMessageSnippet("did not say it read the request").Len(); n != 2 {
		t.Fatalf("%d lines say what was not kept; want 2", n)
	}
	r.instrumentation = &elsewhereAgent{taught: taught, err: errors.New("agent gone")}
	if got := r.learnedElsewhere(context.Background(), "s"); got != nil {
		t.Fatalf("a failed read kept %+v", got)
	}
	r.instrumentation = &prInstr{}
	if got := r.learnedElsewhere(context.Background(), "s"); got != nil {
		t.Fatalf("an agent that cannot say kept %+v", got)
	}
}

// What the learn step learned for a test whose request the agent did not read
// is not kept (readLearning), but the test's own pairing happened: the state
// stays, emptied, so what another window taught that recording is voided as
// on any recording a call of the round consumed.
func TestARecordingAnUnreadTestConsumedStillVoidsWhatAnotherWindowTaughtIt(t *testing.T) {
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	store := &addingStore{}
	core, logs := observer.New(zap.InfoLevel)
	r := &Replayer{logger: zap.New(core), mockDB: store}
	r.requestsUnread.note("test-1", "v3.0.0")
	kept, _, _ := r.readLearning("s", map[string]models.MockState{
		"own-mock": {Name: "own-mock", Clocks: map[string]models.ClockOffset{"lease_at": minute}},
	}, map[string][]string{"own-mock": {"test-1"}})
	elsewhere := []models.MockState{{Name: "own-mock", Clocks: map[string]models.ClockOffset{"dirty_at": minute}, TaughtIn: []models.TeachingWindow{{Test: "test-2"}}}}
	if err := r.persistLearned(context.Background(), "s", kept, elsewhere, map[string]string{"own-mock": "test-1"}, &models.Mapping{}); err != nil {
		t.Fatal(err)
	}
	if own := store.persisted["own-mock"]; len(own.Clocks) != 0 {
		t.Fatalf("the recording was written %+v; want nothing: its test's learning is not kept, and another window's is voided", own)
	}
	if void := logs.FilterMessageSnippet("voided what a call made in another window taught").AllUntimed(); len(void) != 1 || void[0].ContextMap()["mock"] != "own-mock" {
		t.Fatalf("the voided lines %+v; want own-mock's", void)
	}
}

// The record names a recording another window taught under the test whose
// recording it is, by the set's recorded mapping: no test consumed it, so
// the run's own mapping lists it nowhere, and it is never said as the set's
// start-up and pool's when its test is known. A recording no call of the
// round consumed and none taught across windows is said on its test's line.
func TestKeployRecordSaysWhatAnotherWindowTaughtUnderTheRecordingsOwner(t *testing.T) {
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	store := &addingStore{}
	core, logs := observer.New(zap.InfoLevel)
	r := &Replayer{logger: zap.New(core), mockDB: store}
	consumed := map[string]models.MockState{"own-mock": {Name: "own-mock", Clocks: map[string]models.ClockOffset{"at": minute}}}
	elsewhere := []models.MockState{
		{Name: "timer-mock", Clocks: map[string]models.ClockOffset{"claimed_at": minute}, TaughtIn: []models.TeachingWindow{{Test: "test-2", At: time.Unix(2, 0)}}},
		{Name: "timer-mock", Clocks: map[string]models.ClockOffset{"expires_at": minute}, TaughtIn: []models.TeachingWindow{{At: time.Unix(1, 0)}}},
	}
	owner := ownerOf(map[string][]models.MockEntry{"test-1": {{Name: "timer-mock"}}, "test-2": {{Name: "own-mock"}}})
	actual := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "test-2", Mocks: []models.MockEntry{{Name: "own-mock"}}}}}
	if err := r.persistLearned(context.Background(), "s", consumed, elsewhere, owner, actual); err != nil {
		t.Fatal(err)
	}
	if got := store.persisted["timer-mock"].Clocks; len(got) != 2 {
		t.Fatalf("timer-mock written %v; want both teachings merged", got)
	}
	learned := logs.FilterMessageSnippet("from calls made in other windows").AllUntimed()
	if len(learned) != 1 || learned[0].ContextMap()["test"] != "test-1" || learned[0].ContextMap()["taughtIn"] != "between tests, test-2's window" {
		t.Fatalf("the learned lines %+v; want timer-mock's under its owner test-1, its windows in time order", learned)
	}
	own := logs.FilterMessage("learned fields that changed between record and replay").AllUntimed()
	if len(own) != 1 || own[0].ContextMap()["test"] != "test-2" {
		t.Fatalf("the tests' own lines %+v; want test-2's alone", own)
	}
}

// extraStore is addingStore that also says it added a field at a place no
// clock read is, on the recording extra names, as a store that wrote more
// than it was handed would.
type extraStore struct {
	addingStore
	extra string
}

func (e *extraStore) PersistLearned(ctx context.Context, set string, states map[string]models.MockState) (map[string][]string, error) {
	added, err := e.addingStore.PersistLearned(ctx, set, states)
	if _, ok := states[e.extra]; ok {
		added[e.extra] = append(added[e.extra], "docs.state (noise)")
	}
	return added, err
}

// Learning across windows teaches clock reads alone, so keploy record says
// as taught by another window only what the store wrote at a clock read's
// place, as keploy mock record does: anything else written onto the
// recording is said as the recording's own, on the line of mocks no test
// consumed, never as a call in another window's. A round that consumed
// nothing (nil) says it as well.
func TestKeployRecordSaysOnlyClockReadsAsTaughtByAnotherWindow(t *testing.T) {
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	store := &extraStore{extra: "timer-mock"}
	core, logs := observer.New(zap.InfoLevel)
	r := &Replayer{logger: zap.New(core), mockDB: store}
	elsewhere := []models.MockState{{Name: "timer-mock", Clocks: map[string]models.ClockOffset{"claimed_at": minute}, TaughtIn: []models.TeachingWindow{{Test: "test-2"}}}}
	if err := r.persistLearned(context.Background(), "s", nil, elsewhere, map[string]string{"timer-mock": "test-1"}, &models.Mapping{}); err != nil {
		t.Fatal(err)
	}
	learned := logs.FilterMessageSnippet("from calls made in other windows").AllUntimed()
	want := []interface{}{"claimed_at (" + minute.String() + ")"}
	if len(learned) != 1 || !reflect.DeepEqual(learned[0].ContextMap()["fields"], want) {
		t.Fatalf("the learned lines %+v; want timer-mock's clock read alone as taught in another window", learned)
	}
	own := logs.FilterMessage("learned fields that changed between record and replay").AllUntimed()
	if len(own) != 1 || !reflect.DeepEqual(own[0].ContextMap()["fields"], []interface{}{"timer-mock docs.state (noise)"}) {
		t.Fatalf("the own lines %+v; want the noise said as the recording's own", own)
	}
}
