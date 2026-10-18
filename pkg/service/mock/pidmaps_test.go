package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// pidMapCounter is an agent's GET /agent/pidmaps: the first read answers
// before, every later one after, or err for every read, or afterErr for every
// read but the first.
type pidMapCounter struct {
	before, after models.PIDMapRefusals
	err, afterErr error

	mu    sync.Mutex
	reads int
}

func (c *pidMapCounter) GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	// As the agent's HTTP client does: a read on a context that has ended
	// fails.
	if err := ctx.Err(); err != nil {
		return models.PIDMapRefusals{}, err
	}
	if c.err != nil {
		return models.PIDMapRefusals{}, c.err
	}
	if c.reads == 1 {
		return c.before, nil
	}
	if c.afterErr != nil {
		return models.PIDMapRefusals{}, c.afterErr
	}
	return c.after, nil
}

// pidMapReplayInstr is the replay fake whose agent counts refused inserts.
type pidMapReplayInstr struct {
	*composeInstr
	*pidMapCounter
}

func logged(logs *observer.ObservedLogs) string {
	var printed []string
	for _, e := range logs.All() {
		printed = append(printed, e.Level.String()+" "+e.Message+" "+fmt.Sprint(e.ContextMap()))
	}
	return strings.Join(printed, "\n")
}

// A replay during which the agent's PID map refused a process -- whose calls
// therefore reached the real dependency, neither mocked nor counted as missed
// -- fails as capture incomplete, however green everything else is: exit 1
// (or the runner's own code), the error returned as keploy's, and a receipt
// that says capture_incomplete and is not isolated. Refusals before the test
// command ran are not the run's, and an agent that cannot count leaves the run
// as it was.
func TestReplayDuringWhichAProcessWasNotTrackedFails(t *testing.T) {
	refused3 := models.PIDMapRefusals{Target: 3, LastTargetTGID: 4242}
	for _, tc := range []struct {
		name         string
		counter      *pidMapCounter
		runner       models.AppError
		wantExit     int
		wantRunner   int
		wantFailedBy string
		wantIsolated bool
		wantLog      string
		unwantLog    string
		wantNote     string // the receipt's isolationNote; "capture incomplete" when failed and unset
	}{
		{
			name:         "refused during the run: capture incomplete",
			counter:      &pidMapCounter{after: refused3},
			runner:       models.AppError{AppErrorType: models.ErrAppStopped},
			wantExit:     1,
			wantFailedBy: FailedByCaptureIncomplete,
			wantLog:      "replay failed: the agent could not track every process the test command ran",
		},
		{
			name:         "only excluded_pids refused: still incomplete",
			counter:      &pidMapCounter{after: models.PIDMapRefusals{Excluded: 1}},
			runner:       models.AppError{AppErrorType: models.ErrAppStopped},
			wantExit:     1,
			wantFailedBy: FailedByCaptureIncomplete,
			wantLog:      "excluded_pids",
		},
		{
			name:         "a failing runner keeps its exit code; the receipt names the incomplete capture",
			counter:      &pidMapCounter{after: refused3},
			runner:       models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 7},
			wantExit:     7,
			wantRunner:   7,
			wantFailedBy: FailedByCaptureIncomplete,
			wantLog:      "replay failed: the agent could not track every process",
		},
		{
			// Keploy's own failure is the one thing the incomplete capture does
			// not take failedBy ahead of: keploy did not complete the run, and
			// that is what the receipt and the returned error say. The refusal
			// is still said.
			name:         "keploy's own failure keeps failedBy keploy; the refusal is still said",
			counter:      &pidMapCounter{after: refused3},
			runner:       models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the agent died under the test command")},
			wantExit:     1,
			wantRunner:   -1,
			wantFailedBy: FailedByKeploy,
			wantLog:      "replay failed: the agent could not track every process",
			wantNote:     "keploy did not complete the run",
		},
		{
			name:         "refused only before the test command ran: not this run's",
			counter:      &pidMapCounter{before: refused3, after: refused3},
			runner:       models.AppError{AppErrorType: models.ErrAppStopped},
			wantIsolated: true,
			unwantLog:    "could not track",
		},
		{
			name:         "nothing refused: a clean pass",
			counter:      &pidMapCounter{},
			runner:       models.AppError{AppErrorType: models.ErrAppStopped},
			wantIsolated: true,
			unwantLog:    "could not track",
		},
		{
			name:         "an agent that cannot count: unknown, not failed",
			counter:      &pidMapCounter{err: fmt.Errorf("%w: agent returned 404", models.ErrPIDMapRefusalsUnsupported)},
			runner:       models.AppError{AppErrorType: models.ErrAppStopped},
			wantIsolated: true,
			unwantLog:    "could not read whether",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpFromSetup, false, tc.runner)
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			err := New(zap.New(core), pidMapReplayInstr{instr, tc.counter}, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background())
			if failed := tc.wantFailedBy != ""; (err != nil) != failed {
				t.Fatalf("Replay returned %v; want an error: %v", err, failed)
			}
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, tc.wantExit)
			}
			if tc.counter.reads < 2 && tc.counter.err == nil {
				t.Errorf("the agent's refusals were read %d times, want before and after the test command", tc.counter.reads)
			}
			out := logged(logs)
			if tc.wantLog != "" && !strings.Contains(out, tc.wantLog) {
				t.Errorf("no log mentioning %q; got:\n%s", tc.wantLog, out)
			}
			if tc.unwantLog != "" && strings.Contains(out, tc.unwantLog) {
				t.Errorf("logged %q, which is not true of this run; got:\n%s", tc.unwantLog, out)
			}
			r, rerr := ReadReceipt(cfg.Path, "set")
			if rerr != nil || r == nil {
				t.Fatalf("no receipt: %v", rerr)
			}
			if r.FailedBy != tc.wantFailedBy || r.Isolated != tc.wantIsolated || r.ExitCode != tc.wantExit || r.RunnerExitCode != tc.wantRunner {
				t.Errorf("receipt failedBy %q, isolated %v (%s), exit %d, runner exit %d; want %q, %v, %d, %d",
					r.FailedBy, r.Isolated, r.IsolationNote, r.ExitCode, r.RunnerExitCode, tc.wantFailedBy, tc.wantIsolated, tc.wantExit, tc.wantRunner)
			}
			if note := tc.wantNote; tc.wantFailedBy != "" {
				if note == "" {
					note = "capture incomplete"
				}
				if !strings.Contains(r.IsolationNote, note) {
					t.Errorf("receipt isolationNote %q does not say %q", r.IsolationNote, note)
				}
			}
		})
	}
}

