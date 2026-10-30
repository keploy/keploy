package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// skipPusher records the proven gate a replay pushes.
type skipPusher struct {
	Instrumentation
	pushed bool
	skip   map[string]string
	report string
	err    error
}

func (s *skipPusher) PushScopeSkips(_ context.Context, skip map[string]string, report string) error {
	s.pushed, s.skip, s.report = true, skip, report
	return s.err
}

// Under the switch, a replay hands the agent the skip source's deny list, and
// an empty one when it lists nothing, so a long-lived agent never keeps an
// earlier list. The source learns which recording the replay serves.
// provenOn turns the switch on in a build that judges replays, as keploy
// enterprise is.
func provenOn(t *testing.T) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	JudgeReplays()
	t.Cleanup(func() { replaysJudged.Store(false) })
}

func TestPushScopeSkips(t *testing.T) {
	provenOn(t)
	p := &skipPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, config: &config.Config{Path: "/repo/keploy"}}
	require.NoError(t, m.pushScopeSkips(context.Background(), "t2-go", "d1"))
	require.False(t, p.pushed, "no source installed: nothing to push")

	var asked ScopeGateRequest
	RegisterScopeSkipSource(func(_ context.Context, req ScopeGateRequest) (map[string]string, error) {
		asked = req
		return map[string]string{"orders::TestToken": "flaky: replay_only"}, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	require.NoError(t, m.pushScopeSkips(context.Background(), "t2-go", "d1"))
	require.Equal(t, ScopeGateRequest{Set: "t2-go", MocksDigest: "d1", Path: m.config.Path}, asked)
	require.Equal(t, map[string]string{"orders::TestToken": "flaky: replay_only"}, p.skip)

	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, error) { return nil, nil })
	require.NoError(t, m.pushScopeSkips(context.Background(), "t2-go", "d1"))
	require.NotNil(t, p.skip, "an empty list is pushed, not left out")
	require.Empty(t, p.skip)
	require.Empty(t, p.report, "no report named: none pushed")

	// The file the replay writes its report to goes with the list.
	ReportReplayTo("keploy/reports/r1/replay.yaml")
	t.Cleanup(func() { ReportReplayTo("") })
	require.NoError(t, m.pushScopeSkips(context.Background(), "t2-go", "d1"))
	require.Equal(t, "keploy/reports/r1/replay.yaml", p.report)
}

// A list that cannot be read, or an agent that refuses it, refuses the replay.
func TestPushScopeSkipsRefuses(t *testing.T) {
	provenOn(t)
	p := &skipPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, config: &config.Config{}}
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, error) {
		return nil, errors.New("unreadable")
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	require.ErrorContains(t, m.pushScopeSkips(context.Background(), "s", ""), "unreadable")

	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, error) {
		return map[string]string{"a": "b"}, nil
	})
	p.err = errors.New("refused")
	require.ErrorContains(t, m.pushScopeSkips(context.Background(), "s", ""), "refused")

	// An instrumentation without the capability cannot skip a listed test.
	m.instrumentation = &gatePusher{}
	require.Error(t, m.pushScopeSkips(context.Background(), "s", ""))
}

// With the switch off the source is never read.
func TestPushScopeSkipsNeedsTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	p := &skipPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, config: &config.Config{}}
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, error) {
		t.Fatal("read with the switch off")
		return nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	require.NoError(t, m.pushScopeSkips(context.Background(), "s", ""))
	require.False(t, p.pushed)
}

// Each test's receipt line carries the evidence its windows were failed with;
// a test that ran twice keeps both attempts' evidence.
func TestWithEvidence(t *testing.T) {
	ev := func(miss, unmade string) *models.ScopeEvidence {
		e := &models.ScopeEvidence{Reason: "keploy: x"}
		if miss != "" {
			e.Misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: miss}}
		}
		if unmade != "" {
			e.Unconsumed = []string{unmade}
		}
		return e
	}
	tests := []TestReceipt{{Name: "o::A", Outcome: "passed"}, {Name: "o::B", Outcome: "passed"}}
	got := withEvidence(tests, []models.ScopeWindow{
		{Name: "o::A", Evidence: ev("POST /audit", "")},
		{Name: "o::A", Evidence: ev("", "mock-3")},
		{Name: "o::B"},
		{Name: "o::B", Evidence: &models.ScopeEvidence{}},
	})
	require.NotNil(t, got[0].Evidence)
	require.Len(t, got[0].Evidence.Misses, 1)
	require.Equal(t, []string{"mock-3"}, got[0].Evidence.Unconsumed)
	require.Nil(t, got[1].Evidence, "no evidence, nothing attached")

	// Evidence with only keploy's reason (the test's process exited) is
	// keploy's verdict too.
	got = withEvidence([]TestReceipt{{Name: "o::C", Outcome: "failed"}}, []models.ScopeWindow{
		{Name: "o::C", Evidence: &models.ScopeEvidence{Reason: "keploy: the test process exited while the test was open"}},
	})
	require.NotNil(t, got[0].Evidence)
	require.Contains(t, got[0].Evidence.Reason, "exited while the test was open")

	// A test keploy could not read stays unread on its line, through the
	// receipt the CLI reads: one unread attempt is enough. A read test's
	// evidence says nothing of it.
	got = withEvidence([]TestReceipt{{Name: "o::D", Outcome: "failed"}, {Name: "o::E", Outcome: "failed"}}, []models.ScopeWindow{
		{Name: "o::D", Evidence: &models.ScopeEvidence{Reason: "keploy: keploy could not tell what o::D did: x", Unread: true}},
		{Name: "o::D", Evidence: ev("GET /y", "")},
		{Name: "o::E", Evidence: ev("GET /z", "")},
	})
	require.True(t, got[0].Evidence.Unread)
	require.False(t, got[1].Evidence.Unread)
	// An unread attempt after a read one: the line says unread, with the
	// unread attempt's reason.
	late := withEvidence([]TestReceipt{{Name: "o::F", Outcome: "failed"}}, []models.ScopeWindow{
		{Name: "o::F", Evidence: &models.ScopeEvidence{Reason: "keploy: a miss", Misses: []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /a"}}}},
		{Name: "o::F", Evidence: &models.ScopeEvidence{Reason: "keploy: keploy could not tell what o::F did: y", Unread: true}},
	})
	require.True(t, late[0].Evidence.Unread)
	require.Contains(t, late[0].Evidence.Reason, "could not tell what o::F did")
	k := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(k, "set"), 0o755))
	writeTestsReceipt(zap.NewNop(), k, TestsReceipt{Set: "set", Tests: got})
	raw, err := os.ReadFile(filepath.Join(k, "set", TestsReceiptFile))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(raw), "unread"), "only the unread test's line says it: %s", raw)
	read, err := ReadTestsReceipt(k, "set")
	require.NoError(t, err)
	require.True(t, read.Tests[0].Evidence.Unread)
	require.False(t, read.Tests[1].Evidence.Unread)
}

