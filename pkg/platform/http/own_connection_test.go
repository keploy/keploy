package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.uber.org/zap"
)

// A request that runs beside the others and is answered with no body must not
// leave its cancel on a connection another request took (ownConnection). The
// trace holds the transport at the instant it has put the connection back in
// its pool, before the request has its answer, so what made this rare -- the
// connection pooled, the next request holding it, the cancel -- happens in that
// order on every run. Without a connection of its own the read below fails as
// the run's last read did: "failed to get scope windows ...: context canceled",
// with its own context never cancelled.
func TestACancelledRequestLeavesTheNextRequestAlone(t *testing.T) {
	for name, ask := range map[string]func(ctx context.Context, a *AgentClient, uri string){
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
	} {
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
