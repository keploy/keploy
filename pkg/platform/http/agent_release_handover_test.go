package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// handoverAgent is a stand-in compose agent for the handover of the app's
// release to a restarted agent's setup: what /agent/ready answers ("wait" or
// "ok"), the calls it took in order, and a signal for each release asked.
type handoverAgent struct {
	a      *AgentClient
	s      *agentSession
	c      *scriptedContainer
	answer atomic.Value
	asked  chan struct{}
	// hold, when set, holds each /agent/ready until it is closed, or until
	// the row ends (ended).
	hold  chan struct{}
	ended chan struct{}

	mu    sync.Mutex
	calls []string
}

func newHandoverAgent(t *testing.T, answer string, hold chan struct{}) *handoverAgent {
	t.Helper()
	ag := &handoverAgent{c: &scriptedContainer{}, asked: make(chan struct{}, 16), hold: hold, ended: make(chan struct{})}
	ag.answer.Store(answer)
	ag.c.install(t)
	// The agent's exit is the only verdict here.
	bootSilence = time.Hour
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/ready"):
			select {
			case ag.asked <- struct{}{}:
			default:
			}
			if ag.hold != nil {
				select {
				case <-ag.hold:
				case <-ag.ended:
				}
			}
			if ag.answer.Load().(string) == "ok" {
				ag.note("release")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/health"):
			// Read under the switch only, and agreeing.
			healthSays("on", http.StatusOK)(w, 0)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	// Cleanups run last first, so this one lets a held answer go before
	// srv.Close waits for every handler: a row that fails before it opens
	// its hold fails, rather than hanging the test binary.
	t.Cleanup(func() { close(ag.ended) })
	ag.s = storeSession(t, srv.URL, true)
	ag.a = clientFor(srv.URL)
	ag.a.agentContainer = &namedContainer{name: "keploy-v3-test"}
	ag.c.set(true, utils.BootProgress{Beat: 1, Seq: 1, Ready: true}, true)
	return ag
}

func (ag *handoverAgent) note(call string) {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.calls = append(ag.calls, call)
}

func (ag *handoverAgent) took() []string {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return append([]string(nil), ag.calls...)
}

func (ag *handoverAgent) awaitAsked(t *testing.T) {
	t.Helper()
	select {
	case <-ag.asked:
	case <-time.After(30 * time.Second):
		t.Fatal("the release never asked the agent")
	}
}

func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not end", what)
	}
	return nil
}

// A release in flight when a restarted agent's setup reads what to make again
// (reapply) is made again on that agent, last, however late the release then
// learns that the agent it asked is gone. Its watch's verdict, and the look
// that verdict waits for, can come after the setup began: recorded only as it
// ended, the release was not among what the setup made again, the agent
// compose started in its place was never released, and the app behind it
// never started.
func TestAReleaseInFlightWhenARestartedAgentsSetupBeginsIsMadeAgainLast(t *testing.T) {
	ag := newHandoverAgent(t, "wait", nil)
	entered, goOn := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ag.s.record(stepMockOutgoing, func(context.Context) error {
		ag.note("mocks")
		once.Do(func() { close(entered) })
		<-goOn
		return nil
	})

	released := make(chan error, 1)
	go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
	ag.awaitAsked(t)

	setUp := make(chan error, 1)
	go func() { setUp <- ag.s.reapply(context.Background(), zap.NewNop()) }()
	select {
	case <-entered: // the setup has read what to make again
	case <-time.After(30 * time.Second):
		t.Fatal("the restarted agent's setup never made the run's calls again")
	}

	// Only now does the release see the agent it asked gone.
	ag.c.exit(137)
	if err := awaitErr(t, released, "the release"); !errors.Is(err, errReleaseCut) {
		t.Fatalf("the release ended with %v; want it cut by the agent's exit", err)
	}

	// compose's retry started the agent again, which takes a release.
	ag.c.mu.Lock()
	ag.c.running, ag.c.ended, ag.c.code = true, false, 0
	ag.c.mu.Unlock()
	ag.answer.Store("ok")
	close(goOn)
	if err := awaitErr(t, setUp, "the restarted agent's setup"); err != nil {
		t.Fatalf("setting the restarted agent up: %v", err)
	}
	if got := ag.took(); strings.Join(got, ",") != "mocks,release" {
		t.Fatalf("the restarted agent took %v; want its mocks, then the release", got)
	}
}

