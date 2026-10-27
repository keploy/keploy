package replay

import (
	"context"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestLearnedByTest(t *testing.T) {
	added := map[string][]string{
		"mock-1": {"body.id (noise)"},
		"mock-2": {"body.key (echo key)", "header.x (noise)"},
		"mock-9": {"url.path.1 (noise)"},
	}
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}, {Name: "mock-3"}}},
		{ID: "test-2", Mocks: []models.MockEntry{{Name: "mock-2"}}},
		{ID: "test-3", Mocks: []models.MockEntry{{Name: "mock-4"}}},
	}}
	got := learnedByTest(added, mapping)
	want := []learnedLine{
		{test: "test-1", fields: []string{"mock-1 body.id (noise)"}},
		{test: "test-2", fields: []string{"mock-2 body.key (echo key)", "mock-2 header.x (noise)"}},
		{fields: []string{"mock-9 url.path.1 (noise)"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if learnedByTest(nil, mapping) != nil {
		t.Fatal("nothing learned says nothing")
	}
}

// What a person's entries kept from being applied is said once in the
// set's learn summary, apart from what was learned.
func TestPersistLearnedSaysOnceWhatAPersonsEntriesKept(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	store := &learnedStore{prMockDB: &prMockDB{},
		added: map[string][]string{"mock-1": {"body.id (noise)"}},
		blocked: map[string][]string{
			"mock-2": {"body.at (clock); kept the entry there, written by hand"},
			"mock-1": {"body.exp (noise); kept the entry there, written by hand"},
		}}
	r := &Replayer{mockDB: store, logger: zap.New(core)}
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}, {Name: "mock-2"}}}}}
	if err := r.persistLearned(context.Background(), "test-set-0", map[string]models.MockState{"mock-1": {Name: "mock-1"}}, mapping); err != nil {
		t.Fatal(err)
	}
	learned := logs.FilterMessage("learned fields that changed between record and replay").All()
	if len(learned) != 1 || !reflect.DeepEqual(learned[0].ContextMap()["fields"], []any{"mock-1 body.id (noise)"}) {
		t.Fatalf("what was learned: %+v", learned)
	}
	lines := logs.FilterMessage(learn.BlockedMessage).All()
	if len(lines) != 1 {
		t.Fatalf("the blocked list was said %d times, want once", len(lines))
	}
	want := []any{"mock-1 body.exp (noise); kept the entry there, written by hand", "mock-2 body.at (clock); kept the entry there, written by hand"}
	if c := lines[0].ContextMap(); !reflect.DeepEqual(c["fields"], want) || lines[0].Level != zap.WarnLevel || c["testSetID"] != "test-set-0" || c["next_step"] != learn.BlockedNextStep {
		t.Fatalf("the blocked line: %v %v", lines[0].Level, c)
	}
	store.blocked = nil
	logs.TakeAll()
	if err := r.persistLearned(context.Background(), "test-set-0", map[string]models.MockState{"mock-1": {Name: "mock-1"}}, mapping); err != nil {
		t.Fatal(err)
	}
	if n := logs.FilterMessage(learn.BlockedMessage).Len(); n != 0 {
		t.Fatalf("nothing blocked was said %d times", n)
	}
}

// The test written into each learned decision's about is the test that
// made a shared recording's call (consumerOf), else the test whose mapping
// holds the mock (learn.StampStates).
func TestLearnedStampNamesTheTestThatWasOpen(t *testing.T) {
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}, {Name: "mock-shared"}}},
		{ID: "test-2", Mocks: []models.MockEntry{{Name: "mock-2"}, {Name: "mock-shared"}}},
	}}
	s := learnedStamp(mapping, map[string]string{"mock-shared": "test-2"})
	want := map[string]string{"mock-1": "test-1", "mock-2": "test-2", "mock-shared": "test-2"}
	if !reflect.DeepEqual(s.Test, want) || s.Round != models.AboutRoundFull || s.Run == "" {
		t.Fatalf("stamp: %+v, want tests %v", s, want)
	}
}

// decisions is st with no about (MockState.NoiseAbout, ClockAbout, and each
// variable's and bind's About): what was learned, without where it came
// from, which a replay never reads.
func decisions(st models.MockState) models.MockState {
	st.NoiseAbout, st.ClockAbout = nil, nil
	strip := func(vs models.MockVariables) models.MockVariables {
		if vs == nil {
			return nil
		}
		out := vs.Clone()
		for n, v := range out {
			v.About = nil
			out[n] = v
		}
		return out
	}
	st.Variables = strip(st.Variables)
	if st.Binds != nil {
		binds := make(map[string]models.MockVariables, len(st.Binds))
		for t, vs := range st.Binds {
			binds[t] = strip(vs)
		}
		st.Binds = binds
	}
	return st
}
