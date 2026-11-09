package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A background miss (the same request as one of the set's background
// recordings) counts against no test, as its call and as its report, under
// either switch; the same miss without the mark still does. The mark survives
// a sanitized copy and the report's yaml and json, where it is listed.
func TestABackgroundMissCountsAgainstNoTest(t *testing.T) {
	poll := UnmatchedCall{Protocol: "Mongo", ActualSummary: "findAndModify ptw_endpoints", MatchPhase: MatchPhaseBody, Kind: MissExtraCall, Recorded: 1}
	if !poll.CountsAgainstTest() {
		t.Fatal("an unmarked miss counts against no test")
	}
	if !(&MockMismatchReport{Protocol: "Mongo", MatchPhase: MatchPhaseBody}).CountsAgainstTest() {
		t.Fatal("an unmarked miss's report counts against no test")
	}
	bg := poll
	bg.Background = true
	if bg.CountsAgainstTest() {
		t.Error("a background miss counts against its test")
	}
	if (&MockMismatchReport{Protocol: "Mongo", MatchPhase: MatchPhaseBody, Background: true}).CountsAgainstTest() {
		t.Error("a background miss's report counts against its test")
	}
	if !bg.Sanitized().Background {
		t.Error("a sanitized background miss lost its mark")
	}
	y, err := yaml.Marshal(bg)
	if err != nil || !strings.Contains(string(y), "background: true") {
		t.Errorf("the report's yaml does not list the mark: %s (%v)", y, err)
	}
	j, err := json.Marshal(bg)
	if err != nil || !strings.Contains(string(j), `"background":true`) {
		t.Errorf("the report's json does not list the mark: %s (%v)", j, err)
	}
	if y, _ := yaml.Marshal(poll); strings.Contains(string(y), "background") {
		t.Errorf("an unmarked miss writes the mark: %s", y)
	}
}

// A set's background recordings are its worker: mocks, then its startup:
// mocks, each named once; a set with neither names none.
func TestBackgroundMockNamesAreTheWorkerThenTheStartupMocks(t *testing.T) {
	m := &Mapping{
		Worker:    []MockEntry{{Name: "poll-1"}, {Name: ""}, {Name: "poll-2"}},
		Startup:   []MockEntry{{Name: "boot-1"}, {Name: "poll-1"}},
		TestCases: []MappedTestCase{{ID: "test-1", Mocks: []MockEntry{{Name: "own-1"}}}},
	}
	if got, want := m.BackgroundMockNames(), []string{"poll-1", "poll-2", "boot-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("BackgroundMockNames = %v; want %v", got, want)
	}
	if got := (&Mapping{TestCases: m.TestCases}).BackgroundMockNames(); got != nil {
		t.Fatalf("a set with no worker: or startup: names %v", got)
	}
	if got := (*Mapping)(nil).BackgroundMockNames(); got != nil {
		t.Fatalf("no mapping names %v", got)
	}
}

// A background miss that differs from its background recording by a value
// its own test bound keeps, through the sanitized copy and the report's
// yaml and json, the names of each such place and of where the test bound
// it, and no value.
func TestABackgroundMissKeepsTheNamesOfItsTestsBoundValues(t *testing.T) {
	bound := BackgroundBinding{Recording: "mock-2954", Place: "body.docs.msgs.0.q.app_id", Variable: "appId", BoundMock: "mock-226", BoundPlace: "body.docs.msgs.0.appId"}
	bg := UnmatchedCall{Protocol: "MongoDB", MatchPhase: MatchPhaseBody, ActualSummary: "delete on api-server.uiSessionBatches", Background: true, BackgroundBound: []BackgroundBinding{bound}}
	got := bg.Sanitized()
	if !reflect.DeepEqual(got.BackgroundBound, bg.BackgroundBound) {
		t.Fatalf("sanitized BackgroundBound = %+v; want %+v", got.BackgroundBound, bg.BackgroundBound)
	}
	got.BackgroundBound[0].Variable = "changed"
	if bg.BackgroundBound[0].Variable != "appId" {
		t.Fatal("the sanitized copy shares its places with the miss")
	}
	y, err := yaml.Marshal(bg)
	if err != nil || !strings.Contains(string(y), "background_bound:") || !strings.Contains(string(y), "bound_mock: mock-226") {
		t.Fatalf("yaml %s (%v) lost the bound places", y, err)
	}
	j, err := json.Marshal(bg)
	if err != nil || !strings.Contains(string(j), `"background_bound":[{"recording":"mock-2954","place":"body.docs.msgs.0.q.app_id","variable":"appId","bound_mock":"mock-226","bound_place":"body.docs.msgs.0.appId"}]`) {
		t.Fatalf("json %s (%v) lost the bound places", j, err)
	}
	if y, _ := yaml.Marshal(UnmatchedCall{Background: true}); strings.Contains(string(y), "background_bound") {
		t.Fatalf("a background miss with no bound places writes them: %s", y)
	}
}
