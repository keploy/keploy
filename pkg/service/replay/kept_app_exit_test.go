package replay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An application --keep-app-alive keeps across the run's sets that exits
// before it accepts a connection fails the first set as its exit does,
// saying so, and the run stops naming that exit: never "replay completed
// successfully", and never as a stop by the user.
func TestAKeptAppThatExitsBeforeItAcceptsStopsTheRunNamingItsExit(t *testing.T) {
	refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
	h := newPartialRunHarness(t, 2, 0)
	for _, tc := range h.cases {
		tc.HTTPReq.URL = "http://" + refusing + "/x"
	}
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.KeepAppAlive = true
	h.replayer.config.Test.MaxFlakyChecks = 1
	// The application exits once the first set waits for it to accept a
	// connection.
	var exit sync.Once
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "waiting for the application to accept a connection on "+refusing) {
			exit.Do(func() { close(h.instr.appStopped) })
		}
		return nil
	}))
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	err := h.replayer.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "exited before it accepted a connection") {
		t.Errorf("Start: %v, want an error saying the application exited before it accepted a connection", err)
	}
	if logs.FilterMessageSnippet("the application exited before it accepted a connection").Len() == 0 {
		t.Error("no line says the application exited before it accepted a connection")
	}
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	if !strings.Contains(reason, "exited before it accepted a connection") {
		t.Errorf("the run stops with the reason %q, want one naming the application's exit", reason)
	}
	if n := len(h.report.results); n != 0 {
		t.Errorf("%d tests were sent, want none", n)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1", utils.ErrCode)
	}
}

type keptAppKey struct{}

// keptAppExit reads an application's exit only from a run's context that its
// exit ended; a context still running, or one its user stopped, has none.
func TestKeptAppExitIsReadOnlyFromARunItsExitEnded(t *testing.T) {
	exit := models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}

	running, stop := context.WithCancel(context.Background())
	defer stop()
	if got := keptAppExit(running); got != (models.AppError{}) {
		t.Errorf("a running context: %+v, want none", got)
	}
	stopped, stopIt := context.WithCancel(context.Background())
	stopIt()
	if got := keptAppExit(stopped); got != (models.AppError{}) {
		t.Errorf("a context its user stopped: %+v, want none", got)
	}
	ended, endIt := context.WithCancelCause(context.Background())
	endIt(exit)
	// The set reads it through the contexts the run derives from its own.
	child, cancelChild := context.WithCancel(context.WithValue(ended, keptAppKey{}, 1))
	defer cancelChild()
	if got := keptAppExit(child); got != exit {
		t.Errorf("a run its application's exit ended: %+v, want %+v", got, exit)
	}
	other, endOther := context.WithCancelCause(context.Background())
	endOther(errors.New("an agent call failed"))
	if got := keptAppExit(other); got != (models.AppError{}) {
		t.Errorf("a run another error ended: %+v, want none", got)
	}
}

