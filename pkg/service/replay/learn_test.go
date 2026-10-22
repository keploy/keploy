package replay

import (
	"fmt"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
// report. A set with no test to run says so too.
func TestTheReportSaysTheLearnStepRan(t *testing.T) {
	for _, tc := range []struct {
		sw    string
		learn bool
		n     int
		want  string
	}{
		{sw: "1", learn: true, n: 2, want: models.LearnWords},
		{sw: "1", learn: true, n: 0, want: models.LearnWords},
		{sw: "1", learn: false, n: 2},
		{sw: "", learn: true, n: 2},
		{sw: "", learn: false, n: 2},
	} {
		t.Run(fmt.Sprintf("switch=%q learn=%v tests=%d", tc.sw, tc.learn, tc.n), func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.sw)
			h := newPartialRunHarness(t, tc.n, 0)
			h.replayer.config.Test.Learn = tc.learn
			h.replayer.mockDB = &learnedStore{prMockDB: h.mocks}
			_ = h.run(t)
			if h.report.report == nil {
				t.Fatal("no report was written")
			}
			if got := h.report.report.Learn; got != tc.want {
				t.Fatalf("the report's learn is %q; want %q", got, tc.want)
			}
		})
	}
}
