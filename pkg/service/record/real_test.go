package record

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// realRunInst is an agent client that reports the run's mock.real.
type realRunInst struct {
	Instrumentation
	run models.RealRun
	err error
}

func (f realRunInst) PushRealDeps(context.Context, []string) error { return nil }

func (f realRunInst) RealRun(context.Context) (models.RealRun, error) { return f.run, f.err }

// As a recording ends, what keploy could not capture is said at ERROR, once
// per destination and reason with its count: no test case holds what those
// connections carried, so the recording is incomplete and the run fails, as
// for a capture loss. A read that fails is said and fails the run too;
// without the switch or a list nothing is asked.
func TestReportRealRunSaysWhatKeployCouldNotCapture(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	why := "list the address in mock.real"
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: why}
	rec := func(inst Instrumentation, real ...string) (*observer.ObservedLogs, bool) {
		core, logs := observer.New(zapcore.WarnLevel)
		cfg := &config.Config{}
		cfg.Mock.Real = real
		r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: inst}
		r.reportRealRun(context.Background())
		return logs, r.realUncaptured.Load() || r.realUnread.Load() != nil
	}
	logs, failed := rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc, inc, {Destination: "10.0.0.8:443", Why: "x"}}}}, "db:5432")
	require.True(t, failed, "a recording keploy could not capture whole fails")
	require.Equal(t, 2, logs.FilterLevelExact(zapcore.ErrorLevel).Len())
	first := logs.All()[0].ContextMap()
	require.Equal(t, "10.0.0.7:443", first["destination"])
	require.EqualValues(t, 2, first["connections"])
	require.Contains(t, first["next_step"], why)

	logs, failed = rec(realRunInst{err: errors.New("agent gone")}, "db:5432")
	require.True(t, failed, "a recording keploy cannot tell is whole fails")
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).Len())

	logs, failed = rec(realRunInst{run: models.RealRun{Reached: []string{"db:5432"}}}, "db:5432")
	require.False(t, failed)
	require.Zero(t, logs.Len())

	logs, failed = rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}})
	require.Zero(t, logs.Len(), "no list, nothing asked")
	require.False(t, failed)
	t.Setenv(provenswitch.Env, "")
	logs, failed = rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}, "db:5432")
	require.Zero(t, logs.Len(), "no switch, nothing asked")
	require.False(t, failed)
}

// Where a later pass judges each test case (JudgeLossesLater), what keploy
// could not capture of mock.real, and a read of it that failed, are each a
// WARN, not an ERROR: the recording is kept, and they are handed over
// (finish), the failed read as that, not as connections lost.
func TestReportRealRunJudgedLaterIsAWarn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	judgeLater(t)
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}
	for _, inst := range []realRunInst{{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}, {err: errors.New("agent gone")}} {
		core, logs := observer.New(zapcore.WarnLevel)
		cfg := &config.Config{}
		cfg.Mock.Real = []string{"db:5432"}
		r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: inst}
		r.reportRealRun(context.Background())
		require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "an ERROR where a later pass judges: %v", logs.All())
		require.Equal(t, 1, logs.FilterMessageSnippet("the auto-replay that follows judges each of its test cases").Len())
		if inst.err != nil {
			require.False(t, r.realUncaptured.Load(), "a failed read is no connection lost")
			require.NotNil(t, r.realUnread.Load())
			require.Contains(t, *r.realUnread.Load(), "agent gone")
		}
	}
}

// What reportRealRun found of mock.real goes with the capture watch's end
// (finishCapture), whether or not the agent can count capture losses: a
// failed read fails the recording judged on its own, and is handed, as that,
// to a later pass, which fails nothing.
func TestARecordingsEndTakesWhatMockRealFound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}
	for _, c := range []struct {
		name string
		inst realRunInst
	}{
		{"a failed read", realRunInst{err: errors.New("agent gone")}},
		{"a connection lost", realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}},
	} {
		for _, counts := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, agent counts %v", c.name, counts), func(t *testing.T) {
				// A fresh agent for each watch: its reads are counted.
				agent := func() Instrumentation {
					if !counts {
						return &countingInstr{errAt: map[int]error{1: models.ErrCaptureLossesUnsupported}, answers: []models.CaptureLosses{nil}}
					}
					return &countingInstr{answers: []models.CaptureLosses{nil}}
				}
				cfg := &config.Config{}
				cfg.Mock.Real = []string{"db:5432"}
				r := &Recorder{logger: zap.NewNop(), config: cfg, instrumentation: c.inst}
				r.reportRealRun(context.Background())
				w := watchCaptureLosses(context.Background(), zap.NewNop(), agent())
				if w.off == counts {
					t.Fatalf("the watch is off %v for an agent that counts %v", w.off, counts)
				}
				if fail, incomplete := r.finishCapture(w, "test-set-0"); !fail || !incomplete {
					t.Fatalf("judged on its own: fail %v, incomplete %v; want both", fail, incomplete)
				}
				got := judgeLater(t)
				w = watchCaptureLosses(context.Background(), zap.NewNop(), agent())
				if fail, incomplete := r.finishCapture(w, "test-set-0"); fail || !incomplete {
					t.Fatalf("judged later: fail %v, incomplete %v; want not failed, incomplete", fail, incomplete)
				}
				if len(*got) != 1 || (*got)[0].RealUncaptured == (c.inst.err != nil) || ((*got)[0].RealUnread != "") != (c.inst.err != nil) {
					t.Fatalf("handed %+v, want the %s as that", *got, c.name)
				}
			})
		}
	}
}

