package learn

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// learnedOn is what MergeLearned writes on a mock, and where each came
// from, as JSON: two equal ones are one learning.
func learnedOn(t *testing.T, m *models.Mock) string {
	t.Helper()
	b, err := json.Marshal([]any{m.Spec.ReqBodyNoise, m.Spec.NoiseAbout, m.Spec.Variables, m.Spec.Clocks, m.Spec.ClockAbout})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var testStamp = Stamp{Round: models.AboutRoundFull, Run: "2026-10-27T05:00:00Z", Keploy: "3.8.91", Test: map[string]string{"mock-1": "items::TestReadBack"}}

// learnedAll is a state of mock-1 whose learn step learned an ignored
// field, a variable it binds and uses, an echo, and a clock read; the
// protocol gave only the noise a why of its own.
func learnedAll() map[string]models.MockState {
	return map[string]models.MockState{"mock-1": {
		Name:         "mock-1",
		ReqBodyNoise: map[string][]string{"body.eventId": nil},
		NoiseAbout: map[string]models.About{"body.eventId": {
			Why:  "changed between the recording and the full round",
			Seen: &models.AboutSeen{Record: "a", Full: "b"},
			Call: "http audit:8080 POST /v1/events #1",
		}},
		Variables: models.MockVariables{
			"_id": {Recorded: "6adf", Bind: "body.id", Use: []string{"body.ref"}},
			"rid": {Recorded: "r-1", Bind: "header.x-request-id", Answer: []string{"header.x-request-id"}, Echo: true},
		},
		Clocks: map[string]models.ClockOffset{"body.at": {Res: time.Millisecond}},
	}}
}

// checkLearned fails unless a carries every field keploy writes on a
// decision of kind at key it learned on mock-1, reads as learned, and names
// no mock.
func checkLearned(t *testing.T, what string, a *models.About, kind, key string, state models.AboutState) {
	t.Helper()
	if a == nil {
		t.Fatalf("%s: no about", what)
	}
	if a.By != models.AboutByLearned || a.Round != models.AboutRoundFull || a.Run != testStamp.Run || a.Keploy != "3.8.91" || a.Test != "items::TestReadBack" {
		t.Fatalf("%s: the CLI's fields: %+v", what, a)
	}
	if a.Why == "" || a.Undo == "" || len(a.Sum) != 8 {
		t.Fatalf("%s: why, undo or sum missing: %+v", what, a)
	}
	if !strings.Contains(a.Undo, key) {
		t.Fatalf("%s: the undo does not name %s: %q", what, key, a.Undo)
	}
	for _, s := range []string{a.Why, a.Undo} {
		if strings.Contains(s, "mock-") {
			t.Fatalf("%s: names a mock by its name, which a carry renames: %q", what, s)
		}
	}
	if state != models.AboutLearned {
		t.Fatalf("%s (%s): reads as %v, want learned", what, kind, state)
	}
}

// Row 2: each decision keploy adds carries who, why, the round, the run, the
// test, the version, the undo and a sum that reads as learned; what the
// protocol's learn step said (why, seen, call) stays, and a decision it gave
// no why gets the kind's fixed one.
func TestEachLearnedDecisionSaysWhereItCameFrom(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	mock := &models.Mock{Name: "mock-1"}
	st := StampStates(learnedAll(), testStamp)["mock-1"]
	if added := MergeLearned(mock, st, nil); len(added) != 5 {
		t.Fatalf("added: %v", added)
	}
	n := mock.Spec.NoiseAbout["body.eventId"]
	checkLearned(t, "noise", &n, models.AboutKindNoise, "body.eventId", models.NoiseAboutState(&mock.Spec, "body.eventId"))
	if n.Why != "changed between the recording and the full round" || n.Seen == nil || n.Seen.Full != "b" || n.Call == "" {
		t.Fatalf("the protocol's why, seen or call was not kept: %+v", n)
	}
	for _, name := range []string{"_id", "rid"} {
		v := mock.Spec.Variables[name]
		checkLearned(t, "variable "+name, v.About, models.AboutKindVariable, name, models.VariableAboutState(name, v))
	}
	if mock.Spec.Variables["rid"].About.Why != models.AboutWhy(models.AboutKindVariable, true, models.AboutRoundFull) {
		t.Fatalf("an echo got another why: %q", mock.Spec.Variables["rid"].About.Why)
	}
	c := mock.Spec.ClockAbout["body.at"]
	checkLearned(t, "clock", &c, models.AboutKindClock, "body.at", models.ClockAboutState(&mock.Spec, "body.at"))

	// A mock the stamp names no test for was learned between tests.
	other := StampStates(map[string]models.MockState{"mock-9": {Name: "mock-9", ReqBodyNoise: map[string][]string{"body.x": nil}}}, testStamp)
	if got := other["mock-9"].NoiseAbout["body.x"].Test; got != models.AboutBetweenTests {
		t.Fatalf("a mock no test was open for: test %q", got)
	}
}

// StampStates copies what it stamps: the states the agent reported are
// untouched.
func TestStampStatesLeavesTheStatesItReadAlone(t *testing.T) {
	in := learnedAll()
	in["mock-1"] = func(st models.MockState) models.MockState {
		st.Binds = map[string]models.MockVariables{"mock-0": {"_id": {Recorded: "6adf", Bind: "body.docs.0._id"}}}
		return st
	}(in["mock-1"])
	want := learnedAll()["mock-1"]
	out := StampStates(in, testStamp)
	if b := out["mock-1"].Binds["mock-0"]["_id"].About; b == nil || b.Test != "items::TestReadBack" {
		t.Fatalf("a bind at an earlier mock was not stamped with its test: %+v", b)
	}
	got := in["mock-1"]
	got.Binds = nil
	if !reflect.DeepEqual(got, want) || in["mock-1"].Binds["mock-0"]["_id"].About != nil {
		t.Fatalf("the states read were changed:\n%+v\nwant\n%+v", got, want)
	}
}

// Row 7: re-learning what is on disk adds nothing (so the file is not
// rewritten) and leaves each about as it was: the first learner wins.
func TestReLearningADecisionOnDiskChangesNothing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	mock := &models.Mock{Name: "mock-1"}
	MergeLearned(mock, StampStates(learnedAll(), testStamp)["mock-1"], nil)
	before := learnedOn(t, mock)
	later := testStamp
	later.Run, later.Keploy = "2026-10-28T09:00:00Z", "3.8.92"
	if added := MergeLearned(mock, StampStates(learnedAll(), later)["mock-1"], nil); len(added) != 0 {
		t.Fatalf("re-learning added %v", added)
	}
	if got := learnedOn(t, mock); got != before {
		t.Fatalf("re-learning changed the mock:\n%s\nwant\n%s", got, before)
	}
}