// A count the agent keeps but that could not be read -- before the test
// command ran or after -- proves nothing either way: the run is "unverified".
// --strict fails it, as keploy's own failure (exit 1, the error returned,
// failedBy strict), as it fails a run whose miss list is unreadable. Without
// --strict it passes, but says so at WARN, and the receipt does not certify it
// as isolated. An agent that cannot count at all is no lost count: its runs
// pass, isolated, as before.
func TestReplayWhoseUntrackedCountIsLostIsUnverified(t *testing.T) {
	lost := errors.New("agent returned 500: read pid_map_insert_failures[target_namespace_pids]: bad file descriptor")
	unsupported := fmt.Errorf("%w: agent returned 501", models.ErrPIDMapRefusalsUnsupported)
	for _, tc := range []struct {
		name         string
		counter      *pidMapCounter
		strict       bool
		wantExit     int
		wantFailedBy string
		wantIsolated bool
		wantLog      string
	}{
		{name: "lost after the run, --strict: failed", counter: &pidMapCounter{afterErr: lost}, strict: true, wantExit: 1, wantFailedBy: FailedByStrict, wantLog: "replay failed under --strict: keploy could not read whether the agent tracked every process"},
		{name: "lost before the run, --strict: failed", counter: &pidMapCounter{err: lost}, strict: true, wantExit: 1, wantFailedBy: FailedByStrict, wantLog: "replay failed under --strict: keploy could not read whether the agent tracked every process"},
		{name: "lost after the run: passes, said, not isolated", counter: &pidMapCounter{afterErr: lost}, wantLog: "this replay is not verified"},
		{name: "lost before the run: passes, said, not isolated", counter: &pidMapCounter{err: lost}, wantLog: "this replay is not verified"},
		// Controls: nothing was lost.
		{name: "an agent that cannot count, --strict: passes", counter: &pidMapCounter{err: unsupported}, strict: true, wantIsolated: true},
		{name: "counted, nothing refused, --strict: passes", counter: &pidMapCounter{}, strict: true, wantIsolated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Mock.Strict = tc.strict
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			err := New(zap.New(core), pidMapReplayInstr{instr, tc.counter}, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background())
			if failed := tc.wantFailedBy != ""; (err != nil) != failed {
				t.Fatalf("Replay returned %v; want an error: %v", err, failed)
			}
			if err != nil && !strings.Contains(err.Error(), "capture unverified") {
				t.Errorf("Replay returned %q, which does not say the capture is unverified", err)
			}
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, tc.wantExit)
			}
			out := logged(logs)
			if tc.wantLog != "" && !strings.Contains(out, tc.wantLog) {
				t.Errorf("no log mentioning %q; got:\n%s", tc.wantLog, out)
			}
			if tc.wantLog == "" && (strings.Contains(out, "not verified") || strings.Contains(out, "could not read whether")) {
				t.Errorf("said the run is unverified, which nothing lost; got:\n%s", out)
			}
			if strings.Contains(out, "capture incomplete") {
				t.Errorf("reported a refusal nobody read; got:\n%s", out)
			}
			r, rerr := ReadReceipt(cfg.Path, "set")
			if rerr != nil || r == nil {
				t.Fatalf("no receipt: %v", rerr)
			}
			if r.FailedBy != tc.wantFailedBy || r.Isolated != tc.wantIsolated || r.ExitCode != tc.wantExit {
				t.Errorf("receipt failedBy %q, isolated %v (%s), exit %d; want %q, %v, %d",
					r.FailedBy, r.Isolated, r.IsolationNote, r.ExitCode, tc.wantFailedBy, tc.wantIsolated, tc.wantExit)
			}
			if !tc.wantIsolated && !strings.HasPrefix(r.IsolationNote, "capture unverified") {
				t.Errorf("receipt isolationNote %q does not say the capture is unverified", r.IsolationNote)
			}
		})
	}
}