// While a hold is on (compose's bring-up retry may be starting the agent
// again), a release the run asked for sends nothing to the agent, and the
// release a restarted agent's setup makes is not held. Once the hold is let
// go, the release the run asked for goes on.
func TestAHeldReleaseSendsNothingUntilTheHoldIsLetGo(t *testing.T) {
	ag := newHandoverAgent(t, "ok", nil)
	resume := ag.s.holdReleases(context.Background())

	released := make(chan error, 1)
	go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
	select {
	case <-ag.asked:
		t.Fatal("a release the run asked for reached the agent while the releases were held")
	case err := <-released:
		t.Fatalf("a held release ended (%v) before its hold was let go", err)
	case <-time.After(300 * time.Millisecond):
	}

	// The restarted agent's setup releases it, hold or not.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ag.a.makeAgentReady(ctx, true); err != nil {
		t.Fatalf("the setup's release: %v", err)
	}
	if got := ag.took(); strings.Join(got, ",") != "release" {
		t.Fatalf("the agent took %v while the run's release was held; want the setup's release alone", got)
	}

	resume(nil)
	if err := awaitErr(t, released, "the release the hold let go"); err != nil {
		t.Fatalf("the release the hold let go ended with %v", err)
	}
	if got := ag.took(); strings.Join(got, ",") != "release,release" {
		t.Fatalf("the agent took %v; want the setup's release, then the run's", got)
	}
}

// Letting the hold go is what wakes a release it held: with nothing else
// changing in the session, the release goes on as soon as the hold is let go.
func TestLettingAHoldGoWakesTheReleaseItHeld(t *testing.T) {
	ag := newHandoverAgent(t, "ok", nil)
	resume := ag.s.holdReleases(context.Background())

	released := make(chan error, 1)
	go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
	// The release is held by now: it waits for the hold's end alone.
	select {
	case <-ag.asked:
		t.Fatal("a release the run asked for reached the agent while the releases were held")
	case err := <-released:
		t.Fatalf("a held release ended (%v) before its hold was let go", err)
	case <-time.After(300 * time.Millisecond):
	}

	resume(nil)
	if err := awaitErr(t, released, "the release the hold let go"); err != nil {
		t.Fatalf("the release the hold let go ended with %v", err)
	}
	if got := ag.took(); strings.Join(got, ",") != "release" {
		t.Fatalf("the agent took %v; want the run's release", got)
	}
}

// A hold begins only once no release has its request out: one already sent
// may reach the agent the retry starts again. It begins once the answer is
// back, the release going on (held) or not.
func TestAHoldWaitsForAReleasesRequestOut(t *testing.T) {
	t.Run("an answer that releases", func(t *testing.T) { holdWaitsForTheRequestOut(t, "ok") })
	t.Run("an answer that says not yet", func(t *testing.T) { holdWaitsForTheRequestOut(t, "wait") })
}

func holdWaitsForTheRequestOut(t *testing.T, answer string) {
	gate := make(chan struct{})
	ag := newHandoverAgent(t, answer, gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	released := make(chan error, 1)
	go func() { released <- ag.a.MakeAgentReadyForDockerCompose(ctx) }()
	ag.awaitAsked(t)

	held := make(chan func(error), 1)
	go func() { held <- ag.s.holdReleases(context.Background()) }()
	select {
	case resume := <-held:
		resume(nil)
		t.Fatal("the hold began while a release's request was out")
	case <-time.After(300 * time.Millisecond):
	}

	close(gate)
	// The answer lets the hold begin, while a release told "not yet" goes on
	// (held), and ends only with the run.
	select {
	case resume := <-held:
		resume(nil)
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not begin once the release's request was back")
	}
	if answer == "ok" {
		if err := awaitErr(t, released, "the release"); err != nil {
			t.Fatalf("the release ended with %v", err)
		}
		return
	}
	cancel()
	if err := awaitErr(t, released, "the release"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the release ended with %v; want the run's end", err)
	}
}

// A release the agent refused for good takes back the release step it
// recorded as it began, but never one an earlier release recorded: the agent
// compose starts in place of one that took the run's release must take it.
func TestARefusedReleaseKeepsTheReleaseAnEarlierOneRecorded(t *testing.T) {
	ag := newHandoverAgent(t, "ok", nil)
	if err := ag.a.MakeAgentReadyForDockerCompose(context.Background()); err != nil {
		t.Fatalf("the first release: %v", err)
	}
	for len(ag.asked) > 0 {
		<-ag.asked
	}
	ctx, cancel := context.WithCancel(context.Background())
	ag.answer.Store("wait")
	released := make(chan error, 1)
	go func() { released <- ag.a.MakeAgentReadyForDockerCompose(ctx) }()
	ag.awaitAsked(t)
	cancel() // the run's end cuts it: owed to no one
	if err := awaitErr(t, released, "the second release"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the second release ended with %v; want the run's end", err)
	}
	if !ag.s.hasReady() {
		t.Fatal("a release cut by the run's end took back the release an earlier one recorded")
	}
}

// hasReady reports whether a release step is recorded.
func (s *agentSession) hasReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, step := range s.steps {
		if step.kind == stepAgentReady {
			return true
		}
	}
	return false
}
