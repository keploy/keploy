package replay

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// A set's report says it ran record's full round's learn step, in a keploy
// whose learners class each call they refuse (TestReport.Learn), when it
// did, and only then: under the proven switch, with the learn step on. A
// reader counting those refusals (enterprise's tools/emeasure) reads no other
// report. Every report the replay writes for the set says it, whichever way
// the set ends: the first, RUNNING one and the last of a set it runs, and
// the one report of a set with no test to run, a set it ignores whole, and
// a set an interrupted keploy templatize left half-written.
func TestTheReportSaysTheLearnStepRan(t *testing.T) {
	ignored := func(h *prRun) { h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {}} }
	interrupted := func(h *prRun) { h.replayer.testDB = interruptedSet{h.replayer.testDB.(*prTestDB)} }
	for _, tc := range []struct {
		name    string
		sw      string
		learn   bool
		n       int
		setup   func(*prRun)
		reports int
		want    string
	}{
		{name: "a set it runs", sw: "1", learn: true, n: 2, reports: 2, want: models.LearnWords},
		{name: "a set with no test", sw: "1", learn: true, n: 0, reports: 1, want: models.LearnWords},
		{name: "a set it ignores whole", sw: "1", learn: true, n: 2, setup: ignored, reports: 1, want: models.LearnWords},
		{name: "a set an interrupted templatize left", sw: "1", learn: true, n: 2, setup: interrupted, reports: 1, want: models.LearnWords},
		{name: "no learn step", sw: "1", learn: false, n: 2, reports: 2},
		{name: "no learn step, an ignored set", sw: "1", learn: false, n: 2, setup: ignored, reports: 1},
		{name: "no learn step, an interrupted set", sw: "1", learn: false, n: 2, setup: interrupted, reports: 1},
		{name: "switch off", sw: "", learn: true, n: 2, reports: 2},
		{name: "switch off, no learn step", sw: "", learn: false, n: 2, reports: 2},
		{name: "switch off, a set with no test", sw: "", learn: true, n: 0, reports: 1},
	} {
		t.Run(fmt.Sprintf("%s (switch=%q learn=%v tests=%d)", tc.name, tc.sw, tc.learn, tc.n), func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.sw)
			h := newPartialRunHarness(t, tc.n, 0)
			h.replayer.config.Test.Learn = tc.learn
			h.replayer.mockDB = &learnedStore{prMockDB: h.mocks}
			if tc.setup != nil {
				tc.setup(h)
			}
			_ = h.run(t)
			if len(h.report.reports) < tc.reports {
				t.Fatalf("%d reports were written; want %d at least", len(h.report.reports), tc.reports)
			}
			for i, rep := range h.report.reports {
				if rep.Learn != tc.want {
					t.Errorf("report %d of %d (%s) says learn %q; want %q", i+1, len(h.report.reports), rep.Status, rep.Learn, tc.want)
				}
			}
		})
	}
}

// interruptedSet is a test store whose set an interrupted keploy templatize
// left half-written (refuseInterruptedSet refuses it).
type interruptedSet struct{ *prTestDB }

func (interruptedSet) Interrupted(context.Context, string) error {
	return fmt.Errorf("%w: test-set-0 holds test-1.yaml.tmp: finish or undo the keploy templatize that wrote it", setfile.ErrReplaceInterrupted)
}

// filesStore is a mock store that keeps what the learn step learned and
// writes the set's learned files: it records what each was handed, and its
// files' write fails with fail.
type filesStore struct {
	MockDB
	persisted map[string]models.MockState
	files     map[string]map[string]models.MockState
	fail      error
}

func (s *filesStore) PersistLearned(_ context.Context, _ string, states map[string]models.MockState) (map[string][]string, error) {
	s.persisted = states
	return nil, nil
}

func (s *filesStore) WriteLearnedFiles(set string, states map[string]models.MockState) error {
	if s.files == nil {
		s.files = map[string]map[string]models.MockState{}
	}
	s.files[set] = states
	return s.fail
}

// Once what the learn step learned is on the set's mocks, the set's learned
// files are written from the same states; a store whose files' write fails
// says so in a warning, and the recording is kept all the same.
func TestPersistLearnedWritesTheSetsLearnedFiles(t *testing.T) {
	consumed := map[string]models.MockState{"mock-1": {Name: "mock-1"}}
	store := &filesStore{}
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Replayer{mockDB: store, logger: zap.New(core)}
	if err := r.persistLearned(context.Background(), "test-set-0", consumed, &models.Mapping{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.files, map[string]map[string]models.MockState{"test-set-0": consumed}) || !reflect.DeepEqual(store.persisted, consumed) {
		t.Fatalf("files written of %v, persisted %v, want the set's consumed states", store.files, store.persisted)
	}
	if logs.Len() != 0 {
		t.Fatalf("warned: %v", logs.All())
	}
	store.fail = errors.New("disk full")
	if err := r.persistLearned(context.Background(), "test-set-0", consumed, &models.Mapping{}); err != nil {
		t.Fatalf("a failed write of the learned files failed the set: %v", err)
	}
	if logs.FilterMessageSnippet("its learned files were not written").FilterField(zap.String("testSetID", "test-set-0")).Len() != 1 {
		t.Fatalf("no warning that the learned files were not written: %v", logs.All())
	}
	for _, e := range logs.All() {
		if fmt.Sprint(e.ContextMap()["error"]) != "disk full" {
			t.Fatalf("the warning does not say why: %v", e.ContextMap())
		}
	}
}
