package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// watchURI watches the agent at uri, known only by its URI, and returns the
// lines it logged at INFO and above, with what the watch said within 30 s of
// wall time (nil when it said nothing).
func watchURI(t *testing.T, uri string) (*observer.ObservedLogs, error) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case err := <-a.WatchAgentStart(ctx):
		return logs, err
	case <-time.After(30 * time.Second):
		return logs, nil
	}
}

// acceptOnly listens on a port of its own and hands each connection it takes
// to serve; it closes with the test.
func acceptOnly(t *testing.T, serve func(net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			serve(c)
		}
	}()
	return ln
}

// An agent known only by its URI that takes connections and never answers
// its health check healthy (an answer of another status, a forward in front
// of a port nothing listens on) is given up on once what keploy finds there
// has not changed for bootSilence of keploy's runnable time, saying what it
// found, at WARN: never a wait that ends only with its caller's context.
func TestAnAgentURIThatNeverAnswersHealthyIsGivenUpOn(t *testing.T) {
	cases := []struct {
		name string
		uri  func(t *testing.T) string
		want []string
	}{
		{
			name: "it answers, never healthy",
			uri: func(t *testing.T) string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "upstream connect error", http.StatusServiceUnavailable)
				}))
				t.Cleanup(srv.Close)
				return srv.URL + "/agent"
			},
			want: []string{"never answered its health check healthy", "answers 503, not healthy", "upstream connect error"},
		},
		{
			name: "it holds the connection, with no answer",
			uri: func(t *testing.T) string {
				// A frozen agent: the kernel takes the connection, and
				// nothing reads it. Each look waits 20 ms to begin with.
				first := agentLookFirstWait
				agentLookFirstWait = 20 * time.Millisecond
				t.Cleanup(func() { agentLookFirstWait = first })
				var held []net.Conn
				var mu sync.Mutex
				t.Cleanup(func() {
					mu.Lock()
					defer mu.Unlock()
					for _, c := range held {
						_ = c.Close()
					}
				})
				ln := acceptOnly(t, func(c net.Conn) {
					mu.Lock()
					defer mu.Unlock()
					held = append(held, c)
				})
				return "http://" + ln.Addr().String() + "/agent"
			},
			want: []string{"never answered its health check healthy", "holds them with no answer"},
		},
		{
			name: "a forward in front of nothing",
			uri: func(t *testing.T) string {
				ln := acceptOnly(t, func(c net.Conn) { _ = c.Close() })
				return "http://" + ln.Addr().String() + "/agent"
			},
			want: []string{"never answered its health check healthy", "closes them with no answer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastURILooks(t)
			uri := tc.uri(t)
			logs, err := watchURI(t, uri)
			if err == nil {
				t.Fatal("an agent that never answers healthy was waited on with no end")
			}
			if !errors.Is(err, pkg.ErrAgentNotReady) {
				t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
			}
			for _, w := range append([]string{uri}, tc.want...) {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("the watch said %q; want it to say %q", err, w)
				}
			}
			// What it found, said once: the look that found it got no
			// other answer before.
			if strings.Count(err.Error(), tc.want[1]) != 1 || strings.Contains(err.Error(), "its last answer") {
				t.Fatalf("the watch said %q; want what it found said once", err)
			}
			waiting := logs.FilterMessageSnippet("still waiting for the keploy agent at " + uri).All()
			if len(waiting) == 0 || waiting[0].ContextMap()["something_answers"] != true {
				t.Fatalf("said it still waits with %v; want that something answers there", waiting)
			}
			given := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("giving up on the keploy agent at " + uri)
			if given.Len() != 1 || !strings.Contains(given.All()[0].ContextMap()["error"].(string), tc.want[1]) {
				t.Fatalf("logged %d WARN lines that it gives up, with %v; want one, with why", given.Len(), given.All())
			}
		})
	}
}

// An agent that took connections and then takes none (it died, and nothing
// forwards to where it was) is given up on as one that stopped, saying what
// was there before: never as a URI where no agent ever was.
func TestAnAgentURIThatStopsIsGivenUpOnAsStopped(t *testing.T) {
	fastURILooks(t)
	// Its answer changes at every look while it lives (its start makes
	// progress), so only its stop can end the wait.
	var answers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable-int(answers.Add(1)%2))
	}))
	uri := srv.URL + "/agent"
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := a.WatchAgentStart(ctx)
	// Two looks find it answering (fastURILooks: a look a millisecond),
	// then it dies.
	deadline := time.Now().Add(10 * time.Second)
	for logs.FilterMessageSnippet("still waiting").Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	srv.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "the keploy agent at "+uri+" has stopped") || strings.Count(err.Error(), "its health check answers 50") != 1 || !strings.Contains(err.Error(), ", not healthy: \"starting\"") {
			t.Fatalf("the watch said %v; want that the agent at the URI has stopped, and what it answered before, once", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an agent that stopped was waited on with no end")
	}
}