// Row 3 and 8: a person's decision (by hand, or learned and then edited, so
// its sum no longer matches) is never re-stamped and wins over a learned
// one for the same place: a learned use place does not join a hand
// variable, a learned clock read does not narrow a hand one, and hand noise
// keeps its about. One of unknown origin is changed as it always was, and
// never stamped.
func TestAPersonsDecisionWinsAndIsNeverStamped(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	hand := &models.About{By: models.AboutByHand, Why: "ours"}
	edited := &models.About{By: models.AboutByLearned, Run: testStamp.Run, Sum: "00000000"}
	wide := models.ClockOffset{Min: -time.Hour, Max: time.Hour, Res: time.Second}
	mock := &models.Mock{Name: "mock-1", Spec: models.MockSpec{
		ReqBodyNoise: map[string][]string{"body.eventId": {}},
		NoiseAbout:   map[string]models.About{"body.eventId": *hand},
		Variables: models.MockVariables{
			"_id": {Recorded: "6adf", Bind: "body.id", About: hand},
			"rid": {Recorded: "r-1", Bind: "header.x-request-id", Echo: true, About: edited},
		},
		Clocks:     map[string]models.ClockOffset{"body.at": wide},
		ClockAbout: map[string]models.About{"body.at": *hand},
	}}
	// A person's about at a path they took out of the noise: the place
	// stays compared.
	mock.Spec.NoiseAbout["body.gone"] = *hand
	if s := models.VariableAboutState("rid", mock.Spec.Variables["rid"]); s != models.AboutEdited || !s.Hand() {
		t.Fatalf("a learned entry with a wrong sum reads as %v", s)
	}
	before := learnedOn(t, mock)
	all := learnedAll()
	st := all["mock-1"]
	st.ReqBodyNoise["body.gone"] = nil
	if added := MergeLearned(mock, StampStates(all, testStamp)["mock-1"], nil); len(added) != 0 {
		t.Fatalf("a learned decision changed a person's: %v", added)
	}
	if got := learnedOn(t, mock); got != before {
		t.Fatalf("a person's decisions were changed:\n%s\nwant\n%s", got, before)
	}

	// Unknown origin: the use place joins as it always has; no about.
	old := &models.Mock{Name: "mock-1", Spec: models.MockSpec{Variables: models.MockVariables{"_id": {Recorded: "6adf", Bind: "body.id"}}}}
	added := MergeLearned(old, StampStates(learnedAll(), testStamp)["mock-1"], nil)
	if v := old.Spec.Variables["_id"]; !reflect.DeepEqual(v.Use, []string{"body.ref"}) || v.About != nil {
		t.Fatalf("a variable of unknown origin: %+v (added %v)", v, added)
	}
}

