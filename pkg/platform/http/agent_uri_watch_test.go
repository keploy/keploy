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
	"go.keploy.io/server/v3/utils/stall"
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
			// Healthy is 200 alone, as keploy's own health check, the
			// one whose answer ends the wait, takes it
			// (pkg.AgentHealthTicker): an agent that answers 204 never
			// ends the wait, so it is given up on.
			name: "it answers 204",
			uri: func(t *testing.T) string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))
				t.Cleanup(srv.Close)
				return srv.URL + "/agent"
			},
			want: []string{"never answered its health check healthy", "answers 204, not healthy"},
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
	// Each on http, and on a scheme an embedder registered on the default
	// transport (k8s-proxy's keploy-agent://), whose looks go through the
	// round tripper it registered: there too what the agent's server does is
	// what the watch says, never that nothing answers.
	for _, tc := range cases {
		for _, registered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, registered scheme %v", tc.name, registered), func(t *testing.T) {
				fastURILooks(t)
				uri := tc.uri(t)
				if registered {
					uri = registerScheme(t, "pod-token") + strings.TrimPrefix(uri, "http")
				}
				watchNeverHealthy(t, uri, tc.want)
			})
		}
	}
}

// watchNeverHealthy watches the agent at uri, which never answers healthy,
// and fails t unless the watch gives up on it saying want, what it found
// once, and that something answers there; and says why only by its error.
func watchNeverHealthy(t *testing.T, uri string, want []string) {
	t.Helper()
	logs, err := watchURI(t, uri)
	if err == nil {
		t.Fatal("an agent that never answers healthy was waited on with no end")
	}
	if !errors.Is(err, pkg.ErrAgentNotReady) {
		t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
	}
	for _, w := range append([]string{uri}, want...) {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("the watch said %q; want it to say %q", err, w)
		}
	}
	// What it found, said once: the look that found it got no
	// other answer before.
	if strings.Count(err.Error(), want[1]) != 1 || strings.Contains(err.Error(), "its last answer") || strings.Contains(err.Error(), "nothing answers") {
		t.Fatalf("the watch said %q; want what it found said once", err)
	}
	waiting := logs.FilterMessageSnippet("still waiting for the keploy agent at " + uri).All()
	if len(waiting) == 0 || waiting[0].ContextMap()["something_answers"] != true {
		t.Fatalf("said it still waits with %v; want that something answers there", waiting)
	}
	// Why it gives up is said once, by its error: its caller
	// ends its run with it.
	if said := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zapcore.WarnLevel }); said.Len() != 0 {
		t.Fatalf("logged %v; want why it gives up said by its error alone", said.All())
	}
}

// An agent that took connections and then takes none (it died, and nothing
// forwards to where it was) is given up on as one that stopped, saying what
// was there before: never as a URI where no agent ever was.
func TestAnAgentURIThatStopsIsGivenUpOnAsStopped(t *testing.T) {
	fastURILooks(t)
	// keploy's runnable time stands still while it lives (a box that has
	// not run keploy since), so only its stop, and the time that runs
	// after it, can end the wait.
	clk := &stall.ManualClock{}
	uriClock = clk
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
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
	deadline = time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-errs:
			if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "the keploy agent at "+uri+" has stopped") || strings.Count(err.Error(), "its health check answers 503, not healthy: \"starting\"") != 1 {
				t.Fatalf("the watch said %v; want that the agent at the URI has stopped, and what it answered before, once", err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("an agent that stopped was waited on with no end")
		}
		clk.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

// A token an embedder sealed into the agent's URI (k8s-proxy's
// keploy-agent://, its password) is never said, nor any part of it: the
// watch's lines, at any level, and its reason show it as xxxxx, in a URI that
// parses and in one that does not (a bad escape in the token), where what
// does not parse is said with no part of the token either.
func TestAnAgentURIsSealedTokenIsNeverSaid(t *testing.T) {
	cases := map[string]struct{ uri, shown string }{
		"a URI that parses":         {"http://:s3aled-t0ken@%s/agent", "http://:xxxxx@%s/agent"},
		"a URI that does not parse": {"keploy-agent://:s3aled%%zzt0ken@%s/agent", "keploy-agent://xxxxx@%s/agent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fastURILooks(t)
			addr := testnet.RefusedAddr(t, "tcp")
			core, logs := observer.New(zapcore.DebugLevel)
			a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: fmt.Sprintf(tc.uri, addr)}})
			var err error
			select {
			case err = <-a.WatchAgentStart(context.Background()):
			case <-time.After(30 * time.Second):
				t.Fatal("a URI where nothing answers was waited on with no end")
			}
			if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "nothing answers at "+fmt.Sprintf(tc.shown, addr)+":") {
				t.Fatalf("the watch said %v; want that nothing answers at the URI, its password shown as xxxxx", err)
			}
			said := []string{err.Error()}
			for _, l := range logs.All() {
				said = append(said, l.Message, fmt.Sprint(l.ContextMap()))
			}
			for _, s := range said {
				for _, part := range []string{"s3aled", "t0ken", "%zz"} {
					if strings.Contains(s, part) {
						t.Fatalf("said %q, with a part of the URI's password (%s)", s, part)
					}
				}
			}
		})
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
// after any other look (one that found it closed, or an answer), as keploy's
// own health check does, so an agent behind a slow path is found answering.
func TestAnAgentURILookWaitsLongerAfterEachHeldLook(t *testing.T) {
	fastURILooks(t)
	script := []agentLookKind{agentHeld, agentHeld, agentHeld, agentClosed, agentHeld, agentAnswered, agentHeld, agentHeld}
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
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 16 * time.Second, 16 * time.Second, agentLookMaxWait, agentLookMaxWait, agentLookMaxWait}
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

