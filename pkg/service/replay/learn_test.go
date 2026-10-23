package replay

import (
	"context"
	"fmt"
	"reflect"
	"testing"

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
// reader counting those refusals (enterprise's E-measure) reads no other
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
