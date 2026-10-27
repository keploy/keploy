package mock

import (
	"testing"

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
		logMiss(zap.New(core), miss)
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
	logMiss(zap.New(core), models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find on app.docs"})
	if _, named := logs.All()[0].ContextMap()["closest"]; named {
		t.Fatal("a miss with no closest recording names one")
	}
}
