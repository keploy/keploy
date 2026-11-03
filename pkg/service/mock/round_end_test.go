package mock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// markerAgent is an agent that ends a round's streams on request, each with
// its marker after all it had (models.AgentBacklog.EndMarkers), and that says
// at every look that it holds nothing more from before the run ended. Its
// last mock from before the run ended is then still on its way to this
// client: handed on, so not counted (clear is not delivered), it arrives only
// after transit -- a CPU-starved agent's encoder, a full socket.
type markerAgent struct {
	Instrumentation // embedded: nil, so an unexpected call panics loudly

	out        chan *models.Mock
	captureCtx context.Context
	transit    time.Duration
	// endErr is what the request to end the streams returns; markerless
	// ends the stream on that request without its marker (the agent went
	// away); goneAtEnd, when set, is an agent that went away as it was
	// asked: the request is refused with it and the stream ends without its
	// marker; oldAgent
	// predates markers, and never says it can end its streams; notStarted is
	// an agent this client did not start (compose, kubelet or the user runs
	// it).
	endErr     error
	markerless bool
	goneAtEnd  error
	oldAgent   bool
	notStarted bool
	gone       chan struct{}

	endOnce  sync.Once
	endReq   chan struct{}
	ended    atomic.Bool
	endCalls atomic.Int32
	token    atomic.Value
	wg       sync.WaitGroup
}

func newMarkerAgent(transit time.Duration) *markerAgent {
	return &markerAgent{out: make(chan *models.Mock, 4), transit: transit, endReq: make(chan struct{}), gone: make(chan struct{})}
}

func (a *markerAgent) StartedAgent() bool { return !a.notStarted }

func (a *markerAgent) Setup(context.Context, string, models.SetupOptions) error { return nil }
func (a *markerAgent) NotifyGracefulShutdown(context.Context) error             { return nil }

func (a *markerAgent) GetOutgoing(ctx context.Context, _ models.OutgoingOptions) (<-chan *models.Mock, error) {
	a.captureCtx = ctx
	return a.out, nil
}

func (a *markerAgent) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogClear, EndMarkers: !a.oldAgent}, nil
}

func (a *markerAgent) EndRecordingStreams(_ context.Context, token string) error {
	a.endCalls.Add(1)
	a.token.Store(token)
	if a.goneAtEnd != nil {
		a.endOnce.Do(func() { close(a.gone) })
		return a.goneAtEnd
	}
	if a.endErr != nil {
		return a.endErr
	}
	a.endOnce.Do(func() { close(a.endReq) })
	return nil
}

func (a *markerAgent) StreamEnded(kind models.FrameStream) bool {
	return kind == models.MockStream && a.ended.Load()
}

func (a *markerAgent) Run(context.Context, models.RunOptions) models.AppError {
	captured := time.Now()
	ctx := a.captureCtx
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.out)
		// On its way: it reaches this client after transit, or never once
		// the client has ended the stream (the real reader's request is
		// aborted, and what it had not read is gone).
		select {
		case <-time.After(a.transit):
		case <-ctx.Done():
			return
		}
		select {
		case a.out <- &models.Mock{Name: "on-its-way", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: captured}}:
		case <-ctx.Done():
			return
		}
		// A live agent keeps the stream open until it is asked to end it --
		// then all it had, here the mock above, and its marker -- or the
		// client ends it.
		select {
		case <-a.endReq:
			if !a.markerless {
				a.ended.Store(true)
			}
		case <-a.gone:
		case <-ctx.Done():
		}
	}()
	return models.AppError{}
}

