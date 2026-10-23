package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A replay whose agent relayed a Redis connection to the live server (the
// open-source keploy does not mock Redis) did not answer every dependency
// from the recording, though nothing missed and the suite passed: it is not
// isolated, its receipt names what was relayed and why, and the terminal says
// so instead of "every dependency answered from the recording".
func TestAReplayWhoseAgentRelayedRedisIsNotIsolated(t *testing.T) {
	redis := models.RelayedLive{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 4}
	keployDir, code, logs := replayIn(t, replayCase{
		covReport:     profile75,
		relayedBefore: models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 1}},
		relayedAfter:  models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 5}},
	})
	if code != 0 {
		t.Fatalf("exit %d, want 0: a relayed call fails no gate, it only takes the isolation claim away", code)
	}
	r := mustReceipt(t, keployDir)
	if r.Isolated {
		t.Fatalf("a run whose Redis answered live is not isolated: %+v", r)
	}
	if want := "Redis relayed to the live server at 10.0.0.5:6379 (4 connection(s)): open-source keploy does not mock Redis"; r.IsolationNote != want {
		t.Fatalf("isolation note %q, want %q", r.IsolationNote, want)
	}
	if !reflect.DeepEqual(r.Relayed, models.RelayedLives{redis}) {
		t.Fatalf("receipt relayed %+v, want the run's own count %+v (the later read less the earlier)", r.Relayed, redis)
	}
	out := logged(logs)
	if strings.Contains(out, "every dependency answered from the recording") {
		t.Fatalf("the terminal claims isolation for a run whose Redis answered live:\n%s", out)
	}
	if !strings.Contains(out, "this replay did not prove the tests run with the dependencies off") || !strings.Contains(out, "open-source keploy does not mock Redis") {
		t.Fatalf("the terminal does not say why the run is not isolated:\n%s", out)
	}
	if !strings.Contains(out, "use Keploy Enterprise: "+models.RedisEnterpriseURL) {
		t.Fatalf("the terminal does not say where Redis is recorded and mocked:\n%s", out)
	}
	if strings.Contains(out, "offline coverage") {
		t.Fatalf("coverage of a run whose Redis answered live is called offline coverage:\n%s", out)
	}
}

// A connection keploy passes through to the live service, or one to a
// mock.real destination, is named the same way, with its own reason.
func TestEveryRelayedCallIsNamedWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{models.RelayedPassThrough, "calls relayed to the live server at api.example.com:443 (1 connection(s)): keploy's agent passes them through to the real service (its global pass-through, mocking turned off, or its opportunistic TLS intercept)"},
		{models.RelayedReal, "calls relayed to the live server at api.example.com:443 (1 connection(s)): mock.real lists the destination, which every run reaches for real"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			keployDir, _, _ := replayIn(t, replayCase{
				relayedAfter: models.RelayedLives{{Reason: tc.reason, Destination: "api.example.com:443", Connections: 1}},
			})
			r := mustReceipt(t, keployDir)
			if r.Isolated || !strings.HasSuffix(r.IsolationNote, tc.want) {
				t.Fatalf("receipt isolated %v, note %q, want not isolated, %q", r.Isolated, r.IsolationNote, tc.want)
			}
		})
	}
}

// What the agent had relayed before the test command ran (an agent that
// served an earlier run) is not this run's: a run that relayed nothing more
// is isolated.
func TestRelayedBeforeTheRunIsNotTheRuns(t *testing.T) {
	before := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 2}}
	keployDir, _, _ := replayIn(t, replayCase{relayedBefore: before, relayedAfter: before})
	if r := mustReceipt(t, keployDir); !r.Isolated || r.IsolationNote != "" || len(r.Relayed) != 0 {
		t.Fatalf("a run that relayed nothing itself is isolated: %+v", r)
	}
}

