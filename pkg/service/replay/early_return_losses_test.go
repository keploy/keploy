package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// lateLossInstr is a run's agent that keeps the application across sets, as
// the real one does (pkg/service/agent, takeOwnedLosses): late, a loss
// counted in the session of the set in after that set's end took its losses,
// is returned by the next owned take as the open session's own, naming no
// set, while that session is open, and as in's (EarlierSession) once a later
// set's MockOutgoing has begun another. takes counts every owned take. The
// MockOutgoing of the set refuse fails, beginning no session.
type lateLossInstr struct {
	*prLossInstr
	mu     sync.Mutex
	open   string
	armed  bool
	late   models.OwnedCaptureLoss
	in     string
	takes  int
	refuse string
}

func (s *lateLossInstr) MockOutgoing(ctx context.Context, _ models.OutgoingOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := models.TestSetSessionOf(ctx).Set
	if set == s.refuse {
		return errors.New("the keploy agent made no answer to MockOutgoing")
	}
	s.open = set
	return nil
}

func (s *lateLossInstr) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.takes++
	if !s.armed {
		return nil, nil
	}
	s.armed = false
	o := s.late
	if s.open != s.in {
		o.EarlierSession, o.Set = true, s.in
	}
	return []models.OwnedCaptureLoss{o}, nil
}

func (s *lateLossInstr) takesSoFar() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takes
}

// noCasesIn is the harness's test DB with the set empty holding no test case.
type noCasesIn struct {
	*prTestDB
	empty string
}

func (d noCasesIn) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	if set == d.empty {
		return nil, nil
	}
	return d.prTestDB.GetTestCases(ctx, set)
}

// runsNothing makes test-set-1 of h a set that returns before it begins its
// session on the agent: wholly ignored (test.ignoredTests names it with no
// test), or with no test case. It returns the status that set ends with.
func runsNothing(h *prRun, shape string) models.TestSetStatus {
	if shape == "ignored" {
		h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-1": {}}
		return models.TestSetStatusIgnored
	}
	h.replayer.testDB = noCasesIn{prTestDB: &prTestDB{cases: h.cases}, empty: "test-set-1"}
	return models.TestSetStatusNoTestsToRun
}

// unreportedBy is how many lines say, for set, a loss of a set that ended
// before its end could judge it (reportUnappliedOwnedLosses).
func unreportedBy(logs *observer.ObservedLogs, set string) int {
	n := 0
	for _, e := range logs.FilterMessageSnippet("the test-set ended before its end could judge").All() {
		if e.ContextMap()["test-set"] == set {
			n++
		}
	}
	return n
}

