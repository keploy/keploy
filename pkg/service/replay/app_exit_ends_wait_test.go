package replay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// marksHooks records whether each test request's context says the
// application's exit ends it (pkg.AppExitEnds).
type marksHooks struct {
	prHooks
	mu    sync.Mutex
	marks []bool
	rooms []bool
}

func (h *marksHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.mu.Lock()
	h.marks = append(h.marks, pkg.AppExitEnds(ctx))
	// The room for the started application's pid, which a wait names the
	// application's ports from, is on the request's context too.
	utils.NoteAppPID(ctx, 7)
	h.rooms = append(h.rooms, utils.AppPID(ctx) == 7)
	h.mu.Unlock()
	return h.prHooks.SimulateRequest(ctx, tc, set)
}

// The test requests of an application keploy started go out on a context its
// exit ends, so that a refused one waits for its address to accept; those of
// an application keploy did not start (no command) do not, so that a refused
// one keeps the bounded re-send: nothing would end its wait.
func TestTestRequestsWaitOnARefusalOnlyForAnAppKeployStarted(t *testing.T) {
	for _, started := range []bool{true, false} {
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.instrument = started
		hooks := &marksHooks{}
		h.replayer.hookImpl = hooks
		h.run(t)
		if len(hooks.marks) != 2 {
			t.Fatalf("started=%v: %d test requests sent, want 2", started, len(hooks.marks))
		}
		for i, marked := range hooks.marks {
			if marked != started {
				t.Errorf("started=%v: request %d's context says the app's exit ends it: %v", started, i+1, marked)
			}
		}
		for i, room := range hooks.rooms {
			if room != started {
				t.Errorf("started=%v: request %d's context has a room for the application's pid: %v", started, i+1, room)
			}
		}
	}
}

// An application keploy started that exits before it ever accepts a
// connection ends the set's wait for it, and the set fails as the exit does
// (the application halted), saying so: not as a stop by the user. A native
// command and a docker compose stack alike.
func TestAnAppThatExitsBeforeItAcceptsFailsItsSetSayingSo(t *testing.T) {
	for _, kind := range []utils.CmdType{utils.Native, utils.DockerCompose} {
		t.Run(string(kind), func(t *testing.T) {
			refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.CommandType = string(kind)
			if kind == utils.DockerCompose {
				// keploy's agent comes up in the stack before the wait.
				router := chi.NewRouter()
				routes.DefaultRoutes{}.New(router, &composeAgent{}, zap.NewNop())
				srv := httptest.NewServer(router)
				defer srv.Close()
				h.replayer.config.Agent.AgentURI = srv.URL + "/agent"
				h.replayer.instrumentation = &composeAgentInstr{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config)}
			}
			for _, tc := range h.cases {
				tc.HTTPReq.URL = "http://" + refusing + "/x"
			}
			// The application exits once the set waits for it to accept a
			// connection: past the agent's start, at the wait itself.
			var exit sync.Once
			core, _ := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
				if strings.Contains(e.Message, "waiting for the application to accept a connection on "+refusing) {
					exit.Do(func() { close(h.instr.appStopped) })
				}
				return nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
			if status != models.TestSetStatusAppHalted {
				t.Errorf("status %q, want %q: the application exited before it accepted a connection", status, models.TestSetStatusAppHalted)
			}
			if err == nil || !strings.Contains(err.Error(), "exited before it accepted a connection") {
				t.Errorf("error %v, want one saying the application exited before it accepted a connection", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Errorf("error %v reads as the user's stop", err)
			}
			if n := len(h.report.results); n != 0 {
				t.Errorf("%d tests were sent, want none", n)
			}
		})
	}
}