// The receipt counts the run's misses that count against a test
// (models.UnmatchedCall.CountsAgainstTest) under the proven switch only: a
// receipt written with the switch off is what it was before the switch.
func TestTheReceiptCountsDataMissesUnderTheSwitchOnly(t *testing.T) {
	misses := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x"}, {Protocol: "DNS", ActualSummary: "A x"},
		{Protocol: "MySQL", Destination: "10.9.9.9:13306", MatchPhase: models.MatchPhaseNoRequest}}
	t.Setenv(provenswitch.Env, "")
	dir, _, _ := replayIn(t, replayCase{mockErrs: misses})
	require.Nil(t, mustReceipt(t, dir).MissedData, "absent with the switch off")
	require.Nil(t, mustReceipt(t, dir).UnchargedMisses, "absent with the switch off")
	raw, err := os.ReadFile(filepath.Join(dir, "set", ReceiptFile))
	require.NoError(t, err)
	require.NotContains(t, strings.TrimPrefix(string(raw), receiptHeader), "missedData", "the header says what it means; the fields hold none")

	provenOn(t)
	dir, _, _ = replayIn(t, replayCase{mockErrs: misses, mapped: true})
	r := mustReceipt(t, dir)
	require.NotNil(t, r.MissedData)
	require.Equal(t, 1, *r.MissedData, "a DNS lookup is protocol-phase, and a connection that sent nothing made no call")
	require.Len(t, r.UnchargedMisses, 1, "the call no test was charged with is named, not only counted")
	require.Equal(t, "GET /x", r.UnchargedMisses[0].ActualSummary)
}

// Under the switch the receipt names each call MissedData counts that no
// test was charged with: as evidence keeps a miss (no value the app sent),
// with the process that made it (its pid, comm and command line, which only
// this local receipt holds). A call a test's end charged to it is that
// test's, not listed; one that does not count against a test (DNS) is not
// listed either. Each test's line says when its latest run began, so a cut
// round names the last test begun.
func TestTheReceiptNamesTheCallsChargedToNoTest(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	instr.windows = []models.ScopeWindow{
		{Name: "o::TestA", Start: t0, End: t0.Add(time.Second), Outcome: models.ScopeOutcomeFailed},
		// Its retry: the latest run's begin is the line's.
		{Name: "o::TestA", Start: t0.Add(2 * time.Second), End: t0.Add(3 * time.Second), Outcome: models.ScopeOutcomePassed},
		// An app the test started is no run of the test, so not its begin:
		// the run's test windows leave it out (testWindows).
		{Name: "o::TestA", Start: t0.Add(2500 * time.Millisecond), End: t0.Add(3 * time.Second), App: true},
	}
	src := &models.Origin{Comm: "api", Exe: "./api", PID: 4242, Cmdline: "./api --token <redacted>"}
	instr.mockErrors = []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "GET /in-test", Destination: "dep:80", At: t0.Add(500 * time.Millisecond), ChargedTo: "o::TestA"},
		{Protocol: "HTTP", ActualSummary: "POST /teardown", Destination: "dep:80", At: t0.Add(4 * time.Second), Source: src,
			MatchPhase: models.MatchPhaseBody, NextSteps: "body.user.email drifted.", ReceivedReq: "POST /teardown {\"user\":{\"email\":\"a@b.c\"}}",
			Diff: "a@b.c", FieldDiffs: []models.MockFieldDiff{{Path: "body.user.email", Expected: "x@y.z", Actual: "a@b.c"}}},
		{Protocol: "DNS", ActualSummary: "A dep", At: t0.Add(4 * time.Second)},
	}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	r, err := ReadReceipt(keployDir, "set")
	require.NoError(t, err)
	require.Len(t, r.UnchargedMisses, 1)
	u := r.UnchargedMisses[0]
	require.Equal(t, "dep:80", u.Destination)
	require.Equal(t, "body.user.email drifted.", u.NextSteps)
	require.Equal(t, &MissProcess{PID: 4242, Comm: "api", Exe: "./api", Cmdline: "./api --token <redacted>"}, u.Process)
	require.Empty(t, u.ReceivedReq, "no value the app sent")
	require.Empty(t, u.Diff, "no value the app sent")
	for _, d := range u.FieldDiffs {
		require.Empty(t, d.Expected+d.Actual, "no value the app sent")
	}
	raw, err := os.ReadFile(filepath.Join(keployDir, "set", ReceiptFile))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "a@b.c")
	require.NotContains(t, string(raw), "GET /in-test", "a call a test's end charged is that test's")
	// The receipt's JSON form (enterprise's keploy mock status --json embeds
	// it) carries no pid and no command line, neither on the process nor on
	// the call's source.
	js, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(js), `"comm":"api"`)
	require.NotContains(t, string(js), "4242")
	require.NotContains(t, string(js), "--token")
	// So does the receipt as the run holds it, before its file is written
	// and read back (which keeps no pid or command line on a call's source,
	// models.Origin).
	js, err = json.Marshal(Receipt{UnchargedMisses: unchargedMisses(instr.mockErrors)})
	require.NoError(t, err)
	require.Contains(t, string(js), `"comm":"api"`)
	require.NotContains(t, string(js), "4242")
	require.NotContains(t, string(js), "--token")
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.Len(t, tests.Tests, 1)
	require.True(t, tests.Tests[0].Began.Equal(t0.Add(2*time.Second)), "began %v", tests.Tests[0].Began)
}