// keploy test with the application kept across sets: test-set-0 passes, and
// a cut-short end's bytes of its test-2 are counted after test-set-0's end
// took its losses, in test-set-0's session. test-set-1 is wholly ignored, or
// has no test case: it returns before it begins a session of its own, so it
// takes nothing (the session open on the agent is test-set-0's, whose take
// would return the loss as the open session's own) and logs nothing. So
// does a set whose MockOutgoing failed: the agent may not have begun its
// session. The loss stays on the agent, and test-set-2's take, after its
// session began, has it as test-set-0's: test-set-2 passes, charged nothing,
// and the run ends not judged for test-set-0, naming its test-2. The control
// has no set between. With the switch off keploy test takes no owned loss at
// all.
func TestASetThatReturnsBeforeItsSessionLeavesAnEarlierSetsLateLossToTheNext(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesLost, models.CaptureLossAppBytesUntold} {
			for _, between := range []string{"", "ignored", "empty", "unbegun"} {
				name := string(kind) + "/no set between"
				if between != "" {
					name = string(kind) + "/a set " + between + " between"
				}
				if !on {
					name += "/switch off"
				}
				t.Run(name, func(t *testing.T) {
					provenOn(t, on)
					h := newPartialRunHarness(t, 2, 0) // every test case passes
					h.replayer.config.Test.Mocking = true
					h.replayer.config.DisableANSI = true
					h.replayer.setSessionRun = newSetSessionRun()
					core, logs := observer.New(zap.ErrorLevel)
					h.replayer.logger = zap.New(core)
					lost := models.CaptureLoss{Kind: kind, Count: 1, What: "from [::ffff:127.0.0.1]:50914, written in test-2, where keploy's wait for them was cut short"}
					instr := &lateLossInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
						late: models.OwnedCaptureLoss{Test: "test-2", Loss: lost}, in: "test-set-0"}
					h.replayer.instrumentation = instr
					h.replayer.markPIDMaps(context.Background())

					if status := h.runServing(t); status != models.TestSetStatusPassed {
						t.Fatalf("test-set-0 %q, want PASSED", status)
					}
					// test-2's bytes are counted now, after test-set-0's
					// end took its losses, still in test-set-0's session.
					instr.mu.Lock()
					instr.armed = true
					instr.mu.Unlock()
					if between != "" {
						want := models.TestSetStatusFailed
						if between == "unbegun" {
							instr.mu.Lock()
							instr.refuse = "test-set-1"
							instr.mu.Unlock()
						} else {
							want = runsNothing(h, between)
						}
						h.set = "test-set-1"
						before := instr.takesSoFar()
						if status := h.runServing(t); status != want {
							t.Fatalf("test-set-1 %q, want %q", status, want)
						}
						if n := instr.takesSoFar() - before; n != 0 {
							t.Fatalf("test-set-1 took %d time(s), want none: it began no session, so the open one is test-set-0's", n)
						}
						if n := unreportedBy(logs, "test-set-1"); n != 0 {
							t.Fatalf("test-set-1 said %d loss(es) as its own: test-set-0's loss is no loss of a set that began no session", n)
						}
					}
					h.set = "test-set-2"
					if status := h.runServing(t); status != models.TestSetStatusPassed {
						t.Fatalf("test-set-2 %q, want PASSED: the loss is test-set-0's", status)
					}
					for _, r := range h.report.report.Tests {
						if len(r.FailureInfo.CaptureIncomplete) != 0 {
							t.Fatalf("test-set-2's %s charged %+v: the loss is test-set-0's", r.TestCaseID, r.FailureInfo.CaptureIncomplete)
						}
					}
					pass := h.replayer.passesEarlierSetLosses(true)
					if !on {
						if !pass || instr.takesSoFar() != 0 {
							t.Fatalf("switch off: pass %v after %d takes; want a pass, and no take", pass, instr.takesSoFar())
						}
						return
					}
					if pass {
						t.Fatal("FALSE GREEN: the run passes, though test-2 of test-set-0 lost bytes found only after test-set-0's end")
					}
					said := logs.FilterMessageSnippet("keploy found, only after an earlier test set had ended").All()
					if len(said) != 1 {
						t.Fatalf("%d lines say the earlier set's loss, want 1: %+v", len(said), logs.All())
					}
					if m := said[0].ContextMap(); m["made-in-test-set"] != "test-set-0" || m["made-in-test"] != "test-2" ||
						m["read-in-test-set"] != "test-set-2" || m["kind"] != string(kind) {
						t.Fatalf("the line names %v; want test-2 of test-set-0, read in test-set-2, %s", m, kind)
					}
					h.replayer.completeTestReportMu.RLock()
					set0 := h.replayer.completeTestReport["test-set-0"]
					h.replayer.completeTestReportMu.RUnlock()
					if set0.status {
						t.Fatal("test-set-0 is shown passing: its loss was found after its end, so it is not judged")
					}
				})
			}
		}
	}
}

// keptStopInstr is drainInstr whose application, the one --keep-app-alive
// keeps across the run's sets, notes its stop in order ("stop") as its run
// ends, with the set whose session was open then (stoppedIn), and whether
// test-set-0's report was written by then (reportedAtStop, from reports).
type keptStopInstr struct {
	*drainInstr
	reports         *setsReportDB
	open, stoppedIn string
	reportedAtStop  bool
}

