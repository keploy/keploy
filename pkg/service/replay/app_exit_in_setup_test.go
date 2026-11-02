package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// setupExitInstr is a compose stack whose app exits while keploy sets a set
// up, before the set's first test: tell is the exit, told at the setup's
// step named at (setupExitSteps, moreSetupExitSteps), and known is closed
// once keploy keeps it (the app's runner told it, or its returned exit was
// read: returnsExit). The agent's call at that step, the stack going down
// with the app, fails: one under the set's context ends with the set's end,
// one under the run's (the set's mock filter) fails as a gone agent does,
// once the exit is known.
type setupExitInstr struct {
	*composeInstr
	at    string
	died  chan models.AppError
	known chan struct{}
	tell  func()
	// returns: the app's run returns its exit (compose's up returning, as
	// at main) rather than telling it at its die (returnsExit).
	returns bool
	// gone: the agent went down with the app at "mock stats", and every
	// later call to it is refused.
	gone bool
}

// errAgentGone is a call to the agent of a stack that went down with its app.
var errAgentGone = errors.New("dial tcp 172.18.0.2:16789: connect: connection refused")

// setupExitSteps are the setup's steps an exit lands in: the wait for the
// agent of a stack the set brings up (its /health), the set's outgoing
// options, and its mock filter.
var setupExitSteps = []string{"agent health", "MockOutgoing", "UpdateMockParams"}

// moreSetupExitSteps are the setup's steps of a set's first run an exit
// lands in with no agent call of its staging in flight: the read of the
// set's mocks from its store, the wait for its app's readiness (its health
// URL), and the check that the agent holds the set's mocks (its mock stats).
var moreSetupExitSteps = []string{"load mocks", "app ready", "mock stats"}

// Run is the app of a stack a set brings up, which tells its exit at its
// container's die (models.WithAppExit) and returns only at the set's end, or
// returns its exit (returns).
func (s *setupExitInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case e := <-s.died:
		if s.returns {
			return e
		}
		models.AppExitOf(ctx)(e)
		close(s.known)
	case <-ctx.Done():
	}
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
}

// WatchAgentStart, at "agent start", is the start of the agent of a stack
// that goes down with its app: the app's exit is told, and then the agent's
// start fails.
func (s *setupExitInstr) WatchAgentStart(context.Context) <-chan error {
	if s.at != "agent start" {
		return nil
	}
	s.at = ""
	failed := make(chan error, 1)
	go func() {
		s.tell()
		<-s.known
		failed <- errors.New("the keploy-agent container exited with code 1 before it was ready")
	}()
	return failed
}

// returnsExit makes the app's run return its exit (returns), which keploy
// keeps only once the run's return is read: known is then the reader of r's
// app errors logging the exit (applicationFailedToRunLogMessage), which it
// reads only once it is kept, so a call failing after known fails with the
// exit known, as one to a stack compose's up had returned on does.
func (s *setupExitInstr) returnsExit(r *Replayer) {
	s.returns = true
	known := make(chan struct{})
	var once sync.Once
	r.logger = zap.New(exitLoggedCore{r.logger.Core(), func() { once.Do(func() { close(known) }) }})
	s.known = known
}

// exitLoggedCore calls logged at the log of an app's exit by the reader of
// a set's app errors.
type exitLoggedCore struct {
	zapcore.Core
	logged func()
}

func (c exitLoggedCore) Enabled(l zapcore.Level) bool {
	return l >= zapcore.ErrorLevel || c.Core.Enabled(l)
}

func (c exitLoggedCore) With(f []zapcore.Field) zapcore.Core {
	return exitLoggedCore{c.Core.With(f), c.logged}
}

func (c exitLoggedCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Message == applicationFailedToRunLogMessage {
		c.logged()
	}
	return c.Core.Check(e, ce)
}

// GetMockStats, at "mock stats", is the agent's answer of the mocks it holds
// once its stack went down with the app: the app's exit is told, and the
// call is refused, as every later one is.
func (s *setupExitInstr) GetMockStats(ctx context.Context) (models.MockStats, error) {
	if s.at == "mock stats" {
		s.at, s.gone = "", true
		s.tell()
		<-s.known
	}
	if s.gone {
		return models.MockStats{}, errAgentGone
	}
	return s.composeInstr.GetMockStats(ctx)
}

func (s *setupExitInstr) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	if s.gone {
		return errAgentGone
	}
	if s.at != "MockOutgoing" {
		return s.composeInstr.MockOutgoing(ctx, opts)
	}
	s.at = ""
	s.tell()
	<-ctx.Done()
	return ctx.Err()
}

func (s *setupExitInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	if s.at != "UpdateMockParams" {
		return s.composeInstr.UpdateMockParams(ctx, params)
	}
	s.at = ""
	s.tell()
	<-s.known
	return errAgentGone
}