// Without the proven switch the tests receipt (last-replay-tests.yaml, what
// keploy mock status reads) is what it was before: no test's line says when
// it began, in the file or in its JSON form.
func TestWithTheSwitchOffTheTestsReceiptSaysNoBegin(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	instr.windows = []models.ScopeWindow{
		{Name: "o::TestA", Start: t0, End: t0.Add(time.Second), Outcome: models.ScopeOutcomePassed},
	}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = keployDir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	raw, err := os.ReadFile(filepath.Join(keployDir, "set", TestsReceiptFile))
	require.NoError(t, err)
	require.Contains(t, string(raw), "o::TestA", "the test's line is written")
	require.NotContains(t, string(raw), "began", "without the switch no test's line says when it began")
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.Len(t, tests.Tests, 1)
	require.True(t, tests.Tests[0].Began.IsZero(), "began %v", tests.Tests[0].Began)
	js, err := json.Marshal(tests)
	require.NoError(t, err)
	require.NotContains(t, string(js), "began")
}

// Under the switch a run list conflicts with the deny list: --run-only is
// refused before anything runs.
func TestRunOnlyIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test ./...")
	cfg.Path = t.TempDir()
	cfg.Mock.OnMiss = "fail"
	cfg.Mock.RunOnly = []string{"TestA"}
	err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "--run-only is not used with KEPLOY_PROVEN")
}

// A build that does not judge a replay refuses one under the switch: its
// exit code would be the test command's alone.
func TestAReplayUnderTheSwitchNeedsAJudgingBuild(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	replaysJudged.Store(false)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test ./...")
	cfg.Path = t.TempDir()
	cfg.Mock.OnMiss = "fail"
	err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "needs a keploy build that judges each test")
}

// replayErr runs one replay of a one-mock set against instr and returns its
// error.
func replayErr(t *testing.T, instr *composeInstr, cmdType utils.CmdType) error {
	t.Helper()
	return replayErrOnMiss(t, instr, cmdType, "")
}

// replayErrOnMiss is replayErr with --on-miss onMiss ("" for fail).
func replayErrOnMiss(t *testing.T, instr *composeInstr, cmdType utils.CmdType, onMiss string) error {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	writeMapping(t, keployDir)
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	cfg := instrConfig(instr, cmdType, "go test -p 1 -json ./...")
	cfg.Path = keployDir
	cfg.Mock.OnMiss = "fail"
	if onMiss != "" {
		cfg.Mock.OnMiss = onMiss
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	return New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
}

// Under the switch what each test did is keploy's verdict: a replay that
// cannot read it did not complete, and fails saying so.
func TestAReplayThatCannotReadItsTestsFails(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.windowsErr = errors.New("connection reset by peer")
	err := replayErr(t, instr, utils.Native)
	require.ErrorContains(t, err, "keploy could not read what each test did from its agent: connection reset by peer")
	// The test command ran, and passed: the receipt says keploy failed
	// after it, never that it did not run.
	wd, werr := os.Getwd() // replayErr ran it in its repo
	require.NoError(t, werr)
	r := mustReceipt(t, filepath.Join(wd, "keploy"))
	require.Equal(t, FailedByKeploy, r.FailedBy)
	require.Equal(t, 0, r.RunnerExitCode)
	require.Equal(t, 1, r.ExitCode)
	require.Equal(t, "the test command ran; keploy could not read what it did", r.IsolationNote)
}

// When keploy had already failed under the test command (its agent died),
// that is the reason the replay gives, not the read that failed because of
// it.
func TestAReplayWhoseAgentDiedSaysSo(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the keploy agent exited: signal: killed")})
	instr.windowsErr = context.Canceled
	err := replayErr(t, instr, utils.Native)
	require.ErrorContains(t, err, "keploy did not complete the run: the keploy agent exited: signal: killed")
	require.NotContains(t, err.Error(), "could not read")
}

// With the switch off a failed read is no per-test windows, as before the
// switch existed.
func TestAFailedWindowsReadIsNoWindowsWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.windowsErr = errors.New("connection reset by peer")
	require.NoError(t, replayErr(t, instr, utils.Native))
}