// A run its context ended as a set ran stops with a reason that names what
// ended it: the set's own error when it says more than the cancellation, else
// the cause; never "replay completed successfully".
func TestCancelledRunReasonNamesWhatEndedTheRun(t *testing.T) {
	exit := models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}
	setErr := fmt.Errorf("the application exited before it accepted a connection: %w", exit)
	for _, c := range []struct {
		name          string
		setErr, cause error
		want          string
	}{
		{"the set's error", setErr, exit, "failed to run test set: " + setErr.Error()},
		{"a cancelled set, the cause", context.Canceled, exit, "the run stopped during test-set s1: " + exit.Error()},
		{"a wrapped cancellation, the cause", fmt.Errorf("send: %w", context.Canceled), exit, "the run stopped during test-set s1: " + exit.Error()},
		{"no set error, the cause", nil, exit, "the run stopped during test-set s1: " + exit.Error()},
		{"nothing said", context.Canceled, context.Canceled, "the run was cancelled before its test sets ended"},
		{"no cause", context.Canceled, nil, "the run was cancelled before its test sets ended"},
	} {
		if got := cancelledRunReason("s1", c.setErr, c.cause); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// An application that exits part-way through a set (on the set's second
// test) fails the set as its exit does, never as a stop by its user, whether
// keploy runs it for the set or keeps it across the run's sets
// (--keep-app-alive), or docker compose keeps its stack across them, and the
// run stops at that set, naming the exit, its code and the set: never "replay
// completed successfully". No later set runs: none has an application to run
// on. The exit code is 1.
func TestAnAppThatExitsMidSetFailsTheSetAndTheRunNamesItsExit(t *testing.T) {
	for _, c := range []struct {
		name       string
		kept       bool
		compose    bool
		exit       models.AppErrorType
		wantStatus models.TestSetStatus
	}{
		{name: "run per set", wantStatus: models.TestSetStatusAppHalted},
		{name: "kept alive", kept: true, wantStatus: models.TestSetStatusAppHalted},
		{name: "run per set, its command fails", exit: models.ErrCommandError, wantStatus: models.TestSetStatusFaultUserApp},
		{name: "a compose stack kept across sets", compose: true, wantStatus: models.TestSetStatusAppHalted},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.testDB = twoPRSetsDB{h.replayer.testDB.(*prTestDB)}
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			if c.compose {
				// Compose keeps its stack across a run's sets when keploy
				// mocks the dependencies; keploy waits for the agent in it
				// to answer healthy first.
				h.replayer.config.Command = "docker compose up"
				h.replayer.config.CommandType = string(utils.DockerCompose)
				h.replayer.config.Test.Mocking = true
				agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
				defer agent.Close()
				h.replayer.config.Agent.AgentURI = agent.URL
			}
			h.instr.stopErr = c.exit
			h.replayer.config.Test.KeepAppAlive = c.kept
			h.replayer.config.Test.MaxFlakyChecks = 1
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core)
			h.instr.stopAppAfterNUpdates = 2
			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })
			_ = h.replayer.Start(ctx)
			if ran := setsRan(logs); len(ran) != 1 || ran[0] != "test-set-0" {
				t.Errorf("the run ran the sets %v, want it to stop after test-set-0", ran)
			}
			rep := h.report.report
			if rep == nil {
				t.Fatal("no report was persisted")
			}
			if rep.Status != string(c.wantStatus) {
				t.Errorf("the set ended %s, want %s", rep.Status, c.wantStatus)
			}
			stops := logs.FilterMessage("stopping Keploy").All()
			if len(stops) != 1 {
				t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
			}
			reason, _ := stops[0].ContextMap()["reason"].(string)
			exitType := models.ErrAppStopped
			if c.exit != "" {
				exitType = c.exit
			}
			for _, want := range []string{"exited", "test-set-0", "exit code 1", string(exitType)} {
				if !strings.Contains(reason, want) {
					t.Errorf("the run stops with the reason %q, want one that says %q", reason, want)
				}
			}
			if utils.ErrCode != 1 {
				t.Errorf("exit code %d, want 1", utils.ErrCode)
			}
		})
	}
}

