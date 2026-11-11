package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	require.Empty(t, r.CaptureIncomplete)
	// A run that lost nothing names no such field, in the receipt's file or
	// its JSON (keploy mock status --json): older readers see what they did.
	raw, err := os.ReadFile(filepath.Join(dir, "set", ReceiptFile))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "captureIncomplete")
	j, err := json.Marshal(r)
	require.NoError(t, err)
	require.NotContains(t, string(j), "captureIncomplete")

	provenOn(t)
	on := &lossCounter{after: tls}
	dir, code, logs := replayIn(t, replayCase{losses: on, mapped: true, keployFails: true})
	require.Equal(t, 1, code)
	require.Equal(t, 2, on.reads, "read before and after the test command")
	r = mustReceipt(t, dir)
	require.Equal(t, FailedByCaptureIncomplete, r.FailedBy)
	require.False(t, r.Isolated)
	require.Contains(t, logged(logs), "keploy could not capture every call the test command made")
	require.Equal(t, tls, r.CaptureIncomplete, "the receipt keeps the run's loss as data")

	// A PID map's refusal and another loss in one run: the receipt keeps
	// both as data, the refusal with its own errno, as its note says both.
	refused := models.PIDMapRefusals{Target: 2, LastTargetErr: -7}
	dir, code, _ = replayIn(t, replayCase{losses: &lossCounter{after: tls}, pidMaps: &pidMapCounter{after: refused}, mapped: true, keployFails: true})
	require.Equal(t, 1, code)
	r = mustReceipt(t, dir)
	require.Equal(t, FailedByCaptureIncomplete, r.FailedBy)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 2, What: refused.CaptureIncomplete(), Err: -7}, tls[0]}, r.CaptureIncomplete)
	require.Contains(t, r.IsolationNote, refused.CaptureIncomplete())
	require.Contains(t, r.IsolationNote, "to db:5432")

	// A loss keploy judges, one it cannot capture at all and the kernel's
	// evidence, in one run: only the judged loss fails the run, so only it is
	// the receipt's CaptureIncomplete; the one keploy cannot capture is its
	// CannotCapture alone, and the evidence is neither.
	unix := models.CaptureLoss{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}
	skip := models.CaptureLoss{Kind: models.CaptureLossKernelSkip, Count: 3}
	dir, code, _ = replayIn(t, replayCase{losses: &lossCounter{after: models.CaptureLosses{tls[0], unix, skip}}, mapped: true, keployFails: true})
	require.Equal(t, 1, code)
	r = mustReceipt(t, dir)
	require.Equal(t, FailedByCaptureIncomplete, r.FailedBy)
	require.Equal(t, tls, r.CaptureIncomplete, "only what fails the run")
	require.Equal(t, models.CaptureLosses{unix}, r.CannotCapture)

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

// pidLossReplayInstr is replayIn's runner whose agent counts both the
// processes it could not track and the calls it could not capture.
type pidLossReplayInstr struct {
	*runWrites
	*lossCounter
	*pidMapCounter
}

