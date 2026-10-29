package mock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// lossCounter is an agent's GET /agent/capturelosses: the first read answers
// before, every later one after, or err for the nth read (errAt, from 1).
type lossCounter struct {
	before, after models.CaptureLosses
	errAt         map[int]error
	reads         int
}

func (c *lossCounter) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	c.reads++
	if err := c.errAt[c.reads]; err != nil {
		return nil, err
	}
	if c.reads == 1 {
		return c.before, nil
	}
	return c.after, nil
}

// lossInstr is an Instrumentation whose agent counts capture losses; a nil
// Instrumentation panics on any other call.
type lossInstr struct {
	Instrumentation
	*lossCounter
}

func lossService(c *lossCounter) *mockService {
	return &mockService{logger: zap.NewNop(), instrumentation: lossInstr{nil, c}, config: &config.Config{}}
}

// Under the proven switch a mock run's losses are what grew while the test
// command ran, PID-map refusals left out (their own check reports those);
// with the switch off, or an agent that cannot count, nothing is read.
func TestAMockRunsCaptureLossesAreWhatGrewWhileItRan(t *testing.T) {
	ctx := context.Background()
	tls := models.CaptureLoss{Kind: models.CaptureLossTLSAbort, Count: 3, What: "to 10.0.0.1:443"}
	pid := models.CaptureLoss{Kind: models.CaptureLossPIDMap, Count: 5}

	t.Setenv(provenswitch.Env, "")
	off := &lossCounter{after: models.CaptureLosses{tls}}
	m := lossService(off)
	lost, err := m.captureLossesDuring(ctx, m.captureLossBaseline(ctx))
	require.NoError(t, err)
	require.Empty(t, lost)
	require.Zero(t, off.reads, "nothing is read with the switch off")

	t.Setenv(provenswitch.Env, "1")
	c := &lossCounter{before: models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1}}, after: models.CaptureLosses{tls, pid}}
	m = lossService(c)
	lost, err = m.captureLossesDuring(ctx, m.captureLossBaseline(ctx))
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 2, What: "to 10.0.0.1:443"}}, lost,
		"what grew, PID-map refusals left to their own check")

	unsupported := &lossCounter{errAt: map[int]error{1: fmt.Errorf("%w: 404", models.ErrCaptureLossesUnsupported)}}
	m = lossService(unsupported)
	lost, err = m.captureLossesDuring(ctx, m.captureLossBaseline(ctx))
	require.NoError(t, err)
	require.Empty(t, lost)
	require.Equal(t, 1, unsupported.reads, "an agent that cannot count is asked once and left unchecked")

	for _, at := range []int{1, 2} {
		lostRead := &lossCounter{errAt: map[int]error{at: errors.New("connection reset")}}
		m = lossService(lostRead)
		_, err = m.captureLossesDuring(ctx, m.captureLossBaseline(ctx))
		require.Error(t, err, "a count lost at read %d is an error, never 'nothing lost'", at)
	}

	compose := &lossCounter{after: models.CaptureLosses{tls}}
	m = lossService(compose)
	m.config.CommandType = "docker-compose"
	lost, err = m.captureLossesDuring(ctx, m.captureLossBaseline(ctx))
	require.NoError(t, err)
	require.Empty(t, lost)
	require.Zero(t, compose.reads, "under compose the agent stops with the project: not checked")
}

// A receipt keeps one loss per kind, the latest attempt's (the counts are
// since the run began), but for a test's own kinds (a TLS handshake that
// failed or was abandoned), which each attempt was charged its own of: added.
func TestMergeCaptureLossesKeepsOnePerKind(t *testing.T) {
	a := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 1, What: "a"}, {Kind: models.CaptureLossTLSAbort, Count: 1, What: "a"}, {Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "a"}}
	b := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 2, What: "b"}, {Kind: models.CaptureLossTLSAbort, Count: 2, What: "b"}, {Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "b"}, {Kind: models.CaptureLossDropped, Count: 1}}
	require.Equal(t, models.CaptureLosses{
		{Kind: models.CaptureLossParserStall, Count: 2, What: "b"},
		{Kind: models.CaptureLossTLSAbort, Count: 3, What: "b"},
		{Kind: models.CaptureLossTLSAbandoned, Count: 2, What: "b"},
		{Kind: models.CaptureLossDropped, Count: 1},
	}, mergeCaptureLosses(a, b))
}