// Under the switch a docker compose command is refused before anything
// starts, by the record and the replay alike: keploy cannot tell which
// process runs its tests, or read what each did.
func TestComposeIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped})
	err := replayErr(t, instr, utils.DockerCompose)
	require.ErrorContains(t, err, "keploy mock replay with KEPLOY_PROVEN runs the test command natively")
	runs, hits := instr.counts()
	require.Zero(t, runs, "the compose project was started")
	require.Zero(t, hits, "the agent was waited for")

	rec := &runnerInstr{composeInstr: newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	err = recordSet(t, rec, nil, func(c *config.Config) { c.CommandType = string(utils.DockerCompose) })
	require.ErrorContains(t, err, "keploy mock record with KEPLOY_PROVEN runs the test command natively")
	opts, _ := rec.setup()
	require.Zero(t, opts.Mode, "the agent was set up")
}

// Under the switch an instrumentation that cannot read the windows at all is
// an error, never "no tests".
func TestReadWindowsWithoutAReaderIsAnErrorUnderTheSwitch(t *testing.T) {
	m := &mockService{logger: zap.NewNop(), instrumentation: &skipPusher{}}
	t.Setenv(provenswitch.Env, "")
	w, err := m.readWindows(context.Background())
	require.NoError(t, err)
	require.Nil(t, w)
	provenOn(t)
	_, err = m.readWindows(context.Background())
	require.Error(t, err)
}

// Under the switch a call served live is not judged, so --on-miss
// passthrough and record are refused before anything runs.
func TestOnMissOtherThanFailIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	for _, policy := range []string{"passthrough", "record"} {
		instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
		err := replayErrOnMiss(t, instr, utils.Native, policy)
		require.ErrorContains(t, err, "--on-miss "+policy+" is not used with KEPLOY_PROVEN")
		runs, _ := instr.counts()
		require.Zero(t, runs)
	}
}

// namedMockDB is a mock store holding mocks, all in the session tier.
type namedMockDB struct {
	stubMockDB
	mocks []*models.Mock
}

func (l namedMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return l.mocks, nil
}

func named(names ...string) namedMockDB {
	var l namedMockDB
	for _, n := range names {
		l.mocks = append(l.mocks, &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: n})
	}
	return l
}

// Under the switch a set without a mapping file is judged like any other:
// none of its tests owns a mock (design v14 §5.2), so the agent is handed an
// empty table and every mock as the keyless pool slice; a dependency-free
// set stays green, and every other call misses.
func TestASetWithoutMappingsOwnsNothingUnderTheSwitch(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zap.WarnLevel)
	require.NoError(t, New(zap.New(core), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	runs, _ := instr.counts()
	require.Equal(t, 1, runs, "the test command ran")
	require.NotNil(t, instr.table, "an empty table is pushed, not left out")
	require.Empty(t, instr.table)
	require.Equal(t, map[string][]string{models.PoolKeyNone: {"mock-0"}}, instr.pool)
	said := logs.FilterMessageSnippet("no test of this mock set owns a recorded call").All()
	require.Len(t, said, 1, "the set owns nothing, and keploy says so once with the fix")
	require.Contains(t, said[0].ContextMap()["next_step"], "keploy mock record")
}

// Under the switch a mapping that names a mock the recording does not have
// refuses: what the test owns cannot be told, and nothing is replayed
// unjudged.
func TestAMappingNamingAnAbsentMockIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-9\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "keploy/set/mappings.yaml names mocks keploy/set/mocks.yaml does not have: mock-9 (under test o::TestA)")
	require.ErrorContains(t, err, "If you edited mappings.yaml, correct those names; otherwise record the set again with keploy mock record")
	runs, _ := instr.counts()
	require.Zero(t, runs, "the test command ran unjudged")
}

// Under the switch every section of a mapping that names mocks is checked:
// a name under startup:, worker:, shared: or runner: that the recording
// does not have refuses too, each said where it is. Before, only a test's
// list and startup: were checked, so a mistyped worker: entry was ignored
// without a word.
func TestAMappingNamingAnAbsentMockAnywhereIsRefusedSayingWhere(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mock_entries:\n        - name: mock-0\nstartup:\n    - name: mock-7\nworker:\n    - name: mock-1\n    - name: mock-8\nshared:\n    - name: mock-9\nrunner:\n    - name: mock-10\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, named("mock-0", "mock-1"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "names mocks keploy/set/mocks.yaml does not have: mock-10 (under runner:), mock-7 (under startup:), mock-8 (under worker:), mock-9 (under shared:)")
	runs, _ := instr.counts()
	require.Zero(t, runs, "the test command ran unjudged")
}