// haltedRunReason names the application's exit, with its code when it has
// one, and the set: the exit keploy saw, else the one the set's error wraps;
// with no exit known, the set's status and its error.
func TestHaltedRunReasonNamesTheExitAndTheSet(t *testing.T) {
	exit3 := models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}
	for _, c := range []struct {
		name   string
		exit   models.AppError
		setErr error
		want   string
	}{
		{"an exit with a code", exit3, nil, "the application exited during test-set s1 with exit code 3: exited due to command error: exit status 3"},
		{"a signal", models.AppError{AppErrorType: models.ErrCommandError, ExitCode: -1, Err: errors.New("signal: killed")}, nil, "the application exited during test-set s1: exited due to command error: signal: killed"},
		{"no code known", models.AppError{AppErrorType: models.ErrAppStopped, Err: errors.New("stopped")}, nil, "the application exited during test-set s1: app stopped: stopped"},
		{"no exit seen", models.AppError{}, nil, "test-set s1 ended APP_HALTED"},
		{"the exit the set's error wraps", models.AppError{}, fmt.Errorf("the application exited before keploy's agent in its stack was ready: %w", exit3), "the application exited during test-set s1 with exit code 3: exited due to command error: exit status 3"},
		{"the exit seen, not the set's error's", exit3, fmt.Errorf("wraps: %w", models.AppError{AppErrorType: models.ErrAppStopped, ExitCode: 9}), "the application exited during test-set s1 with exit code 3: exited due to command error: exit status 3"},
		{"no exit known, the set's error", models.AppError{}, errors.New("the stack went away"), "test-set s1 ended APP_HALTED: the stack went away"},
	} {
		if got := haltedRunReason("s1", models.TestSetStatusAppHalted, c.exit, c.setErr); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A run whose sets ended stops with what stopped it; when nothing did, with
// the halts of the sets it went on past, each with its exit, and only with
// "replay completed successfully" when none halted.
func TestRunStopReasonNamesTheSetsTheRunWentOnPast(t *testing.T) {
	halted := []string{"the application exited during test-set s1 with exit code 3: x", "test-set s2 ended APP_HALTED"}
	for _, c := range []struct {
		name, reason string
		halted       []string
		want         string
	}{
		{"none halted", runCompletedReason, nil, runCompletedReason},
		{"sets halted", runCompletedReason, halted, halted[0] + "; " + halted[1]},
		{"stopped for another reason", "failed to run test set: boom", halted, "failed to run test set: boom"},
	} {
		if got := runStopReason(c.reason, c.halted); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A set whose context ended before an application keploy started for it
// exited fails as the exit of the application --keep-app-alive keeps does
// when that exit ended the run, handing the exit on; the user's stop, or a
// run still going, is USER_ABORT and hands on nothing.
func TestStoppedSetStatusIsTheKeptExitsOrTheUsersStop(t *testing.T) {
	exit := models.AppError{AppErrorType: models.ErrAppStopped, ExitCode: 1}
	running, stopRunning := context.WithCancel(context.Background())
	defer stopRunning()
	stopped, stop := context.WithCancel(context.Background())
	stop()
	ended, end := context.WithCancelCause(context.Background())
	end(exit)
	for _, c := range []struct {
		name       string
		ctx        context.Context
		wantStatus models.TestSetStatus
		wantExit   models.AppError
	}{
		{"the kept application's exit", ended, models.TestSetStatusAppHalted, exit},
		{"the user's stop", stopped, models.TestSetStatusUserAbort, models.AppError{}},
		{"a run still going", running, models.TestSetStatusUserAbort, models.AppError{}},
	} {
		var handed []models.AppError
		status := stoppedSetStatus(c.ctx, func(e models.AppError) { handed = append(handed, e) })
		if status != c.wantStatus {
			t.Errorf("%s: %s, want %s", c.name, status, c.wantStatus)
		}
		switch {
		case c.wantExit == (models.AppError{}) && len(handed) != 0:
			t.Errorf("%s: handed on %+v, want nothing", c.name, handed)
		case c.wantExit != (models.AppError{}) && (len(handed) != 1 || handed[0] != c.wantExit):
			t.Errorf("%s: handed on %+v, want %+v once", c.name, handed, c.wantExit)
		}
	}
}

// insertCancelledReportDB is the harness's report store, whose first report
// insert ends the application --keep-app-alive keeps and then fails with the
// set's context's error, as a store that fails on a cancelled context does.
type insertCancelledReportDB struct {
	*prReportDB
	once sync.Once
	exit chan struct{}
}

func (f *insertCancelledReportDB) InsertReport(ctx context.Context, runID, setID string, rep *models.TestReport) error {
	first := false
	f.once.Do(func() { first = true })
	if !first {
		return f.prReportDB.InsertReport(ctx, runID, setID, rep)
	}
	close(f.exit)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(prTimeout):
		return errors.New("the kept application's exit never ended the set's context")
	}
}

// A kept application that exits between a set's steps, where the step under
// way returns the set's cancellation as its error, stops the run with a
// reason that names the exit (the run's cause) and the set.
func TestAKeptAppsExitBetweenStepsStopsTheRunNamingItsExit(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.KeepAppAlive = true
	h.replayer.config.Test.MaxFlakyChecks = 1
	h.replayer.reportDB = &insertCancelledReportDB{prReportDB: h.report, exit: h.instr.appStopped}
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	for _, want := range []string{"test-set-0", string(models.ErrAppStopped)} {
		if !strings.Contains(reason, want) {
			t.Errorf("the run stops with the reason %q, want one that says %q", reason, want)
		}
	}
}

// Under docker compose with keploy's mocks off, a stack runs per set and a
// set whose application halts does not stop the run: the next set runs on a
// stack of its own. The run's stop names each set that halted, with its
// own exit, never "replay completed successfully". Here the application
// exits mid-way through the first set and, at once, on the second set's
// stack before keploy's agent in it is ready, its command failing.
func TestAComposeSetsHaltDoesNotStopTheRunButItsStopNamesIt(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.testDB = twoPRSetsDB{h.replayer.testDB.(*prTestDB)}
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Test.MaxFlakyChecks = 1
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer agent.Close()
	h.replayer.config.Agent.AgentURI = agent.URL
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	h.instr.stopAppAfterNUpdates = 2
	h.instr.stopErrs = []models.AppErrorType{models.ErrAppStopped, models.ErrCommandError}
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)
	ran := setsRan(logs)
	if len(ran) < 2 {
		t.Fatalf("the run ran the sets %v, want it to go on past the halted set", ran)
	}
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	parts := strings.Split(reason, "; ")
	if len(parts) != 2 {
		t.Fatalf("the run stops with the reason %q, want one that names each of the 2 halted sets", reason)
	}
	for i, c := range []struct {
		set  string
		exit models.AppErrorType
	}{{"test-set-0", models.ErrAppStopped}, {"test-set-1", models.ErrCommandError}} {
		for _, want := range []string{"exited during test-set " + c.set, "exit code 1", string(c.exit)} {
			if !strings.Contains(parts[i], want) {
				t.Errorf("the run's reason names %q for its set %d, want it to say %q", parts[i], i, want)
			}
		}
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1", utils.ErrCode)
	}
}

// twoPRSetsDB is the harness's test DB with two sets that hold the same tests.
type twoPRSetsDB struct{ *prTestDB }

func (twoPRSetsDB) GetAllTestSetIDs(context.Context) ([]string, error) {
	return []string{"test-set-0", "test-set-1"}, nil
}

// A set the run runs again (a flaky check's next attempt) and that halts on
// each attempt is named once in the run's stop, not once per attempt: docker
// compose, a stack per set, where the run goes on past a halted set.
func TestASetThatHaltsOnEachAttemptIsNamedOnceInTheRunsStop(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Test.MaxFlakyChecks = 2
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer agent.Close()
	h.replayer.config.Agent.AgentURI = agent.URL
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	h.instr.stopAppAfterNUpdates = 2
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)
	if ran := setsRan(logs); len(ran) != 2 || ran[0] != "test-set-0" || ran[1] != "test-set-0" {
		t.Fatalf("the run ran %v, the case needs test-set-0 run twice", ran)
	}
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	if n := strings.Count(reason, "test-set-0"); n != 1 || strings.Contains(reason, "; ") {
		t.Errorf("the run stops with the reason %q, want it to name test-set-0 once", reason)
	}
	if !strings.Contains(reason, "exited during test-set test-set-0") {
		t.Errorf("the run stops with the reason %q, want it to name test-set-0's exit", reason)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1", utils.ErrCode)
	}
}

