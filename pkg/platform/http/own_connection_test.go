package http

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.uber.org/zap"
)

// requestsOnAnOwnConnection are the requests that go on a connection of their
// own because they can be cancelled before they are answered (ownConnection).
var requestsOnAnOwnConnection = map[string]func(ctx context.Context, a *AgentClient, uri string){
	"the ask to end a round's streams": func(ctx context.Context, a *AgentClient, _ string) {
		_ = a.EndRecordingStreams(ctx, "rec-1")
	},
	"the watch for the agent's stop notice": func(ctx context.Context, a *AgentClient, _ string) {
		_, _ = a.awaitStopNotice(ctx)
	},
	"the ask whether the agent takes container lists": func(ctx context.Context, a *AgentClient, uri string) {
		_, _, _ = a.agentTakesContainers(ctx, uri)
	},
	"the container list": func(ctx context.Context, a *AgentClient, uri string) {
		_ = a.postContainers(ctx, uri, containerkey.Snapshot{})
	},
	"the network name list": func(ctx context.Context, a *AgentClient, uri string) {
		a.dockerClient = inspectOnly{}
		_ = a.postNetworkNames(ctx, uri, "keploy-v3", 0)
	},
}

// A request that runs beside the others and is answered with no body must not
// leave its cancel on a connection another request took (ownConnection). The
// trace holds the transport at the instant it has put the connection back in
// its pool, before the request has its answer, so what made this rare -- the
// connection pooled, the next request holding it, the cancel -- happens in that
// order on every run. Without a connection of its own the read below fails as
// the run's last read did: "failed to get scope windows ...: context canceled",
// with its own context never cancelled.
func TestACancelledRequestLeavesTheNextRequestAlone(t *testing.T) {
	for name, ask := range requestsOnAnOwnConnection {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/scope/windows" {
					_, _ = w.Write([]byte("[]"))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			a := &AgentClient{logger: zap.NewNop(), client: agentHTTPClient(""), conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}

			askCtx, cancelAsk := context.WithCancel(context.Background())
			defer cancelAsk()
			var pooled atomic.Bool
			read := make(chan error, 1)
			trace := &httptrace.ClientTrace{PutIdleConn: func(err error) {
				if err != nil || !pooled.CompareAndSwap(false, true) {
					return
				}
				tookConn := make(chan struct{})
				var took sync.Once
				readCtx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
					GotConn: func(httptrace.GotConnInfo) { took.Do(func() { close(tookConn) }) },
				})
				ended := make(chan error, 1)
				go func() {
					_, err := a.GetScopeWindows(readCtx)
					ended <- err
				}()
				<-tookConn
				cancelAsk()
				// The read has no answer while the transport is held here:
				// it ends only when the cancel closes the connection.
				read <- <-ended
			}}
			ask(httptrace.WithClientTrace(askCtx, trace), a, srv.URL)
			if !pooled.Load() {
				cancelAsk()
				_, err := a.GetScopeWindows(context.Background())
				read <- err
			}
			if err := <-read; err != nil {
				t.Fatalf("the request after it failed with its cancel, on a context nothing cancelled: %v", err)
			}
		})
	}
}

// peerThatNeverCloses is an agent that takes any number of requests on a
// connection and leaves ending it to the client: a connection of its ends only
// when the client closes it, and neither a bodiless answer nor a request's
// `Connection: close` ends one from this side.
type peerThatNeverCloses struct {
	ln     net.Listener
	answer string // what it answers each request; empty: it never answers
	asked  chan struct{}
	closed chan struct{} // gets one value per connection the client closed
	wg     sync.WaitGroup

	mu    sync.Mutex
	conns []net.Conn
}

func newPeerThatNeverCloses(t *testing.T, answer string) *peerThatNeverCloses {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &peerThatNeverCloses{ln: ln, answer: answer, asked: make(chan struct{}, 8), closed: make(chan struct{}, 8)}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.serve(conn)
				p.closed <- struct{}{}
			}()
		}
	}()
	// Here, not as each connection ends: a row that fails leaves its
	// connections open, and they have to go for serve to return.
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

// connections is how many connections the peer has taken.
func (p *peerThatNeverCloses) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// serve answers conn's requests until the client closes it.
func (p *peerThatNeverCloses) serve(conn net.Conn) {
	br := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		p.asked <- struct{}{}
		if p.answer != "" {
			if _, err := io.WriteString(conn, p.answer); err != nil {
				return
			}
		}
	}
}

// A connection of a request's own ends with the request (ownConnection), or
// the agent holds one for every round and every push of a long run, and the
// fix for a poisoned pool is a slow leak. Each shape runs the request twice, on
// a transport with no idle timeout: a connection kept, in the client's pool or
// in a pool only that one request could reach, is never closed, and this row
// never gets its close. Its wait is the event itself, with no budget of its
// own; `go test`'s -timeout names the row that a leak holds.
func TestAnOwnConnectionEndsWithItsRequest(t *testing.T) {
	requests := map[string]func(ctx context.Context, a *AgentClient, uri string){
		"the ask to stop the agent": func(_ context.Context, a *AgentClient, _ string) {
			_ = a.requestAgentStop()
		},
	}
	for name, ask := range requestsOnAnOwnConnection {
		requests[name] = ask
	}
	for name, ask := range requests {
		for shape, answer := range map[string]string{
			"answered with no body":        "HTTP/1.1 204 No Content\r\n\r\n",
			"answered with a body":         "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 2\r\n\r\nno",
			"cancelled before its answer":  "",
			"answered with an unread body": "HTTP/1.1 200 OK\r\nContent-Length: 8192\r\n\r\n" + strings.Repeat("x", 8192),
		} {
			if answer == "" && name == "the ask to stop the agent" {
				continue // Its context is its own: nothing cancels it from outside.
			}
			t.Run(name+"/"+shape, func(t *testing.T) {
				peer := newPeerThatNeverCloses(t, answer)
				uri := "http://" + peer.ln.Addr().String()
				a := &AgentClient{
					logger: zap.NewNop(),
					client: http.Client{Transport: &instanceTransport{base: &http.Transport{}}},
					conf:   &config.Config{Agent: config.Agent{AgentURI: uri}},
				}
				const times = 2
				for i := range times {
					// Not cancelled while the row waits for the close, but
					// for the shape that is about a cancel: a cancel closes
					// a connection itself, so it would hide a response body
					// nobody closed.
					ctx, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					ended := make(chan struct{})
					go func() {
						defer close(ended)
						ask(ctx, a, uri)
					}()
					<-peer.asked
					if answer == "" {
						cancel()
					}
					<-ended
					if got := peer.connections(); got != i+1 {
						t.Fatalf("request %d of %d found %d connections: it shared one", i+1, times, got)
					}
				}
				for range times {
					<-peer.closed
				}
			})
		}
	}
}