// Under the switch a mapping with a test entry that lists mocks under both
// mock_entries and mocks is refused before the test command runs, naming the
// test and both keys: read as before, the mocks list would be dropped and its
// mock served to no test.
func TestAMappingWithBothMockListsIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mock_entries:\n        - name: mock-0\n      mocks:\n        - name: mock-1\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, named("mock-0", "mock-1"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "its mappings cannot be read")
	require.ErrorContains(t, err, "test \"o::TestA\" in mappings has both `mocks` and `mock_entries`")
	// The edit comes first: a record would replace it. The typed refusal is
	// kept for a caller.
	require.ErrorContains(t, err, "if you edited the set's mappings file, correct that edit (a record would replace it)")
	var both *models.BothMockListsError
	require.ErrorAs(t, err, &both)
	runs, _ := instr.counts()
	require.Zero(t, runs, "the test command ran")
}

// Each key's pool slice is every mock no test lists, under the key its
// startup entry gives it, else the keyless slice; a test's mock is in no
// slice, and names nothing loaded has are reported.
func TestPoolSlices(t *testing.T) {
	mp := &models.Mapping{Startup: []models.MockEntry{{Name: "mock-1", Key: "o"}, {Name: "mock-2"}, {Name: "mock-0", Key: "o"}, {Name: "mock-8", Key: "o"}}}
	held := named("mock-0", "mock-1", "mock-2", "mock-3").mocks
	// mock-cf is decoded but never held (a connection failure): a mapping
	// may name it, and it is in no slice.
	decoded := append(append([]*models.Mock(nil), held...), named("mock-cf").mocks...)
	mp.Startup = append(mp.Startup, models.MockEntry{Name: "mock-cf"})
	pool, unknown := poolSlices(mp, map[string][]string{"o::TestA": {"mock-0", "mock-9"}, "o::TestB": {"mock-cf"}}, held, decoded)
	require.Equal(t, map[string][]string{"o": {"mock-1"}, models.PoolKeyNone: {"mock-2", "mock-3"}}, pool)
	require.Equal(t, []string{"mock-8", "mock-9"}, unknown)
	pool, unknown = poolSlices(nil, nil, nil, nil)
	require.Empty(t, pool)
	require.Empty(t, unknown)
}

// Under the switch docker run is refused like compose: keploy cannot tell
// which process in the container runs the tests.
func TestDockerRunIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	err := replayErr(t, instr, utils.DockerRun)
	require.ErrorContains(t, err, "inside a container keploy cannot tell which process runs the tests")
}

// Under the switch a set whose tests all made no call is judged like any
// other: its mappings name its tests, with no mock.
func TestASetOfTestsWithoutCallsIsJudgedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestPure\n      dir: o\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.NoError(t, err)
	runs, _ := instr.counts()
	require.Equal(t, 1, runs)
}

// The nothing-verified gate (a replay that served no recorded call fails)
// is the switch-off rule; under the switch the verdict's first rule (no test
// ran) takes its place, since a test recorded with no call serves nothing
// and passes.
func TestNothingVerifiedIsTheSwitchOffRule(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			if on {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestPure\n      dir: o\n"), 0o644))
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			require.NoError(t, New(zap.NewNop(), instr, loadedMockDB{n: 1}, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
			want := 1
			if on {
				want = 0
			}
			require.Equal(t, want, utils.ErrCode)
		})
	}
}

// --strict's miss check is the verdict's second rule under the switch.
func TestStrictIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test ./...")
	cfg.Path = t.TempDir()
	cfg.Mock.OnMiss = "fail"
	cfg.Mock.Strict = true
	err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "--strict is not used with KEPLOY_PROVEN")
}

// A record that captured no call names keploy's Go integration under the
// switch, and keeps its advice with the switch off.
func TestNothingCapturedNext(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	require.Contains(t, nothingCapturedNext(), "defer keploytest.T(t)()")
	require.NotContains(t, nothingCapturedNext(), "docker")
	t.Setenv(provenswitch.Env, "")
	require.Equal(t, "confirm the test command actually calls an external dependency (HTTP, MySQL, ...), and on macOS run it via a docker command", nothingCapturedNext())
}

