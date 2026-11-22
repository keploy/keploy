package replay

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// setLossInstr is a run's agent that keeps the application across sets: it
// notes the test set each session's MockOutgoing names
// (models.TestSetSessionOf), and its owned take, in set later, returns late,
// a loss of an earlier set's found after that set's end, once.
type setLossInstr struct {
	*prLossInstr
	smu      sync.Mutex
	sessions []models.TestSetSession
	open     string
	later    string
	late     []models.OwnedCaptureLoss
	takes    int
}

func (s *setLossInstr) MockOutgoing(ctx context.Context, _ models.OutgoingOptions) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	set := models.TestSetSessionOf(ctx)
	s.sessions = append(s.sessions, set)
	s.open = set.Set
	return nil
}

func (s *setLossInstr) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.takes++
	if s.open != s.later {
		return nil, nil
	}
	out := s.late
	s.late = nil
	return out, nil
}

// keploy test with the application kept across sets: a connection test-set-0
// used closes while test-set-1 runs, with bytes test-set-0 wrote lost, or
// untold, found after test-set-0's end. Each set's session names its set
// under one token of the run's, so the agent returns the loss stamped with
// test-set-0 (models.OwnedCaptureLoss.EarlierSession) to test-set-1's take:
// no test of test-set-1 is charged and it passes, and the run ends not
// judged for test-set-0 -- its verdict pass is false, the run's summary
// names test-set-0 and its test (or that the bytes were written in none) and
// shows test-set-0 failing. With the switch off keploy test takes no owned
// loss at all (it judges capture losses only under the switch): the run
// passes, and the summary lists none.
func TestAnEarlierSetsLossFoundAfterItsEndLeavesThatSetNotJudged(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesLost, models.CaptureLossAppBytesUntold} {
			for _, test := range []string{"test-2", ""} {
				name := string(kind) + "/written in " + test
				if !on {
					name += "/switch off"
				}
				t.Run(name, func(t *testing.T) {
					provenOn(t, on)
					h := newPartialRunHarness(t, 2, 0) // every test case passes
					h.replayer.config.Test.Mocking = true
					h.replayer.config.DisableANSI = true
					h.replayer.setSessionRun = newSetSessionRun()
					lost := models.CaptureLoss{Kind: kind, Count: 1, What: "from [::ffff:127.0.0.1]:50914"}
					instr := &setLossInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
						later: "test-set-1", late: []models.OwnedCaptureLoss{{Test: test, Loss: lost, EarlierSession: true, Set: "test-set-0"}}}
					h.replayer.instrumentation = instr
					h.replayer.markPIDMaps(context.Background())

					if status := h.runServing(t); status != models.TestSetStatusPassed {
						t.Fatalf("test-set-0 %q, want PASSED: its loss was found after its end", status)
					}
					h.set = "test-set-1"
					if status := h.runServing(t); status != models.TestSetStatusPassed {
						t.Fatalf("test-set-1 %q, want PASSED: the loss is test-set-0's", status)
					}
					for _, r := range h.report.report.Tests {
						if len(r.FailureInfo.CaptureIncomplete) != 0 || r.Status != models.TestStatusPassed {
							t.Fatalf("test-set-1's %s is %s, charged %+v: the loss is test-set-0's", r.TestCaseID, r.Status, r.FailureInfo.CaptureIncomplete)
						}
					}
					if cv := h.report.report.CaptureVerification; cv != nil {
						t.Fatalf("test-set-1's capture_verification %+v: the loss is test-set-0's", cv)
					}
					run := h.replayer.setSessionRun
					if run == "" || len(instr.sessions) != 2 ||
						instr.sessions[0] != (models.TestSetSession{Run: run, Set: "test-set-0"}) ||
						instr.sessions[1] != (models.TestSetSession{Run: run, Set: "test-set-1"}) {
						t.Fatalf("sessions %+v, want test-set-0 then test-set-1, each of run %q", instr.sessions, run)
					}

					pass := h.replayer.passesEarlierSetLosses(true)
					// The summary keploy prints without colours, then with
					// them: each says it once.
					var summary string
					for _, plain := range []bool{true, false} {
						var out bytes.Buffer
						h.replayer.console = &out
						h.replayer.config.DisableANSI = plain
						h.replayer.printSummary(context.Background(), pass)
						summary += out.String()
					}
					if !on {
						if !pass || instr.takes != 0 || strings.Contains(summary, "not judged") {
							t.Fatalf("switch off: pass %v, takes %d, summary %q; want a pass, no take, no set listed", pass, instr.takes, summary)
						}
						return
					}
					if pass {
						t.Fatal("the run passed: test-set-0's loss was found after its end, so test-set-0 is not judged")
					}
					where := "test-2 of test-set-0"
					if test == "" {
						where = "no test (between two of its tests, or after its last) of test-set-0"
					}
					if strings.Count(summary, "Test sets not judged, a capture loss of theirs found after their end: 1") != 2 ||
						strings.Count(summary, where+": "+models.CaptureLosses{lost}.LogText()) != 2 {
						t.Fatalf("summaries %q, want each to name %q and what was lost", summary, where)
					}
					h.replayer.completeTestReportMu.RLock()
					set0, set1 := h.replayer.completeTestReport["test-set-0"], h.replayer.completeTestReport["test-set-1"]
					h.replayer.completeTestReportMu.RUnlock()
					if set0.status || !set1.status {
						t.Fatalf("summary rows: test-set-0 passing %v, test-set-1 passing %v; want test-set-0 failing and test-set-1 passing", set0.status, set1.status)
					}
				})
			}
		}
	}
}