// A failing runner's own exit code and failedBy survive a lost count under
// --strict: the lost count is the weakest evidence, so it must not relabel a
// run the suite already failed (B1 reorder; mutant MK8). Here the test command
// fails with exit 7 while the agent's count is lost; the receipt must still
// say FailedByRunner and exit 7, and keploy must not return the run as its own
// failure -- while still SAYING the capture was not verified.
func TestReplayUnderStrictWithAFailingRunnerAndALostCountKeepsFailedByRunner(t *testing.T) {
	lost := errors.New("agent returned 500: bad file descriptor")
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 7})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	cfg := instrConfig(instr, utils.Native, "pytest -q")
	cfg.Mock.OnMiss = string(models.MissFail)
	cfg.Mock.Strict = true
	cfg.Path = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	err := New(zap.New(core), pidMapReplayInstr{instr, &pidMapCounter{afterErr: lost}}, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background())
	if err != nil {
		t.Fatalf("Replay returned %v; a failing runner is the suite's failure, not keploy's, so no error is returned", err)
	}
	if utils.ErrCode != 7 {
		t.Errorf("exit code %d, want the runner's 7", utils.ErrCode)
	}
	r, rerr := ReadReceipt(cfg.Path, "set")
	if rerr != nil || r == nil {
		t.Fatalf("no receipt: %v", rerr)
	}
	if r.FailedBy != FailedByRunner || r.ExitCode != 7 || r.RunnerExitCode != 7 {
		t.Errorf("receipt failedBy %q exit %d runner %d, want %q, 7, 7 (the lost count must not relabel the runner's failure)",
			r.FailedBy, r.ExitCode, r.RunnerExitCode, FailedByRunner)
	}
	// The lost count is still said, even though it did not decide the verdict.
	if !strings.Contains(logged(logs), "this replay is not verified") {
		t.Errorf("the lost count was not reported; want a \"not verified\" line")
	}
}

