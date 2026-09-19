package mock

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// agentLifetime says where the keploy agent lives relative to the wrapped
// command, which is the whole of what compose changes.
type agentLifetime int

const (
	// Native: the agent is keploy's own sibling process and is already
	// answering by the time Setup returns.
	agentUpFromSetup agentLifetime = iota
	// Compose: the agent is a service INSIDE the project keploy generates, so
	// nothing agent-shaped answers until the project has been started.
	agentUpAfterRun
	// Compose, broken project: the project dies and the agent never appears.
	agentNeverUp
)

// composeAgentBootProbes is how many /health probes the fake's agent takes to
// answer. More than one on purpose: an agent container under CI daemon
// contention has been observed taking two minutes to boot, which is what the
// wait's 330s budget and its retry loop exist for. With a single probe, an
// implementation that checks once and gives up passes every other assertion
// here.
const composeAgentBootProbes = 3

// composeInstr is that shape as an Instrumentation, plus a real HTTP server on
// /health so the reachability wait under test is the production one
// (pkg.AgentHealthTicker) rather than a stand-in.
//
// It records the order of everything keploy did to it, because both ways this
// path has broken are ordering bugs: waiting for the agent before starting the
// project (the agent never comes up), and posting /agent/ready before the
// proxy is armed (that post is what releases the app service from the agent's
// compose healthcheck, so the app runs uninstrumented).
type composeInstr struct {
	Instrumentation // embedded: nil, so any unexpected call panics loudly

	health    *httptest.Server
	lifetime  agentLifetime
	runBlocks bool
	runPanics bool
	runResult models.AppError

	// What the agent reports back when asked for the replay outcome. Under
	// compose it is routinely asked after it has already been stopped, so both
	// reads failing is the normal case there, not an exotic one.
	consumedMocks []models.MockState
	consumedErr   error
	mockErrors    []models.UnmatchedCall
	mockErrorsErr error

	mu         sync.Mutex
	events     []string
	runs       int
	healthHits int
}

func newInstr(t *testing.T, lifetime agentLifetime, runBlocks bool, runResult models.AppError) *composeInstr {
	t.Helper()
	f := &composeInstr{
		lifetime:  lifetime,
		runBlocks: runBlocks,
		runResult: runResult,
	}
	f.health = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		f.healthHits++
		f.mu.Unlock()
		if !f.agentUp() {
			// In reality nothing is listening at all and the probe gets a
			// connection refusal; both are "not healthy" to isAgentHealthy,
			// and a served 503 avoids racing a listener open and closed.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.health.Close)
	return f
}

func (f *composeInstr) agentUp() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.lifetime {
	case agentUpFromSetup:
		return true
	case agentNeverUp:
		return false
	default:
		// Compose: the project has to be running for the agent service to exist
		// at all, and then the container takes a while to boot.
		return f.runs > 0 && f.healthHits >= composeAgentBootProbes
	}
}

func (f *composeInstr) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *composeInstr) seq() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *composeInstr) counts() (runs, healthHits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs, f.healthHits
}

func (f *composeInstr) Setup(context.Context, string, models.SetupOptions) error { return nil }

func (f *composeInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	f.mu.Lock()
	f.runs++
	f.events = append(f.events, "run")
	f.mu.Unlock()
	if f.runPanics {
		// Panic only once the agent is up, so the receive under test is the one
		// in Record/Replay and not startComposeApp's own startup select. Bounded
		// and ctx-aware: if the agent never comes up, returning an exit is far
		// better than spinning until the test reports a deadlock that never
		// happened.
		for !f.agentUp() {
			select {
			case <-ctx.Done():
				return models.AppError{AppErrorType: models.ErrCtxCanceled}
			case <-time.After(10 * time.Millisecond):
			}
		}
		panic("the app runner blew up")
	}
	if f.runBlocks {
		<-ctx.Done() // a compose project runs until it is torn down
	}
	return f.runResult
}

func (f *composeInstr) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	if !f.agentUp() {
		return nil, errors.New("failed to start capturing outgoing calls: nothing is listening on the agent")
	}
	f.record("arm")
	out := make(chan *models.Mock)
	close(out)
	return out, nil
}

func (f *composeInstr) MockOutgoing(context.Context, models.OutgoingOptions) error {
	if !f.agentUp() {
		return errors.New("failed to enable mock serving: nothing is listening on the agent")
	}
	f.record("arm")
	return nil
}

func (f *composeInstr) MakeAgentReadyForDockerCompose(context.Context) error {
	if !f.agentUp() {
		return errors.New("nothing is listening on the agent")
	}
	f.record("ready")
	return nil
}

