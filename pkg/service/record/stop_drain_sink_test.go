package record

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// heldAgent models the real AgentClient's mock stream: a reader goroutine
// that stops sending, and closes the channel, once the stream's ctx is
// cancelled (the HTTP request torn down). What it has not sent by then is
// lost. It holds `held` mocks captured before the stop.
type heldAgent struct {
	*fakeInstr
	held    int
	at      time.Time
	started chan struct{}
}

func (f *heldAgent) GetOutgoing(ctx context.Context, _ models.OutgoingOptions) (<-chan *models.Mock, error) {
	ch := make(chan *models.Mock, 4) // a small reader buffer
	go func() {
		defer close(ch)
		<-f.started
		for i := 0; i < f.held; i++ {
			select {
			case ch <- mockAt(fmt.Sprintf("mock-%d", i), f.at):
			case <-ctx.Done():
				return
			}
			time.Sleep(time.Millisecond) // the agent delivers steadily
		}
	}()
	return ch, nil
}

func newHeldAgentRecorder(t *testing.T, held int, logger *zap.Logger) (*heldAgent, FrameChan, context.CancelFunc) {
	t.Helper()
	f := &heldAgent{fakeInstr: &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase)},
		held: held, at: time.Now(), started: make(chan struct{})}
	r := &Recorder{logger: logger, instrumentation: f, config: &config.Config{}, frameQuiet: 100 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); frames.Abandon() })
	return f, frames, cancel
}

// A sink that is still draining is not cut off. Here it spends longer than the
// quiet bound persisting some mocks (a 12 MB, 8,209-row mock; a disk or CPU
// stall), while the agent still holds more captured before the stop. The
// quiet bound kept running while the forwarder waited on the consumer, so it
// fired meanwhile, and the next select picked at random between it and the
// ready stream: the stream was cancelled with the agent's frames unsent, and
// nothing said so (the review measured 8 of 20 runs persisting 26 of 200).
func TestGetTestAndMockChans_SlowSinkIsNotCutOff(t *testing.T) {
	const held, runs = 60, 3
	for run := 0; run < runs; run++ {
		f, frames, stop := newHeldAgentRecorder(t, held, zap.NewNop())
		var got atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range frames.Outgoing {
				if n := got.Add(1); n%10 == 0 {
					time.Sleep(250 * time.Millisecond) // a slow persist, over the 100 ms bound
				}
			}
		}()
		stop()
		close(f.started)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("the drain hung")
		}
		if int(got.Load()) != held {
			t.Fatalf("run %d: persisted %d of %d mocks captured before the stop: a sink still draining was cut off", run, got.Load(), held)
		}
	}
}

// A second interrupt ends a drain that would otherwise go on while frames keep
// coming, and says what it had saved: the user can always stop.
func TestGetTestAndMockChans_SecondInterruptEndsTheDrain(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	core, logs := observer.New(zapcore.WarnLevel)
	f, frames, stop := newHeldAgentRecorder(t, 1_000_000, zap.New(core))
	var got atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range frames.Outgoing {
			got.Add(1)
		}
	}()
	stop()
	close(f.started)
	for got.Load() < 50 {
		time.Sleep(time.Millisecond)
	}
	utils.MarkInterruptedAgain()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second interrupt did not end the drain")
	}
	said := false
	for _, e := range logs.FilterMessageSnippet("at a second interrupt").All() {
		if e.ContextMap()["frames"] == "mocks" && e.ContextMap()["savedSinceTheStop"].(int64) > 0 {
			said = true
		}
	}
	if !said {
		t.Fatalf("ending the drain at a second interrupt did not say what it had saved: %v", logs.All())
	}
}

// blockedMappingsAgent is an agent from before this change whose mapping stream
// sent no headers until its first mapping: GetMappings blocks in its request.
type blockedMappingsAgent struct{ *fakeInstr }

