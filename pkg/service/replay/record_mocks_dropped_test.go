package replay

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// droppedRecordingConf is a test-set config whose recording's agent dropped
// mocks after capturing them (models.TestSet.RecordMocksDropped).
type droppedRecordingConf struct {
	prTestSetConf
	d *models.MockDropSummary
}

func (c droppedRecordingConf) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{RecordMocksDropped: c.d}, nil
}

func loginDropped() *models.MockDropSummary {
	return &models.MockDropSummary{ByPressure: 1, Named: []string{"mock-3: memory pressure"}}
}

// ownMiss is a test's own call that no mock answered.
func ownMiss() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /v1/secret/app", Destination: "openbao:8200"}}, nil
}

// Under the proven switch a call that found no mock, in a set whose
// recording's agent dropped mocks after capturing them, leaves the set not
// judged: the mock it needed may be one the recording lacks. Its reason names
// the drop (counts and the first dropped) and where the call was made: a
// test's own, or one no test owns (before the first test, after the last).
func TestRunTestSet_AMissInASetWhoseRecordingDroppedMocksIsNotJudged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reads func() ([]models.UnmatchedCall, error)
		where string
	}{
		{"a test's own call", misses(nil, ownMiss), "1 test(s) (test-1)"},
		{"a call before the first test", misses(beforeFirst), "1 call(s) no test owns"},
		{"a call after the last test", misses(nil, nil, afterLast), "1 call(s) no test owns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.testSetConf = droppedRecordingConf{d: loginDropped()}
			h.instr.mockErrorsFor = tc.reads

			status := h.run(t)

			rep := h.report.report
			if status != models.TestSetStatusInternalErr || rep.Status != string(models.TestSetStatusInternalErr) {
				t.Fatalf("status %s (report %s); a miss in a set whose recording dropped mocks must leave it not judged", status, rep.Status)
			}
			if !h.replayer.notJudged("test-set-0") {
				t.Fatal("the set is not marked not judged")
			}
			for _, want := range []string{"the recording's agent dropped mocks", "1 by memory pressure, 0 for its mock output", "mock-3: memory pressure", tc.where} {
				if !strings.Contains(rep.FailureReason, want) {
					t.Fatalf("failure reason %q does not say %q", rep.FailureReason, want)
				}
			}
		})
	}
}

// A set whose recording dropped mocks is judged as any other when none of its
// calls missed (what was dropped, it did not need), and a summary that counts
// no drop marks nothing: such a set's miss fails it, as without the mark.
func TestRunTestSet_ASetWhoseRecordingDroppedMocksIsJudgedWithoutAMiss(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.testSetConf = droppedRecordingConf{d: loginDropped()}
	h.instr.mockErrorsFor = misses()
	if status := h.run(t); status != models.TestSetStatusPassed || h.replayer.notJudged("test-set-0") {
		t.Fatalf("no miss: status %s, not judged %v; want PASSED, judged", status, h.replayer.notJudged("test-set-0"))
	}

	for _, d := range []*models.MockDropSummary{nil, {}} {
		h = newPartialRunHarness(t, 2, 0)
		h.replayer.testSetConf = droppedRecordingConf{d: d}
		h.instr.mockErrorsFor = misses(nil, ownMiss)
		status := h.run(t)
		if status != models.TestSetStatusFailed || h.replayer.notJudged("test-set-0") {
			t.Fatalf("summary %+v, a test's miss: status %s, not judged %v; want FAILED, judged", d, status, h.replayer.notJudged("test-set-0"))
		}
		if strings.Contains(h.report.report.FailureReason, "dropped mocks") {
			t.Fatalf("summary %+v: failure reason %q names a drop", d, h.report.report.FailureReason)
		}
	}
}

// With the switch off the verdict is as before, and a WARN names the drop and
// what to do beside the miss.
func TestRunTestSet_AMissInASetWhoseRecordingDroppedMocksIsNamedWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	base := newPartialRunHarness(t, 2, 0)
	base.instr.mockErrorsFor = misses(nil, ownMiss)
	want := base.run(t)

	core, logs := observer.New(zap.WarnLevel)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.logger = zap.New(core)
	h.replayer.testSetConf = droppedRecordingConf{d: loginDropped()}
	h.instr.mockErrorsFor = misses(nil, ownMiss)
	if status := h.run(t); status != want || h.replayer.notJudged("test-set-0") {
		t.Fatalf("status %s, not judged %v; want %s as without the mark, judged", status, h.replayer.notJudged("test-set-0"), want)
	}
	found := false
	for _, e := range logs.FilterMessageSnippet("the recording's agent dropped mocks").All() {
		if e.Level == zap.WarnLevel && strings.Contains(e.Message, "1 test(s) (test-1)") && e.ContextMap()["next_step"] == mocksDroppedNext {
			found = true
		}
	}
	if !found {
		t.Fatalf("no WARN names the drop with its next step: %+v", logs.All())
	}
}

// noteMissed keeps a test whose last run had a call that counts against it,
// once, and takes it out when its last run had none (a DNS or an answered
// call does not count).
func TestNoteMissedKeepsATestsLastRun(t *testing.T) {
	missed := func(calls ...models.UnmatchedCall) *models.TestResult {
		return &models.TestResult{FailureInfo: models.FailureInfo{UnmatchedCalls: calls}}
	}
	l := noteMissed(nil, "t1", missed(models.UnmatchedCall{Protocol: "HTTP"}))
	l = noteMissed(l, "t1", missed(models.UnmatchedCall{Protocol: "HTTP"}))
	l = noteMissed(l, "t2", missed(models.UnmatchedCall{Protocol: "Mongo"}))
	if !slices.Equal(l, []string{"t1", "t2"}) {
		t.Fatalf("missed %v; want [t1 t2]", l)
	}
	l = noteMissed(l, "t1", missed(models.UnmatchedCall{Protocol: "DNS"}, models.UnmatchedCall{Protocol: "HTTP", Answer: "200"}))
	if !slices.Equal(l, []string{"t2"}) {
		t.Fatalf("missed %v after t1's run with no counted miss; want [t2]", l)
	}
}