// Each of the agent's counts that keploy could not read stays on the
// receipt as data, whatever reason the isolation note keeps. The note keeps
// one: a call that matched nothing, a failed test command, a process the
// agent could not track, a capture loss the agent counted, or the PID-map
// count ahead of the capture-loss count. A reader judging a test that
// passed beside another test's misses has no other way to learn the run is
// not proven. Mutation: drop either field, set one only while the run
// still reads isolated, set the capture-loss one only when the PID-map
// count was read or no process was refused, or set the PID-map one only
// when no capture loss failed the run.
func TestAReceiptKeepsEachUnreadCountAsData(t *testing.T) {
	provenOn(t)
	tracking := errors.New("agent returned 500: read pid_map_insert_failures[target_namespace_pids]: bad file descriptor")
	capture := errors.New("connection reset")
	missed := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /x"}}
	failed := models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 1}
	for _, tc := range []struct {
		name                   string
		c                      replayCase
		wantNote               string
		wantTracking, wantLoss bool
		wantExitZero           bool
	}{
		{name: "process tracking unread beside a call that matched nothing",
			c:        replayCase{mockErrs: missed, pidMaps: &pidMapCounter{afterErr: tracking}},
			wantNote: "1 call(s) matched nothing", wantTracking: true},
		{name: "call capture unread beside a call that matched nothing",
			c:        replayCase{mockErrs: missed, pidMaps: &pidMapCounter{}, losses: &lossCounter{errAt: map[int]error{2: capture}}},
			wantNote: "1 call(s) matched nothing", wantLoss: true},
		{name: "both unread, the test command failed",
			c:        replayCase{runExit: failed, pidMaps: &pidMapCounter{afterErr: tracking}, losses: &lossCounter{errAt: map[int]error{2: capture}}},
			wantNote: "the test command failed", wantTracking: true, wantLoss: true},
		{name: "both unread, a clean run: the note keeps process tracking",
			c:        replayCase{pidMaps: &pidMapCounter{afterErr: tracking}, losses: &lossCounter{errAt: map[int]error{2: capture}}},
			wantNote: "could not read whether the agent tracked every process", wantTracking: true, wantLoss: true, wantExitZero: true},
		// A process the agent refused fails the run, and the note names
		// it; the capture-loss count unread is still kept.
		{name: "call capture unread beside a process the agent could not track",
			c:        replayCase{keployFails: true, pidMaps: &pidMapCounter{after: models.PIDMapRefusals{Target: 1}}, losses: &lossCounter{errAt: map[int]error{2: capture}}},
			wantNote: "capture incomplete", wantLoss: true},
		// A capture loss the agent counted fails the run, and the note
		// names it; the PID-map count unread is still kept.
		{name: "process tracking unread beside a capture loss the agent counted",
			c: replayCase{keployFails: true, pidMaps: &pidMapCounter{afterErr: tracking},
				losses: &lossCounter{after: models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}}},
			wantNote: "capture incomplete", wantTracking: true},
		// Controls: both counts read.
		{name: "both read, a call that matched nothing",
			c:        replayCase{mockErrs: missed, pidMaps: &pidMapCounter{}},
			wantNote: "1 call(s) matched nothing"},
		{name: "both read, a clean run", c: replayCase{pidMaps: &pidMapCounter{}}, wantExitZero: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.mapped = true
			dir, code, _ := replayIn(t, tc.c)
			if tc.wantExitZero {
				require.Zero(t, code, "no --strict: an unread count alone fails nothing")
			}
			r := mustReceipt(t, dir)
			if tc.wantNote == "" {
				require.True(t, r.Isolated, r.IsolationNote)
			} else {
				require.False(t, r.Isolated)
				require.Contains(t, r.IsolationNote, tc.wantNote)
			}
			if tc.wantTracking {
				require.Contains(t, r.ProcessTrackingUnread, "could not read whether the agent tracked every process")
				require.Contains(t, r.ProcessTrackingUnread, "bad file descriptor", "why it could not be read")
			} else {
				require.Empty(t, r.ProcessTrackingUnread)
			}
			if tc.wantLoss {
				require.Contains(t, r.CallCaptureUnread, "could not read whether the agent captured every call")
				require.Contains(t, r.CallCaptureUnread, "connection reset", "why it could not be read")
			} else {
				require.Empty(t, r.CallCaptureUnread)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "set", ReceiptFile))
			require.NoError(t, err)
			require.Equal(t, tc.wantTracking, strings.Contains(string(raw), "processTrackingUnread:"), "written to the file, and only when unread")
			require.Equal(t, tc.wantLoss, strings.Contains(string(raw), "callCaptureUnread:"), "written to the file, and only when unread")
		})
	}
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