// Under --strict, a run whose agent REPORTED a missed call fails as the suite's
// contract (exit 1, no returned error, the misses named). A lost PID-map count
// -- no evidence of anything -- must not take that name away: it is the
// weakest evidence and is decided last (the reviewer's B1 case).
func TestStrictReportedMissKeepsItsNameOverALostCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		counter *pidMapCounter
	}{
		{"control: count read, nothing refused", &pidMapCounter{}},
		{"count lost after the run", &pidMapCounter{afterErr: errors.New("agent returned 500: bad file descriptor")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			instr.mockErrors = []models.UnmatchedCall{{Protocol: "http", Destination: "dep:80"}}
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Mock.Strict = true
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			err := New(zap.New(core), pidMapReplayInstr{instr, tc.counter}, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background())
			if err != nil {
				t.Errorf("a suite's reported miss was returned as keploy's own failure: %v", err)
			}
			if utils.ErrCode != 1 {
				t.Errorf("exit code %d, want 1", utils.ErrCode)
			}
			if !strings.Contains(logged(logs), "recorded dependency calls were missed") {
				t.Errorf("the reported miss was never named at ERROR")
			}
			if r, rerr := ReadReceipt(cfg.Path, "set"); rerr != nil || r == nil || r.FailedBy != FailedByStrict {
				t.Errorf("receipt %+v (%v): want failedBy %q, the miss's", r, rerr, FailedByStrict)
			}
		})
	}
}

// Under compose the agent has stopped with the project by the time the run
// ends, so the run's refusals come from the account it left on stopping.
func TestComposeReplayReadsRefusalsFromWhatTheStoppedAgentLeft(t *testing.T) {
	for _, tc := range []struct {
		name     string
		left     *models.PIDMapRefusals
		lostErr  string
		liveErr  error // the live (pre-run) read's error, for an agent that never counts
		strict   bool
		wantExit int
	}{
		{name: "it left refusals: capture incomplete", left: &models.PIDMapRefusals{Target: 2}, wantExit: 1},
		{name: "it left none", left: &models.PIDMapRefusals{}, wantExit: 0},
		// The live agent counted before the run, but its stop account says
		// nothing: a count that was lost (the agent restarted, perhaps to an
		// older image), not "never counts" -- unverified, which only --strict
		// fails.
		{name: "it counted live, but its stop account says nothing: unverified, passes", left: nil, wantExit: 0},
		{name: "it counted live, but its stop account says nothing, --strict: failed", left: nil, strict: true, wantExit: 1},
		// An agent too old to count at all answers the live read 404/501, so
		// no base is ever known: unknown, never failed, --strict included.
		{name: "an agent too old to count at all, --strict: unknown, not failed", left: nil, liveErr: fmt.Errorf("%w: agent returned 404", models.ErrPIDMapRefusalsUnsupported), strict: true, wantExit: 0},
		// The stopping agent could not read its count: unverified, which only
		// --strict fails.
		{name: "it left why its count was lost, --strict: failed", lostErr: "bad file descriptor", strict: true, wantExit: 1},
		{name: "it left why its count was lost: passes", lostErr: "bad file descriptor", wantExit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.runUntilReady = true
			instr.consumedErr = errors.New("connection refused")
			instr.mockErrorsErr = errors.New("connection refused")
			instr.leftOutcome = models.MockOutcome{Consumed: []models.MockState{{Name: "mock-0"}}, PIDMapRefusals: tc.left, PIDMapRefusalsError: tc.lostErr}
			// Live, the agent answers only the read before the app runs.
			counter := &pidMapCounter{err: tc.liveErr}
			cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Mock.Strict = tc.strict
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			_ = New(zap.New(core), pidMapReplayInstr{instr, counter}, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background())
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d; logs:\n%s", utils.ErrCode, tc.wantExit, logged(logs))
			}
			if counter.reads != 1 {
				t.Errorf("the live agent was asked %d times, want once, before the app ran: after, it has stopped", counter.reads)
			}
			if r, rerr := ReadReceipt(cfg.Path, "set"); tc.lostErr != "" && (rerr != nil || r == nil || r.Isolated || !strings.Contains(r.IsolationNote, "bad file descriptor")) {
				t.Errorf("a lost count left receipt %+v (%v); want it not isolated, saying why", r, rerr)
			}
		})
	}
}

// pidMapRecordInstr is drainInstrumentation (it captures mock-0 and mock-1)
// whose agent counts refused inserts, and answers only while the recording's
// stream is open: an agent that is being stopped too holds its HTTP server
// only that long (cli/agent.go, WaitForRecordingClients).
type pidMapRecordInstr struct {
	*drainInstrumentation
	*pidMapCounter
}

