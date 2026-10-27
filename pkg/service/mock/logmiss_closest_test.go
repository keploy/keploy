package mock

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Under the proven switch a miss's log line names the recording its matcher
// found closest and why, in the words of the test's reason; with the switch
// off the line is what it was.
func TestLogMissNamesTheClosestRecordingUnderTheSwitch(t *testing.T) {
	miss := models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "update on app.docs", Destination: "[::1]:27017",
		MatchPhase: models.MatchPhaseBody, ClosestMock: "mock-12",
		NextSteps: "body.cmd.updates[].u.$set.createdBy differs from the recording; if the change is expected, re-record the mock set with 'keploy mock record'"}
	for _, on := range []bool{true, false} {
		t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[on])
		core, logs := observer.New(zap.WarnLevel)
		logMiss(zap.New(core), miss, "")
		entries := logs.All()
		if len(entries) != 1 {
			t.Fatalf("switch %v: %d log lines, want 1", on, len(entries))
		}
		fields := entries[0].ContextMap()
		if fields["call"] != "update on app.docs" || fields["next_step"] != miss.NextSteps {
			t.Fatalf("switch %v: logged %v", on, fields)
		}
		closest, named := fields["closest"]
		switch {
		case on && closest != "mock-12 (a call of the same shape, its request body differs)":
			t.Fatalf("under the switch the line names closest %q, want the mock and why", closest)
		case !on && (named || len(fields) != 4):
			t.Fatalf("with the switch off the line holds %v, want protocol, call, destination and next step alone", fields)
		}
	}
	// A miss that names no closest recording logs none.
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.WarnLevel)
	logMiss(zap.New(core), models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find on app.docs"}, "")
	if _, named := logs.All()[0].ContextMap()["closest"]; named {
		t.Fatal("a miss with no closest recording names one")
	}
}

// Under the proven switch a miss's log line names the test the miss is
// charged to, the one the run's account of each test counts it against
// (attributeMocks), so the line and the account agree by construction; a
// miss charged to no test (made before the set's first test, between two
// tests, or of no time) names none, and with the switch off the line is what
// it was.
func TestLogMissNamesTheTestItIsChargedTo(t *testing.T) {
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	timed := []models.ScopeWindow{
		{Name: "TestStringIDReadBack", Start: base, End: base.Add(time.Second)},
		{Name: "TestOther", Start: base.Add(2 * time.Second), End: base.Add(3 * time.Second)},
	}
	expected := map[string][]models.MockEntry{"TestStringIDReadBack": nil, "TestOther": nil}
	miss := func(at time.Time) models.UnmatchedCall {
		return models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find on app.users", Destination: "[::1]:27017",
			MatchPhase: models.MatchPhaseBody, ClosestMock: "mock-3", At: at}
	}
	// A window of no times (an agent that did not say when its test ran)
	// holds a miss of no time by its bounds: it is still charged to no test.
	untimed := []models.ScopeWindow{{Name: "TestStringIDReadBack"}}
	for _, c := range []struct {
		name    string
		windows []models.ScopeWindow
		miss    models.UnmatchedCall
		test    string
	}{
		{"in the first test", timed, miss(base.Add(500 * time.Millisecond)), "TestStringIDReadBack"},
		{"in the second test", timed, miss(base.Add(2500 * time.Millisecond)), "TestOther"},
		{"before the set's first test", timed, miss(base.Add(-time.Second)), ""},
		{"between two tests", timed, miss(base.Add(1500 * time.Millisecond)), ""},
		{"of no time", timed, miss(time.Time{}), ""},
		{"of no time in a window of no times", untimed, miss(time.Time{}), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			windows := c.windows
			var charged []string
			for _, f := range attributeMocks(windows, expected, nil, []models.UnmatchedCall{c.miss}) {
				if len(f.Missed) > 0 {
					charged = append(charged, f.Flow)
				}
			}
			for _, on := range []bool{true, false} {
				t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[on])
				test, _ := chargedTest(windows, expected, c.miss)
				core, logs := observer.New(zap.WarnLevel)
				logMiss(zap.New(core), c.miss, test)
				if len(logs.All()) != 1 {
					t.Fatalf("switch %v: %d log lines, want 1", on, len(logs.All()))
				}
				fields := logs.All()[0].ContextMap()
				named, has := fields["test"]
				switch {
				case !on && has:
					t.Fatalf("with the switch off the line names test %v", named)
				case on && c.test == "" && has:
					t.Fatalf("a miss charged to no test names test %v", named)
				case on && c.test != "" && named != c.test:
					t.Fatalf("the line names test %v, want %q", named, c.test)
				}
			}
			var want []string
			if c.test != "" {
				want = []string{c.test}
			}
			if !slices.Equal(charged, want) {
				t.Fatalf("the account charges %v, the line names %q", charged, c.test)
			}
		})
	}
}

// A run's log line for each miss names, under the proven switch, the test
// its tests receipt charges the miss to, and none for a miss made before the
// set's first test, which the receipt charges to no test.
func TestTheRunsMissLineNamesTheTestItsReceiptChargesIt(t *testing.T) {
	provenOn(t)
	start := time.Now().Add(-time.Minute)
	in := models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find on app.users", Destination: "[::1]:27017",
		MatchPhase: models.MatchPhaseBody, ClosestMock: "mock-0", At: start.Add(time.Second)}
	before := in
	before.ActualSummary, before.At = "insert on app.events", start.Add(-time.Second)
	dir, _, logs := replayIn(t, replayCase{mapped: true, mockErrs: []models.UnmatchedCall{in, before},
		windows: []models.ScopeWindow{{Name: "o::TestA", Start: start, End: start.Add(2 * time.Second), Outcome: models.ScopeOutcomeFailed}}})
	named := map[string]any{}
	for _, e := range logs.All() {
		if e.Message == "no recorded mock matched an outgoing call" {
			f := e.ContextMap()
			named[f["call"].(string)] = f["test"]
		}
	}
	if len(named) != 2 || named["find on app.users"] != "o::TestA" || named["insert on app.events"] != nil {
		t.Fatalf("the miss lines name tests %v, want find's o::TestA and insert's none", named)
	}
	r, err := ReadTestsReceipt(dir, "set")
	if err != nil {
		t.Fatal(err)
	}
	var charged []string
	for _, tr := range r.Tests {
		if tr.Missed > 0 {
			charged = append(charged, fmt.Sprintf("%s:%d", tr.Name, tr.Missed))
		}
	}
	if !slices.Equal(charged, []string{"o::TestA:1"}) {
		t.Fatalf("the receipt charges %v, want o::TestA with the one miss the line names it for", charged)
	}
}