func (b *blockedMappingsAgent) GetMappings(ctx context.Context, _ models.IncomingOptions) (<-chan models.TestMockMapping, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A recording whose agent emits no mappings (a proxyless or DaemonSet one) did
// not stop until the mapping drain's fixed 15 s cap, and then warned that
// mappings.yaml may be missing tests and told the user to re-record, on every
// passing run. The tail is bounded by progress: nothing arriving for the quiet
// bound after the frame drains ends it, silently.
func TestGetTestAndMockChans_QuietMappingStreamEndsWithoutWaitingOrWarning(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	r := &Recorder{logger: zap.New(core), instrumentation: &blockedMappingsAgent{f}, config: &config.Config{}, frameQuiet: 50 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	var wg sync.WaitGroup
	for _, drain := range []func(){
		func() {
			for range frames.Incoming {
			}
		},
		func() {
			for range frames.Outgoing {
			}
		},
		func() {
			for range frames.Mappings {
			}
		},
	} {
		wg.Add(1)
		go func() { defer wg.Done(); drain() }()
	}
	start := time.Now()
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the stop did not end")
	}
	if took := time.Since(start); took > mappingIdleGrace+2*time.Second {
		t.Errorf("a stop with no mappings took %v: a fixed wait, not the quiet bound (%v)", took, mappingIdleGrace)
	}
	if n := logs.Len(); n != 0 {
		t.Errorf("a stop with no mappings warned: %v", logs.All())
	}
}

// A mapping consumer that takes longer than the quiet bound to persist
// (mappings.yaml is rewritten per batch) is not cut off: the mappings the
// agent holds behind it all arrive.
func TestGetTestAndMockChans_SlowMappingSinkIsNotCutOff(t *testing.T) {
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: &config.Config{}, frameQuiet: 50 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	go func() {
		for range frames.Incoming {
		}
	}()
	go func() {
		for range frames.Outgoing {
		}
	}()
	const held = 4
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range frames.Mappings {
			got = append(got, m.TestName)
			if len(got) == 1 {
				time.Sleep(mappingIdleGrace + time.Second) // one slow flush
			}
		}
	}()
	cancel()
	go func() {
		// The mappings come once the test-case and mock drains have ended,
		// as the agent emits a test's mapping only after its mocks (their
		// quiet bound, then the reader's grace: ~300 ms here).
		time.Sleep(time.Second)
		for i := 0; i < held; i++ {
			select {
			case f.mappings <- models.TestMockMapping{TestName: fmt.Sprintf("test-%d", i)}:
			case <-time.After(30 * time.Second):
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the mapping drain hung")
	}
	if len(got) != held {
		t.Fatalf("persisted %d of %d mappings the agent held: a slow flush ended the drain (%v)", len(got), held, got)
	}
}

// pendingAgent says it still holds frames from before the stop until told
// otherwise.
type pendingAgent struct {
	*fakeInstr
	pending atomic.Bool
	asked   atomic.Int32
}

func (p *pendingAgent) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	p.asked.Add(1)
	if p.pending.Load() {
		return models.AgentBacklog{Verdict: models.BacklogPending}, nil
	}
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

// Quiet is not proof that the agent has handed over what it held at the stop:
// a parser behind the traffic can be quiet for longer than the grace. An agent
// that can tell is asked, and the stream stays open while it says it may still
// hand something over.
func TestGetTestAndMockChans_DrainWaitsWhileTheAgentSaysItHoldsMore(t *testing.T) {
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	agent := &pendingAgent{fakeInstr: f}
	agent.pending.Store(true)
	r := &Recorder{logger: zap.NewNop(), instrumentation: agent, config: &config.Config{}, frameQuiet: 50 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range frames.Outgoing {
			got = append(got, m.Name)
		}
	}()
	go func() {
		for range frames.Incoming {
		}
	}()
	captured := time.Now()
	cancel()
	// Quiet for many graces: the parser is behind.
	time.Sleep(10 * 50 * time.Millisecond)
	select {
	case f.outgoing <- mockAt("late-but-before-the-stop", captured):
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was ended while the agent said it still held frames from before the stop")
	}
	agent.pending.Store(false)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end once the agent had nothing more from before the stop")
	}
	if len(got) != 1 || agent.asked.Load() == 0 {
		t.Fatalf("persisted %v (agent asked %d times)", got, agent.asked.Load())
	}
}

// A drain the agent holds open while nothing arrives (it says a parser is
// still behind the stop) is ended by a second interrupt too, and says so.
func TestGetTestAndMockChans_SecondInterruptEndsAQuietDrainTheAgentHoldsOpen(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	core, logs := observer.New(zapcore.WarnLevel)
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	agent := &pendingAgent{fakeInstr: f}
	agent.pending.Store(true) // behind, for good
	r := &Recorder{logger: zap.New(core), instrumentation: agent, config: &config.Config{}, frameQuiet: 50 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range frames.Outgoing {
		}
	}()
	go func() {
		for range frames.Incoming {
		}
	}()
	cancel()
	waitAsked := time.Now().Add(5 * time.Second)
	for agent.asked.Load() < 2 {
		if time.Now().After(waitAsked) {
			t.Fatal("the drain never asked the agent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("the drain ended while the agent said it still held frames from before the stop")
	default:
	}
	utils.MarkInterruptedAgain()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second interrupt did not end a drain the agent held open")
	}
	if logs.FilterMessageSnippet("at a second interrupt").Len() == 0 {
		t.Fatalf("ending the drain at a second interrupt did not say so: %v", logs.All())
	}
}

// backlogAgent answers a stop's look at its backlog (AgentClient.BacklogBefore)
// with whatever answer says: what the agent's /record/pending says, or no
// answer at all.
type backlogAgent struct {
	*fakeInstr
	answer func() (models.AgentBacklog, error)
	asked  atomic.Int32
}

func (a *backlogAgent) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	a.asked.Add(1)
	return a.answer()
}

// drainRun stops a recording whose agent then hands over two mocks captured
// before the stop, `gap` apart (a parser behind the traffic that took that
// long to get to the second), under a quiet bound of `quiet`. agent answers
// the drain's looks; holding reports, for its answers, whether the second
// mock is still to come. It returns the names persisted and the recorder.
func drainRun(t *testing.T, quiet, gap time.Duration, answer func(holding bool) (models.AgentBacklog, error)) ([]string, *Recorder, *observer.ObservedLogs) {
	t.Helper()
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	var holding atomic.Bool
	holding.Store(true)
	agent := &backlogAgent{fakeInstr: f, answer: func() (models.AgentBacklog, error) { return answer(holding.Load()) }}
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Recorder{logger: zap.New(core), instrumentation: agent, config: &config.Config{}, frameQuiet: quiet}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range frames.Outgoing {
			mu.Lock()
			got = append(got, m.Name)
			mu.Unlock()
		}
	}()
	go func() {
		for range frames.Incoming {
		}
	}()
	captured := time.Now()
	cancel() // the stop
	select {
	case f.outgoing <- mockAt("pre-stop-1", captured):
	case <-time.After(5 * time.Second):
		t.Fatal("the first mock from before the stop was not taken")
	}
	// The parser gets to the second one only after many quiet bounds. The
	// sleep is the lag under test, not a wait for anything to settle.
	time.Sleep(gap)
	select {
	case f.outgoing <- mockAt("pre-stop-2", captured):
		holding.Store(false)
	case <-time.After(5 * time.Second):
		holding.Store(false)
		t.Logf("the drain no longer read the stream: pre-stop-2 is lost")
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), got...), r, logs
}

