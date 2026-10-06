package record

import (
	"context"
	"fmt"
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

func (p *pendingAgent) PendingBefore(context.Context, time.Time) (bool, bool, error) {
	p.asked.Add(1)
	return p.pending.Load(), true, nil
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