// A new keploy with an older agent, which has no GET /agent/relayed (404):
// nothing shows the run answered every dependency from the recording, so its
// isolation is unknown and never claimed. A read that fails says so too.
func TestAnAgentThatCannotSayWhatItRelayedLeavesIsolationUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an older agent", fmt.Errorf("%w: agent returned 404", models.ErrRelayedLiveUnsupported), "an agent older than this keploy, or one that does not count them"},
		{"a failed read", errors.New("connection reset by peer"), "connection reset by peer"},
	} {
		older := errors.Is(tc.err, models.ErrRelayedLiveUnsupported)
		t.Run(tc.name, func(t *testing.T) {
			keployDir, code, logs := replayIn(t, replayCase{relayedErr: tc.err})
			if code != 0 {
				t.Fatalf("exit %d, want 0: an unknown is no failure", code)
			}
			r := mustReceipt(t, keployDir)
			if r.Isolated || !strings.HasPrefix(r.IsolationNote, "unknown: ") || !strings.Contains(r.IsolationNote, tc.want) {
				t.Fatalf("receipt isolated %v, note %q, want unknown, naming %q", r.Isolated, r.IsolationNote, tc.want)
			}
			out := logged(logs)
			if strings.Contains(out, "every dependency answered from the recording") {
				t.Fatal("isolation claimed on an agent that could not say what it relayed")
			}
			// An older agent: the next step is one that says what it relays.
			if got := strings.Contains(out, "run keploy's agent at this keploy's version"); got != older {
				t.Fatalf("next step to run an agent that says what it relays: %v, want %v:\n%s", got, older, out)
			}
		})
	}
}

// What the agent relayed live is named on the terminal whatever reason
// decided the run is not isolated: a run whose test command failed, and
// whose Redis was relayed to the live server, is told both, with Keploy
// Enterprise as the next step for its Redis. That the agent could not say
// what it relayed is not named (nor its next step) when an earlier reason
// decided: it is the weakest reason, said only when it is the one.
func TestTheRelayedCallsAreNamedWhateverReasonDecided(t *testing.T) {
	failed := models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 2}
	_, _, logs := replayIn(t, replayCase{
		runExit:      failed,
		relayedAfter: models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 1}},
	})
	out := logged(logs)
	if !strings.Contains(out, "this replay did not prove the tests run with the dependencies off") || !strings.Contains(out, "the test command failed") {
		t.Fatalf("the terminal does not say the run is not isolated because its test command failed:\n%s", out)
	}
	if !strings.Contains(out, "Redis relayed to the live server at 10.0.0.5:6379 (1 connection(s)): open-source keploy does not mock Redis") {
		t.Fatalf("the terminal does not name the Redis relayed live of a run whose test command failed:\n%s", out)
	}
	if !strings.Contains(out, "use Keploy Enterprise: "+models.RedisEnterpriseURL) {
		t.Fatalf("the terminal does not give the Redis relayed live its next step:\n%s", out)
	}

	_, _, logs = replayIn(t, replayCase{
		runExit:    failed,
		relayedErr: fmt.Errorf("%w: agent returned 404", models.ErrRelayedLiveUnsupported),
	})
	out = logged(logs)
	if !strings.Contains(out, "the test command failed") {
		t.Fatalf("the terminal does not say the test command failed:\n%s", out)
	}
	if strings.Contains(out, "does not say which calls it relayed") || strings.Contains(out, "run keploy's agent at this keploy's version") {
		t.Fatalf("a run not isolated because its test command failed is told about an agent that cannot say what it relayed:\n%s", out)
	}
}

// Under docker compose the agent stops with the project and leaves its
// count in the account the CLI reads: an account that has it decides the
// claim; one an older agent left, without it, leaves it unknown.
func TestComposeReplayReadsWhatTheStoppedAgentRelayed(t *testing.T) {
	redis := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "redis:6379", Connections: 2}}
	for _, tc := range []struct {
		name         string
		relayed      models.RelayedLives
		noRelayed    bool
		wantIsolated bool
		wantNote     string
	}{
		{name: "relayed none", wantIsolated: true},
		{name: "relayed Redis", relayed: redis, wantNote: "Redis relayed to the live server at redis:6379 (2 connection(s)): open-source keploy does not mock Redis"},
		{name: "an older agent's account", noRelayed: true, wantNote: "unknown: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := composeReplayReceipt(t, func(instr *composeInstr) {
				instr.relayed, instr.leftNoRelayed = tc.relayed, tc.noRelayed
			})
			if r.Isolated != tc.wantIsolated || !strings.HasPrefix(r.IsolationNote, tc.wantNote) {
				t.Fatalf("receipt isolated %v, note %q, want isolated %v, note starting %q", r.Isolated, r.IsolationNote, tc.wantIsolated, tc.wantNote)
			}
		})
	}
}