// A record of two test workers whose tests overlap, with a call from a
// process in no worker's keploy instance made while both were open: a build
// that judges the record keeps the recording, and its per-test file charges
// each open test, and only those, with the call, which leaves them without a
// verdict; a build that does not judge it keeps no recording, and fails.
func TestARecordChargesEachTestOpenAtACallFromNoWorker(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	passed := models.AppError{AppErrorType: models.ErrAppStopped}
	run := func(t *testing.T, appErr models.AppError) (*mockService, string, error) {
		dir := t.TempDir()
		ws := []models.ScopeWindow{mark("orders::TestA", 0, 10*time.Millisecond), mark("orders::TestB", 2*time.Millisecond, 12*time.Millisecond), mark("orders::TestC", 20*time.Millisecond, 30*time.Millisecond)}
		for i, inst := range []uint32{1, 2, 1} {
			ws[i].Outcome, ws[i].Dir, ws[i].Instance = models.ScopeOutcomePassed, "orders", inst
		}
		instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, appErr), windows: ws}
		shared, own := mockAt("shared-0", runnerT0.Add(5*time.Millisecond)), mockAt("own-c", runnerT0.Add(25*time.Millisecond))
		shared.SourcePID, own.Instance = 4242, 1
		instr.mocks = []*models.Mock{shared, own}
		cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 2 -json ./...")
		cfg.Path = dir
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = 0 })
		svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
		return &mockService{config: cfg}, dir, svc.Record(context.Background())
	}
	t.Run("judged", func(t *testing.T) {
		judgedBuild(t)
		ms, dir, err := run(t, passed)
		require.NoError(t, err)
		r, err := ReadRecordTestsReceipt(dir, ms.setName())
		require.NoError(t, err)
		require.NotNil(t, r, "no per-test file")
		charged := map[string]models.CaptureLosses{}
		for _, tr := range r.Tests {
			if tr.Evidence != nil {
				charged[tr.Name] = tr.Evidence.CannotCapture
			}
		}
		want := models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: 1, What: "the latest: mock mock-0, from process 4242"}} // named as recorded
		require.Equal(t, map[string]models.CaptureLosses{"orders::TestA": want, "orders::TestB": want}, charged)
	})
	notJudged := func(t *testing.T, appErr models.AppError, code int) {
		ms, dir, err := run(t, appErr)
		require.ErrorContains(t, err, "1 call(s) came from no test worker while workers' tests were open")
		require.Equal(t, code, utils.ErrCode)
		_, statErr := os.Stat(filepath.Join(dir, ms.setName(), "mocks.yaml"))
		require.True(t, os.IsNotExist(statErr), "the recording was kept: %v", statErr)
		_, statErr = os.Stat(filepath.Join(dir, stagingSetID(ms.setName())))
		require.True(t, os.IsNotExist(statErr), "the staging set was left behind: %v", statErr)
	}
	t.Run("not judged", func(t *testing.T) { notJudged(t, passed, utils.ExitKeployError) })
	// A test command that failed on its own keeps its exit code: keploy's
	// refusal does not hide the command's verdict.
	t.Run("not judged, the test command failed", func(t *testing.T) {
		notJudged(t, models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}, 3)
	})
}

// The run's PID map losses each carry their own map's errno as data, beside
// the run's verdict as words, so a reader of both never takes one map's
// cause for the other's.
func TestTheRunsPIDMapLossesCarryEachMapsOwnErrno(t *testing.T) {
	refused := models.PIDMapRefusals{Target: 4, LastTargetErr: -7, Excluded: 2, LastExcludedErr: -110}
	got, err := (&mockService{}).runLosses(context.Background(), lossBase{checked: true}, nil, nil, refused)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{
		{Kind: models.CaptureLossPIDMap, Count: 4, What: refused.CaptureIncomplete(), Err: -7},
		{Kind: models.CaptureLossPIDMapExcluded, Count: 2, What: refused.CaptureIncomplete(), Err: -110},
	}, got)
}