// Under the switch the replay hands the agent each key's pool slice from the
// recording's start-up mocks (design v14 §5.2, U21); a keyless one is in the
// keyless slice.
func TestTheReplayPushesThePoolSlices(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\nstartup:\n    - name: mock-1\n      key: o\n    - name: mock-2\n    - name: mock-3\n      key: users\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, named("mock-0", "mock-1", "mock-2", "mock-3", "mock-4"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	require.Equal(t, map[string][]string{"o": {"mock-1"}, models.PoolKeyNone: {"mock-2", "mock-4"}, "users": {"mock-3"}}, instr.pool, "a mock no test lists and no startup entry keys is the keyless slice's")
	require.NotNil(t, instr.begins, "the replay sends the agent the tests' recorded begins")
}

// Under the switch the replay hands the agent, with the table, the recorded
// window of every test of the set from its mappings (keploy test's staging
// sends them from its test cases): the learn step then reads another test's
// recording as that test's, not as an earlier place of the paired one. A
// recording the mapping lists that the agent never holds (a connection
// failure, models.AgentBound) is left out of the table but still ends its
// test's window: it was made in it.
func TestTheReplayPushesTheRecordedTestWindows(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-1\n      at: 2026-01-02T03:04:10Z\n    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-2\n        - name: mock-3\n      at: 2026-01-02T03:04:20Z\nstartup:\n    - name: mock-0\n      key: o\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-1"}, {Name: "mock-2"}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	db := named("mock-0", "mock-1", "mock-2", "mock-3")
	for i, span := range [][2]int{{1, 2}, {11, 12}, {21, 23}, {25, 26}} {
		db.mocks[i].Spec.ReqTimestampMock = t0.Add(time.Duration(span[0]) * time.Second)
		db.mocks[i].Spec.ResTimestampMock = t0.Add(time.Duration(span[1]) * time.Second)
	}
	db.mocks[3].Kind = models.ConnectionFailure
	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	require.Equal(t, []string{"mock-2"}, instr.table["o::TestB"], "the agent is handed only the recordings it holds")
	require.Equal(t, []models.TestWindow{
		{TestCase: "o::TestA", Start: t0.Add(5 * time.Second), End: t0.Add(12 * time.Second)},
		{TestCase: "o::TestB", Start: t0.Add(15 * time.Second), End: t0.Add(26 * time.Second)},
	}, instr.recordedWindows)
}

// Under the switch the replay never arms the whole pool before its run: the
// table push armed the slice of calls made before any claim, and arming the
// whole pool would let any test's mock answer them. With the switch off it
// stages the whole pool, as before.
func TestTheWholePoolIsNotArmedBeforeTheRunUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			if on {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n"), 0o644))
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
			require.Equal(t, !on, slices.Contains(instr.seq(), "stage"), "%v", instr.seq())
		})
	}
}

// What the wire refused during the run comes with the windows; the replay's
// receipt carries it for keploy's verdict, and the windows the rest of the
// replay reads have no refusal in them.
func TestTheReceiptCarriesTheRunsRefusals(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	refused := models.RunRefusal{Code: models.ScopeErrOneTestProcess, Message: "process 7 is a second test process", Remedy: "run go test -p 1"}
	instr.windows = []models.ScopeWindow{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}, {Refused: &refused}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	r, err := ReadReceipt(keployDir, "set")
	require.NoError(t, err)
	require.Equal(t, []models.RunRefusal{refused}, r.Refusals)
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.Len(t, tests.Tests, 1, "a refusal is no test")
}

// Where the run's calls to local listeners went comes with the windows, a
// row each; the replay's per-test receipt carries the rows for keploy's
// summary, and the windows the rest of the replay reads have no row in them.
func TestTheTestsReceiptCarriesTheRunsTopology(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	live := []models.LiveRow{{Test: "o::TestA", Exe: "/usr/bin/node", Port: 3000}, {Exe: "/usr/bin/node", Port: 3000}}
	mocked := []models.MockedRow{{Test: "o::TestA", Addr: "127.0.0.1:5432", PID: 42, Exe: "/usr/bin/postgres", Port: 5432}}
	instr.windows = []models.ScopeWindow{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}, {Live: &live[0]}, {Mocked: &mocked[0]}, {Live: &live[1]}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.Len(t, tests.Tests, 1, "a row is no test")
	require.Equal(t, live, tests.Live)
	require.Equal(t, mocked, tests.Mocked)
}

// Every reader of a run's windows keeps those that are no test's apart: the
// claims, the refusals and the topology rows, from the agent itself and from
// the account a docker compose agent left as it stopped alike.
func TestTheRunsWindowsAreSplitOnEveryPath(t *testing.T) {
	claim := models.ScopeWindow{Claim: "o"}
	refused := models.RunRefusal{Code: models.ScopeErrOneTestProcess, Message: "a second test process"}
	live := models.LiveRow{Test: "o::TestA", Exe: "/usr/bin/node", Port: 3000}
	mocked := models.MockedRow{Test: "o::TestA", Addr: "127.0.0.1:5432", Port: 5432}
	all := []models.ScopeWindow{claim, {Name: "o::TestA", Outcome: models.ScopeOutcomePassed}, {Refused: &refused}, {Live: &live}, {Mocked: &mocked}}
	check := func(t *testing.T, m *mockService, got []models.ScopeWindow) {
		t.Helper()
		require.Equal(t, []models.ScopeWindow{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}}, got, "only the test's window is a test's")
		require.Equal(t, []models.ScopeWindow{claim}, m.claims)
		require.Equal(t, []models.RunRefusal{refused}, m.refusals)
		require.Equal(t, []models.LiveRow{live}, m.live)
		require.Equal(t, []models.MockedRow{mocked}, m.mocked)
	}
	t.Run("compose", func(t *testing.T) {
		instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
		instr.leftOutcome = models.MockOutcome{Windows: append([]models.ScopeWindow(nil), all...)}
		cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
		m := &mockService{logger: zap.NewNop(), instrumentation: instr, config: cfg}
		got, err := m.replayWindows(context.Background())
		require.NoError(t, err)
		check(t, m, got)
	})
	t.Run("agent", func(t *testing.T) {
		provenOn(t)
		instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
		instr.windows = append([]models.ScopeWindow(nil), all...)
		cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
		m := &mockService{logger: zap.NewNop(), instrumentation: instr, config: cfg}
		got, err := m.replayWindows(context.Background())
		require.NoError(t, err)
		check(t, m, got)
	})
}

