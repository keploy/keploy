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
func TestTheClientSaysWhatTheSessionLostForAnAgentItDidNotStart(t *testing.T) {
	sum := models.StreamEndSummary{TestCasesSent: 3, TestCasesSuppressed: 9, LeftOutInFlight: 2, Unsettled: 1, MassSuppressed: true}
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
		}
		want := []string{
			"the keploy agent left some test cases out of the recording: each stayed in flight (a slow endpoint, a long poll, a stream) while mocks it may have made had to be let go",
			"the keploy agent saved some test cases before it had parsed all the traffic they overlap; if a connection's parser failed on it afterwards, they lack mocks",
			"the keploy agent left out a large share of this recording's test cases: it could not capture their mocks",
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

// The marker's late losses (what test cases streamed with nothing lost had
// lost by the stream's end) are kept for the recording to take, whoever
// started the agent, and taken once.
func TestTheClientKeepsTheLateLossesTheMarkerCarriesForTheRecording(t *testing.T) {
	lost := models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: mysql exchange not recorded"}}
	sum := models.StreamEndSummary{TestCasesSent: 2, LateLosses: map[string]models.CaptureLosses{"test-2": lost}}
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
		cfg := &config.Config{}
		cfg.Agent.AgentURI = srv.URL
		client := New(zap.NewNop(), nil, cfg)
		client.startedAgent.Store(started)
		ctx, cancel := context.WithCancel(context.Background())
		grp, gctx := errgroup.WithContext(ctx)
		gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
		tcs, err := client.GetIncoming(gctx, models.IncomingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-tcs:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the stream did not end at its marker")
		}
		for range tcs {
		}
		cancel()
		got := client.TakeLateTestCaseLosses()
		if len(got) != 1 || len(got["test-2"]) != 1 || got["test-2"][0] != lost[0] {
			t.Fatalf("started %v: the client kept %+v, want test-2's %+v", started, got, lost)
		}
		if again := client.TakeLateTestCaseLosses(); len(again) != 0 {
			t.Fatalf("started %v: taken twice: %+v", started, again)
		}
	}
}