func (p pidMapRecordInstr) GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error) {
	if c := p.drainInstrumentation.captureCtx; c != nil && c.Err() != nil {
		return models.PIDMapRefusals{}, errors.New("the agent stopped with the recording's stream")
	}
	return p.pidMapCounter.GetPIDMapRefusals(ctx)
}

// A recording during which the agent's PID map refused a process is missing
// that process's calls, with nothing to say where. It is not kept -- an
// existing set is left as it was -- and the run fails; a recording during
// which nothing was refused is kept as before.
func TestRecordDuringWhichAProcessWasNotTrackedKeepsNothing(t *testing.T) {
	const set = "orders"
	refused := models.PIDMapRefusals{Target: 1, LastTargetTGID: 99}
	for _, tc := range []struct {
		name     string
		counter  *pidMapCounter
		wantKept bool
		wantWarn string
		// stop: the user stops the recording while the test command runs;
		// runErr: how the test command ends; wantExit: the run's exit code
		// when nothing is kept (keploy's own by default).
		stop     bool
		runErr   models.AppError
		wantExit int
	}{
		{name: "refused during the run", counter: &pidMapCounter{after: refused}},
		// However the run ended: a stopped recording is short of nothing
		// the user chose, it is short of what the agent never saw.
		{name: "refused during a run the user stopped", counter: &pidMapCounter{after: refused}, stop: true},
		// The runner's own failure is the run's exit code; the recording is
		// still not kept.
		{name: "refused during a run whose test command failed", counter: &pidMapCounter{after: refused},
			runErr: models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 7}, wantExit: 7},
		{name: "refused only before the run", counter: &pidMapCounter{before: models.PIDMapRefusals{Target: 5}, after: models.PIDMapRefusals{Target: 5}}, wantKept: true},
		{name: "an agent that cannot count", counter: &pidMapCounter{err: models.ErrPIDMapRefusalsUnsupported}, wantKept: true},
		// A lost count is no evidence against the recording, which is kept;
		// it is not vouched for in silence.
		{name: "a count lost after the run", counter: &pidMapCounter{afterErr: errors.New("agent returned 500")}, wantKept: true, wantWarn: "the recording is kept, but keploy could not verify it is complete"},
		{name: "a count lost before the run", counter: &pidMapCounter{err: errors.New("agent returned 500")}, wantKept: true, wantWarn: "the recording is kept, but keploy could not verify it is complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			db := newStatefulMockDB()
			db.sets[set] = []string{"old-0"}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			drain := &drainInstrumentation{out: make(chan *models.Mock, 4), emitAfter: 5 * time.Millisecond, runErr: tc.runErr}
			if tc.stop {
				drain.onRun = stop
			}
			inst := pidMapRecordInstr{drain, tc.counter}
			cfg := &config.Config{}
			cfg.Mock.Name = set
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			err := New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(ctx)
			got := db.get(set)
			if tc.wantKept {
				if err != nil || utils.ErrCode != 0 {
					t.Fatalf("Record returned %v, exit %d; want a kept recording and exit 0", err, utils.ErrCode)
				}
				if len(got) != 2 {
					t.Fatalf("set %q holds %v, want the 2 recorded mocks", set, got)
				}
				warned := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("could not verify it is complete").Len()
				if want := map[bool]int{true: 1, false: 0}[tc.wantWarn != ""]; warned != want {
					t.Fatalf("%d WARN line(s) saying the recording is unverified, want %d; got:\n%s", warned, want, logged(logs))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "the recording is incomplete") {
				t.Fatalf("Record returned %v, want the recording is incomplete", err)
			}
			wantExit := tc.wantExit
			if wantExit == 0 {
				wantExit = utils.ExitKeployError
			}
			if utils.ErrCode != wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, wantExit)
			}
			if len(got) != 1 || got[0] != "old-0" {
				t.Fatalf("set %q holds %v after an incomplete recording, want the old recording untouched", set, got)
			}
			if staged := db.get(stagingSetID(set)); len(staged) != 0 {
				t.Fatalf("the incomplete recording was left staged: %v", staged)
			}
			if out := logged(logs); !strings.Contains(out, "the recording was not kept: the agent could not track every process") {
				t.Errorf("no ERROR saying why the recording was not kept; got:\n%s", out)
			}
		})
	}
}