func (k *keptStopInstr) MockOutgoing(ctx context.Context, _ models.OutgoingOptions) error {
	k.lmu.Lock()
	defer k.lmu.Unlock()
	k.open = models.TestSetSessionOf(ctx).Set
	return nil
}

func (k *keptStopInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	appErr := k.prInstr.Run(ctx, opts)
	reported := false
	if k.reports != nil {
		k.reports.mu.Lock()
		_, reported = k.reports.statuses["test-set-0"]
		k.reports.mu.Unlock()
	}
	k.lmu.Lock()
	k.order = append(k.order, "stop")
	if k.stoppedIn == "" {
		k.stoppedIn, k.reportedAtStop = k.open, reported
	}
	k.lmu.Unlock()
	return appErr
}

// keploy test over a whole run, --keep-app-alive keeping the application
// across sets: test-set-0 runs, and test-set-1, the run's last by place, is
// wholly ignored or has no test case, so it begins no session. test-set-0 is
// the last set that runs: its final attempt stops the application before its
// end's read, has the agent count the closes the stop made, and takes them,
// so bytes a connection lost as the application stopped fail test-set-0. They
// are never left to the run's teardown, after every read, in no verdict.
// With the switch off test-set-0's final attempt stops it too, before its
// report's read of the calls the application made after its last test, and
// nothing drains or takes: the set passes.
func TestTheKeptApplicationStopsAtTheEndOfTheLastSetThatRuns(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, last := range []string{"ignored", "empty"} {
			name := "the last set " + last
			if !on {
				name += "/switch off"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, 2, 0) // every test case passes
				reports := &setsReportDB{prReportDB: h.report}
				h.replayer.reportDB = reports
				h.replayer.testDB = twoPRSetsDB{&prTestDB{cases: h.cases}}
				want := runsNothing(h, last)
				if db, ok := h.replayer.testDB.(noCasesIn); ok {
					h.replayer.testDB = twoSetsNoCasesIn{noCasesIn: db}
				}
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.KeepAppAlive = true
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.Test.Mocking = true
				h.replayer.config.DisableANSI = true
				lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1,
					What: "from [::ffff:127.0.0.1]:50914, written as the application stopped"}
				instr := &keptStopInstr{drainInstr: &drainInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
					stopLoss: &models.OwnedCaptureLoss{Loss: lost}}, reports: reports}
				h.replayer.instrumentation = instr

				code, statuses := startEarlierSetRun(t, h, reports)
				instr.lmu.Lock()
				order, stoppedIn, reportedAtStop := strings.Join(instr.order, ","), instr.stoppedIn, instr.reportedAtStop
				instr.lmu.Unlock()
				if statuses["test-set-1"] != string(want) {
					t.Fatalf("test-set-1 is %q (all %v), want %q", statuses["test-set-1"], statuses, want)
				}
				if stoppedIn != "test-set-0" || reportedAtStop || strings.Count(order, "stop") != 1 {
					t.Fatalf("the application stopped in %q's session, test-set-0's report written first: %v, order %q; want it stopped once, at test-set-0's end, before its report", stoppedIn, reportedAtStop, order)
				}
				if !on {
					if statuses["test-set-0"] != string(models.TestSetStatusPassed) || strings.Contains(order, "drain") {
						t.Fatalf("switch off: test-set-0 %q, the agent's drain and takes %q; want PASSED and no drain", statuses["test-set-0"], order)
					}
					return
				}
				if !strings.Contains(order, "stop,drain,take") {
					t.Fatalf("order %q; want test-set-0's end to stop the application before its drain and its take", order)
				}
				if statuses["test-set-0"] != string(models.TestSetStatusFailed) || code == 0 {
					t.Fatalf("test-set-0 %q, exit code %d; want FAILED and non-zero: bytes were lost as the application stopped", statuses["test-set-0"], code)
				}
			})
		}
	}
}

// twoSetsNoCasesIn is noCasesIn over the run's two sets, test-set-0 and
// test-set-1.
type twoSetsNoCasesIn struct{ noCasesIn }

func (twoSetsNoCasesIn) GetAllTestSetIDs(context.Context) ([]string, error) {
	return []string{"test-set-0", "test-set-1"}, nil
}