// recordWithMarkerAgent records with a, and returns what was saved, whether
// the set was promoted, the error, the exit code and the logs.
func recordWithMarkerAgent(t *testing.T, a *markerAgent) ([]string, bool, error, int, *observer.ObservedLogs) {
	t.Helper()
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "marker-test"
	core, logs := observer.New(zapcore.DebugLevel)
	err := New(zap.New(core), a, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	a.wg.Wait()
	return db.inserted(), db.promoted.Load(), err, utils.ErrCode, logs
}

// A mock the agent had handed on, and this client had not yet read, when the
// agent said it held nothing more from before the run ended ("clear") is
// saved: the round ends on the agent's marker, not when this client ends the
// stream after the drain's quiet, which a mock still on its way outlasts
// under load. It used to be lost, silently, the run passing.
func TestRecord_AMockStillOnItsWayWhenTheAgentSaysClearIsSaved(t *testing.T) {
	a := newMarkerAgent(3 * mockDrainQuiet)
	got, promoted, err, code, logs := recordWithMarkerAgent(t, a)
	require.NoError(t, err)
	require.Zero(t, code)
	require.Equal(t, []string{"on-its-way"}, got, "the mock on its way when the agent said clear was not saved")
	require.True(t, promoted)
	require.EqualValues(t, 1, a.endCalls.Load(), "the agent was not asked once to end the round's streams")
	tok := models.StreamEndToken(a.captureCtx)
	require.NotEmpty(t, tok, "the round's stream carried no token for the agent to end it by")
	require.Equal(t, tok, a.token.Load(), "the agent was asked to end streams of another token than the round's")
	require.Equal(t, 1, logs.FilterMessage("read the stream to the keploy agent's end marker").Len())
	require.Zero(t, logs.FilterMessage(quietTailUnconfirmed).Len(), "a round read to the agent's marker was said to be unconfirmed")
}

// Each round has its own token, and asks the agent to end the streams of
// that one.
func TestRecord_EachRoundEndsItsOwnStreams(t *testing.T) {
	first, second := newMarkerAgent(time.Millisecond), newMarkerAgent(time.Millisecond)
	recordWithMarkerAgent(t, first)
	recordWithMarkerAgent(t, second)
	a, b := models.StreamEndToken(first.captureCtx), models.StreamEndToken(second.captureCtx)
	require.NotEmpty(t, a)
	require.NotEqual(t, a, b, "two rounds opened their streams with one token")
	require.Equal(t, a, first.token.Load(), "the first round asked the agent to end another round's streams")
	require.Equal(t, b, second.token.Load(), "the second round asked the agent to end another round's streams")
}

// The agent took the request to end the round's streams and then ended the
// mock stream without its marker: it went away (stopped, crashed) before it
// had put on it all it had. The round is incomplete: said at ERROR, the run
// fails with keploy's exit code, and the set is not saved over the existing
// one.
func TestRecord_AStreamThatClosesBeforeItsMarkerFailsTheRound(t *testing.T) {
	a := newMarkerAgent(time.Millisecond)
	a.markerless = true
	_, promoted, err, code, logs := recordWithMarkerAgent(t, a)
	require.Error(t, err)
	require.Equal(t, utils.ExitKeployError, code)
	require.False(t, promoted, "an incomplete round was promoted over the existing set")
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("ended its stream before its end marker").Len(), "%v", logs.All())
}

// An agent that answers but does not take the request to end its streams
// leaves them for this client to end, as before markers: the run is not
// failed for it, and it is said at WARN, a mock on its way being lost.
func TestRecord_AnEndRequestTheAgentDoesNotTakeEndsTheRoundAsBefore(t *testing.T) {
	a := newMarkerAgent(time.Hour) // never arrives: the stream is ended here
	a.endErr = errors.New("the agent answered 500 Internal Server Error")
	got, _, err, code, logs := recordWithMarkerAgent(t, a)
	require.NoError(t, err)
	require.Zero(t, code)
	require.Empty(t, got)
	require.EqualValues(t, 1, a.endCalls.Load())
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("did not take the request to end its streams").Len(), "%v", logs.All())
}

// An agent this client started that went away as it was asked to end the
// round -- its port refuses the request, and its stream ends without its
// marker -- left the round before it was through: the run fails whichever of
// the two this client sees first, never at random.
func TestRecord_AnAgentGoneAtTheEndRequestFailsTheRound(t *testing.T) {
	for i := 0; i < 4; i++ {
		a := newMarkerAgent(time.Millisecond)
		a.goneAtEnd = refusedErr(t)
		_, promoted, err, code, logs := recordWithMarkerAgent(t, a)
		require.Error(t, err, "run %d", i)
		require.Equal(t, utils.ExitKeployError, code, "run %d", i)
		require.False(t, promoted, "run %d: an incomplete round was promoted over the existing set", i)
		require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("ended its stream before its end marker").Len(), "run %d: %v", i, logs.All())
	}
}

// An agent this client did not start (compose, kubelet or the user runs and
// stops it) that ends its stream before its marker is said at WARN, as the
// drain says such an end: the run is not failed for it.
func TestRecord_AnAgentThisClientDidNotStartEndingBeforeItsMarkerIsAWarning(t *testing.T) {
	a := newMarkerAgent(time.Millisecond)
	a.markerless, a.notStarted = true, true
	_, promoted, err, code, logs := recordWithMarkerAgent(t, a)
	require.NoError(t, err)
	require.Zero(t, code)
	require.True(t, promoted)
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("ended its stream before its end marker").Len(), "%v", logs.All())
	require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "%v", logs.All())
}

// An agent that predates markers is never asked to end the streams: the
// round ends as it did, on the drain's quiet and the agent's clear. A mock
// still on its way then is lost unseen, so the saved recording is said at
// WARN to be unconfirmed.
func TestRecord_AnAgentWithoutMarkersIsNotAskedToEndTheRound(t *testing.T) {
	a := newMarkerAgent(time.Millisecond)
	a.oldAgent = true
	_, promoted, err, code, logs := recordWithMarkerAgent(t, a)
	require.NoError(t, err)
	require.Zero(t, code)
	require.True(t, promoted)
	require.Zero(t, a.endCalls.Load())
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(quietTailUnconfirmed).Len(), "%v", logs.All())
}

// kindEnder is an agent client whose end request is taken, and whose streams
// of the kinds in ended ended with their markers.
type kindEnder struct{ ended map[models.FrameStream]bool }

func (kindEnder) EndRecordingStreams(context.Context, string) error { return nil }
func (k kindEnder) StreamEnded(kind models.FrameStream) bool        { return k.ended[kind] }

