package record

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// endingAgent is an agent that ends its streams on request, each with a
// marker (models.AgentBacklog.EndMarkers): it says "clear" once the test sets
// clear, and on the client's request to end its streams (asked) the test
// plays what such an agent then does -- puts on each stream what it still had,
// however late a starved agent gets to it, then its marker (end).
type endingAgent struct {
	*fakeInstr
	clear atomic.Bool
	// clears counts the "clear" answers: one for each frame drain's look.
	clears   atomic.Int32
	asked    chan struct{}
	askOnce  sync.Once
	asks     atomic.Int32
	asking   atomic.Value // string: the token of the latest request
	endMu    sync.Mutex
	endState map[models.FrameStream]bool
	// tokens, when set, gets the token each stream is opened with.
	tokens chan string
}

func newEndingAgent() *endingAgent {
	return &endingAgent{
		fakeInstr: &fakeInstr{
			mappings: make(chan models.TestMockMapping),
			incoming: make(chan *models.TestCase),
			outgoing: make(chan *models.Mock),
		},
		asked:    make(chan struct{}),
		endState: map[models.FrameStream]bool{},
	}
}

func (e *endingAgent) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	if !e.clear.Load() {
		return models.AgentBacklog{Verdict: models.BacklogPending, EndMarkers: true}, nil
	}
	e.clears.Add(1)
	return models.AgentBacklog{Verdict: models.BacklogClear, EndMarkers: true}, nil
}

func (e *endingAgent) EndRecordingStreams(_ context.Context, token string) error {
	e.asking.Store(token)
	e.asks.Add(1)
	e.askOnce.Do(func() { close(e.asked) })
	return nil
}

// token is the token the client's latest request to end its streams named.
func (e *endingAgent) token() string {
	tok, _ := e.asking.Load().(string)
	return tok
}

func (e *endingAgent) GetIncoming(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	e.opened(ctx)
	return e.fakeInstr.GetIncoming(ctx, opts)
}

func (e *endingAgent) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	e.opened(ctx)
	return e.fakeInstr.GetOutgoing(ctx, opts)
}

func (e *endingAgent) GetMappings(ctx context.Context, opts models.IncomingOptions) (<-chan models.TestMockMapping, error) {
	e.opened(ctx)
	return e.fakeInstr.GetMappings(ctx, opts)
}

// opened notes the token a stream is opened with: what the agent client puts
// on the stream's request (models.StreamEndTokenHeader).
func (e *endingAgent) opened(ctx context.Context) {
	if e.tokens != nil {
		e.tokens <- models.StreamEndToken(ctx)
	}
}

func (e *endingAgent) StreamEnded(kind models.FrameStream) bool {
	e.endMu.Lock()
	defer e.endMu.Unlock()
	return e.endState[kind]
}

// end ends kind's stream with its marker: the client's reader sees the
// marker (StreamEnded), and closes the channel.
func (e *endingAgent) end(kind models.FrameStream, ch func()) {
	e.endMu.Lock()
	e.endState[kind] = true
	e.endMu.Unlock()
	ch()
}