// noteSetStop keeps one reason per set, the latest noted, in the sets' order.
func TestNoteSetStopKeepsEachSetsLatestReasonInOrder(t *testing.T) {
	at := map[string]int{}
	var stops []string
	stops = noteSetStop(stops, at, "a", "a1")
	stops = noteSetStop(stops, at, "b", "b1")
	stops = noteSetStop(stops, at, "a", "a2")
	if got := strings.Join(stops, ","); got != "a2,b1" {
		t.Fatalf("the stops are %q, want \"a2,b1\"", got)
	}
}

// haltOnceInstr is the harness's instrumentation whose application exits
// part-way through its first run, as the harness's does (stopAppAfterNUpdates).
// Every later run calls again as it starts and stays up until its run's
// context ends.
type haltOnceInstr struct {
	*prInstr
	again func()
	runs  atomic.Int32
}

func (f *haltOnceInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	if f.runs.Add(1) == 1 {
		return f.prInstr.Run(ctx, opts)
	}
	f.again()
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
}

// A set run again after it halted (docker compose, a stack per set) whose
// next attempt ends INTERNAL_ERR and stops the run is named once in the
// run's stop, with that latest attempt's internal error, not also with the
// halt of the attempt before it.
func TestASetThatHaltsThenStopsTheRunIsNamedOnceWithItsLatestEnd(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Test.MaxFlakyChecks = 2
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer agent.Close()
	h.replayer.config.Agent.AgentURI = agent.URL
	// The first attempt's application exits part-way through the set. As
	// the next attempt's starts, test-1's result can no longer be written:
	// that attempt ends INTERNAL_ERR.
	h.instr.stopAppAfterNUpdates = 2
	h.replayer.instrumentation = &haltOnceInstr{prInstr: h.instr, again: func() {
		h.report.mu.Lock()
		h.report.failInsertOf = map[string]error{"test-1": errors.New("report store is unwritable")}
		h.report.mu.Unlock()
	}}
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)
	if ran := setsRan(logs); len(ran) != 2 || ran[0] != "test-set-0" || ran[1] != "test-set-0" {
		t.Fatalf("the run ran %v, the case needs test-set-0 run twice", ran)
	}
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	if want := "test-set test-set-0 ended INTERNAL_ERR: "; !strings.HasPrefix(reason, want) || strings.Count(reason, "test-set-0") != 1 {
		t.Errorf("the run stops with the reason %q, want only test-set-0's latest end, %q...", reason, want)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1", utils.ErrCode)
	}
}