// always is a strictness that does not change.
func always(strict bool) func() bool { return func() bool { return strict } }

func closed() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// Every stream of the round is read to its marker: the test-case stream of
// --record-requests as much as the mock stream. One that closed without it
// fails a strict round; under compose, where the agent goes with the project
// as before markers, it does not.
func TestEndRound_EveryStreamEndsOnItsMarker(t *testing.T) {
	streams := []roundStream{{kind: models.MockStream, done: closed()}, {kind: models.TestCaseStream, done: closed()}}
	for _, tc := range []struct {
		name   string
		ended  map[models.FrameStream]bool
		strict bool
		lost   bool
	}{
		{"both marked", map[models.FrameStream]bool{models.MockStream: true, models.TestCaseStream: true}, true, false},
		{"test cases unmarked", map[models.FrameStream]bool{models.MockStream: true}, true, true},
		{"mocks unmarked", map[models.FrameStream]bool{models.TestCaseStream: true}, true, true},
		{"unmarked, an agent this client does not stop", map[models.FrameStream]bool{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
			utils.ErrCode = 0
			core, logs := observer.New(zapcore.WarnLevel)
			m := &mockService{logger: zap.New(core)}
			lost, refused := m.endRound(&roundEnd{ender: kindEnder{tc.ended}, token: "t", endable: true}, always(tc.strict), streams, recordDrainWords)
			require.NoError(t, refused)
			require.Equal(t, tc.lost, lost)
			unmarked := 0
			for _, s := range streams {
				if !tc.ended[s.kind] {
					unmarked++
				}
			}
			switch {
			case tc.lost:
				require.Equal(t, utils.ExitKeployError, utils.ErrCode)
				require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "%v", logs.All())
			default:
				require.Zero(t, utils.ErrCode)
				require.Equal(t, unmarked, logs.FilterLevelExact(zapcore.WarnLevel).Len(), "each stream that ended before its marker is said at WARN: %v", logs.All())
				require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "%v", logs.All())
			}
		})
	}
}

// blockedEnder is an agent client whose end request is taken, but whose
// streams never end.
type blockedEnder struct{}

func (blockedEnder) EndRecordingStreams(context.Context, string) error { return nil }
func (blockedEnder) StreamEnded(models.FrameStream) bool               { return false }

// No clock ends a round waiting on its markers: it says what it is still
// doing, and, once the run was interrupted, that another interrupt stops it
// now; the next one does, and the round is lost.
func TestEndRound_TheUsersNextInterruptEndsIt(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted() // the stop
	core, logs := observer.New(zapcore.InfoLevel)
	m := &mockService{logger: zap.New(core)}
	lost := make(chan bool, 1)
	go func() {
		lost <- noRefusal(m.endRound(&roundEnd{ender: blockedEnder{}, token: "t", endable: true}, always(true),
			[]roundStream{{kind: models.MockStream, done: make(chan struct{})}}, recordDrainWords))
	}()
	offered := recordDrainWords.saving + "; " + recordDrainWords.stopNow
	within := time.After(time.Minute) // only turns a hang into a failure
	for logs.FilterMessage(offered).Len() == 0 {
		select {
		case l := <-lost:
			t.Fatalf("the round ended (lost=%v) before it offered to stop now: %v", l, logs.All())
		case <-within:
			t.Fatalf("the round never offered to stop now: %v", logs.All())
		case <-time.After(time.Millisecond):
		}
	}
	utils.MarkInterruptedAgain() // the user, as told
	select {
	case l := <-lost:
		require.True(t, l, "a round the user ended was not lost")
	case <-within:
		t.Fatal("the user's interrupt after the offer did not end the round")
	}
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(recordDrainWords.gaveUp).Len())
}

// goneEnder is an agent client whose agent died as it was asked to end the
// round: the request is refused. Each stream of the kinds in ended had
// already been sent its marker. refused is the refusal (refusedErr).
type goneEnder struct {
	ended   map[models.FrameStream]bool
	refused error
}

func (g goneEnder) EndRecordingStreams(context.Context, string) error { return g.refused }
func (g goneEnder) StreamEnded(kind models.FrameStream) bool          { return g.ended[kind] }