// The per-test table the agent gets names only mocks it holds: a mock the
// set decoded that keploy never hands the agent (a connection failure) is no
// test's, so the agent does not refuse the begin of the test that lists it.
func TestTheTableNamesOnlyMocksTheAgentHolds(t *testing.T) {
	got := heldOnly(map[string][]string{"o::TestA": {"mock-0", "mock-cf"}, "o::TestB": {"mock-cf"}, "o::TestC": {}}, named("mock-0").mocks)
	require.Equal(t, map[string][]string{"o::TestA": {"mock-0"}, "o::TestB": {}, "o::TestC": {}}, got)
}

// A test owns a test mock only if a mock its mapping lists is held and
// data-plane: a mapping of handshakes and DNS lookups alone owns none.
func TestOwnsTestMocks(t *testing.T) {
	data := &models.Mock{Name: "mock-0", Kind: models.HTTP}
	cfg := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
	dns := &models.Mock{Name: "mock-2", Kind: models.DNS}
	loaded := []*models.Mock{data, cfg, dns}
	require.True(t, ownsTestMocks(map[string][]string{"o::TestA": {"mock-1", "mock-0"}}, loaded))
	require.False(t, ownsTestMocks(map[string][]string{"o::TestA": {"mock-1", "mock-2"}, "o::TestB": {}}, loaded))
	require.False(t, ownsTestMocks(map[string][]string{}, loaded))
}

// The refusal names the mapping file the set has: mappings.json for a set
// stored as JSON, never a mappings.yaml that does not exist.
func TestARefusalNamesTheMappingFileTheSetHas(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "set", "mappings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &mockService{config: &config.Config{Path: dir}, mockDB: mockdb.New(zap.NewNop(), dir, "mocks")}
	err := m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"mock-9"})
	if err == nil || !strings.Contains(err.Error(), "/set/mappings.json names mocks") || !strings.Contains(err.Error(), "If you edited mappings.json") || strings.Contains(err.Error(), "mappings.yaml") {
		t.Fatalf("refusal: %v", err)
	}
}

// A mock a hand edit gave a kind this keploy has no reader for (Http ->
// HTTP) is skipped when the set's mocks are read (mockdb logs it), so the
// test that lists it would own nothing: the replay refuses the set, and the
// refusal names the document in the mocks file, with its kind, as the edit
// to correct, not the name in mappings.yaml, which is right. A name the
// mocks file does not have is still named as one.
func TestARefusalNamesAMockOfAKindKeployCannotRead(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	mocks := "version: api.keploy.io/v1beta1\nkind: HTTP\nname: mock-0\nspec:\n    req:\n        method: GET\n        url: http://h/a\n---\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-1\nspec:\n    req:\n        method: GET\n        url: http://h/b\n"
	maps := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mock_entries:\n        - name: mock-0\n    - id: o::TestB\n      dir: o\n      mock_entries:\n        - name: mock-1\n"
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte(mocks), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(maps), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), keployDir, "mocks"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), `keploy/set/mocks.yaml has mocks keploy/set/mappings.yaml names that this keploy skips: mock-0 (under test o::TestA), of kind "HTTP", which this keploy cannot read.`)
	require.Contains(t, err.Error(), "If you edited mocks.yaml, give each mock this keploy cannot read back the kind and spec keploy wrote. If a newer keploy or keploy enterprise recorded them, upgrade keploy or use keploy enterprise; otherwise")
	require.NotContains(t, err.Error(), "correct those names")
	runs, _ := instr.counts()
	require.Zero(t, runs, "a refused set runs no test")

	// With a name the mocks file does not have beside it, both are named.
	m := &mockService{config: &config.Config{Path: keployDir}, mockDB: mockdb.New(zap.NewNop(), keployDir, "mocks")}
	err = m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"mock-9", "mock-0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `skips: mock-0, of kind "HTTP", which this keploy cannot read; keploy/set/mappings.yaml names mocks keploy/set/mocks.yaml does not have: mock-9.`)
	require.Contains(t, err.Error(), "upgrade keploy or use keploy enterprise. If you edited mappings.yaml, correct those names; otherwise")
}

