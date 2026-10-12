package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// skipPusher records the proven gate a replay pushes.
type skipPusher struct {
	Instrumentation
	pushed bool
	skip   map[string]string
	err    error
}

func (s *skipPusher) PushScopeSkips(_ context.Context, skip map[string]string) error {
	s.pushed, s.skip = true, skip
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
}

// The receipt counts the run's non-DNS misses under the proven switch only:
// a receipt written with the switch off is what it was before the switch.
func TestTheReceiptCountsDataMissesUnderTheSwitchOnly(t *testing.T) {
	misses := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x"}, {Protocol: "DNS", ActualSummary: "A x"}}
	t.Setenv(provenswitch.Env, "")
	dir, _, _ := replayIn(t, replayCase{mockErrs: misses})
	require.Nil(t, mustReceipt(t, dir).MissedData, "absent with the switch off")
	raw, err := os.ReadFile(filepath.Join(dir, "set", ReceiptFile))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "missedData")

	provenOn(t)
	dir, _, _ = replayIn(t, replayCase{mockErrs: misses, mapped: true})
	r := mustReceipt(t, dir)
	require.NotNil(t, r.MissedData)
	require.Equal(t, 1, *r.MissedData, "a DNS lookup is protocol-phase")
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
	return New(zap.NewNop(), instr, stubMockDB{}, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
}

// Under the switch what each test did is keploy's verdict: a replay that
// cannot read it did not complete, and fails saying so.
func TestAReplayThatCannotReadItsTestsFails(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.windowsErr = errors.New("connection reset by peer")
	err := replayErr(t, instr, utils.Native)
	require.ErrorContains(t, err, "keploy could not read what each test did from its agent: connection reset by peer")
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

// Under the switch each test is judged by its mapping: a set without
// per-test mappings is refused, not replayed unjudged.
func TestASetWithoutMappingsIsRefusedUnderTheSwitch(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, stubMockDB{}, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.ErrorContains(t, err, "it has no per-test mappings")
	runs, _ := instr.counts()
	require.Zero(t, runs, "the test command ran unjudged")
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
	err := New(zap.NewNop(), instr, stubMockDB{}, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	require.NoError(t, err)
	runs, _ := instr.counts()
	require.Equal(t, 1, runs)
}