// An agent this client started that can no longer be reached to ask has
// died: its stream is judged by its marker, whether it closed before the
// refused request or after it -- lost without one, through with one (a
// marker the agent sent before it went counts).
func TestEndRound_AnAgentGoneIsJudgedByItsMarkersInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		closeBefore bool
		marked      bool
		lost        bool
	}{
		{"closed before the refusal, no marker", true, false, true},
		{"closed after the refusal, no marker", false, false, true},
		{"closed after the refusal, its marker read", false, true, false},
		{"closed before the refusal, its marker read", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
			utils.ErrCode = 0
			done := make(chan struct{})
			if tc.closeBefore {
				close(done)
			}
			core, logs := observer.New(zapcore.DebugLevel)
			m := &mockService{logger: zap.New(core)}
			lost := make(chan bool, 1)
			ender := goneEnder{map[models.FrameStream]bool{models.MockStream: tc.marked}, refusedErr(t)}
			go func() {
				lost <- noRefusal(m.endRound(&roundEnd{ender: ender, token: "t", endable: true}, always(true),
					[]roundStream{{kind: models.MockStream, done: done}}, recordDrainWords))
			}()
			if !tc.closeBefore {
				// The round has taken the refusal, its stream still open:
				// it says so, and goes on waiting, not ending on it.
				within := time.After(time.Minute) // only turns a hang into a failure
				for logs.FilterMessageSnippet("could not reach the keploy agent to end its streams").Len() == 0 {
					select {
					case l := <-lost:
						t.Fatalf("the round ended (lost=%v) on the refusal, before its stream closed", l)
					case <-within:
						t.Fatal("the round never took the refusal")
					case <-time.After(time.Millisecond):
					}
				}
				close(done)
			}
			select {
			case l := <-lost:
				require.Equal(t, tc.lost, l)
			case <-time.After(time.Minute):
				t.Fatal("the round did not end once its stream closed")
			}
			said := func(l zapcore.Level) int { return logs.FilterLevelExact(l).Len() }
			if tc.lost {
				require.Equal(t, utils.ExitKeployError, utils.ErrCode)
				require.Equal(t, 1, said(zapcore.ErrorLevel), "%v", logs.All())
			} else {
				require.Zero(t, utils.ErrCode)
				require.Zero(t, said(zapcore.WarnLevel)+said(zapcore.ErrorLevel), "%v", logs.All())
			}
		})
	}
}

// stopAtEnd is an agent client whose end request is taken, and that runs
// stop as it is asked: the user's stop reaching keploy and its agent at
// that moment, which ends the stream without its marker.
type stopAtEnd struct {
	stop func()
	done chan struct{}
}

func (e stopAtEnd) EndRecordingStreams(context.Context, string) error {
	e.stop()
	close(e.done)
	return nil
}
func (stopAtEnd) StreamEnded(models.FrameStream) bool { return false }

// Whether the round is strict is asked as each stream is judged, not once
// as it starts: a stop the user makes while the round ends (keploy and its
// agent both reached) is the user's, the run ending unjudged, and not the
// agent crashing, which would fail it with keploy's code.
func TestEndRound_StrictIsAskedWhenAStreamIsJudged(t *testing.T) {
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	var stopped atomic.Bool
	ender := stopAtEnd{stop: func() { stopped.Store(true) }, done: make(chan struct{})}
	core, logs := observer.New(zapcore.WarnLevel)
	m := &mockService{logger: zap.New(core)}
	lost, refused := m.endRound(&roundEnd{ender: ender, token: "t", endable: true}, func() bool { return !stopped.Load() },
		[]roundStream{{kind: models.TestCaseStream, done: ender.done}}, replayDrainWords)
	require.NoError(t, refused)
	require.False(t, lost, "a stream ended by the user's stop was taken for the agent crashing")
	require.Zero(t, utils.ErrCode)
	require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "%v", logs.All())
}

// A stop-now the user already took, at an earlier read's offer, is not
// waited out on the agent's markers: the streams are ended here, as before
// markers, said at WARN, and the round is not lost for it (the user gave up
// waiting, not the recording).
func TestEndRound_AStopNowTakenBeforeIsNotWaitedOut(t *testing.T) {
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted() // the stop
	utils.OfferStopNow()
	utils.MarkInterruptedAgain() // the user's answer to the earlier offer
	core, logs := observer.New(zapcore.WarnLevel)
	m := &mockService{logger: zap.New(core)}
	lost := make(chan bool, 1)
	go func() {
		lost <- noRefusal(m.endRound(&roundEnd{ender: blockedEnder{}, token: "t", endable: true}, always(true),
			[]roundStream{{kind: models.MockStream, done: make(chan struct{})}}, recordDrainWords))
	}()
	select {
	case l := <-lost:
		require.False(t, l, "a round ended here at the user's earlier stop-now was taken for lost")
	case <-time.After(time.Minute):
		t.Fatal("a round waited on its markers after the user had already said to stop waiting")
	}
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("not waiting for the keploy agent").Len(), "%v", logs.All())
}

// markerRunner is a recording with --record-requests whose agent ends both
// its streams on request, with their markers, and says "clear" while the
// app's last request, recorded as a test case, is still on its way to this
// client. incomingMarkerless ends the test-case stream without its marker.
type markerRunner struct {
	*runnerInstr
	transit            time.Duration
	tc                 *models.TestCase
	incomingMarkerless bool
	endOnce            sync.Once
	endReq             chan struct{}
	mocksEnded         atomic.Bool
	casesEnded         atomic.Bool
}

func (r *markerRunner) StartedAgent() bool { return true }