func (f *composeInstr) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	f.record("store")
	return nil
}

func (f *composeInstr) UpdateMockParams(context.Context, models.MockFilterParams) error {
	f.record("stage")
	return nil
}
func (f *composeInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return f.consumedMocks, f.consumedErr
}

func (f *composeInstr) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return f.mockErrors, f.mockErrorsErr
}
func (f *composeInstr) NotifyGracefulShutdown(context.Context) error { return nil }

// stubMockDB is an empty set that accepts everything: these tests are about the
// order of the agent calls, not about persistence.
type stubMockDB struct {
	MockDB // embedded: nil, so an unexpected call panics loudly
}

func (stubMockDB) InsertMock(context.Context, *models.Mock, string) error { return nil }
func (stubMockDB) DeleteMocksForSet(context.Context, string) error        { return nil }
func (stubMockDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (stubMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (stubMockDB) ResetCounterID()      {}
func (stubMockDB) SetCounterID(_ int64) {}

func instrConfig(instr *composeInstr, cmdType utils.CmdType, command string) *config.Config {
	cfg := &config.Config{}
	cfg.CommandType = string(cmdType)
	cfg.Command = command
	cfg.Mock.Name = "set"
	cfg.Agent.AgentURI = instr.health.URL
	return cfg
}

// shortAgentBudget is how long a regression takes to surface here: an ordering
// that waits for the agent before starting the project never sees it go
// healthy, so the run fails when this budget expires. The 330s default would
// make that a five-and-a-half-minute test.
//
// 20s, not less: a HEALTHY run spends composeAgentBootProbes ticks (~3s) in the
// same wait, and the margin is what keeps a loaded CI runner from turning a
// pass into a failure.
func shortAgentBudget(t *testing.T) {
	t.Helper()
	t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "20")
}

// waitForEvent blocks until the recorded sequence contains event, failing at
// once if the subcommand returns first — that return is what a broken ordering
// produces, and its error says why.
func waitForEvent(t *testing.T, instr *composeInstr, event string, done <-chan error, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		for _, e := range instr.seq() {
			if e == event {
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("returned before reaching %q (%v); it did %v", event, err, instr.seq())
		case <-deadline:
			t.Fatalf("never reached %q; it did %v", event, instr.seq())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The ordering contract under compose, in one assertion per subcommand:
//
//	run   — the project has to be up first, because the agent is in it
//	arm   — only then can the proxy be armed
//	ready — and only then may the app be released, because POST /agent/ready
//	        is what satisfies the agent service's compose healthcheck that the
//	        app's depends_on is waiting on
//
// Releasing before arming is silent: the app starts, makes its first
// dependency calls straight past an unarmed proxy, and the recording is short
// or the replay misses.
func TestComposeOrdering(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantSeq []string
		run     func(Service, context.Context) error
	}{
		{
			name:    "record",
			wantSeq: []string{"run", "arm", "ready"},
			run:     func(s Service, ctx context.Context) error { return s.Record(ctx) },
		},
		{
			// Replay has two more steps between arming and releasing, and both
			// have to land first: the app is released into a proxy that is in
			// mock-serving mode but has no mocks in it otherwise.
			name:    "replay",
			wantSeq: []string{"run", "arm", "store", "stage", "ready"},
			run:     func(s Service, ctx context.Context) error { return s.Replay(ctx) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortAgentBudget(t)
			instr := newInstr(t, agentUpAfterRun, true, models.AppError{})
			svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, instrConfig(instr, utils.DockerCompose, "docker compose up"))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.run(svc, ctx) }()

			waitForEvent(t, instr, "ready", done, 60*time.Second)
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			if got := instr.seq(); !reflect.DeepEqual(got, tc.wantSeq) {
				t.Fatalf("agent calls went %v, want %v", got, tc.wantSeq)
			}
			runs, healthHits := instr.counts()
			if runs != 1 {
				t.Fatalf("started the compose project %d times, want exactly 1", runs)
			}
			// Without this, a fixed sleep in place of the reachability wait —
			// or a single probe with no retry loop — passes every assertion
			// above and fails in production the moment the agent container
			// takes longer than that to boot.
			if healthHits < composeAgentBootProbes {
				t.Fatalf("polled the agent health endpoint %d time(s), want at least %d: the wait for the agent is not a retrying reachability wait",
					healthHits, composeAgentBootProbes)
			}
		})
	}
}

// Every other command type is untouched: Setup leaves a reachable agent behind,
// the proxy is armed before the app runs, and no healthcheck gates the app on
// /agent/ready. Posting it would write a file only the generated compose
// healthcheck ever reads; probing /health would add a tick to every run.
//
// docker-run is here because replay used to post /agent/ready for EVERY command
// type and now does so only for compose — a behaviour change beyond the
// ordering fix, and the one an agent-in-a-container mode would notice first.
func TestNonComposeOrderingIsUntouched(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdType utils.CmdType
		command string
		run     func(Service, context.Context) error
	}{
		{"native/record", utils.Native, "pytest -q", func(s Service, ctx context.Context) error { return s.Record(ctx) }},
		{"native/replay", utils.Native, "pytest -q", func(s Service, ctx context.Context) error { return s.Replay(ctx) }},
		{"docker-run/record", utils.DockerRun, "docker run --name app img", func(s Service, ctx context.Context) error { return s.Record(ctx) }},
		{"docker-run/replay", utils.DockerRun, "docker run --name app img", func(s Service, ctx context.Context) error { return s.Replay(ctx) }},
		{"docker-start/record", utils.DockerStart, "docker start -a app", func(s Service, ctx context.Context) error { return s.Record(ctx) }},
		{"docker-start/replay", utils.DockerStart, "docker start -a app", func(s Service, ctx context.Context) error { return s.Replay(ctx) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, instrConfig(instr, tc.cmdType, tc.command))

			if err := tc.run(svc, context.Background()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			// No "ready" in either expectation: only compose has a healthcheck
			// gating the app on it.
			want := []string{"arm", "run"}
			if strings.Contains(tc.name, "replay") {
				want = []string{"arm", "store", "stage", "run"}
			}
			if got := instr.seq(); !reflect.DeepEqual(got, want) {
				t.Fatalf("agent calls went %v, want %v", got, want)
			}
			runs, healthHits := instr.counts()
			if runs != 1 {
				t.Fatalf("ran the command %d times, want exactly 1", runs)
			}
			if healthHits != 0 {
				t.Fatalf("polled the agent health endpoint %d times on the %s path, want 0", healthHits, tc.cmdType)
			}
		})
	}
}

