package replay

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