func (r *markerRunner) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	if _, err := r.composeInstr.GetOutgoing(ctx, opts); err != nil {
		return nil, err
	}
	out := make(chan *models.Mock)
	go func() {
		defer close(out)
		for _, mk := range r.mocks {
			select {
			case out <- mk:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-r.endReq:
			r.mocksEnded.Store(true)
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func (r *markerRunner) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() {
		defer close(out)
		select {
		case <-time.After(r.transit):
		case <-ctx.Done():
			return
		}
		select {
		case out <- r.tc:
		case <-ctx.Done():
			return
		}
		select {
		case <-r.endReq:
			if !r.incomingMarkerless {
				r.casesEnded.Store(true)
			}
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func (r *markerRunner) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogClear, EndMarkers: true}, nil
}

func (r *markerRunner) EndRecordingStreams(context.Context, string) error {
	r.endOnce.Do(func() { close(r.endReq) })
	return nil
}

func (r *markerRunner) StreamEnded(kind models.FrameStream) bool {
	switch kind {
	case models.MockStream:
		return r.mocksEnded.Load()
	case models.TestCaseStream:
		return r.casesEnded.Load()
	}
	return false
}

// promotingMockDB is stubMockDB that remembers whether the staged set was
// promoted over the existing one.
type promotingMockDB struct {
	stubMockDB
	promoted atomic.Bool
}

func (p *promotingMockDB) PromoteStagedSet(context.Context, string, string) error {
	p.promoted.Store(true)
	return nil
}

// recordRequestsWithMarkers records with --record-requests against r, and
// returns whether the set was promoted, the logs, and the error.
func recordRequestsWithMarkers(t *testing.T, r *markerRunner, db *memTestDB) (bool, *observer.ObservedLogs, error) {
	t.Helper()
	cfg := instrConfig(r.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = t.TempDir()
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zapcore.WarnLevel)
	mdb := &promotingMockDB{}
	svc := New(zap.New(core), r, mdb, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	err := svc.Record(context.Background())
	return mdb.promoted.Load(), logs, err
}

// With --record-requests the test-case stream ends on its marker too: the
// app's last request, still on its way when the agent said "clear", is
// stored, not lost to the stream's close.
func TestRecord_ARequestStillOnItsWayIsStoredWithRecordRequests(t *testing.T) {
	r := &markerRunner{runnerInstr: newRunnerInstr(t, sequentialMarks()...), transit: 3 * mockDrainQuiet, endReq: make(chan struct{}),
		tc: caseAt("test-1", runnerT0.Add(5*time.Millisecond))}
	r.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	db := &memTestDB{}
	promoted, _, err := recordRequestsWithMarkers(t, r, db)
	require.NoError(t, err)
	require.True(t, promoted)
	require.Equal(t, []string{"test-1"}, db.inserted, "the request on its way when the agent said clear was not stored")
}

// A test-case stream that ends before its marker, from an agent this client
// started, leaves the round incomplete: the run fails with keploy's code.
func TestRecord_ATestCaseStreamThatClosesBeforeItsMarkerFailsTheRound(t *testing.T) {
	r := &markerRunner{runnerInstr: newRunnerInstr(t, sequentialMarks()...), transit: time.Millisecond, endReq: make(chan struct{}),
		tc: caseAt("test-1", runnerT0.Add(5*time.Millisecond)), incomingMarkerless: true}
	r.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	promoted, logs, err := recordRequestsWithMarkers(t, r, &memTestDB{})
	require.Error(t, err)
	require.Equal(t, utils.ExitKeployError, utils.ErrCode)
	require.False(t, promoted, "an incomplete round was promoted over the existing set")
	said := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("ended its stream before its end marker").FilterField(zap.String("stream", string(models.TestCaseStream)))
	require.Equal(t, 1, said.Len(), "the ERROR does not name the test-case stream: %v", logs.All())
}

// markerIncoming is a replay whose agent ends its request stream on request,
// with its marker, and says "clear" at every look while the app's last
// request is still on its way to this client.
type markerIncoming struct {
	*runnerInstr
	transit time.Duration
	tc      *models.TestCase
	// markerless ends the stream on request without its marker; started is
	// an agent this client started.
	markerless bool
	started    bool
	endOnce    sync.Once
	endReq     chan struct{}
	ended      atomic.Bool
	endCalls   atomic.Int32
}

func (l *markerIncoming) StartedAgent() bool { return l.started }

func (l *markerIncoming) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() {
		defer close(out)
		select {
		case <-time.After(l.transit):
		case <-ctx.Done():
			return
		}
		select {
		case out <- l.tc:
		case <-ctx.Done():
			return
		}
		select {
		case <-l.endReq:
			if !l.markerless {
				l.ended.Store(true)
			}
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func (l *markerIncoming) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogClear, EndMarkers: true}, nil
}

func (l *markerIncoming) EndRecordingStreams(context.Context, string) error {
	l.endCalls.Add(1)
	l.endOnce.Do(func() { close(l.endReq) })
	return nil
}

func (l *markerIncoming) StreamEnded(kind models.FrameStream) bool {
	return kind == models.TestCaseStream && l.ended.Load()
}

// noRefusal is endRound's lost, for a round whose agent never answers the
// end request with a refusal (it takes it, blocks, or is gone): one that did
// is this test's own failure, loud even in the goroutine it runs in.
func noRefusal(lost bool, refused error) bool {
	if refused != nil {
		panic(fmt.Sprintf("an end request the agent took, or could not answer, was taken for refused: %v", refused))
	}
	return lost
}

// A replay compares the app's last request, still on its way to this client
// when the agent said it held nothing more from before the run ended: its
// watch ends on the agent's marker, not on the drain's quiet, which it
// outlasted.
func TestReplayComparesARequestStillOnItsWayWhenTheAgentSaysClear(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	late := &markerIncoming{runnerInstr: instr, transit: 3 * mockDrainQuiet, endReq: make(chan struct{}),
		tc: httpCase("", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0.Add(5*time.Millisecond))}
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	db := &memTestDB{existing: []*models.TestCase{httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0)}}
	var got ReplayOutcome
	RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { got = o })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), late, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	require.NoError(t, svc.Replay(context.Background()))
	require.Len(t, got.Cases, 1)
	require.NotNil(t, got.Cases[0].Actual, "the request on its way when the agent said clear was not compared")
	require.True(t, got.Cases[0].Passed)
	require.EqualValues(t, 1, late.endCalls.Load(), "the agent was not asked once to end the watch's stream")
}

// A replay whose agent, started by this client, ended its request stream
// before its marker compared part of the run: it fails as keploy's, not the
// suite's -- the receipt says so, and does not certify the run as isolated --
// and no verdict on part of it passes.
func TestReplayShortOfRequestsTheAgentCapturedFailsAsKeploys(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	late := &markerIncoming{runnerInstr: instr, transit: time.Millisecond, endReq: make(chan struct{}), markerless: true, started: true,
		tc: httpCase("", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0.Add(5*time.Millisecond))}
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	db := &memTestDB{existing: []*models.TestCase{httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0)}}
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), late, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	reported := false
	RegisterReplayOutcomeReporter(func(context.Context, ReplayOutcome) { reported = true })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	err := svc.Replay(context.Background())
	require.False(t, reported, "a run short of the requests the agent captured was reported, a lost request as one the app did not make")
	require.Error(t, err, "a replay short of the requests the agent captured was not keploy's failure")
	require.Contains(t, err.Error(), "could not read every request")
	require.Equal(t, utils.ExitKeployError, utils.ErrCode)
	receipt, rerr := ReadReceipt(dir, "set")
	require.NoError(t, rerr)
	require.Equal(t, FailedByKeploy, receipt.FailedBy)
	require.False(t, receipt.Isolated, "a run short of requests was certified isolated")
	requireReported(t, receipt)
}