// P1 (stress audit rank 2): a stop's drain ended after its quiet bound with no
// frame from before the stop, so a mock a lagging parser handed over later than
// that was lost (reproduced at 3.5 s against the 3 s bound, logged at DEBUG
// only). An agent whose capture can tell says it still holds it, and the drain
// waits for it.
func TestDrain_KeepsAMockThatComesLaterThanTheQuietBound(t *testing.T) {
	const quiet = 50 * time.Millisecond
	got, r, _ := drainRun(t, quiet, 7*quiet, func(holding bool) (models.AgentBacklog, error) {
		if holding {
			return models.AgentBacklog{Verdict: models.BacklogPending, WaitingOn: "connection 1: its parser is behind"}, nil
		}
		return models.AgentBacklog{Verdict: models.BacklogClear}, nil
	})
	if len(got) != 2 {
		t.Fatalf("persisted %v: a mock from before the stop that came after the quiet bound was lost", got)
	}
	if r.captureLost.Load() {
		t.Fatal("captureLost set for a drain that got everything")
	}
}

// P7: a look the agent does not answer (a timed-out /record/pending) said
// "nothing pending", and the drain ended and lost the rest. An answer that did
// not come is never read as "no": the drain waits.
func TestDrain_ALookTheAgentDoesNotAnswerDoesNotLoseAMock(t *testing.T) {
	const quiet = 50 * time.Millisecond
	got, r, logs := drainRun(t, quiet, 7*quiet, func(holding bool) (models.AgentBacklog, error) {
		if holding {
			return models.AgentBacklog{}, fmt.Errorf("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")
		}
		return models.AgentBacklog{Verdict: models.BacklogClear}, nil
	})
	if len(got) != 2 {
		t.Fatalf("persisted %v: a look the agent did not answer was read as nothing pending", got)
	}
	if r.captureLost.Load() {
		t.Fatal("captureLost set for a drain that got everything")
	}
	if logs.FilterMessageSnippet("could not ask the keploy agent").Len() == 0 {
		t.Fatalf("a look the agent did not answer was not said at INFO: %v", logs.All())
	}
}

// An agent that can never tell (it predates the question, or its capture has
// no watermark) leaves the drain the quiet rule, and it says so.
func TestDrain_AnAgentThatCannotTellEndsOnQuiet(t *testing.T) {
	const quiet = 50 * time.Millisecond
	got, r, logs := drainRun(t, quiet, 7*quiet, func(bool) (models.AgentBacklog, error) {
		return models.AgentBacklog{Verdict: models.BacklogUnsupported}, nil
	})
	if len(got) != 1 {
		t.Fatalf("persisted %v, want only the first: an agent that cannot tell leaves the quiet rule", got)
	}
	if r.captureLost.Load() {
		t.Fatal("captureLost set by the quiet rule")
	}
	if logs.FilterMessageSnippet("cannot say whether it still holds").Len() != 1 {
		t.Fatalf("that the agent cannot tell was not said once: %v", logs.All())
	}
}