// isolation itself, row by row: relayed calls take the claim away ahead of
// what the counts say, and an unknown is the last, weakest reason.
func TestIsolationRows(t *testing.T) {
	clean := replayCounts{consumed: 1}
	redis := relayedCheck{known: true, rows: models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "r:6379", Connections: 1}}}
	none := relayedCheck{known: true}
	unknown := relayedCheck{unread: models.ErrRelayedLiveUnsupported}
	for _, tc := range []struct {
		name     string
		counts   replayCounts
		relayed  relayedCheck
		isolated bool
		note     string
	}{
		{"nothing relayed, nothing missed", clean, none, true, ""},
		{"Redis relayed", clean, redis, false, "Redis relayed to the live server at r:6379"},
		{"Redis relayed and a miss: the relay is named", replayCounts{consumed: 1, missedData: 1, missed: 1}, redis, false, "Redis relayed"},
		{"unknown, nothing missed", clean, unknown, false, "unknown: "},
		{"unknown and a miss: the miss is named", replayCounts{consumed: 1, missedData: 1, missed: 1}, unknown, false, "1 call(s) matched nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolated, note := isolation(0, models.MissFail, tc.counts, nil, tc.relayed)
			if isolated != tc.isolated || !strings.HasPrefix(note, tc.note) {
				t.Fatalf("isolation = %v, %q; want %v, %q", isolated, note, tc.isolated, tc.note)
			}
		})
	}
}

// composeReplayReceipt runs one compose replay whose agent left an account
// serving two recorded calls and missing none, after set adjusts the fake,
// and returns its receipt.
func composeReplayReceipt(t *testing.T, set func(instr *composeInstr)) *Receipt {
	t.Helper()
	instr := newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.runUntilReady = true
	instr.consumedErr = errors.New("connection refused")
	instr.mockErrorsErr = errors.New("connection refused")
	instr.leftOutcome = models.MockOutcome{Consumed: []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}}
	set(instr)

	cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
	cfg.Mock.OnMiss = string(models.MissFail)
	cfg.Path = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	if err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background()); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	r, err := ReadReceipt(cfg.Path, "set")
	if err != nil || r == nil {
		t.Fatalf("no receipt: %v", err)
	}
	return r
}

// The mocks the agent says it leaves out of the pool it serves (its answer
// to the store: a set's Redis mocks an older keploy recorded, when it relays
// Redis live) are not the run's loaded mocks: the summary and the receipt
// count what the agent holds to serve. A set of nothing but such mocks is
// said to be one, and does not fail as a replay that verified nothing.
func TestTheLoadedCountLeavesOutWhatTheAgentLeftOut(t *testing.T) {
	for _, tc := range []struct {
		name       string
		leaveOut   []string
		consumed   []models.MockState
		wantLoaded int64
		wantExit   int
		wantLog    string
	}{
		{name: "an agent that leaves nothing out", consumed: []models.MockState{{Name: "mock-0"}}, wantLoaded: 3},
		{name: "one Redis mock left out", leaveOut: []string{"mock-1"}, consumed: []models.MockState{{Name: "mock-0"}}, wantLoaded: 2},
		{name: "every mock a Redis mock", leaveOut: []string{"mock-0", "mock-1", "mock-2"}, wantLoaded: 0,
			wantLog: "every mock of the set is a Redis mock this keploy does not serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks, instr.leaveOut = tc.consumed, tc.leaveOut
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.OnMiss = string(models.MissFail)
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			if err := New(zap.New(core), instr, loadedMockDB{n: 3}, nil, nil, nil, cfg).Replay(context.Background()); err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit %d, want %d", utils.ErrCode, tc.wantExit)
			}
			var summary map[string]any
			var printed []string
			for _, e := range logs.All() {
				printed = append(printed, e.Message)
				if e.Message == "mock replay summary" {
					summary = e.ContextMap()
				}
			}
			if summary == nil || summary["loaded"] != tc.wantLoaded {
				t.Fatalf("summary %v, want loaded %d", summary, tc.wantLoaded)
			}
			if joined := strings.Join(printed, "\n"); tc.wantLog != "" && !strings.Contains(joined, tc.wantLog) {
				t.Fatalf("no log saying %q:\n%s", tc.wantLog, joined)
			}
		})
	}
}