// requireReported checks a replay that fails as keploy's for what it
// compares went on to read and report the run (its receipt has the run's
// counts), not that it gave up before (a receipt of -1 counts).
func requireReported(t *testing.T, receipt *Receipt) {
	t.Helper()
	require.Equal(t, []int{0, 0, 0}, []int{receipt.Loaded, receipt.Consumed, receipt.Missed}, "the run was given up before it was read, not reported short: %+v", receipt)
}

// quietStream is a replay's agent that hands over the app's request, then
// holds its request stream open until this client ends it, as a live agent
// does. It cannot be asked what it still holds, nor to end its stream: it
// gives the drain no word that ends it.
type quietStream struct {
	*runnerInstr
	tc *models.TestCase
}

func (q quietStream) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() {
		defer close(out)
		select {
		case out <- q.tc:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return out, nil
}

// quietIncoming is a quietStream whose agent answers every look with says,
// and the request to end its streams with endErr.
type quietIncoming struct {
	quietStream
	says     models.AgentBacklog
	endErr   error
	endCalls *atomic.Int32
}

func (q quietIncoming) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return q.says, nil
}

func (q quietIncoming) EndRecordingStreams(context.Context, string) error {
	q.endCalls.Add(1)
	return q.endErr
}

func (quietIncoming) StreamEnded(models.FrameStream) bool { return false }

// unconfirmedReplay replays the set "set" (TestA's one recorded request)
// with agent, its requests compared (--record-requests), under cmdType: what
// it said, whether it reported an outcome, its receipt, and what Replay
// returned.
func unconfirmedReplay(t *testing.T, instr *runnerInstr, agent Instrumentation, cmdType utils.CmdType) (*observer.ObservedLogs, bool, *Receipt, error) {
	t.Helper()
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	db := &memTestDB{existing: []*models.TestCase{httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0)}}
	command := "./shop.test -test.v"
	if cmdType == utils.DockerCompose {
		command = "docker compose up"
	}
	cfg := instrConfig(instr.composeInstr, cmdType, command)
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zapcore.DebugLevel)
	svc := New(zap.New(core), agent, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	reported := false
	RegisterReplayOutcomeReporter(func(context.Context, ReplayOutcome) { reported = true })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	err := svc.Replay(context.Background())
	receipt, rerr := ReadReceipt(dir, "set")
	require.NoError(t, rerr)
	return logs, reported, receipt, err
}

