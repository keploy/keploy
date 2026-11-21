package replay

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// startupLookupMiss is the first test's read: a lookup the application made
// as it started, which no mock answered, handed over as the set's.
func startupLookupMiss() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A vault.staging.internal.", HostTag: "#cd56",
		BeforeSet: true, Kind: models.MissOutsideTest}}, nil
}

// dropLines are the log entries that name the recording's dropped mocks.
func dropLines(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterMessageSnippet("the recording's agent dropped mocks").All()
}

// A call the application made outside every test that found no mock, in a
// set whose recording's agent dropped mocks, may have needed one of them: in
// either mode a WARN names the drop and its next step beside it, and the set
// is judged as without the mark (such a call fails nothing). A set whose
// recording dropped none says nothing of a drop.
func TestRunTestSet_ACallOutsideEveryTestInASetWhoseRecordingDroppedMocksIsNamedBesideTheDrop(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			reads   func() ([]models.UnmatchedCall, error)
			atEnd   func() ([]models.UnmatchedCall, error)
			shaped  string
			dropped *models.MockDropSummary
		}{
			{"a start-up lookup", misses(startupLookupMiss), nil, "DNS A {}.{}.{}. @before_first_test;", loginDropped()},
			{"a lookup after the last test", misses(), func() ([]models.UnmatchedCall, error) { return []models.UnmatchedCall{stopLookup}, nil },
				"DNS A {}.{}.{}. @after_last_test;", loginDropped()},
			{"a start-up lookup, no drop", misses(startupLookupMiss), nil, "DNS A {}.{}.{}. @before_first_test;", nil},
			{"a lookup after the last test, a zero summary", misses(), func() ([]models.UnmatchedCall, error) { return []models.UnmatchedCall{stopLookup}, nil },
				"DNS A {}.{}.{}. @after_last_test;", &models.MockDropSummary{}},
		} {
			t.Run(fmt.Sprintf("proven=%v/%s", proven, tc.name), func(t *testing.T) {
				provenOn(t, proven)
				h := newPartialRunHarness(t, 2, 0)
				withSetDir(t, h)
				logs := observeWarnings(h)
				h.replayer.testSetConf = droppedRecordingConf{d: tc.dropped}
				h.instr.mockErrorsFor = tc.reads
				agent := readAsAnAgentIs(h)
				agent.atEnd = tc.atEnd

				if status := h.run(t); status != models.TestSetStatusPassed || h.replayer.notJudged("test-set-0") {
					t.Fatalf("status %s, not judged %v (reason %q); a call outside every test fails nothing, a drop or not",
						status, h.replayer.notJudged("test-set-0"), h.report.report.FailureReason)
				}
				if got := calledShaped(h.report.report.OutsideTestMisses); got != tc.shaped {
					t.Fatalf("the report names %s outside every test, want %s", got, tc.shaped)
				}
				got := dropLines(logs)
				if tc.dropped == nil || !tc.dropped.Dropped() {
					if len(got) != 0 {
						t.Fatalf("a recording that dropped no mock: the log names a drop: %+v", got)
					}
					return
				}
				if len(got) != 1 {
					t.Fatalf("%d lines name the drop, want one WARN: %+v", len(got), got)
				}
				e := got[0]
				if e.Level != zapcore.WarnLevel || !strings.Contains(e.Message, "a call of 1 call(s) no test owns found no mock") ||
					e.ContextMap()["next_step"] != mocksDroppedNext || e.ContextMap()["test-set"] != "test-set-0" {
					t.Fatalf("the drop's line %s %q %v; want a WARN naming the call no test owns, the set and the drop's next step",
						e.Level, e.Message, e.ContextMap())
				}
			})
		}
	}
}