// A token an embedder sealed into the agent's URI (k8s-proxy's
// keploy-agent://, its password) is never said: the watch's lines and its
// reason show it as xxxxx.
func TestAnAgentURIsSealedTokenIsNeverSaid(t *testing.T) {
	fastURILooks(t)
	uri := "http://:sealed-token@" + testnet.RefusedAddr(t, "tcp") + "/agent"
	logs, err := watchURI(t, uri)
	if err == nil {
		t.Fatal("a URI where nothing answers was waited on with no end")
	}
	if strings.Contains(err.Error(), "sealed-token") || !strings.Contains(err.Error(), ":xxxxx@") {
		t.Fatalf("the watch said %q; want the URI's password shown as xxxxx", err)
	}
	for _, l := range logs.All() {
		if strings.Contains(l.Message, "sealed-token") || strings.Contains(fmt.Sprint(l.ContextMap()), "sealed-token") {
			t.Fatalf("logged the URI's password: %q %v", l.Message, l.ContextMap())
		}
	}
}

// A look at an agent known only by its URI that ran out of time with no
// answer (held) makes the next look wait twice as long, up to
// agentLookMaxWait; any other look keeps the wait as it is.
func TestNextAgentLookWait(t *testing.T) {
	cases := []struct {
		wait time.Duration
		held bool
		want time.Duration
	}{
		{time.Second, false, time.Second},
		{time.Second, true, 2 * time.Second},
		{14 * time.Second, true, 28 * time.Second},
		{15 * time.Second, true, agentLookMaxWait},
		{16 * time.Second, true, agentLookMaxWait},
		{agentLookMaxWait, true, agentLookMaxWait},
		{agentLookMaxWait, false, agentLookMaxWait},
	}
	for _, tc := range cases {
		if got := nextAgentLookWait(tc.wait, tc.held); got != tc.want {
			t.Errorf("nextAgentLookWait(%s, held %v) = %s; want %s", tc.wait, tc.held, got, tc.want)
		}
	}
}

// The watch of an agent known only by its URI gives each look the wait the
// last look's left it: from agentLookFirstWait, twice as long after each look
// that held the connection with no answer, up to agentLookMaxWait, and as long
// after any other look, as keploy's own health check does, so an agent behind
// a slow path is found answering.
func TestAnAgentURILookWaitsLongerAfterEachHeldLook(t *testing.T) {
	fastURILooks(t)
	script := []agentLookKind{agentHeld, agentHeld, agentHeld, agentClosed, agentHeld, agentHeld, agentHeld, agentHeld}
	var (
		mu    sync.Mutex
		waits []time.Duration
		n     atomic.Int32
	)
	prev := lookAtAgent
	lookAtAgent = func(_ context.Context, _ *http.Client, _ string, wait time.Duration) agentLook {
		mu.Lock()
		defer mu.Unlock()
		defer n.Add(1)
		waits = append(waits, wait)
		if i := len(waits) - 1; i < len(script) {
			return agentLook{kind: script[i], found: "scripted"}
		}
		return agentLook{kind: agentHealthy, status: http.StatusOK, found: "scripted"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	watchScripted(t, &n, len(script)+2)
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 16 * time.Second, agentLookMaxWait, agentLookMaxWait, agentLookMaxWait, agentLookMaxWait}
	if fmt.Sprint(waits[:len(want)]) != fmt.Sprint(want) {
		t.Fatalf("the looks waited %v; want %v", waits[:len(want)], want)
	}
}

// watchScriptedLooks watches an agent known only by its URI whose looks find
// before, then then for good, and returns the count of looks, with what the
// watch said within 30 s of wall time (nil when it said nothing).
func watchScriptedLooks(t *testing.T, before []agentLook, then agentLook) (int32, error) {
	t.Helper()
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		if i := int(n.Add(1)) - 1; i < len(before) {
			return before[i]
		}
		return then
	}
	t.Cleanup(func() { lookAtAgent = prev })
	_, err := watchURI(t, "http://127.0.0.1:1/agent")
	return n.Load(), err
}

