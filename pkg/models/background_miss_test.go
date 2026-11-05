package models

import (
	"encoding/json"
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