// With the switch off a test's miss and a call outside every test are named
// in the one WARN beside the drop. Under the switch the test's miss leaves
// the set not judged, its reason naming the drop: nothing names it again.
func TestRunTestSet_ATestsMissAndAStartupLookupNameTheDropOnce(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			reads := func() ([]models.UnmatchedCall, error) {
				lookup, _ := startupLookupMiss()
				own, _ := ownMiss()
				return append(lookup, own...), nil
			}
			base := newPartialRunHarness(t, 2, 0)
			withSetDir(t, base)
			base.instr.mockErrorsFor = misses(reads)
			want := base.run(t)

			h := newPartialRunHarness(t, 2, 0)
			withSetDir(t, h)
			logs := observeWarnings(h)
			h.replayer.testSetConf = droppedRecordingConf{d: loginDropped()}
			h.instr.mockErrorsFor = misses(reads)

			status := h.run(t)

			got := dropLines(logs)
			if len(got) != 1 {
				t.Fatalf("%d lines name the drop, want one: %+v", len(got), got)
			}
			if !proven {
				if status != want || h.replayer.notJudged("test-set-0") {
					t.Fatalf("status %s, not judged %v; want %s as without the mark, judged", status, h.replayer.notJudged("test-set-0"), want)
				}
				if e := got[0]; e.Level != zapcore.WarnLevel || !strings.Contains(e.Message, "a call of 1 test(s) (test-1) and 1 call(s) no test owns found no mock") {
					t.Fatalf("the drop's line %s %q; want a WARN naming the test and the call no test owns", e.Level, e.Message)
				}
				return
			}
			if status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") {
				t.Fatalf("status %s, not judged %v; a test's miss in a set whose recording dropped mocks leaves it not judged", status, h.replayer.notJudged("test-set-0"))
			}
			if e := got[0]; e.Level == zapcore.WarnLevel || !strings.Contains(e.Message, "test-set is not judged") {
				t.Fatalf("the drop's line %s %q; want only the not-judged line", e.Level, e.Message)
			}
		})
	}
}

// droppedWithPostScript is a set whose recording's agent dropped mocks and
// whose post-script touches path.
type droppedWithPostScript struct {
	postScriptTouches
	d *models.MockDropSummary
}

func (c droppedWithPostScript) Read(ctx context.Context, id string) (*models.TestSet, error) {
	ts, err := c.postScriptTouches.Read(ctx, id)
	if ts != nil {
		ts.RecordMocksDropped = c.d
	}
	return ts, err
}

// With the switch off the lookup an application makes as keploy stops it,
// after the set's post-script, is read just before the report: the drop is
// named beside it too.
func TestRunTestSet_ALookupTheAppMakesAsItStopsIsNamedBesideTheDropWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	withSetDir(t, h)
	logs := observeWarnings(h)
	agent := readAsAnAgentIs(h)
	app := &stopsAfterPost{readsAsAnAgentDoes: agent, post: filepath.Join(t.TempDir(), "post-ran")}
	h.replayer.instrumentation = app
	h.replayer.testSetConf = droppedWithPostScript{postScriptTouches: postScriptTouches{path: app.post}, d: loginDropped()}
	agent.atEnd = func() ([]models.UnmatchedCall, error) {
		if !app.exited.Load() {
			return nil, nil
		}
		return []models.UnmatchedCall{stopLookup}, nil
	}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); a lookup the application makes as it stops fails nothing", status, h.report.report.FailureReason)
	}
	if n := app.stoppedN.Load(); n != 1 {
		t.Fatalf("the application was stopped %d times, want once", n)
	}
	if got, want := calledShaped(h.report.report.OutsideTestMisses), "DNS A {}.{}.{}. @after_last_test;"; got != want {
		t.Fatalf("the report names %s outside every test, want %s", got, want)
	}
	got := dropLines(logs)
	if len(got) != 1 || got[0].Level != zapcore.WarnLevel || !strings.Contains(got[0].Message, "a call of 1 call(s) no test owns found no mock") ||
		got[0].ContextMap()["next_step"] != mocksDroppedNext {
		t.Fatalf("the drop's lines %+v; want one WARN beside the lookup made as the application stopped", got)
	}
}

// Under the switch a call no test owns that counts (a write the application
// made as it started) leaves a set whose recording dropped mocks not judged,
// its reason naming the drop; a start-up lookup beside it, which fails
// nothing, does not name the drop again.
func TestRunTestSet_ACountedCallAndAStartupLookupNameTheDropOnceUnderTheSwitch(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	withSetDir(t, h)
	logs := observeWarnings(h)
	h.replayer.testSetConf = droppedRecordingConf{d: loginDropped()}
	h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
		lookup, _ := startupLookupMiss()
		write, _ := beforeFirst()
		return append(lookup, write...), nil
	})

	status := h.run(t)

	if status != models.TestSetStatusInternalErr || !h.replayer.notJudged("test-set-0") {
		t.Fatalf("status %s, not judged %v; a counted call no test owns in a set whose recording dropped mocks leaves it not judged",
			status, h.replayer.notJudged("test-set-0"))
	}
	if got, want := calledShaped(h.report.report.OutsideTestMisses), "DNS A {}.{}.{}. @before_first_test;"; got != want {
		t.Fatalf("the report names %s outside every test, want %s", got, want)
	}
	got := dropLines(logs)
	if len(got) != 1 || got[0].Level == zapcore.WarnLevel || !strings.Contains(got[0].Message, "test-set is not judged") ||
		!strings.Contains(got[0].Message, "1 call(s) no test owns") {
		t.Fatalf("the drop's lines %+v; want only the not-judged line, naming the counted call", got)
	}
}