// An answer of a status the wait has not seen is a step of the agent's
// start: one whose health check answers 599, then 598, and so on, each for
// under bootSilence, is waited for, past bootSilence many times over, until
// it answers healthy.
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
		return agentLook{kind: agentAnswered, status: 599 - i/4, found: "scripted"}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	watchScripted(t, &n, 24+20)
}

// An agent that never answers healthy and whose answers go back and forth
// (503, then 502, then 503 again...) makes no progress past its second
// status: the wait is given up on bootSilence of keploy's runnable time
// after the first look that found 502, at the 10th look (a second a look),
// as never healthy; never waited for as long as the answers keep turning.
func TestAnAgentURIWhoseAnswersGoBackAndForthIsGivenUpOn(t *testing.T) {
	fastURILooks(t)
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, *http.Client, string, time.Duration) agentLook {
		i := int(n.Add(1)) - 1
		// 4 looks at each status, under the 5 of bootSilence.
		status := http.StatusServiceUnavailable - (i/4)%2
		return agentLook{kind: agentAnswered, status: status, found: fmt.Sprintf("its health check answers %d, not healthy", status)}
	}
	t.Cleanup(func() { lookAtAgent = prev })
	_, err := watchURI(t, "http://127.0.0.1:1/agent")
	if looks := n.Load(); !errors.Is(err, pkg.ErrAgentNotReady) || looks != 10 || !strings.Contains(err.Error(), "never answered its health check healthy") {
		t.Fatalf("the watch said %v after %d looks; want that it never answered healthy, after 10", err, looks)
	}
}

// A look whose dial is never taken within the look's wait (an address that
// drops what is sent to it: a deleted pod's stale IP, a network policy's
// drop) finds that nothing takes a connection there, as a refused one does:
// never that something holds it, and the next look waits as long as it did.
func TestALookWhoseDialIsNeverTakenFindsNothingThere(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			return nil, errors.New("the test is over")
		}
	}}}
	wait := 50 * time.Millisecond
	l := lookAtAgentHealth(context.Background(), client, "http://192.0.2.1:9/agent/health", wait)
	if l.kind != agentRefused || !strings.Contains(l.found, "nothing takes a connection there") {
		t.Fatalf("the look found %+v; want that nothing takes a connection there", l)
	}
	if next := nextAgentLookWait(wait, l.kind == agentHeld); next != wait {
		t.Fatalf("the next look waits %s; want %s, as this one did", next, wait)
	}
}

// Each look at an agent known only by its URI, on http, goes on a connection
// of its own: never one kept from an earlier request on the default
// transport, which keploy's own health check sends on (pkg.AgentHealthTicker),
// so a look cut off at its wait's end never closes a connection another
// request may be on (ownConnections).
func TestEachLookAtAnAgentURIIsOnAConnectionOfItsOwn(t *testing.T) {
	fastURILooks(t)
	var asked, conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	if _, err := watchURI(t, srv.URL+"/agent"); !errors.Is(err, pkg.ErrAgentNotReady) {
		t.Fatalf("the watch said %v; want pkg.ErrAgentNotReady", err)
	}
	if asked.Load() < 2 || conns.Load() < asked.Load() {
		t.Fatalf("%d looks took %d connections; want one each", asked.Load(), conns.Load())
	}
}