// A set run again after it halted (docker compose, a stack per set) whose
// next attempt runs clean keeps that halt in the run's stop: a set is named
// with the end of its latest attempt that halted, or of the one whose
// INTERNAL_ERR stopped the run (always its last). The attempt named need
// not be its latest: here the latest ran clean. Its report, the latest
// attempt's, says PASSED, and the halt still fails the run: it exits 1.
func TestASetThatHaltsThenRunsCleanKeepsItsHaltInTheRunsStop(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Test.MaxFlakyChecks = 2
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer agent.Close()
	h.replayer.config.Agent.AgentURI = agent.URL
	// The first attempt's application exits part-way through the set; the
	// next attempt's stays up and the set runs clean.
	h.instr.stopAppAfterNUpdates = 2
	h.replayer.instrumentation = &haltOnceInstr{prInstr: h.instr, again: func() {}}
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)
	if ran := setsRan(logs); len(ran) != 2 || ran[0] != "test-set-0" || ran[1] != "test-set-0" {
		t.Fatalf("the run ran %v, the case needs test-set-0 run twice", ran)
	}
	h.report.mu.Lock()
	rep := h.report.report
	h.report.mu.Unlock()
	if rep == nil || rep.Status != string(models.TestSetStatusPassed) {
		t.Fatalf("the set's report is %+v, the case needs its second attempt PASSED", rep)
	}
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	if want := "the application exited during test-set test-set-0 with exit code 1: app stopped"; reason != want {
		t.Errorf("the run stops with the reason %q, want the first attempt's halt, %q", reason, want)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1", utils.ErrCode)
	}
}