// stalledDrain stops a recording whose agent holds a mock from before the stop
// that never comes, and answers every look with answer. It returns the
// recorder and its logs once the drain has ended.
func stalledDrain(t *testing.T, answer func(look int32) (models.AgentBacklog, error)) (*Recorder, *observer.ObservedLogs, *backlogAgent) {
	t.Helper()
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	agent := &backlogAgent{fakeInstr: f}
	agent.answer = func() (models.AgentBacklog, error) { return answer(agent.asked.Load()) }
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Recorder{logger: zap.New(core), instrumentation: agent, config: &config.Config{}, frameQuiet: 5 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range frames.Outgoing {
		}
	}()
	go func() {
		for range frames.Incoming {
		}
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a drain on a backlog that made no progress never ended")
	}
	return r, logs, agent
}

// A mock that truly never arrives: the agent says it still holds one, and its
// capture makes no progress look after look. The drain ends after
// drainStallLooks such looks, at ERROR, and marks the recording incomplete.
func TestDrain_ABacklogThatNeverMovesEndsLoudly(t *testing.T) {
	r, logs, agent := stalledDrain(t, func(int32) (models.AgentBacklog, error) {
		return models.AgentBacklog{Verdict: models.BacklogPending, Progress: 7, WaitingOn: "connection 9: its parser is stuck"}, nil
	})
	if !r.captureLost.Load() {
		t.Fatal("the recording was not marked incomplete")
	}
	lost := logs.FilterMessageSnippet("stopped waiting for the keploy agent").FilterLevelExact(zapcore.ErrorLevel)
	if lost.Len() == 0 {
		t.Fatalf("the loss was not said at ERROR: %v", logs.All())
	}
	if on := lost.All()[0].ContextMap()["waiting_on"]; on != "connection 9: its parser is stuck" {
		t.Fatalf("the error did not say what the agent was waiting on: %v", lost.All()[0].ContextMap())
	}
	// Each of the two frame streams (mocks, test cases) makes its own looks.
	if n := agent.asked.Load(); n < drainStallLooks {
		t.Fatalf("gave up after %d looks, want at least %d", n, drainStallLooks)
	}
}

// An agent that never answers at all, on a stream that stays open, ends the
// drain the same way, loudly.
func TestDrain_AnAgentThatNeverAnswersEndsLoudly(t *testing.T) {
	r, logs, _ := stalledDrain(t, func(int32) (models.AgentBacklog, error) {
		return models.AgentBacklog{}, fmt.Errorf("connection refused")
	})
	if !r.captureLost.Load() {
		t.Fatal("the recording was not marked incomplete")
	}
	if logs.FilterMessageSnippet("stopped waiting for the keploy agent").FilterLevelExact(zapcore.ErrorLevel).Len() == 0 {
		t.Fatalf("the loss was not said at ERROR: %v", logs.All())
	}
}

// A backlog the agent is working through (its progress moves look after
// look) is waited for well past drainStallLooks looks, however long it takes.
func TestDrain_ABacklogThatMovesIsWaitedFor(t *testing.T) {
	r, _, agent := stalledDrain(t, func(look int32) (models.AgentBacklog, error) {
		if look > 3*drainStallLooks {
			return models.AgentBacklog{Verdict: models.BacklogClear}, nil
		}
		return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(look)}, nil
	})
	if r.captureLost.Load() {
		t.Fatalf("a backlog that moved on every look was given up on after %d looks", agent.asked.Load())
	}
}

