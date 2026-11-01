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
	"go.uber.org/zap"
)

// Another client in keploy's process reaches the same agent on the default
// pool: enterprise's own client to the agent does, for its time-freeze window
// calls, which the agent answers with no body. Its cancel at the instant the
// transport has put such a request's connection back in the pool closes that
// connection under the request that took it next (golang/go#82547). The
// AgentClient's connections are a pool of its own (agentPool), so that request
// is never the AgentClient's: on the default pool, its read of what a run did
// failed with the other client's cancel, on a context nothing cancelled
// ("failed to get scope windows ...: context canceled"). The trace holds the
// other client's transport at that instant, the AgentClient's read takes a
// connection then, and the other client's request is cancelled, on every run.
func TestAnotherClientsCancelLeavesTheAgentClientsReadAlone(t *testing.T) {
	for _, token := range []string{"", "session-token"} {
		name := "New's client"
		if token != "" {
			name = "a client with the session's token"
		}
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/scope/windows" {
					_, _ = w.Write([]byte("[]"))
					return
				}
				w.WriteHeader(http.StatusNoContent) // the other client's answer: no body
			}))
			defer srv.Close()
			cfg := &config.Config{Agent: config.Agent{AgentURI: srv.URL}}
			a := New(zap.NewNop(), nil, cfg)
			if token != "" {
				a.client = agentHTTPClient(token)
			}
			// The other client: on http.DefaultTransport, as enterprise's
			// agentBearerTransport with its nil base is.
			other := &http.Client{}

			otherCtx, cancelOther := context.WithCancel(context.Background())
			defer cancelOther()
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
				cancelOther()
				// On the shared pool the read has no answer while the
				// transport is held here: it ends only when the cancel closes
				// the connection it took.
				read <- <-ended
			}}
			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(otherCtx, trace), http.MethodPost, srv.URL+"/time-freeze/window", nil)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := other.Do(req); err == nil {
				_ = res.Body.Close()
			}
			if !pooled.Load() {
				t.Fatal("precondition: the other client's connection was never put back in the pool")
			}
			if err := <-read; err != nil {
				t.Fatalf("the AgentClient's read failed with another client's cancel, on a context nothing cancelled: %v", err)
			}
		})
	}
}