// A compose project that dies on its own — a bad compose file, a failed build,
// a port already taken — must be reported with what it did, not by sitting out
// the whole agent budget waiting for an agent that is never going to start.
func TestComposeProjectThatDiesFailsFast(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "120")
	instr := newInstr(t, agentNeverUp, false, models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 1})
	svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, instrConfig(instr, utils.DockerCompose, "docker compose up"))

	start := time.Now()
	err := svc.Record(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Record succeeded against a compose project that never came up")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("took %s to report a dead compose project; it waited out the agent budget instead of noticing the exit", elapsed)
	}
}

// A compose project that dies during agent bring-up must still mirror its exit
// code. `keploy mock` propagates the wrapped runner's code, and this path --
// the project crashing before the agent answered -- reported a generic failure
// instead, so the same crash exited 7 or 1 depending on which of two racing
// paths noticed it first.
func TestComposeProjectThatDiesMirrorsItsExitCode(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "120")
	instr := newInstr(t, agentNeverUp, false, models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 7})
	svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, instrConfig(instr, utils.DockerCompose, "docker compose up"))

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	if err := svc.Record(context.Background()); err == nil {
		t.Fatal("Record succeeded against a compose project that died")
	}
	if utils.ErrCode != 7 {
		t.Fatalf("exit code %d, want the runner's 7", utils.ErrCode)
	}
}

// Under compose the app's exit reaches Record over a channel rather than as
// Run's return value, so a panic inside Run has to still deliver one. It does
// not on its own: utils.Recover swallows the panic instead of re-panicking, so
// the goroutine ends having sent nothing. The receive would then block forever
// — and because it sits ahead of the deferred teardown, not even Ctrl+C could
// break out. The native branch has no such hazard: Run is called inline there,
// and a panic simply propagates.
func TestComposePanicInRunStillDeliversAnExit(t *testing.T) {
	shortAgentBudget(t)
	instr := newInstr(t, agentUpAfterRun, false, models.AppError{})
	instr.runPanics = true
	svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, instrConfig(instr, utils.DockerCompose, "docker compose up"))

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	done := make(chan error, 1)
	go func() { done <- svc.Record(context.Background()) }()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Record never returned after Run panicked: the app exit was never delivered and the receive is blocked for good")
	}

	if utils.ErrCode == 0 {
		t.Error("a panicking app runner exited keploy cleanly; it must not report success")
	}
}