// A take's losses of earlier sets (models.OwnedCaptureLoss.EarlierSession)
// are kept for the run, with the set whose take returned them, and the rest
// returned to the set's own end; a take with none is returned whole. A loss
// whose set the agent no longer names is an earlier test set's.
func TestATakeKeepsOnlyTheLossesOfEarlierSets(t *testing.T) {
	var e earlierSetLosses
	own := models.OwnedCaptureLoss{Test: "test-1", Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1}}
	late := models.OwnedCaptureLoss{Loss: own.Loss, EarlierSession: true}
	rest, taken := e.take([]models.OwnedCaptureLoss{own}, "test-set-1")
	if len(rest) != 1 || !reflect.DeepEqual(rest[0], own) || len(taken) != 0 {
		t.Fatalf("rest %+v, taken %+v; want the take whole", rest, taken)
	}
	rest, taken = e.take([]models.OwnedCaptureLoss{late, own}, "test-set-1")
	if len(rest) != 1 || !reflect.DeepEqual(rest[0], own) || len(taken) != 1 || taken[0].readIn != "test-set-1" {
		t.Fatalf("rest %+v, taken %+v; want the earlier set's kept, read in test-set-1", rest, taken)
	}
	if all := e.all(); len(all) != 1 || all[0].madeWhere() != "no test (between two of its tests, or after its last) of an earlier test set" {
		t.Fatalf("kept %+v", all)
	}
	e.reset()
	if all := e.all(); len(all) != 0 {
		t.Fatalf("a new run keeps %+v", all)
	}
}

// Each loss of an earlier set is said at ERROR as it is taken, by what it
// lost: one keploy cannot tell carried a call (not judged), or one it could
// not capture. The run's verdict pass marks only the sets its summary has a
// row for, and the summary counts the sets, not the losses.
func TestAnEarlierSetsLossIsSaidAndCountedBySet(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	r := &Replayer{logger: zap.New(core), completeTestReport: map[string]TestReportVerdict{"test-set-0": {status: true}}}
	lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:50914"}
	untold := models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: "from 127.0.0.1:50915"}
	rest := r.takeEarlierSetLosses([]models.OwnedCaptureLoss{
		{Test: "test-1", Loss: lost, EarlierSession: true, Set: "test-set-0"},
		{Test: "test-2", Loss: untold, EarlierSession: true, Set: "test-set-0"},
		{Loss: lost, EarlierSession: true},
	}, "test-set-2")
	if len(rest) != 0 {
		t.Fatalf("rest %+v", rest)
	}
	said := logs.All()
	if len(said) != 3 || !strings.Contains(said[0].Message, "could not capture a call") || !strings.Contains(said[1].Message, "cannot tell whether a connection") {
		t.Fatalf("said %+v", said)
	}
	if got := said[1].ContextMap(); got["made-in-test-set"] != "test-set-0" || got["made-in-test"] != "test-2" || got["read-in-test-set"] != "test-set-2" {
		t.Fatalf("the line says %+v", got)
	}
	if r.passesEarlierSetLosses(true) {
		t.Fatal("the run passed")
	}
	if _, ok := r.completeTestReport[""]; ok || r.completeTestReport["test-set-0"].status || len(r.completeTestReport) != 1 {
		t.Fatalf("summary rows %+v: want test-set-0 failing and no row added", r.completeTestReport)
	}
	if s := r.earlierSetLossesSummary(); !strings.Contains(s, "found after their end: 2\n") || !strings.Contains(s, "of an earlier test set: ") {
		t.Fatalf("summary %q, want 2 sets, one of them unnamed", s)
	}
}