// Under the proven switch, a replay during which keploy lost a call fails as
// capture incomplete, as keploy's failure, whatever the tests said; with the
// switch off the same agent's count is not read and the run is what it was.
func TestAReplayThatLostACallFailsUnderTheSwitchOnly(t *testing.T) {
	tls := models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to db:5432"}}

	t.Setenv(provenswitch.Env, "")
	off := &lossCounter{after: tls}
	dir, code, _ := replayIn(t, replayCase{losses: off})
	require.Zero(t, code)
	require.Zero(t, off.reads, "the switch off: not read")
	r := mustReceipt(t, dir)
	require.True(t, r.Isolated)
	require.Empty(t, r.FailedBy)

	provenOn(t)
	on := &lossCounter{after: tls}
	dir, code, logs := replayIn(t, replayCase{losses: on, mapped: true, keployFails: true})
	require.Equal(t, 1, code)
	require.Equal(t, 2, on.reads, "read before and after the test command")
	r = mustReceipt(t, dir)
	require.Equal(t, FailedByCaptureIncomplete, r.FailedBy)
	require.False(t, r.Isolated)
	require.Contains(t, logged(logs), "keploy could not capture every call the test command made")

	// Lost before the test command ran: not this run's.
	before := &lossCounter{before: tls, after: tls}
	dir, code, _ = replayIn(t, replayCase{losses: before, mapped: true})
	require.Zero(t, code)
	require.Empty(t, mustReceipt(t, dir).FailedBy)
}

// Under the proven switch a recording during which keploy lost a call is not
// kept; the existing set is left as it was.
func TestARecordingThatLostACallIsNotKeptUnderTheSwitch(t *testing.T) {
	const set = "orders"
	tls := models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 2, What: "to db:3306"}}
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint("switch ", on), func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			core, logs := observer.New(zapcore.DebugLevel)
			db := newStatefulMockDB()
			db.sets[set] = []string{"old-0"}
			c := &lossCounter{after: tls}
			inst := lossRecordInstr{&drainInstrumentation{out: make(chan *models.Mock, 4), emitAfter: 5 * time.Millisecond}, c}
			cfg := &config.Config{}
			cfg.Mock.Name = set
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			err := New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
			got := db.get(set)
			if !on {
				require.NoError(t, err)
				require.Zero(t, utils.ErrCode)
				require.Len(t, got, 2, "kept")
				require.Zero(t, c.reads)
				return
			}
			require.ErrorContains(t, err, "the recording is incomplete")
			require.Equal(t, utils.ExitKeployError, utils.ErrCode)
			require.Equal(t, []string{"old-0"}, got, "the old recording untouched")
			require.Empty(t, db.get(stagingSetID(set)), "nothing left staged")
			require.Contains(t, logged(logs), "the recording was not kept: keploy could not capture every call")
		})
	}
}

// lossReplayInstr is replayIn's runner (every optional extension it has kept)
// whose agent counts capture losses.
type lossReplayInstr struct {
	*runWrites
	*lossCounter
}

// lossRecordInstr is drainInstrumentation (it captures mock-0 and mock-1)
// whose agent counts capture losses, and answers only while the recording's
// stream is open (an agent being stopped too holds its server only that long).
type lossRecordInstr struct {
	*drainInstrumentation
	*lossCounter
}

func (l lossRecordInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	if c := l.drainInstrumentation.captureCtx; c != nil && c.Err() != nil {
		return nil, errors.New("the agent stopped with the recording's stream")
	}
	return l.lossCounter.GetCaptureLosses(ctx)
}

// Under the switch, a capture-loss count that could not be read after the
// test command ran leaves the replay unverified, said as what it is: not a
// count of processes.
func TestAReplayWhoseLossCountIsLostIsUnverified(t *testing.T) {
	provenOn(t)
	lost := &lossCounter{errAt: map[int]error{2: errors.New("connection reset")}}
	dir, code, logs := replayIn(t, replayCase{losses: lost, mapped: true})
	require.Zero(t, code, "no --strict: an unverified replay passes")
	r := mustReceipt(t, dir)
	require.False(t, r.Isolated)
	require.Contains(t, r.IsolationNote, "could not read whether the agent captured every call")
	require.Contains(t, logged(logs), "keploy could not read whether the agent captured every call the test command made")
	require.NotContains(t, logged(logs), "tracked every process")
}

// Decision 20: a test command that called what keploy cannot capture at all
// (a Unix socket) is not failed for it. The replay is unverified: it passes,
// and its receipt does not certify it isolated (--strict is refused under the
// switch). A recording is kept, with a WARN.
func TestACallKeployCannotCaptureLeavesAMockRunUnverified(t *testing.T) {
	provenOn(t)
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}

	dir, code, logs := replayIn(t, replayCase{losses: &lossCounter{after: db}, mapped: true})
	require.Zero(t, code)
	r := mustReceipt(t, dir)
	require.False(t, r.Isolated)
	require.Empty(t, r.FailedBy)
	require.Contains(t, r.IsolationNote, "keploy cannot capture")
	require.Equal(t, db, r.CannotCapture, "the receipt says so as data, not text only")
	require.Contains(t, logged(logs), "the test command called what keploy cannot capture")

	// A real loss beside it takes the note; the data still says what keploy
	// cannot capture.
	tls := models.CaptureLoss{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}
	dir, _, _ = replayIn(t, replayCase{losses: &lossCounter{after: append(models.CaptureLosses{tls}, db...)}, mapped: true, keployFails: true})
	r = mustReceipt(t, dir)
	require.NotContains(t, r.IsolationNote, "keploy cannot capture")
	require.Contains(t, r.IsolationNote, "TLS handshake")
	require.Equal(t, db, r.CannotCapture)
}