// The reason the watch gives up with says what the looks found before the
// last: an agent that answered healthy and then stalls no longer answers
// healthy (it is never said to have never answered so), and its last answer
// is said where the last look that found something take its connection got
// none, and only there.
func TestAnAgentURIsReasonSaysWhatItFoundBefore(t *testing.T) {
	healthy := agentLook{kind: agentHealthy, status: http.StatusOK, found: "its health check answers healthy"}
	starting := agentLook{kind: agentAnswered, status: http.StatusServiceUnavailable, found: "answers 503: starting"}
	held := agentLook{kind: agentHeld, found: "holds it"}
	closed := agentLook{kind: agentClosed, found: "closes it"}
	refused := agentLook{kind: agentRefused, found: "refuses it"}
	cases := []struct {
		name      string
		before    []agentLook
		then      agentLook
		want, not []string
	}{
		{
			name: "healthy, then held", before: []agentLook{starting, healthy}, then: held,
			want: []string{"no longer answers its health check healthy", ": holds it; its last answer: its health check answers healthy"},
			not:  []string{"never answered"},
		},
		{
			name: "answering, then closed", before: []agentLook{starting}, then: closed,
			want: []string{"never answered its health check healthy", ": closes it; its last answer: answers 503: starting"},
			not:  []string{"no longer"},
		},
		{
			name: "healthy, then stopped", before: []agentLook{starting, healthy}, then: refused,
			want: []string{"has stopped", "(refuses it); the last look that found something there: its health check answers healthy"},
			not:  []string{"its last answer"},
		},
		{
			name: "answering, then held, then stopped", before: []agentLook{starting, held}, then: refused,
			want: []string{"has stopped", "; the last look that found something there: holds it; its last answer: answers 503: starting"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastURILooks(t)
			_, err := watchScriptedLooks(t, tc.before, tc.then)
			if !errors.Is(err, pkg.ErrAgentNotReady) {
				t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("the watch said %q; want it to say %q", err, w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(err.Error(), w) {
					t.Fatalf("the watch said %q; want it not to say %q", err, w)
				}
			}
		})
	}
}

// The wait is given up on at the first look bootSilence of keploy's runnable
// time past the first look that found what the looks find since, however long
// keploy ran before the watch began: not at once, and not a look later.
func TestAnAgentURIIsGivenUpOnAtBootSilenceFromTheFirstLook(t *testing.T) {
	fastURILooks(t)
	clk := &steppingClock{}
	clk.Advance(time.Hour)
	uriClock = clk
	looks, err := watchScriptedLooks(t, nil, agentLook{kind: agentHeld, found: "holds it"})
	// A second of runnable time a look: the 6th is 5 s, bootSilence, past
	// the first.
	if !errors.Is(err, pkg.ErrAgentNotReady) || looks != 6 {
		t.Fatalf("the watch said %v after %d looks; want pkg.ErrAgentNotReady after 6", err, looks)
	}
}

// An agent URI its health check cannot be asked at (no scheme keploy can send
// on, or one that does not parse) is given up on as one where nothing
// answers, saying so: never a wait with no watch, which only its caller's
// context would end.
func TestAnAgentURIItsHealthCheckCannotBeAskedAtIsGivenUpOn(t *testing.T) {
	cases := map[string]string{"no scheme": "localhost:16789", "does not parse": "http://[::1:16789"}
	for name, uri := range cases {
		t.Run(name, func(t *testing.T) {
			fastURILooks(t)
			logs, err := watchURI(t, uri)
			if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "nothing answers at "+uri+":") {
				t.Fatalf("the watch said %v; want that nothing answers at %s", err, uri)
			}
			waiting := logs.FilterMessageSnippet("still waiting for the keploy agent at " + uri).All()
			if len(waiting) == 0 || waiting[0].ContextMap()["something_answers"] != false {
				t.Fatalf("said it still waits with %v; want that nothing answers there", waiting)
			}
		})
	}
}

// A look on a connection kept from an earlier look (the round tripper an
// embedder registered for its scheme keeps its connections, as k8s-proxy's
// does) that the agent then holds with no answer finds it held, not refused:
// the connection was taken, though no dial connected it for this look. The
// look waits its own wait for the answer.
func TestALookOnAKeptConnectionTheAgentHoldsFindsItHeld(t *testing.T) {
	var asked, conns atomic.Int32
	hold := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hold) })
	healthURL := registerScheme(t, "pod-token") + "://" + srv.Listener.Addr().String() + "/agent/health"
	client := &http.Client{Transport: ownConnections(http.DefaultTransport)}
	if l := lookAtAgentHealth(context.Background(), client, healthURL, 5*time.Second); l.kind != agentAnswered {
		t.Fatalf("the first look found %+v; want an answer", l)
	}
	// The look waits its own wait, not the first look's.
	first := agentLookFirstWait
	agentLookFirstWait = time.Hour
	t.Cleanup(func() { agentLookFirstWait = first })
	looked := make(chan agentLook, 1)
	go func() { looked <- lookAtAgentHealth(context.Background(), client, healthURL, 100*time.Millisecond) }()
	var l agentLook
	select {
	case l = <-looked:
	case <-time.After(30 * time.Second):
		t.Fatal("the look waited past its wait")
	}
	if conns.Load() != 1 {
		t.Fatalf("the looks took %d connections; want the second on the first's", conns.Load())
	}
	if l.kind != agentHeld {
		t.Fatalf("the look on the kept connection found %+v; want it held", l)
	}
}

// An answer of another status is a step of the agent's start: one whose
// health check answers 503, then 502, then 503 again, each for under
// bootSilence, is waited for, past bootSilence many times over, until it
// answers healthy.
func TestAnAgentURIWhoseAnswerChangesIsWaitedFor(t *testing.T) {
	fastURILooks(t)
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		i := int(n.Add(1)) - 1
		if i >= 24 {
			return agentLook{kind: agentHealthy, status: http.StatusOK, found: "scripted"}
		}
		// 4 looks at each status, under the 5 of bootSilence.
		return agentLook{kind: agentAnswered, status: http.StatusServiceUnavailable - (i/4)%2, found: "scripted"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	watchScripted(t, &n, 24+20)
}