// exitReadMockDB is a set's mock store whose first read of the set's mocks
// tells the app's exit and, once it is known, fails.
type exitReadMockDB struct {
	*prMockDB
	once sync.Once
	s    *setupExitInstr
}

func (m *exitReadMockDB) GetFilteredMocks(ctx context.Context, set string, after, before time.Time, mapped, needed map[string]bool) ([]*models.Mock, error) {
	m.once.Do(func() {
		m.s.tell()
		<-m.s.known
		m.failReads.Store(true)
	})
	return m.prMockDB.GetFilteredMocks(ctx, set, after, before, mapped, needed)
}

// composeExitInSetup is a compose set of three tests whose app exits with
// code 3 at the setup's step at, in the compose shape given
// (composeExitShapes): its harness, app and instrumentation.
func composeExitInSetup(t *testing.T, on, serving bool, at string) (*prRun, *heldRequests, *setupExitInstr) {
	t.Helper()
	provenOn(t, on)
	c := &composeInstr{}
	h := composeHarnessOf(t, c, 3)
	exit := models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}
	s := &setupExitInstr{composeInstr: c, at: at, died: make(chan models.AppError, 1), known: make(chan struct{})}
	s.tell = func() { s.died <- exit }
	if serving {
		ctx, kept := listenedTo(context.Background())
		h.replayer.keptExit = kept
		s.tell = func() { models.AppExitOf(ctx)(exit) }
		s.known = kept.told
		// The first set of a kept stack waits for its agent too.
		h.replayer.isFirstTestSet = true
	}
	switch at {
	case "load mocks":
		h.replayer.mockDB = &exitReadMockDB{prMockDB: h.mocks, s: s}
	case "app ready":
		// The wait for the app's readiness polls its health URL, which the
		// app, exiting, never answers; only the set's end ends the wait.
		var once sync.Once
		health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			once.Do(s.tell)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(health.Close)
		h.replayer.config.Test.HealthURL = health.URL + "/health"
		h.replayer.config.Test.HealthPollTimeout = 10 * prTimeout
	case "mock stats":
		// The agent holds what keploy stored for the set only where it
		// stored some: a set with none is not checked.
		h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
	}
	if at == "agent health" || at == "agent start" {
		var once sync.Once
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if at == "agent health" {
				once.Do(s.tell)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(agent.Close)
		h.replayer.config.Agent.AgentURI = agent.URL
	}
	h.replayer.instrumentation = s
	app := &heldRequests{requests: map[string]int{}}
	h.replayer.hookImpl = prHooks{onRequest: app.onRequest}
	return h, app, s
}

// runSet runs h's set named set and returns its status and error.
func runSet(h *prRun, set string, serving bool) (models.TestSetStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	h.cancel = cancel
	return h.replayer.RunTestSet(ctx, set, "test-run-0", serving)
}

// haltedBeforeTests fails t unless the set named set halted on the app's
// exit with no error, its report APP_HALTED with want in its reason, its 3
// tests counted and none of them sent.
func haltedBeforeTests(t *testing.T, h *prRun, app *heldRequests, set string, status models.TestSetStatus, err error, want string, sentBefore map[string]int) {
	t.Helper()
	if err != nil {
		t.Fatalf("status %s, error %v; want the set halted on the app's exit, with no error of its setup", status, err)
	}
	if status != models.TestSetStatusAppHalted {
		t.Fatalf("status %s, want APP_HALTED", status)
	}
	rep := h.report.report
	if rep == nil || rep.TestSet != set || rep.Status != string(models.TestSetStatusAppHalted) {
		t.Fatalf("report %+v; want %s APP_HALTED", rep, set)
	}
	if !strings.Contains(rep.FailureReason, want) || strings.Contains(rep.FailureReason, "context canceled") {
		t.Fatalf("reason %q; want %q", rep.FailureReason, want)
	}
	if rep.Total != 3 || rep.Success+rep.Failure != 0 {
		t.Fatalf("total %d, %d passed, %d failed; want its 3 tests, none run", rep.Total, rep.Success, rep.Failure)
	}
	for _, name := range []string{"test-1", "test-2", "test-3"} {
		if n := app.count(name) - sentBefore[name]; n != 0 {
			t.Fatalf("%s was sent %d times by %s; the app had exited before the set's first test", name, n, set)
		}
	}
	if h.replayer.saysResultsUnread(models.TestSetStatusAppHalted, set) {
		t.Fatal("the set's missing results would be said as an error; it said why it ran none")
	}
}