// An agent that ends its stream after the stop while it said it still held
// something (it was killed, or crashed) has lost it: said at ERROR, and the
// recording fails. One that ends it before any look is said at WARN.
func TestDrain_AnAgentThatEndsItsStreamWhileItHoldsMoreIsALoss(t *testing.T) {
	run := func(t *testing.T, lookFirst bool) (*Recorder, *observer.ObservedLogs) {
		f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
		agent := &backlogAgent{fakeInstr: f}
		// Pending, its capture moving on every look: the drain never gives
		// up on it, so nothing but the stream's end ends the mock drain.
		agent.answer = func() (models.AgentBacklog, error) {
			return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(agent.asked.Load())}, nil
		}
		core, logs := observer.New(zapcore.DebugLevel)
		quiet := time.Hour // no look happens unless the test waits for one
		if lookFirst {
			quiet = 5 * time.Millisecond
		}
		r := &Recorder{logger: zap.New(core), instrumentation: agent, config: &config.Config{}, frameQuiet: quiet}
		g, gctx := errgroup.WithContext(context.Background())
		ctx, cancel := context.WithCancel(gctx)
		ctx = context.WithValue(ctx, models.ErrGroupKey, g)
		frames, err := r.GetTestAndMockChans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer frames.Abandon()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range frames.Outgoing {
			}
		}()
		go func() {
			for range frames.Incoming {
			}
		}()
		cancel()
		if lookFirst {
			// The mock drain's own look: the two streams' drains share the
			// agent, so its count of looks does not say whose they were.
			deadline := time.Now().Add(30 * time.Second)
			for logs.FilterMessageSnippet("looked at what the keploy agent still holds").FilterField(zap.String("frames", "mocks")).Len() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the mock drain never asked the agent")
				}
				time.Sleep(time.Millisecond)
			}
		}
		close(f.outgoing) // the agent goes away
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the drain did not end with its stream")
		}
		// The test-case stream stays open: only the mock drain's end is
		// asserted, and the deferred Abandon ends the other silently.
		return r, logs
	}
	t.Run("after it said it held more", func(t *testing.T) {
		r, logs := run(t, true)
		if !r.captureLost.Load() || logs.FilterMessageSnippet("ended its stream while it still held").FilterLevelExact(zapcore.ErrorLevel).FilterField(zap.String("frames", "mocks")).Len() == 0 {
			t.Fatalf("the loss was not said at ERROR and counted (captureLost=%v): %v", r.captureLost.Load(), logs.All())
		}
	})
	// The stop and the stream's end can both be ready when the forwarder
	// selects, and it takes either first: it is said whichever it takes.
	// Each run is one such select, and a forwarder that went by the order it
	// took them in missed about half: 32 runs catch it all but surely.
	t.Run("before any look", func(t *testing.T) {
		for i := 0; i < 32; i++ {
			r, logs := run(t, false)
			if r.captureLost.Load() {
				t.Fatalf("run %d: a stream ended before the agent was asked failed the recording", i)
			}
			if logs.FilterMessageSnippet("ended its stream before it said").FilterLevelExact(zapcore.WarnLevel).FilterField(zap.String("frames", "mocks")).Len() == 0 {
				t.Fatalf("run %d: an agent ending its stream after the stop was not said at WARN: %v", i, logs.All())
			}
		}
	})
	// Natively the drains start only once the app is stopped, well after the
	// recording's stop began: a stream that ends in between ended after the
	// stop.
	t.Run("after the stop began, before the drain", func(t *testing.T) {
		f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Recorder{logger: zap.New(core), instrumentation: &backlogAgent{fakeInstr: f}, config: &config.Config{}, frameQuiet: time.Hour}
		g, gctx := errgroup.WithContext(context.Background())
		ctx, cancel := context.WithCancel(gctx)
		defer cancel()
		ctx = context.WithValue(ctx, models.ErrGroupKey, g)
		frames, err := r.GetTestAndMockChans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer frames.Abandon()
		go func() {
			for range frames.Incoming {
			}
		}()
		r.stopping.Store(true) // Start's teardown began; ctx is cancelled later
		close(f.outgoing)
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range frames.Outgoing {
			}
		}()
		select {
		case <-drained:
		case <-time.After(30 * time.Second):
			t.Fatal("the mock drain did not end with its stream")
		}
		if logs.FilterMessageSnippet("ended its stream before it said").FilterLevelExact(zapcore.WarnLevel).FilterField(zap.String("frames", "mocks")).Len() != 1 {
			t.Fatalf("an agent ending its stream after the stop began was not said at WARN: %v", logs.All())
		}
		close(f.incoming)
	})
	// A stream that ends while the recording runs is not one ended after the
	// stop.
	t.Run("while the recording runs", func(t *testing.T) {
		f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Recorder{logger: zap.New(core), instrumentation: &backlogAgent{fakeInstr: f}, config: &config.Config{}, frameQuiet: time.Hour}
		g, gctx := errgroup.WithContext(context.Background())
		ctx, cancel := context.WithCancel(gctx)
		defer cancel()
		ctx = context.WithValue(ctx, models.ErrGroupKey, g)
		frames, err := r.GetTestAndMockChans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer frames.Abandon()
		go func() {
			for range frames.Incoming {
			}
		}()
		close(f.outgoing)
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range frames.Outgoing {
			}
		}()
		select {
		case <-drained:
		case <-time.After(30 * time.Second):
			t.Fatal("the mock drain did not end with its stream")
		}
		if r.captureLost.Load() || logs.FilterMessageSnippet("ended its stream").Len() != 0 {
			t.Fatalf("a stream that ended while the recording ran was taken for one ended after the stop: %v", logs.All())
		}
		close(f.incoming)
	})
}

// drainMarkInstr records whether each frame stream was opened on a context
// marked to drain after the stop (models.WithDrainsAfterStop).
type drainMarkInstr struct {
	*fakeInstr
	marked sync.Map
}

func (d *drainMarkInstr) GetIncoming(ctx context.Context, o models.IncomingOptions) (<-chan *models.TestCase, error) {
	d.marked.Store("incoming", models.DrainsAfterStop(ctx))
	return d.fakeInstr.GetIncoming(ctx, o)
}

func (d *drainMarkInstr) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	d.marked.Store("outgoing", models.DrainsAfterStop(ctx))
	return d.fakeInstr.GetOutgoing(ctx, o)
}