// The session's telemetry says a recording incomplete for the agent's captures
// not all handed over or written, as before, and for what the capture watch
// found, even where a later pass judges it and the run does not fail.
func TestTheSessionsEndSaysWhyItIsIncomplete(t *testing.T) {
	for _, c := range []struct {
		name                                   string
		stop                                   string
		heldBack, lossy, storeWrote, agentLost bool
		status, reason                         string
	}{
		{"clean", "", false, false, true, false, "completed", ""},
		{"stopped", "agent not ready", true, true, true, false, "aborted", "agent not ready"},
		{"held back", "", true, false, true, false, "incomplete", "the agent's captures from before the stop were not all handed over"},
		{"not written", "", true, true, false, false, "incomplete", "mocks the recording captured were not written to the mock file"},
		{"capture lost", "", false, true, true, false, "incomplete", "keploy's agent could not capture every call while it recorded, or could not be checked"},
	} {
		status, reason := sessionEnd(c.stop, c.heldBack, c.lossy, c.storeWrote, c.agentLost)
		if status != c.status || reason != c.reason {
			t.Errorf("%s: %q, %q; want %q, %q", c.name, status, reason, c.status, c.reason)
		}
	}
}

// silentRealInst is an agent client whose agent never answers the read of
// what the run saw of mock.real (GET /real/run): the read ends only as its
// context does. It answers every read of its count (nothing lost), and its
// app ends once appEnd is closed; its frame streams end when keploy says it
// is shutting down.
type silentRealInst struct {
	*fakeInstr
	appEnd chan struct{}
	asked  chan struct{} // closed as the read comes
	ended  chan struct{} // closed as it ends
}

func newSilentRealInst() *silentRealInst {
	return &silentRealInst{fakeInstr: newFakeInstr(), appEnd: make(chan struct{}), asked: make(chan struct{}), ended: make(chan struct{})}
}

func (s *silentRealInst) PushRealDeps(context.Context, []string) error { return nil }

func (s *silentRealInst) RealRun(ctx context.Context) (models.RealRun, error) {
	close(s.asked)
	<-ctx.Done()
	close(s.ended)
	return models.RealRun{}, ctx.Err()
}

func (s *silentRealInst) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	return nil, nil
}

func (s *silentRealInst) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case <-s.appEnd:
	case <-ctx.Done():
	}
	return models.AppError{AppErrorType: models.ErrAppStopped}
}

func (s *silentRealInst) NotifyGracefulShutdown(context.Context) error {
	close(s.mappings)
	close(s.incoming)
	close(s.outgoing)
	return nil
}

// realRecorder is a Recorder of mock.real list real, logging at Info.
func realRecorder(inst Instrumentation, real ...string) (*Recorder, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)
	cfg := &config.Config{}
	cfg.Mock.Real = real
	return &Recorder{logger: zap.New(core), config: cfg, instrumentation: inst}, logs
}

// saidTheRealBound waits for the stop to say, at Info, that it waits for the
// agent to say what it could not capture of mock.real, that Ctrl+C does not
// cut the wait short, and the read's bound.
func saidTheRealBound(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	const want = "Still waiting for the keploy agent to say which mock.real connections it could not capture; Ctrl+C does not cut this short: the read is given up once keploy has had 1m0s of its own time to run without an answer"
	deadline := time.Now().Add(30 * time.Second) // bounds only a wait that never says so
	for logs.FilterMessage(want).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the wait never said it waits, with its bound: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if e := logs.FilterMessage(want).All()[0]; e.Level != zapcore.InfoLevel {
		t.Fatalf("said at %v, want Info", e.Level)
	}
}