// A replay whose request drain ended on the stream's quiet -- its agent
// cannot be asked what it still holds, cannot say (it answers the question
// 404 or 501: models.BacklogUnsupported), or said it holds nothing more but
// cannot end its stream after what it sent -- cannot confirm it compares
// every request the app served: one the agent was slower than the quiet to
// hand over would read as a request the app never made. Replay is the judge,
// so it is not judged: it fails as keploy's, said at ERROR with the quiet it
// went by, the receipt does not certify it isolated, and no outcome is
// reported. A compose agent that keeps its stream open is no different. It
// used to be judged on what came before the quiet, in silence.
func TestReplayADrainEndedOnQuietFailsAsKeploys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		says    *models.AgentBacklog // nil: an agent that cannot be asked
		cmdType utils.CmdType
	}{
		{"an agent that cannot tell", &models.AgentBacklog{Verdict: models.BacklogUnsupported}, utils.Native},
		{"clear, without end markers", &models.AgentBacklog{Verdict: models.BacklogClear}, utils.Native},
		{"an agent that cannot be asked", nil, utils.Native},
		{"a compose agent that cannot tell", &models.AgentBacklog{Verdict: models.BacklogUnsupported}, utils.DockerCompose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := newRunnerInstr(t, sequentialMarks()...)
			if tc.cmdType == utils.DockerCompose {
				instr = &runnerInstr{composeInstr: newInstr(t, agentUpAfterRun, false, models.AppError{AppErrorType: models.ErrAppStopped}), windows: sequentialMarks()}
				instr.runUntilReady = true
			}
			stream := quietStream{runnerInstr: instr, tc: httpCase("", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0.Add(5*time.Millisecond))}
			endCalls := &atomic.Int32{}
			var agent Instrumentation = stream
			if tc.says != nil {
				agent = quietIncoming{quietStream: stream, says: *tc.says, endCalls: endCalls}
			}
			logs, reported, receipt, err := unconfirmedReplay(t, instr, agent, tc.cmdType)
			require.False(t, reported, "a run whose request drain ended on quiet was judged")
			require.Error(t, err, "a run whose request drain ended on quiet was not keploy's failure")
			require.Contains(t, err.Error(), requestsUnconfirmed)
			require.Contains(t, err.Error(), mockDrainQuiet.String()+" of quiet", "the reason does not say the quiet keploy went by")
			require.Equal(t, utils.ExitKeployError, utils.ErrCode)
			said := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(quietRequestsUnconfirmed).All()
			require.Len(t, said, 1, "the drain's end on quiet was not said at ERROR: %v", logs.All())
			fields := said[0].ContextMap()
			require.Equal(t, "set", fields["mock-set"])
			require.Equal(t, mockDrainQuiet, fields["quiet_for"])
			require.Equal(t, quietRequestsUnconfirmedNext, fields["next_step"])
			require.Equal(t, FailedByKeploy, receipt.FailedBy)
			require.False(t, receipt.Isolated, "a run that may be short of requests was certified isolated")
			requireReported(t, receipt)
			require.Zero(t, endCalls.Load(), "an agent that gave no word to end on was asked to end its streams")
		})
	}
}

// A replay whose agent said it holds nothing more from before the run ended,
// and that it ends its streams on request, then answered that request without
// taking it (an error that is not the agent gone), has its request stream
// ended here: a request still on its way is not in what the run compares,
// and would read as one the app never made. Replay is the judge, so the run
// is not judged: it fails as keploy's, said at ERROR naming the agent's
// answer, the receipt does not certify it isolated, and no outcome is
// reported. It used to be judged after a WARN. keploy mock record keeps
// saving the recording (TestRecord_AnEndRequestTheAgentDoesNotTakeEndsTheRoundAsBefore).
func TestReplayAnEndRequestTheAgentRefusesFailsAsKeploys(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	refusal := errors.New("the agent answered 500 Internal Server Error")
	agent := quietIncoming{quietStream: quietStream{runnerInstr: instr, tc: httpCase("", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0.Add(5*time.Millisecond))},
		says: models.AgentBacklog{Verdict: models.BacklogClear, EndMarkers: true}, endErr: refusal, endCalls: &atomic.Int32{}}
	logs, reported, receipt, err := unconfirmedReplay(t, instr, agent, utils.Native)
	require.EqualValues(t, 1, agent.endCalls.Load(), "the agent was not asked once to end the watch's stream")
	require.False(t, reported, "a run whose agent refused to end its request stream was judged")
	require.Error(t, err, "a run whose agent refused to end its request stream was not keploy's failure")
	require.Contains(t, err.Error(), requestsEndRefused+" ("+refusal.Error()+")", "the reason does not name the agent's answer")
	require.Equal(t, utils.ExitKeployError, utils.ErrCode)
	said := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(endRefusedRequestsUnconfirmed).All()
	require.Len(t, said, 1, "the refusal was not said at ERROR: %v", logs.All())
	fields := said[0].ContextMap()
	require.Equal(t, "set", fields["mock-set"])
	require.Equal(t, refusal.Error(), fields["error"])
	require.Equal(t, endRefusedRequestsUnconfirmedNext, fields["next_step"])
	require.Zero(t, logs.FilterMessage(endRefused).Len(), "the refusal was said as a recording's WARN: %v", logs.All())
	require.Equal(t, FailedByKeploy, receipt.FailedBy)
	require.False(t, receipt.Isolated, "a run whose agent refused to end its request stream was certified isolated")
	requireReported(t, receipt)
}