func (d *drainMarkInstr) GetMappings(ctx context.Context, o models.IncomingOptions) (<-chan models.TestMockMapping, error) {
	d.marked.Store("mappings", models.DrainsAfterStop(ctx))
	return d.fakeInstr.GetMappings(ctx, o)
}

// A recording drains the agent's streams after its stop and closes them
// before it stops the agent, so it opens all three marked to say so: the
// agent holds a stop signal for them.
func TestGetTestAndMockChans_OpensItsStreamsMarkedToDrain(t *testing.T) {
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
	inst := &drainMarkInstr{fakeInstr: f}
	r := &Recorder{logger: zap.NewNop(), instrumentation: inst, config: &config.Config{}, frameQuiet: time.Hour}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	defer cancel()
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	deadline := time.Now().Add(30 * time.Second)
	for {
		n := 0
		inst.marked.Range(func(_, _ any) bool { n++; return true })
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of the three streams were opened", n)
		}
		time.Sleep(time.Millisecond)
	}
	inst.marked.Range(func(k, v any) bool {
		if v != true {
			t.Errorf("the %s stream was opened without the mark to drain after the stop", k)
		}
		return true
	})
	close(f.incoming)
	close(f.outgoing)
	close(f.mappings)
}

// When the agent ends its stream after a look that said it still held
// something, the recording fails, and the ERROR says whether the loss is
// certain: a test case or mock handed on since may have been what it held (a
// mapping, which the look does not cover, may not).
func TestAgentEndedDrain_SaysWhetherTheLossIsCertain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		since   func(r *Recorder)
		certain bool
	}{
		{"nothing since", func(*Recorder) {}, true},
		{"a mapping since", func(r *Recorder) { r.frameProgress.Add(1) }, true},
		{"a mock since", func(r *Recorder) { r.frameProgress.Add(1); r.heldFrames.Add(1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			r := &Recorder{logger: zap.New(core)}
			var looks backlogLooks
			looks.Stall = drainStallLooks
			looks.heldFrames = r.heldFrames.Load()
			looks.Look(models.AgentBacklog{Verdict: models.BacklogPending, Progress: 1}, nil, r.frameProgress.Load())
			tc.since(r)
			r.agentEndedDrain("mocks", &looks)
			errs := logs.FilterLevelExact(zapcore.ErrorLevel)
			certain := errs.FilterMessageSnippet("still held what it captured before the stop: that is lost").Len()
			possible := errs.FilterMessageSnippet("some of that may be lost").Len()
			if !r.captureLost.Load() || logs.FilterLevelExact(zapcore.WarnLevel).Len() != 0 {
				t.Fatalf("want the recording failed, at ERROR: captureLost=%v: %v", r.captureLost.Load(), logs.All())
			}
			if tc.certain && (certain != 1 || possible != 0) || !tc.certain && (certain != 0 || possible != 1) {
				t.Fatalf("certain=%v: said certain %d, possible %d times: %v", tc.certain, certain, possible, logs.All())
			}
		})
	}
}

// The agent ending its mapping stream after the stop, before the drain ended
// it, is said at WARN: the mappings it still held are lost, and the tests they
// belong to replay with no mocks. While the recording runs it is not.
func TestDrain_AnAgentThatEndsItsMappingStreamAfterTheStopIsSaid(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Recorder{logger: zap.New(core), instrumentation: &backlogAgent{fakeInstr: f}, config: &config.Config{}, frameQuiet: time.Hour}
		g, gctx := errgroup.WithContext(context.Background())
		ctx, cancel := context.WithCancel(gctx)
		ctx = context.WithValue(ctx, models.ErrGroupKey, g)
		frames, err := r.GetTestAndMockChans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range frames.Incoming {
			}
		}()
		go func() {
			for range frames.Outgoing {
			}
		}()
		r.stopping.Store(stopped) // the recording's stop began, or not
		close(f.mappings)         // the agent ends the stream
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range frames.Mappings {
			}
		}()
		select {
		case <-drained:
		case <-time.After(30 * time.Second):
			t.Fatal("the mapping forwarder did not end with its stream")
		}
		said := logs.FilterMessageSnippet("ended its mapping stream").FilterLevelExact(zapcore.WarnLevel).Len()
		if stopped && said != 1 {
			t.Fatalf("an agent ending its mapping stream after the stop was not said once at WARN: %v", logs.All())
		}
		if !stopped && said != 0 {
			t.Fatalf("a mapping stream that ended while the recording ran was taken for one ended after the stop: %v", logs.All())
		}
		frames.Abandon()
		cancel()
	}
}