// keploy test over a whole run, the application kept across sets:
// test-set-1's owned take returns a loss of test-set-0's that the agent found
// only after test-set-0's end. Start names each set's session by one token of
// the run's, drawn anew for each run; test-set-1 passes, charged nothing; the
// run exits non-zero, its summary naming test-set-0 and its test. A second
// run on the same Replayer, with no such loss, passes: the first run's loss
// is not the second's. With the switch off keploy test takes no owned loss,
// and both runs pass.
func TestStart_AnEarlierSetsLossFoundAfterItsEndLeavesTheRunNotJudged(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 2, 0)
			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = twoPRSetsDB{&prTestDB{cases: h.cases}}
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.Test.Mocking = true
			h.replayer.config.DisableANSI = true
			core, logs := observer.New(zap.ErrorLevel)
			h.replayer.logger = zap.New(core)
			lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from [::ffff:127.0.0.1]:50914"}
			instr := &setLossInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
				later: "test-set-1", late: []models.OwnedCaptureLoss{{Test: "test-2", Loss: lost, EarlierSession: true, Set: "test-set-0"}}}
			h.replayer.instrumentation = instr

			var code int
			var statuses map[string]string
			console := consoleOf(h.replayer, func() { code, statuses = startEarlierSetRun(t, h, reports) })
			first := runOfSessions(t, instr, "the first run")
			if statuses["test-set-1"] != string(models.TestSetStatusPassed) {
				t.Fatalf("test-set-1 is %q (all %v); the loss is test-set-0's", statuses["test-set-1"], statuses)
			}
			why := logs.FilterMessageSnippet("test run not judged: keploy found 1 capture loss(es) of earlier test sets").All()
			if !on {
				if code != 0 || len(why) != 0 || strings.Contains(console, "not judged") {
					t.Fatalf("switch off: exit code %d, said %+v, console %q; want a pass with no set listed", code, why, console)
				}
			} else {
				if code == 0 {
					t.Fatal("the run exits 0: test-set-0's loss was found after its end, so test-set-0 is not judged")
				}
				where := "test-2 of test-set-0: " + models.CaptureLosses{lost}.LogText()
				if len(why) != 1 || !strings.Contains(why[0].Message, "made in test-2 of test-set-0") ||
					!strings.Contains(console, "Test sets not judged, a capture loss of theirs found after their end: 1") ||
					!strings.Contains(console, where) {
					t.Fatalf("the run said %+v and the console\n%s\nwant the run's line and the summary naming %q", why, console, where)
				}
			}

			// The same Replayer runs again, and no take returns such a loss.
			code, statuses = startEarlierSetRun(t, h, reports)
			if second := runOfSessions(t, instr, "the second run"); second == first {
				t.Fatalf("both runs named their sessions %q: an agent kept from the first would take the second's sets for its own", first)
			}
			if code != 0 || statuses["test-set-0"] != string(models.TestSetStatusPassed) || statuses["test-set-1"] != string(models.TestSetStatusPassed) {
				t.Fatalf("the second run exits %d with the sets %v; want 0 and both PASSED: the first run's loss is not the second's", code, statuses)
			}
		})
	}
}

// runOfSessions is the run token every session instr was started with names,
// one token for them all and not "", with test-set-0's sessions before
// test-set-1's; it forgets them, for the next run.
func runOfSessions(t *testing.T, instr *setLossInstr, which string) string {
	t.Helper()
	instr.smu.Lock()
	defer instr.smu.Unlock()
	got := instr.sessions
	instr.sessions = nil
	var sets []string
	for _, s := range got {
		if s.Run == "" || s.Run != got[0].Run {
			t.Fatalf("%s's sessions %+v: want each named by one token of the run's", which, got)
		}
		if len(sets) == 0 || sets[len(sets)-1] != s.Set {
			sets = append(sets, s.Set)
		}
	}
	if fmt.Sprint(sets) != "[test-set-0 test-set-1]" {
		t.Fatalf("%s's sessions %+v: want test-set-0's, then test-set-1's", which, got)
	}
	return got[0].Run
}