// An app's exit while keploy sets its set up (after the set began, before
// its first test) halts the set as an exit during its tests does: the set is
// APP_HALTED, its reason names the exit's code, its tests are counted and
// none of them is sent. The set's setup, whose calls to the agent end with
// the set's end, did not fail on its own: the repro's set returned "context
// canceled" from staging its mocks, and the run stopped on "failed to run
// test set: context canceled", the set unreported.
func TestAnAppsExitInASetsSetupHaltsTheSet(t *testing.T) {
	shapes := append(composeExitShapes[:len(composeExitShapes):len(composeExitShapes)], []struct {
		name        string
		on, serving bool
	}{
		{"a stack per set whose run returns the exit, switch off", false, false},
		{"a stack per set whose run returns the exit, switch on", true, false},
	}...)
	for _, tc := range shapes {
		for _, at := range append(setupExitSteps[:len(setupExitSteps):len(setupExitSteps)], moreSetupExitSteps...) {
			t.Run(tc.name+", at "+at, func(t *testing.T) {
				h, app, s := composeExitInSetup(t, tc.on, tc.serving, at)
				if strings.Contains(tc.name, "returns") {
					s.returnsExit(h.replayer)
				}
				status, err := runSet(h, "test-set-0", tc.serving)
				haltedBeforeTests(t, h, app, "test-set-0", status, err,
					"application startup failed: it exited with code 3 - check application logs", nil)
			})
		}
	}
}

// The app the run keeps for every set, exiting while keploy sets a later set
// up, had started: that set says it exited while keploy set it up, not that
// it failed to start.
func TestAKeptAppsExitInALaterSetsSetupSaysItHadStarted(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, at := range setupExitSteps[1:] {
			t.Run(fmt.Sprintf("switch on %v, at %s", on, at), func(t *testing.T) {
				h, app, s := composeExitInSetup(t, on, true, "")
				if status, err := runSet(h, "test-set-0", true); err != nil || status != models.TestSetStatusPassed {
					t.Fatalf("first set: status %s, error %v; want PASSED", status, err)
				}
				sent := map[string]int{"test-1": app.count("test-1"), "test-2": app.count("test-2"), "test-3": app.count("test-3")}
				h.replayer.isFirstTestSet = false
				s.at = at
				status, err := runSet(h, "test-set-1", true)
				haltedBeforeTests(t, h, app, "test-set-1", status, err,
					"application exited while keploy set this test set up, before its first test: it exited with code 3 - check application logs", sent)
			})
		}
	}
}

// A setup that fails while keploy knows of no exit of the app's own is not
// the app's halt: its failure stands, as at main. Neither is the user's
// stop (Ctrl+C), which compose's stop of the app follows, nor an error of
// the app's runner itself (models.ErrInternal).
func TestASetupFailureWithNoAppExitStands(t *testing.T) {
	for _, tc := range composeExitShapes {
		for _, why := range []string{"no exit told", "the user's stop", "the runner's own error"} {
			t.Run(tc.name+", "+why, func(t *testing.T) {
				at := "MockOutgoing"
				if why == "no exit told" {
					at = "UpdateMockParams"
				}
				h, _, s := composeExitInSetup(t, tc.on, tc.serving, at)
				tell := s.tell
				switch why {
				case "no exit told":
					known := make(chan struct{})
					close(known)
					s.known, s.tell = known, func() {}
				case "the user's stop":
					s.tell = func() { h.cancel(); tell() }
				case "the runner's own error":
					internal := models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("keploy could not watch the app's container")}
					if tc.serving {
						s.tell = func() { h.replayer.keptExit.tell(internal) }
					} else {
						s.tell = func() { s.died <- internal }
					}
				}
				status, err := runSet(h, "test-set-0", tc.serving)
				if err == nil {
					t.Fatalf("status %s, no error; want the setup's failure to stand", status)
				}
				if rep := h.report.report; rep != nil && rep.Status == string(models.TestSetStatusAppHalted) {
					t.Fatalf("report %s %q; the setup's failure is no exit of the app's own", rep.Status, rep.FailureReason)
				}
			})
		}
	}
}

// An agent's start that fails once the app's exit is told, before the reader
// of the app's errors ends the set (held here in its log of the exit until
// the set's report is kept), is the exit's: the stack went down with the
// app. The set halts on the exit, as at the agent's /health, not on "the
// keploy-agent docker compose started did not come up".
func TestAnAgentsStartFailingOnceTheAppsExitIsToldHaltsTheSet(t *testing.T) {
	for _, tc := range composeExitShapes {
		t.Run(tc.name, func(t *testing.T) {
			h, app, _ := composeExitInSetup(t, tc.on, tc.serving, "agent start")
			hold := holdExitLog(t, h.replayer)
			h.report.afterReport = hold.release
			status, err := runSet(h, "test-set-0", tc.serving)
			if hold.boundHit.Load() {
				t.Fatalf("the reader was let go by the row's bound (status %s, error %v): the set kept no report", status, err)
			}
			haltedBeforeTests(t, h, app, "test-set-0", status, err,
				"application startup failed: it exited with code 3 - check application logs", nil)
		})
	}
}