// gatedMockDrain stops a recording whose agent says, at the mock drain's first
// look, that it still holds something, and holds that look open while
// whileLooking runs: what it does to the stream is all there when the drain
// goes on, so the drain's next select sees it at once. The test-case stream
// ends while the recording runs, so only the mock drain looks.
func gatedMockDrain(t *testing.T, quiet time.Duration, whileLooking func(f *fakeInstr), answer ...func(look int32, f *fakeInstr) (models.AgentBacklog, error)) (*Recorder, *observer.ObservedLogs) {
	t.Helper()
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock, 1)}
	entered, gate := make(chan struct{}), make(chan struct{})
	var first sync.Once
	agent := &backlogAgent{fakeInstr: f}
	agent.answer = func() (models.AgentBacklog, error) {
		first.Do(func() {
			close(entered)
			<-gate
		})
		if len(answer) > 0 {
			return answer[0](agent.asked.Load(), f)
		}
		return models.AgentBacklog{Verdict: models.BacklogPending, Progress: uint64(agent.asked.Load())}, nil
	}
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Recorder{logger: zap.New(core), instrumentation: agent, config: &config.Config{}, frameQuiet: quiet}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	defer cancel()
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	wait := func(done <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not happen", what)
		}
	}
	incDone := make(chan struct{})
	go func() {
		defer close(incDone)
		for range frames.Incoming {
		}
	}()
	close(f.incoming)
	wait(incDone, "the test-case drain ending with its stream")
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		for range frames.Outgoing {
		}
	}()
	cancel() // the stop
	wait(entered, "the mock drain's look")
	whileLooking(f)
	close(gate)
	wait(outDone, "the mock drain ending")
	return r, logs
}

// The drain's verdict when the agent's stream ends after a look that said the
// agent still held something: what came since, and how the stream ended,
// decide it, never the order the drain's select took ready events in.
func TestDrain_WhatEndsTheStreamAfterALookDecidesTheVerdict(t *testing.T) {
	// verdict checks the drain said it once: "certain" or "possible" at ERROR
	// with the recording failed, or "warn" with it not failed.
	verdict := func(t *testing.T, r *Recorder, logs *observer.ObservedLogs, want string) {
		t.Helper()
		mocks := logs.FilterField(zap.String("frames", "mocks"))
		got := map[string]int{
			"certain":  mocks.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("that is lost").Len(),
			"possible": mocks.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("some of that may be lost").Len(),
			"warn":     mocks.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("ended its stream before it said").Len(),
		}
		for k, n := range got {
			if (k == want) != (n == 1) || n > 1 {
				t.Fatalf("want %s: said %v, captureLost=%v: %v", want, got, r.captureLost.Load(), logs.All())
			}
		}
		if r.captureLost.Load() != (want != "warn") {
			t.Fatalf("want %s: captureLost=%v", want, r.captureLost.Load())
		}
	}
	// A second interrupt that reaches the agent too ends its stream as the
	// user gives the drain up. The interrupt and the stream's end are both
	// ready when the drain goes on: a drain that went by the order its
	// select took them in missed about half, and 16 runs catch that all but
	// surely.
	t.Run("a second interrupt", func(t *testing.T) {
		defer utils.ResetInterruptedAgain()
		for i := 0; i < 16; i++ {
			r, logs := gatedMockDrain(t, 5*time.Millisecond, func(f *fakeInstr) {
				utils.MarkInterruptedAgain()
				close(f.outgoing)
			})
			utils.ResetInterruptedAgain()
			if r.captureLost.Load() || logs.FilterMessageSnippet("ended its stream").Len() != 0 {
				t.Fatalf("run %d: a second interrupt was taken for the agent going away: %v", i, logs.All())
			}
			if logs.FilterMessageSnippet("at a second interrupt").FilterLevelExact(zapcore.WarnLevel).FilterField(zap.String("frames", "mocks")).Len() != 1 {
				t.Fatalf("run %d: the second interrupt was not said once at WARN: %v", i, logs.All())
			}
		}
	})
	// A mock that may be what the agent held (captured before the stop, or
	// undated) came after the look: the recording fails, the loss possible.
	for name, at := range map[string]time.Time{"a mock from before the stop": time.Now(), "an undated mock": {}} {
		at := at
		t.Run(name, func(t *testing.T) {
			r, logs := gatedMockDrain(t, time.Second, func(f *fakeInstr) {
				f.outgoing <- &models.Mock{Name: "maybe-held", Spec: models.MockSpec{ReqTimestampMock: at}}
				close(f.outgoing)
			})
			verdict(t, r, logs, "possible")
		})
	}
	// One captured after the stop cannot be what it held from before it.
	t.Run("a mock from after the stop", func(t *testing.T) {
		r, logs := gatedMockDrain(t, time.Second, func(f *fakeInstr) {
			f.outgoing <- &models.Mock{Name: "later", Spec: models.MockSpec{ReqTimestampMock: time.Now().Add(time.Hour)}}
			close(f.outgoing)
		})
		verdict(t, r, logs, "certain")
	})
	t.Run("nothing", func(t *testing.T) {
		r, logs := gatedMockDrain(t, time.Second, func(f *fakeInstr) { close(f.outgoing) })
		verdict(t, r, logs, "certain")
	})
	// The agent was already gone when the drain first looked (it could not be
	// reached): that look says nothing of what it held, and nothing else did.
	// A real refused dial: its error is the platform's own, as an agent that
	// is gone gives it.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, refused := net.Dial("tcp4", addr)
	if refused == nil {
		t.Fatal("a dial to a closed port connected")
	}
	t.Run("the agent gone at the first look", func(t *testing.T) {
		r, logs := gatedMockDrain(t, 5*time.Millisecond, func(f *fakeInstr) { close(f.outgoing) },
			func(int32, *fakeInstr) (models.AgentBacklog, error) { return models.AgentBacklog{}, refused })
		verdict(t, r, logs, "warn")
	})
	// It said it still held something, then was gone at the next look (it
	// could not be reached) as its stream ended: that look says nothing, the
	// one before it decides, and so does what came since that one.
	goneAtTheSecondLook := func() func(look int32, f *fakeInstr) (models.AgentBacklog, error) {
		var end sync.Once // a later look, on a slow machine, finds it ended
		return func(look int32, f *fakeInstr) (models.AgentBacklog, error) {
			if look == 1 {
				return models.AgentBacklog{Verdict: models.BacklogPending, Progress: 1}, nil
			}
			end.Do(func() { close(f.outgoing) }) // the agent goes away
			return models.AgentBacklog{}, refused
		}
	}
	t.Run("gone after it said it held something, nothing between", func(t *testing.T) {
		r, logs := gatedMockDrain(t, time.Second, func(*fakeInstr) {}, goneAtTheSecondLook())
		verdict(t, r, logs, "certain")
	})
	t.Run("gone after it said it held something, a mock between", func(t *testing.T) {
		beforeTheStop := time.Now()
		r, logs := gatedMockDrain(t, time.Second, func(f *fakeInstr) {
			f.outgoing <- &models.Mock{Name: "maybe-held", Spec: models.MockSpec{ReqTimestampMock: beforeTheStop}}
		}, goneAtTheSecondLook())
		verdict(t, r, logs, "possible")
	})
	// Gone before any answer, though mocks from before the stop were still
	// coming after it. That does not tell an agent's backlog from this CLI's
	// own lag in taking what the agent had already sent (a slow disk), and a
	// complete recording must not fail for that: said at WARN.
	t.Run("gone before any answer, while mocks from before the stop still came", func(t *testing.T) {
		beforeTheStop := time.Now()
		r, logs := gatedMockDrain(t, time.Second, func(f *fakeInstr) {
			f.outgoing <- &models.Mock{Name: "backlog", Spec: models.MockSpec{ReqTimestampMock: beforeTheStop}}
		}, func() func(int32, *fakeInstr) (models.AgentBacklog, error) {
			var end sync.Once
			return func(look int32, f *fakeInstr) (models.AgentBacklog, error) {
				if look > 1 {
					end.Do(func() { close(f.outgoing) }) // the agent goes away
				}
				return models.AgentBacklog{}, refused
			}
		}())
		verdict(t, r, logs, "warn")
	})
}