// composeDrainInstr is a docker compose stack's agent (composeInstr) whose
// proxy counts the closes the app container's stop made only as it drains
// (drainInstr): the stop is noted in the drain's order ("stop").
type composeDrainInstr struct {
	*composeInstr
	d *drainInstr
}

func (c *composeDrainInstr) StopComposeApp(ctx context.Context) (string, error) {
	c.d.lmu.Lock()
	c.d.order = append(c.d.order, "stop")
	c.d.lmu.Unlock()
	return c.composeInstr.StopComposeApp(ctx)
}

func (c *composeDrainInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	return c.d.GetCaptureLosses(ctx)
}

func (c *composeDrainInstr) DrainCaptureLosses(ctx context.Context) error {
	return c.d.DrainCaptureLosses(ctx)
}

func (c *composeDrainInstr) TakeOwnedCaptureLosses(ctx context.Context) ([]models.OwnedCaptureLoss, error) {
	return c.d.TakeOwnedCaptureLosses(ctx)
}

// docker compose, its stack brought up per set or kept across sets (a reused
// stack): under the switch each set's end stops the app's container before
// its read, whatever the set's place in the run, so bytes a connection lost
// as it stopped fail that set. A later set wholly ignored, or with no test
// case, finds the app stopped: it stops nothing and takes nothing, and no
// close of the app is left to the run's teardown.
func TestAComposeSetStopsItsAppBeforeAnIgnoredOrEmptyLastSet(t *testing.T) {
	provenOn(t, true)
	for _, kept := range []bool{false, true} {
		for _, last := range []string{"ignored", "empty"} {
			name := "a stack per set/the last set " + last
			if kept {
				name = "a reused stack/the last set " + last
			}
			t.Run(name, func(t *testing.T) {
				c := &composeInstr{}
				h := composeHarness(t, c)
				h.replayer.config.Test.Mocking = true
				lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1,
					What: "from [::ffff:127.0.0.1]:50914, written as the application stopped"}
				d := &drainInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
					stopLoss: &models.OwnedCaptureLoss{Loss: lost}}
				h.replayer.instrumentation = &composeDrainInstr{composeInstr: c, d: d}
				h.replayer.markPIDMaps(context.Background())
				run := h.run
				if kept {
					run = h.runServing
				}
				if status := run(t); status != models.TestSetStatusFailed {
					t.Fatalf("test-set-0 %q, want FAILED: bytes were lost as its app container stopped", status)
				}
				d.lmu.Lock()
				order := strings.Join(d.order, ",")
				d.lmu.Unlock()
				if c.stops != 1 || !strings.HasSuffix(order, "stop,drain,take") {
					t.Fatalf("%d stop(s), order %q; want test-set-0's end to stop the app's container, then drain, then take", c.stops, order)
				}
				want := runsNothing(h, last)
				h.set = "test-set-1"
				resetResults(h)
				if status := run(t); status != want {
					t.Fatalf("test-set-1 %q, want %q", status, want)
				}
				d.lmu.Lock()
				after := strings.Join(d.order, ",")
				d.lmu.Unlock()
				if c.stops != 1 || after != order {
					t.Fatalf("test-set-1: %d stop(s), order %q; want nothing stopped or taken after test-set-0's end", c.stops, after)
				}
			})
		}
	}
}