// A frame the agent has when it says "clear" -- on its way to this client,
// or a mapping it gets to only later, being starved -- is read when it comes:
// the drains end at the agent's markers, not on a quiet bound. Before, the
// frame drains closed their streams and waited 250 ms for what was already on
// its way, and the mapping drain ended once its stream had been quiet for 3 s
// after them: a mock 500 ms late, and a mapping 4 s late, were lost, and the
// recording said nothing.
//
// Under docker compose too: the agent there is not one this client started
// (it holds no stop for the client), and its streams still end at the
// markers its recording asked for, by the recording's token.
func TestTheDrainsReadEachStreamToTheAgentsMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{name: "native", command: ""},
		{name: "docker compose", command: string(utils.DockerCompose)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEndingAgent()
			cfg := &config.Config{CommandType: tc.command}
			core, logs := observer.New(zap.DebugLevel)
			r := &Recorder{logger: zap.New(core), instrumentation: e, config: cfg}
			g, gctx := errgroup.WithContext(context.Background())
			ctx, stop := context.WithCancel(gctx)
			ctx = context.WithValue(ctx, models.ErrGroupKey, g)
			frames, err := r.GetTestAndMockChans(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { stop(); frames.Abandon() })

			var mu sync.Mutex
			var mocks, mapped, tests []string
			var consumers sync.WaitGroup
			consumers.Add(3)
			go func() {
				defer consumers.Done()
				for m := range frames.Outgoing {
					mu.Lock()
					mocks = append(mocks, m.Name)
					mu.Unlock()
				}
			}()
			go func() {
				defer consumers.Done()
				for m := range frames.Mappings {
					mu.Lock()
					mapped = append(mapped, m.TestName)
					mu.Unlock()
				}
			}()
			go func() {
				defer consumers.Done()
				for tc := range frames.Incoming {
					mu.Lock()
					tests = append(tests, tc.Name)
					mu.Unlock()
				}
			}()

			captured := time.Now()
			// offer puts a frame on a stream, and reports whether the client
			// took it: one the client no longer reads is lost.
			offer := func(f func()) bool {
				taken := make(chan struct{})
				go func() { f(); close(taken) }()
				select {
				case <-taken:
					return true
				case <-time.After(10 * time.Second): // a client still reading takes it at once
					return false
				}
			}
			if !offer(func() { e.incoming <- tcAt("test-1", captured) }) {
				t.Fatal("setup: the test case the agent captured before the stop was never taken")
			}
			stop()
			if !offer(func() { e.outgoing <- mockAt("held", captured) }) {
				t.Fatal("setup: the mock the agent held at the stop was never taken")
			}
			e.clear.Store(true)
			deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
			for e.clears.Load() < 2 {               // each frame drain has been told "clear"
				if time.Now().After(deadline) {
					t.Fatal("the frame drains never asked the agent what it still held")
				}
				time.Sleep(time.Millisecond)
			}
			// What a starved agent then puts on its streams, late, before
			// each marker: a mock already on its way at the "clear", past the
			// 250 ms a closed stream was given ...
			time.Sleep(500 * time.Millisecond)
			if !offer(func() { e.outgoing <- mockAt("in-transit", captured) }) {
				// The client stopped reading: the frames it was not there
				// for are lost (left on their way: the streams stay open).
				t.Fatal("the mock on its way at the stop's clear was lost: the client had stopped reading its stream")
			}
			select {
			case <-e.asked:
			case <-time.After(time.Minute): // only turns a hang into a failure
				t.Fatal("the client never asked the agent to end its streams")
			}
			if got := e.token(); got == "" {
				t.Fatal("the client asked the agent to end its streams without naming its recording")
			}
			e.end(models.MockStream, func() { close(e.outgoing) })
			e.end(models.TestCaseStream, func() { close(e.incoming) })
			// ... and a mapping it gets to past the 3 s the mapping stream
			// was given once the frame drains were done.
			time.Sleep(4 * time.Second)
			if !offer(func() {
				e.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"held", "in-transit"}}
			}) {
				t.Fatal("the mapping a starved agent got to late was lost: the client had stopped reading its stream")
			}
			e.end(models.MappingStream, func() { close(e.mappings) })

			consumers.Wait()
			if err := g.Wait(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(tests) != 1 || len(mocks) != 2 || len(mapped) != 1 {
				t.Fatalf("saved test cases %v, mocks %v and mappings %v; want the test case, both mocks and the mapping", tests, mocks, mapped)
			}
			if n := e.asks.Load(); n != 1 {
				t.Fatalf("the client asked the agent to end its streams %d times, want once", n)
			}
			early := logs.Filter(func(e observer.LoggedEntry) bool {
				return e.Level >= zap.WarnLevel && strings.Contains(e.Message, "ended its")
			})
			if early.Len() != 0 {
				t.Fatalf("a stream the agent ended at its marker was said to be ended early: %v", early.All())
			}
			// Each drain says it read its stream to the marker: what the
			// lanes check, in keploy's own log.
			for _, frames := range []string{"test cases", "mocks", "mappings"} {
				said := logs.FilterMessage(StreamEndedLog).FilterField(zap.String("frames", frames)).Len()
				if said != 1 {
					t.Fatalf("the %s drain said %d times that it read its stream to the agent's marker, want once", frames, said)
				}
			}
		})
	}
}

// Each recording asks the agent to end its own streams: by a token of its
// own, carried on each stream's request, so another recording's streams on
// the same agent are not ended by its stop.
func TestEachRecordingNamesItsOwnStreams(t *testing.T) {
	tokens := map[string]bool{}
	for i := 0; i < 2; i++ {
		e := newEndingAgent()
		e.tokens = make(chan string, 3)
		r := &Recorder{logger: zap.NewNop(), instrumentation: e, config: &config.Config{}}
		g, gctx := errgroup.WithContext(context.Background())
		ctx, stop := context.WithCancel(gctx)
		ctx = context.WithValue(ctx, models.ErrGroupKey, g)
		frames, err := r.GetTestAndMockChans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		stop()
		frames.Abandon()
		close(e.incoming)
		close(e.outgoing)
		close(e.mappings)
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		close(e.tokens)
		var mine string
		for tok := range e.tokens {
			if tok == "" || (mine != "" && tok != mine) {
				t.Fatalf("a recording's streams carried tokens %q and %q: want one, its own", mine, tok)
			}
			mine = tok
		}
		if tokens[mine] {
			t.Fatalf("two recordings named their streams %q", mine)
		}
		tokens[mine] = true
	}
}

// An agent that cannot end its streams (it never says it can) keeps today's
// drain: the client never asks it, and ends the streams on quiet, as before.
func TestAnAgentThatCannotEndItsStreamsIsNeverAsked(t *testing.T) {
	e := newEndingAgent()
	quiet := &quietAgent{endingAgent: e}
	cfg := &config.Config{}
	r := &Recorder{logger: zap.NewNop(), instrumentation: quiet, config: cfg, frameQuiet: 50 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, stop := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(); frames.Abandon() })
	go func() {
		for range frames.Outgoing { //nolint:revive // draining
		}
	}()
	go func() {
		for range frames.Incoming { //nolint:revive // draining
		}
	}()
	go func() {
		for range frames.Mappings { //nolint:revive // draining
		}
	}()
	stop()
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drains did not end on quiet")
	}
	if n := e.asks.Load(); n != 0 {
		t.Fatalf("an agent that cannot end its streams was asked to %d times", n)
	}
}

// quietAgent says "clear" without saying it ends its streams: an agent from
// before they could.
type quietAgent struct{ *endingAgent }

func (*quietAgent) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}