// A refused end request once the user stopped the run is no failure of
// keploy's: an agent the stop reached may refuse as it goes, and the run ends
// unjudged as one the user stopped. It is said at WARN, the exit code is left
// for the stop, and the run is not made short.
func TestReplayEndRefused_AfterTheUsersStopIsNoFailure(t *testing.T) {
	utils.ClearInterrupted()
	t.Cleanup(utils.ClearInterrupted)
	defer func(code int) { utils.ErrCode = code }(utils.ErrCode)
	utils.ErrCode = 0
	core, logs := observer.New(zapcore.DebugLevel)
	m := &mockService{logger: zap.New(core)}
	refusal := errors.New("the agent answered 503 Service Unavailable")
	require.Empty(t, m.replayEndRefused("set", nil), "an end request the agent took was taken for refused")
	require.Zero(t, logs.Len(), "%v", logs.All())
	utils.MarkInterrupted()
	require.Empty(t, m.replayEndRefused("set", refusal), "a refusal after the user's stop made the run short")
	require.Zero(t, utils.ErrCode, "a refusal after the user's stop took the exit code from the stop")
	require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "%v", logs.All())
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(endRefused).Len(), "%v", logs.All())
}

// deadlineInstr is an agent that answers each epilogue read at once, and
// remembers whether the read's context had a deadline.
type deadlineInstr struct {
	Instrumentation // embedded: nil, so an unexpected call panics loudly

	mu        sync.Mutex
	deadlines map[string]bool
}

func (d *deadlineInstr) saw(read string, ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deadlines == nil {
		d.deadlines = map[string]bool{}
	}
	_, has := ctx.Deadline()
	d.deadlines[read] = has
}

func (d *deadlineInstr) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	d.saw("consumed", ctx)
	return nil, nil
}

func (d *deadlineInstr) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	d.saw("misses", ctx)
	return nil, nil
}

func (d *deadlineInstr) DrainCapturedMocks(ctx context.Context) ([]*models.Mock, error) {
	d.saw("captured", ctx)
	return nil, nil
}

func (d *deadlineInstr) GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	d.saw("windows", ctx)
	return nil, nil
}

// A replay's per-test windows carry the begins keploy refused, which fail
// the run whatever the switch, and a failed read fails it: so the read has
// no deadline either way (an agent slow to answer on a loaded host is no
// failed read; its answer, the connection's close or the run's context ends
// it). A record's, with the switch off, is a best-effort epilogue read and
// keeps its bound (agentEpilogueTimeout).
func TestAReplaysWindowsReadHasNoDeadline(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			inst := &deadlineInstr{}
			m := &mockService{logger: zap.NewNop(), instrumentation: inst, config: &config.Config{}}
			_, err := m.replayWindows(context.Background())
			require.NoError(t, err)
			require.Equal(t, map[string]bool{"windows": false}, inst.deadlines, "the replay's read had a deadline")
			inst.deadlines = nil
			m.agentWindows(context.Background())
			require.Equal(t, map[string]bool{"windows": true}, inst.deadlines, "the record's best-effort read had no bound")
		})
	}
}

// Under the proven switch replay's epilogue reads -- what the agent served
// and missed, and what it captured on miss -- have no deadline (design rule
// 15): the agent's answer, the connection's close or the run's context ends
// each. With the switch off they keep their bound (agentEpilogueTimeout).
func TestReplayEpilogueReadsHaveNoDeadlineUnderTheSwitch(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			inst := &deadlineInstr{}
			m := &mockService{logger: zap.NewNop(), instrumentation: inst, config: &config.Config{}}
			m.reportOutcome(context.Background(), 0, replayDetail{}, func() bool { return false })
			_ = m.persistCaptured(context.Background(), "set")
			require.Equal(t, map[string]bool{"consumed": !on, "misses": !on, "captured": !on}, inst.deadlines)
		})
	}
}

// slowOutcome answers what the agent served only once released: an agent
// whose answer takes longer than any bound would allow.
type slowOutcome struct {
	deadlineInstr
	release chan struct{}
}

func (s *slowOutcome) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	select {
	case <-s.release:
		return []models.MockState{{Name: "mock-0"}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// The run's context still ends a read with no deadline: the user's interrupt
// or the agent's death cancels it, and the read reports the cancel, not an
// answer.
func TestReplayOutcomeReadEndsWithTheRunsContext(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	inst := &slowOutcome{release: make(chan struct{})}
	m := &mockService{logger: zap.NewNop(), instrumentation: inst, config: &config.Config{}}
	ctx, cancel := context.WithCancel(context.Background())
	counts := make(chan replayCounts, 1)
	go func() { counts <- m.reportOutcome(ctx, 1, replayDetail{}, func() bool { return true }) }()
	select {
	case c := <-counts:
		t.Fatalf("the read ended (%+v) with neither an answer nor the run's context done", c)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case c := <-counts:
		require.Equal(t, -1, c.consumed, "a read the run's context ended was taken for an answer")
	case <-time.After(time.Minute):
		t.Fatal("the run's context did not end the read")
	}
	close(inst.release)
}