// hangingPIDMapReader is an agent that keeps the connection and never
// answers, until its context ends.
type hangingPIDMapReader struct{ started chan struct{} }

func (h hangingPIDMapReader) GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error) {
	close(h.started)
	<-ctx.Done()
	return models.PIDMapRefusals{}, ctx.Err()
}

// Under the proven switch the read has no deadline, even on a context no
// interrupt cancels (a recording's persistCtx). The user ends it as a drain
// is ended: once the run was interrupted, its notice says another interrupt
// stops it now, and the next one does; the count is then unread. A copy of
// the stop's signal that comes before that offer (sudo hands its own on) ends
// nothing.
func TestASwitchedCountReadEndsAtTheUsersNextInterrupt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted() // the stop
	var mu sync.Mutex
	at := time.Now()
	defer utils.SetNoticeClockForTest(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })()

	core, logs := observer.New(zapcore.InfoLevel)
	r := hangingPIDMapReader{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := readPIDMapRefusals(context.WithoutCancel(context.Background()), zap.New(core), r)
		done <- err
	}()
	<-r.started
	waitFor := func(snippet string) {
		t.Helper()
		for logs.FilterMessageSnippet(snippet).Len() == 0 {
			select {
			case err := <-done:
				t.Fatalf("the read ended (%v) before it said %q: under the switch it has no deadline", err, snippet)
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitFor("waiting for keploy's agent")
	utils.MarkInterruptedAgain() // a copy of the stop, before any offer
	select {
	case err := <-done:
		t.Fatalf("the read ended (%v) at an interrupt that came before it offered to stop", err)
	case <-time.After(5 * mockDrainNotice):
	}
	mu.Lock()
	at = at.Add(time.Hour) // a whole notice period after the stop: it offers
	mu.Unlock()
	waitFor("press Ctrl+C again to stop now")
	utils.MarkInterruptedAgain() // the user's answer
	if err := <-done; err == nil || !strings.Contains(err.Error(), "given up at the user's interrupt") {
		t.Fatalf("read error %v, want one naming the user's interrupt", err)
	}
}

// composeInstrWithOutcome is a compose agent that counts, and leaves on
// stopping the account a replay reads.
type composeInstrWithOutcome struct {
	*pidMapCounter
	outcomeReads int
}

func (c *composeInstrWithOutcome) ComposeAgentOutcome() (models.MockOutcome, error) {
	c.outcomeReads++
	return models.MockOutcome{}, nil
}

// Under compose the agent stops with the project and leaves no account of a
// recording: a recording is not checked, and says so only at Debug, while a
// replay reads the account the agent left.
func TestAComposeRecordingIsNotCheckedForUntrackedProcesses(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	reader := &composeInstrWithOutcome{pidMapCounter: &pidMapCounter{}}
	m := &mockService{logger: zap.New(core), instrumentation: struct {
		Instrumentation
		*composeInstrWithOutcome
	}{nil, reader}, config: &config.Config{CommandType: string(utils.DockerCompose)}}
	// Not read before the run either: a read that failed would WARN about a
	// count nothing checks.
	reader.pidMapCounter.err = errors.New("agent returned 500")
	if b := m.pidMapBaseline(context.Background(), "record"); b.known || b.unreadable != nil || reader.reads != 0 {
		t.Fatalf("a compose recording's baseline is %+v after %d read(s), want none read", b, reader.reads)
	}
	reader.pidMapCounter.err = nil
	base := pidMapBase{known: true}

	if check := m.pidMapRefusedDuring(context.Background(), base, "record"); check.known || check.unreadable != nil {
		t.Fatalf("a compose recording's check is %+v, want unchecked (neither known nor unreadable)", check)
	}
	if reader.outcomeReads != 0 {
		t.Fatalf("a compose recording read the stopped agent's account %d time(s): the agent leaves none for a recording", reader.outcomeReads)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARN line(s) for a compose recording, which cannot be checked: %v", n, logs.All())
	}

	// A replay reads it: this one left no count, from hooks that do not
	// count, after a count was read at the start, so the count was lost.
	check := m.pidMapRefusedDuring(context.Background(), base, "replay")
	if reader.outcomeReads != 1 || check.unreadable == nil {
		t.Fatalf("a compose replay read the account %d time(s), check %+v; want one read, and a lost count", reader.outcomeReads, check)
	}
}