// lastSetThatRuns is the last set of a run that begins its session on the
// agent: a set wholly ignored, or with no test case, is passed over from the
// end; one partly ignored runs, and so does one whose test cases cannot be
// read (stopping the application before a set that runs would fail its
// tests). None that runs is -1.
func TestTheLastSetThatRunsPassesOverIgnoredAndEmptySetsFromTheEnd(t *testing.T) {
	for _, c := range []struct {
		name    string
		ignored map[string][]string
		empty   []string
		broken  string
		want    int
	}{
		{name: "every set runs", want: 2},
		{name: "the last wholly ignored", ignored: map[string][]string{"s2": {}}, want: 1},
		{name: "the last partly ignored", ignored: map[string][]string{"s2": {"test-1"}}, want: 2},
		{name: "the last empty", empty: []string{"s2"}, want: 1},
		{name: "the last two ignored and empty", ignored: map[string][]string{"s2": {}}, empty: []string{"s1"}, want: 0},
		{name: "an earlier set empty", empty: []string{"s0"}, want: 2},
		{name: "the last unreadable", broken: "s2", want: 2},
		{name: "none runs", ignored: map[string][]string{"s0": {}, "s2": {}}, empty: []string{"s1"}, want: -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.IgnoredTests = c.ignored
			h.replayer.testDB = setsDB{prTestDB: &prTestDB{cases: h.cases}, empty: c.empty, broken: c.broken}
			if got := h.replayer.lastSetThatRuns(context.Background(), []string{"s0", "s1", "s2"}); got != c.want {
				t.Fatalf("the last set that runs is %d, want %d", got, c.want)
			}
		})
	}
}

// setsDB is the harness's test DB with the sets empty holding no test case
// and the set broken unreadable.
type setsDB struct {
	*prTestDB
	empty  []string
	broken string
}

func (d setsDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	if set == d.broken {
		return nil, errors.New("failed to read the testcase")
	}
	for _, e := range d.empty {
		if set == e {
			return nil, nil
		}
	}
	return d.prTestDB.GetTestCases(ctx, set)
}

// A loss an earlier session of the very set whose take returned it made (the
// agent's session of the set was begun again: a later attempt of the set, or
// its mocks registered again on the agent part way through it) is said as
// that, never as one found after an earlier test set had ended, the set
// still running; one of another set, or of a set the agent no longer names,
// read where no set is named, is an earlier test set's. Each leaves the run
// not judged for its set.
func TestAnEarlierSessionOfTheSameSetIsSaidAsThat(t *testing.T) {
	lost := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:50914"}
	untold := models.CaptureLoss{Kind: models.CaptureLossAppBytesUntold, Count: 1, What: "from 127.0.0.1:50915"}
	for _, c := range []struct {
		name, set, readIn string
		loss              models.CaptureLoss
		says, where       string
	}{
		{"the same set, bytes lost", "test-set-0", "test-set-0", lost,
			"keploy found, only after it began this test set's session on its agent again (for a later attempt of the set, or to register its mocks again part way through it), that it could not capture a call on a connection of the set's earlier session",
			"test-2 of test-set-0, in an earlier session of that set's on keploy's agent"},
		{"the same set, an untold close", "test-set-0", "test-set-0", untold,
			"that it cannot tell whether a connection of the set's earlier session carried a call it did not capture",
			"test-2 of test-set-0, in an earlier session of that set's on keploy's agent"},
		{"another set", "test-set-0", "test-set-1", lost,
			"keploy found, only after an earlier test set had ended, that it could not capture a call on a connection of that set's",
			"test-2 of test-set-0"},
		{"a set no longer named, read where none is", "", "", lost,
			"keploy found, only after an earlier test set had ended, that",
			"test-2 of an earlier test set"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.ErrorLevel)
			r := &Replayer{logger: zap.New(core), completeTestReport: map[string]TestReportVerdict{"test-set-0": {status: true}}}
			rest := r.takeEarlierSetLosses([]models.OwnedCaptureLoss{{Test: "test-2", Loss: c.loss, EarlierSession: true, Set: c.set}}, c.readIn)
			if len(rest) != 0 {
				t.Fatalf("rest %+v, want the loss kept for the run", rest)
			}
			said := logs.All()
			if len(said) != 1 || !strings.Contains(said[0].Message, c.says) {
				t.Fatalf("said %+v, want one line saying %q", said, c.says)
			}
			if all := r.earlierSetLosses.all(); len(all) != 1 || all[0].madeWhere() != c.where {
				t.Fatalf("kept %+v, want one made in %q", all, c.where)
			}
			if r.passesEarlierSetLosses(true) {
				t.Fatal("the run passed")
			}
		})
	}
}