// realReadNotRead is why the one ERROR a mock.real read not answered gives
// says it was not.
func realReadNotRead(t *testing.T, logs *observer.ObservedLogs) string {
	t.Helper()
	errs := logs.FilterMessageSnippet("keploy could not read from its agent which mock.real connections it could not capture").All()
	if len(errs) != 1 || errs[0].Level != zapcore.ErrorLevel {
		t.Fatalf("%d ERRORs saying the mock.real read was not read, want 1: %v", len(errs), logs.All())
	}
	return fmt.Sprint(errs[0].ContextMap()["error"])
}

// As a recording ends under the switch with a mock.real list, a read of what
// it could not capture the agent never answers holds the stop only until
// keploy had its bound to run: it says it waits, and its bound; the stop
// does not end it, a press finds no offer, and wall time keploy could not run
// gives nothing up. The bound does, and the recording fails, saying the read
// was not answered. Before, the read had no bound: the stop waited for it
// forever.
func TestReportRealRunIsGivenUpOnItsBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	clk := manualCounts(t)
	inst := newSilentRealInst()
	r, logs := realRecorder(inst, "db:5432")
	ctx, stop := context.WithCancel(context.Background())
	stop() // the stop has ended record's context: the read is made after it
	done := make(chan struct{})
	go func() { r.reportRealRun(ctx); close(done) }()
	within(t, inst.asked, "the mock.real read")
	saidTheRealBound(t, logs)
	if utils.MarkInterruptedAgain() {
		t.Fatal("the mock.real read stood an offer of the stop's")
	}
	for i := 0; i < 10; i++ {
		clk.Pass() // wall time passes; keploy cannot run
	}
	time.Sleep(50 * time.Millisecond) // the wait looks again, every millisecond: it says so once
	select {
	case <-done:
		t.Fatal("the mock.real read ended on the stop, a press, or wall time keploy could not run")
	default:
	}
	if n := logs.FilterMessageSnippet("Still waiting for the keploy agent to say which mock.real").Len(); n != 1 {
		t.Fatalf("said %d times that it waits, want once", n)
	}
	giveUp(t, clk)
	within(t, done, "the mock.real read, given up on its bound")
	within(t, inst.ended, "the end of the read the agent never answered")
	require.NotNil(t, r.realUnread.Load(), "a recording whose mock.real read was not answered fails")
	require.Equal(t, errRealRunSilent.Error(), realReadNotRead(t, logs))
}

// The interrupt that gave the stop's waits up (a drain's offer answered:
// utils.StopNow) ends the mock.real read at once; the recording fails,
// saying a Ctrl+C stopped the wait.
func TestReportRealRunEndsWhenTheUserGaveTheStopsWaitsUp(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	manualCounts(t)
	inst := newSilentRealInst()
	r, logs := realRecorder(inst, "db:5432")
	done := make(chan struct{})
	go func() { r.reportRealRun(context.Background()); close(done) }()
	within(t, inst.asked, "the mock.real read")
	_, withdraw := utils.OfferStopNow() // a drain said another interrupt stops it now
	defer withdraw()
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer the drain's offer")
	}
	within(t, done, "the mock.real read, at the press that gave the stop's waits up")
	within(t, inst.ended, "the end of the read")
	require.NotNil(t, r.realUnread.Load(), "a recording whose mock.real read was not answered fails")
	require.Equal(t, errRealRunStopped.Error(), realReadNotRead(t, logs))
}

// keploy record under the switch, with a mock.real list and an agent that
// never answers the read of what it could not capture of it: the stop says
// it waits, with the bound, and Start returns once the read is given up on
// it, the recording failed (exit 1), saying why. Before, Start never
// returned.
func TestStart_AMockRealReadTheAgentNeverAnswersHoldsNoStop(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := manualCounts(t)
	inst := newSilentRealInst()
	r := exitCodeRecorder(inst)
	r.config.Mock.Real = []string{"db:5432"}
	core, logs := observer.New(zapcore.InfoLevel)
	r.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	select {
	case inst.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	close(inst.appEnd) // the stop
	within(t, inst.asked, "the stop's mock.real read")
	saidTheRealBound(t, logs)
	giveUp(t, clk)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("Start did not return once the mock.real read was given up on its bound")
	}
	if utils.ErrCode != utils.ExitKeployError {
		t.Fatalf("exit code %d, want %d: a recording keploy cannot tell is whole fails", utils.ErrCode, utils.ExitKeployError)
	}
	require.Equal(t, errRealRunSilent.Error(), realReadNotRead(t, logs))
}
