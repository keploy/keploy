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

// watchURI watches the agent at uri, known only by its URI, and returns what
// the watch said within 30 s of wall time (nil when it said nothing), with
// the lines it logged at INFO and above.
func watchURI(t *testing.T, uri string) (error, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case err := <-a.WatchAgentStart(ctx):
		return err, logs
	case <-time.After(30 * time.Second):
		return nil, logs
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
			err, logs := watchURI(t, uri)
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
		if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "the keploy agent at "+uri+" has stopped") || !strings.Contains(err.Error(), "; its last answer: its health check answers 50") || !strings.Contains(err.Error(), ", not healthy: \"starting\"") {
			t.Fatalf("the watch said %v; want that the agent at the URI has stopped, and what it answered before", err)
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
	err, logs := watchURI(t, uri)
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
