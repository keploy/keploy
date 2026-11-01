package http

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// The test-case stream's end marker carries what the agent's session lost
// (models.StreamEndSummary). For an agent this client did not start (docker
// compose's, kubelet's) the client says it in its own log: the agent's log
// reaches the user through compose, which can drop its last lines as the
// client tears the stack down, a moment after its drains end at the markers.
// For an agent it started, whose log is the user's own, it says nothing more.
// The requests a pause of the memory guard cost are said with their count and
// the number of connections they came on.
func TestTheClientSaysWhatTheSessionLostForAnAgentItDidNotStart(t *testing.T) {
	sum := models.StreamEndSummary{TestCasesSent: 3, TestCasesSuppressed: 9, LeftOutInFlight: 2, NotHandedOn: 4, UnnamedCallerConns: 6, TLSRelayedConns: 7, Unsettled: 1, MassSuppressed: true,
		NotRecordedUnderPressure: models.PressureSkipSummary{Requests: 5, Connections: 2}}
	const pauseSaid = "the keploy agent did not record some requests: it was short of memory when they came, and its memory guard had paused recording; each connection's requests after the pause are recorded"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+models.StreamEndPart+`"`)
		p, _ := mw.CreatePart(h)
		_ = json.NewEncoder(p).Encode(sum)
		_ = mw.Close()
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	for _, started := range []bool{false, true} {
		core, logs := observer.New(zap.WarnLevel)
		cfg := &config.Config{}
		cfg.Agent.AgentURI = srv.URL
		client := New(zap.New(core), nil, cfg)
		client.startedAgent.Store(started)
		ctx, cancel := context.WithCancel(context.Background())
		grp, gctx := errgroup.WithContext(ctx)
		gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
		tcs, err := client.GetIncoming(gctx, models.IncomingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case _, ok := <-tcs:
			if ok {
				t.Fatal("the stream carried a test case it never had")
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the stream did not end at its marker")
		}
		cancel()
		if !client.StreamEnded(models.TestCaseStream) {
			t.Fatal("the stream's end was not read as its marker")
		}
		said := map[string]bool{}
		for _, e := range logs.All() {
			said[e.Message] = true
			if e.Message == tlsRelayedConnsSaid {
				if f := e.ContextMap(); f["connections"] != uint64(7) || f["next_step"] == nil {
					t.Fatalf("the TLS connections relayed unread were said as %v, want 7 connections and a next step", f)
				}
			}
			if e.Message == pauseSaid {
				f := e.ContextMap()
				if f["requests"] != uint64(5) || f["connections"] != int64(2) {
					t.Fatalf("the pause's cost was said as %v, want 5 requests on 2 connections", f)
				}
			}
		}
		want := []string{
			"the keploy agent left some test cases out of the recording: each stayed in flight (a slow endpoint, a long poll, a stream) while mocks it may have made had to be let go",
			"the keploy agent saved some test cases before it had parsed all the traffic they overlap; if a connection's parser failed on it afterwards, they lack mocks",
			"the keploy agent left out a large share of this recording's test cases: it could not capture their mocks",
			pauseSaid,
			"the keploy agent lost some test cases: the app served their requests, and the agent's ingress stopped before their test cases reached the recording",
			unnamedCallerConnsSaid,
			tlsRelayedConnsSaid,
		}
		for _, m := range want {
			if !started && !said[m] {
				t.Fatalf("for an agent the client did not start it did not say %q (said %v)", m, said)
			}
		}
		if started && logs.Len() != 0 {
			t.Fatalf("for an agent the client started, whose log is the user's own, it said %v", logs.All())
		}
	}
}