func TestACallKeployCannotCaptureLeavesARecordingKept(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}
	const set = "orders"
	core, rlogs := observer.New(zapcore.DebugLevel)
	mdb := newStatefulMockDB()
	mdb.sets[set] = []string{"old-0"}
	inst := lossRecordInstr{&drainInstrumentation{out: make(chan *models.Mock, 4), emitAfter: 5 * time.Millisecond}, &lossCounter{after: db}}
	cfg := &config.Config{}
	cfg.Mock.Name = set
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.New(core), inst, mdb, nil, FileStore{}, nil, cfg).Record(context.Background()))
	require.Zero(t, utils.ErrCode)
	require.Len(t, mdb.get(set), 2, "kept")
	require.Contains(t, logged(rlogs), "the recording is kept, but the test command called what keploy cannot capture")
}

// GetScopeWindows: the run reported no test (a recording under the switch
// reads them).
func (lossRecordInstr) GetScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	return nil, nil
}

// A test whose only evidence is a call keploy cannot capture at all keeps it on
// its receipt line (NotJudged, latest count per kind), so the line reads as
// not judged, never as keploy's failure with no call to name.
func TestAReceiptKeepsWhatLeftATestNotJudged(t *testing.T) {
	db := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: n, What: "/var/run/postgresql/.s.PGSQL.5432"}}
	}
	got := withEvidence([]TestReceipt{{Name: "o::A", Outcome: "passed"}}, []models.ScopeWindow{
		{Name: "o::A", Evidence: &models.ScopeEvidence{Reason: "keploy: not judged", CannotCapture: db(1)}},
		{Name: "o::A", Evidence: &models.ScopeEvidence{Reason: "keploy: not judged", CannotCapture: db(2)}},
	})
	require.NotNil(t, got[0].Evidence)
	require.Equal(t, db(2), got[0].Evidence.CannotCapture)
	require.False(t, got[0].Evidence.Empty(), "a line with only its reason reads as keploy's failure")
}

// runnerLossInstr is a test-runner record whose agent counts capture losses.
type runnerLossInstr struct {
	*runnerInstr
	*lossCounter
}

// A judged record's per-test file carries what the recording lacks because
// keploy cannot capture it at all (TestsReceipt.CannotCapture), so whatever
// judges the recording does not take it as fully captured.
func TestARecordsTestsFileCarriesWhatKeployCannotCapture(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	judgedBuild(t)
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}
	dir := t.TempDir()
	w := mark("orders::TestPure", 0, 10*time.Millisecond)
	w.Outcome, w.Dir = models.ScopeOutcomePassed, "orders"
	instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
		windows: []models.ScopeWindow{w}}
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = dir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), runnerLossInstr{instr, &lossCounter{after: db}}, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	require.NoError(t, svc.Record(context.Background()))
	set := (&mockService{config: cfg}).setName()
	r, err := ReadRecordTestsReceipt(dir, set)
	require.NoError(t, err)
	require.NotNil(t, r, "no per-test file")
	require.Equal(t, db, r.CannotCapture)
}

// A passed test whose window a set-aside test's recording answered keeps the
// serve on its receipt line, each attempt's, named with that test; the line
// stays passed: a serve is never a failure (ScopeEvidence.ServedAside is not
// part of Empty).
func TestAReceiptNamesWhatASetAsideTestServed(t *testing.T) {
	got := withEvidence([]TestReceipt{{Name: "o::K", Outcome: "passed"}}, []models.ScopeWindow{
		{Name: "o::K", Evidence: &models.ScopeEvidence{ServedAside: []string{models.ServedAsideLine("mock-194", "o::TestAppCRUD")}}},
		{Name: "o::K", Evidence: &models.ScopeEvidence{ServedAside: []string{models.ServedAsideLine("mock-195", "o::TestAppCRUD")}}},
	})
	require.NotNil(t, got[0].Evidence)
	require.Equal(t, []string{"mock-194 served from o::TestAppCRUD (set aside)", "mock-195 served from o::TestAppCRUD (set aside)"}, got[0].Evidence.ServedAside)
	require.True(t, got[0].Evidence.Empty(), "a serve fails nothing")
	require.Equal(t, "passed", got[0].Outcome)
}