// A docker compose application that exits while keploy waits for its agent in
// the stack to answer healthy ends the set as its exit does (the application
// halted, or its command failed), saying so: not as the user's stop. The
// user's own stop in that wait stays the user's stop.
func TestAComposeAppThatExitsBeforeItsAgentIsHealthyFailsItsSetAsItsExit(t *testing.T) {
	for _, c := range []struct {
		name string
		// exit is how the application exits; "" when the user stops keploy
		// instead.
		exit   models.AppErrorType
		status models.TestSetStatus
	}{
		{"the app halts", models.ErrAppStopped, models.TestSetStatusAppHalted},
		{"the app's command fails", models.ErrCommandError, models.TestSetStatusFaultUserApp},
		{"the user stops keploy", "", models.TestSetStatusUserAbort},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.CommandType = string(utils.DockerCompose)
			h.instr.stopErr = c.exit
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			// keploy's agent never answers healthy. The application exits, or
			// the user stops keploy, at the agent's first health check: while
			// keploy waits for it.
			var once sync.Once
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				once.Do(func() {
					if c.exit == "" {
						cancel()
					} else {
						close(h.instr.appStopped)
					}
				})
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer agent.Close()
			h.replayer.config.Agent.AgentURI = agent.URL + "/agent"
			status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
			if status != c.status {
				t.Errorf("status %q, want %q", status, c.status)
			}
			if c.exit == "" {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("error %v, want the user's stop (context.Canceled)", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "the application exited before keploy's agent in its stack was ready") {
					t.Errorf("error %v, want one saying the application exited before keploy's agent was ready", err)
				}
				if errors.Is(err, context.Canceled) {
					t.Errorf("error %v reads as the user's stop", err)
				}
			}
			if n := len(h.report.results); n != 0 {
				t.Errorf("%d tests were sent, want none", n)
			}
		})
	}
}

// A wait for the application that ends with no exit of its own is the
// user's stop; one that ends with its exit fails the set as that exit does.
func TestNeverReadyStatusIsTheExitsOrTheUsersStop(t *testing.T) {
	for _, c := range []struct {
		err    models.AppError
		status models.TestSetStatus
		exited bool
	}{
		{models.AppError{}, models.TestSetStatusUserAbort, false},
		{models.AppError{AppErrorType: models.ErrCtxCanceled}, models.TestSetStatusUserAbort, false},
		{models.AppError{AppErrorType: models.ErrAppStopped, ExitCode: 0}, models.TestSetStatusAppHalted, true},
		{models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 1}, models.TestSetStatusFaultUserApp, true},
	} {
		if status, exited := neverReadyStatus(c.err); status != c.status || exited != c.exited {
			t.Errorf("%+v: (%q, %v), want (%q, %v)", c.err, status, exited, c.status, c.exited)
		}
	}
}

// notingInstr starts the application and notes its pid where keploy keeps it,
// as ExecuteCommand does for a native command.
type notingInstr struct{ *prInstr }

func (n *notingInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	utils.NoteAppPID(ctx, 7)
	return n.prInstr.Run(ctx, opts)
}

type pidHooks struct {
	prHooks
	mu   sync.Mutex
	pids []int
}

func (h *pidHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.mu.Lock()
	h.pids = append(h.pids, utils.AppPID(ctx))
	h.mu.Unlock()
	return h.prHooks.SimulateRequest(ctx, tc, set)
}

// An application kept alive across test sets is started once, by Start, not
// by a set: the pid it notes is the one each set's waits read, to name the
// ports it listens on instead of the one they wait on.
func TestTheKeptAppsPidIsTheOneEachSetsWaitsRead(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.instrumentation = &notingInstr{h.instr}
	hooks := &pidHooks{}
	h.replayer.hookImpl = hooks
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.KeepAppAlive = true
	h.replayer.config.Test.MaxFlakyChecks = 1
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if err := h.replayer.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.pids) != 2 {
		t.Fatalf("%d test requests sent, want 2", len(hooks.pids))
	}
	for i, pid := range hooks.pids {
		if pid != 7 {
			t.Errorf("request %d's context holds pid %d, want the kept application's 7", i+1, pid)
		}
	}
}