// A mocks.yaml saved by hand without its final newline: the replay's read
// skips the last mock as a write cut short (mockdb nextDoc), though it parses
// whole and its kind is right, so the test that lists it owns nothing and the
// set is refused. The refusal says that, and that the fix is the newline; it
// does not blame the mock's kind, which is right, or the name in
// mappings.yaml. The reason is the read's own (mockdb.MockFileReader), so it
// cannot drift from what the replay did.
func TestARefusalNamesALastMockWithNoFinalNewlineAsCutShort(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	mocks := "version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\nspec:\n    req:\n        method: GET\n        url: http://h/a\n---\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-1\nspec:\n    req:\n        method: GET\n        url: http://h/b"
	maps := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mock_entries:\n        - name: mock-0\n    - id: o::TestB\n      dir: o\n      mock_entries:\n        - name: mock-1\n"
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte(mocks), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(maps), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), keployDir, "mocks"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "names that this keploy skips: mock-1 (under test o::TestB), the file's last document, whose last line has no newline: keploy reads it as a write cut short.")
	require.Contains(t, err.Error(), "If you saved mocks.yaml by hand, end it with a newline once its last mock is whole; otherwise record the set again")
	for _, wrong := range []string{"of kind", "kind and spec", "upgrade", "correct those names"} {
		require.NotContains(t, err.Error(), wrong)
	}
	runs, _ := instr.counts()
	require.Zero(t, runs, "a refused set runs no test")

	// Beside a mistyped kind, each is named with its own reason, and the
	// fix names both edits.
	m := &mockService{config: &config.Config{Path: keployDir}, mockDB: mockdb.New(zap.NewNop(), keployDir, "mocks")}
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte(strings.Replace(mocks, "kind: Http\nname: mock-0", "kind: HTTP\nname: mock-0", 1)), 0o644))
	err = m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"mock-1", "mock-0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `mock-0, of kind "HTTP", which this keploy cannot read; mock-1, the file's last document`)
	require.Contains(t, err.Error(), "end it with a newline once its last mock is whole. If you edited mocks.yaml, give each mock this keploy cannot read back the kind and spec keploy wrote. If a newer")

	// An NDJSON set whose last line parses and does not decode, with no
	// newline, is skipped the same way. Its JSON is whole, so the refusal
	// says what is wrong, the spec, with the decoder's own error: the fix is
	// the spec, not the newline alone (which would make the read fail on it).
	require.NoError(t, os.Remove(filepath.Join(keployDir, "set", "mocks.yaml")))
	line := `{"version":"api.keploy.io/v1beta1","kind":"Http","name":"mock-1","spec":{"req":[1,2]}}`
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.json"), []byte(line), 0o644))
	err = m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"mock-1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "keploy/set/mocks.json has mocks keploy/set/mappings.yaml names that this keploy skips: mock-1, the file's last line, which has no newline and whose spec does not decode (")
	require.Contains(t, err.Error(), "cannot unmarshal array")
	require.Contains(t, err.Error(), "If you saved mocks.json by hand, give its last line the spec keploy wrote and end the line with a newline; otherwise")
}

// A connection failure this keploy cannot replay, and an NDJSON mock of a
// kind keploy reads only from YAML, are each named with their own fix. The
// connection failure's is an upgrade or the fields keploy wrote, never
// "use keploy enterprise", which replays it no better; the YAML-only kind's
// is the YAML format (and keploy enterprise for a "-" kind), never an
// upgrade, since no keploy reads it from NDJSON.
func TestARefusalGivesASkippedConnectionFailureAndAYAMLOnlyKindTheirOwnFix(t *testing.T) {
	keployDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	cf := "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-0\nspec:\n    address: 10.0.3.7:443\n    phase: request\n    outcome: reset\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n"
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte(cf), 0o644))
	m := &mockService{config: &config.Config{Path: keployDir}, mockDB: mockdb.New(zap.NewNop(), keployDir, "mocks")}
	err := m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"cf-0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "skips: cf-0, a connection failure this keploy cannot replay.")
	require.Contains(t, err.Error(), "If a newer keploy recorded a connection failure, upgrade keploy to replay it; if you edited one in mocks.yaml, give it back the fields keploy wrote; otherwise record")
	for _, wrong := range []string{"enterprise", "kind and spec", "correct those names"} {
		require.NotContains(t, err.Error(), wrong)
	}

	require.NoError(t, os.Remove(filepath.Join(keployDir, "set", "mocks.yaml")))
	line := `{"version":"api.keploy.io/v1beta1","kind":"Redis-Ent","name":"mock-0","spec":{}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.json"), []byte(line), 0o644))
	err = m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"mock-0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `skips: mock-0, of kind "Redis-Ent", which keploy reads only from a YAML mock file.`)
	require.Contains(t, err.Error(), `Keploy reads a mock of such a kind only from a YAML mock file, and one with a "-" in its kind only with keploy enterprise: record the set again in YAML, not as mocks.json; otherwise`)
	for _, wrong := range []string{"upgrade", "use keploy enterprise", "correct those names"} {
		require.NotContains(t, err.Error(), wrong)
	}
}

// unreadStore is a MockDB whose set's mock file cannot be read again.
type unreadStore struct{ MockDB }

func (unreadStore) SkippedMocks(context.Context, string) (string, map[string]models.SkippedMock) {
	return "", nil
}

// When the mocks file cannot be read again, the refusal does not call a
// listed name one the file does not have, nor tell the user to correct
// mappings.yaml: it says the replay did not read them.
func TestARefusalDoesNotCallANameAbsentWhenTheMocksFileCannotBeReadAgain(t *testing.T) {
	keployDir := filepath.Join(t.TempDir(), "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("x"), 0o644))
	m := &mockService{config: &config.Config{Path: keployDir}, mockDB: unreadStore{}}
	err := m.refuseUnknownMocks(context.Background(), "set", &models.Mapping{}, []string{"mock-0"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "keploy/set/mappings.yaml names mocks the replay did not read from keploy/set/mocks.yaml: mock-0.")
	require.Contains(t, err.Error(), "Check that mocks.yaml has each of them, and that keploy can read it; otherwise")
	for _, wrong := range []string{"does not have", "correct those names"} {
		require.NotContains(t, err.Error(), wrong)
	}
}