// The agent delivered every mock before the stop and then went away; only the
// consumer was behind (it starts taking them after the stop). Nothing the
// agent said shows it held anything, and a complete recording must not fail
// for this CLI's own lag: said at WARN, the recording not failed.
func TestDrain_ConsumerLagIsNotAnAgentBacklog(t *testing.T) {
	f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock, 16)}
	agent := &backlogAgent{fakeInstr: f, answer: func() (models.AgentBacklog, error) {
		return models.AgentBacklog{Verdict: models.BacklogPending}, nil
	}}
	core, logs := observer.New(zapcore.WarnLevel)
	r := &Recorder{logger: zap.New(core), instrumentation: agent, config: &config.Config{}, frameQuiet: time.Hour}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	defer cancel()
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	go func() {
		for range frames.Incoming {
		}
	}()
	captured := time.Now()
	for i := 0; i < 16; i++ { // all delivered by the agent before the stop
		f.outgoing <- &models.Mock{Name: fmt.Sprintf("m-%d", i), Spec: models.MockSpec{ReqTimestampMock: captured}}
	}
	cancel()          // the stop
	close(f.outgoing) // the agent goes away, having handed over everything
	done := make(chan struct{})
	n := 0
	go func() {
		defer close(done)
		for range frames.Outgoing { // the consumer catches up only now
			n++
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the mock drain did not end with its stream")
	}
	if n != 16 || r.captureLost.Load() || logs.FilterLevelExact(zapcore.ErrorLevel).Len() != 0 {
		t.Fatalf("handed on %d of 16; captureLost=%v: a lagging consumer failed a complete recording: %v", n, r.captureLost.Load(), logs.All())
	}
}