// keploy's own variable, extended by a later learning, keeps its about (the
// first learner's why, run and test) and its sum follows, so it still reads
// as learned; a hand edit after that reads as one (row 8).
func TestKeploysOwnVariableKeepsItsAboutAndItsSumFollows(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	mock := &models.Mock{Name: "mock-1"}
	first := map[string]models.MockState{"mock-1": {Name: "mock-1", Variables: models.MockVariables{"_id": {Recorded: "6adf", Bind: "body.id"}}}}
	MergeLearned(mock, StampStates(first, testStamp)["mock-1"], nil)
	was := *mock.Spec.Variables["_id"].About
	later := testStamp
	later.Run = "2026-10-28T09:00:00Z"
	if added := MergeLearned(mock, StampStates(learnedAll(), later)["mock-1"], nil); !slices.Contains(added, "body.ref (variable _id: use)") {
		t.Fatalf("added: %v", added)
	}
	v := mock.Spec.Variables["_id"]
	if v.About.Run != was.Run || v.About.Why != was.Why || v.About.Sum == was.Sum {
		t.Fatalf("the about after the change: %+v, was %+v", v.About, was)
	}
	if s := models.VariableAboutState("_id", v); s != models.AboutLearned {
		t.Fatalf("keploy's own extended variable reads as %v", s)
	}
	v.Use = append(v.Use, "body.other")
	if s := models.VariableAboutState("_id", v); s != models.AboutEdited {
		t.Fatalf("a use place added by hand reads as %v", s)
	}
}

// Noise at a place that turns out to be a variable's goes, and keploy's own
// about with it; a person's about there stays (an orphan keploy mock check
// reports), so their words are not lost.
func TestNoiseAtAVariablesPlaceTakesOnlyKeploysAbout(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	mock := &models.Mock{Name: "mock-1", Spec: models.MockSpec{
		ReqBodyNoise: map[string][]string{"body.id": nil, "body.ref": nil},
		NoiseAbout: map[string]models.About{
			"body.id":  models.About{Run: testStamp.Run}.Stamped(models.AboutKindNoise, "body.id", false, func(run string) string { return models.NoiseSum("body.id", run) }),
			"body.ref": {By: models.AboutByHand, Why: "ours"},
		},
	}}
	st := map[string]models.MockState{"mock-1": {Name: "mock-1", Variables: models.MockVariables{"_id": {Recorded: "6adf", Bind: "body.id", Use: []string{"body.ref"}}}}}
	MergeLearned(mock, StampStates(st, testStamp)["mock-1"], nil)
	if len(mock.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("noise kept at a variable's places: %v", mock.Spec.ReqBodyNoise)
	}
	if _, ok := mock.Spec.NoiseAbout["body.id"]; ok {
		t.Fatal("keploy's about of noise it dropped was kept")
	}
	if a, ok := mock.Spec.NoiseAbout["body.ref"]; !ok || a.Why != "ours" {
		t.Fatalf("a person's about was dropped: %+v", mock.Spec.NoiseAbout)
	}
}

// keploy's own clock read, narrowed by a later learning (Meet), keeps its
// about and its sum follows, so it still reads as learned.
func TestKeploysOwnClockReadNarrowedKeepsItsAbout(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	mock := &models.Mock{Name: "mock-1"}
	wide := map[string]models.MockState{"mock-1": {Name: "mock-1", Clocks: map[string]models.ClockOffset{"body.at": {Min: -time.Hour, Max: time.Hour, Res: time.Millisecond}}}}
	MergeLearned(mock, StampStates(wide, testStamp)["mock-1"], nil)
	was := mock.Spec.ClockAbout["body.at"]
	if added := MergeLearned(mock, StampStates(learnedAll(), testStamp)["mock-1"], nil); !slices.Contains(added, "body.at (clock)") {
		t.Fatalf("the narrowed clock read was not added: %v", added)
	}
	a := mock.Spec.ClockAbout["body.at"]
	if a.Sum == was.Sum || a.Why != was.Why || models.ClockAboutState(&mock.Spec, "body.at") != models.AboutLearned {
		t.Fatalf("the narrowed clock read's about: %+v, was %+v", a, was)
	}
}
