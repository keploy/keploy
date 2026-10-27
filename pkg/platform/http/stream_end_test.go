package http_test

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// streamEndAgent serves the agent's three frame streams, each with two frames
// and then its end marker, and then holds the stream open as an agent does:
// the client's stream ends at the marker, not at the connection's end. It
// counts the connections to each stream: a stream that ended at its marker
// is not resumed. It notes the headers each stream's request carried in
// headers, by stream.
func streamEndAgent(t *testing.T, conns map[string]*atomic.Int32, headers *sync.Map) *httptest.Server {
	t.Helper()
	hold := func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/outgoing", func(w http.ResponseWriter, r *http.Request) {
		conns["outgoing"].Add(1)
		headers.Store("outgoing", r.Header.Clone())
		enc := gob.NewEncoder(w)
		for _, n := range []string{"m-1", "m-2"} {
			_ = enc.Encode(models.Mock{Name: n, Kind: models.HTTP})
		}
		_ = enc.Encode(models.Mock{Kind: models.StreamEndKind})
		hold(w, r)
	})
	mux.HandleFunc("/mappings", func(w http.ResponseWriter, r *http.Request) {
		conns["mappings"].Add(1)
		headers.Store("mappings", r.Header.Clone())
		enc := json.NewEncoder(w)
		for _, n := range []string{"test-1", "test-2"} {
			_ = enc.Encode(models.TestMockMapping{TestName: n})
		}
		_ = enc.Encode(models.TestMockMapping{StreamEnd: true})
		hold(w, r)
	})
	mux.HandleFunc("/incoming", func(w http.ResponseWriter, r *http.Request) {
		conns["incoming"].Add(1)
		headers.Store("incoming", r.Header.Clone())
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		for _, n := range []string{"test-1", "test-2"} {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="metadata"`)
			p, _ := mw.CreatePart(h)
			_ = json.NewEncoder(p).Encode(models.TestCase{Name: n})
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+models.StreamEndPart+`"`)
		_, _ = mw.CreatePart(h)
		// The next boundary ends the marker's part, as the agent's close of
		// its writer does.
		_, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="delimiter"`}})
		hold(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The client reads each of the agent's frame streams to its end marker, says
// the stream ended so (StreamEnded), and neither forwards the marker nor
// resumes the stream: the recording's drain then knows every frame the agent
// had for it is read.
//
// Each stream's request names its recording (models.StreamEndTokenHeader) for
// an agent this client did not start too (docker compose's, kubelet's), whose
// streams it does not ask the agent to hold after a stop: the recording's
// request to end its streams reaches them by that name.
func TestTheClientReadsEachStreamToItsMarker(t *testing.T) {
	conns := map[string]*atomic.Int32{"outgoing": {}, "mappings": {}, "incoming": {}}
	var headers sync.Map
	srv := streamEndAgent(t, conns, &headers)
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
	gctx = models.WithStreamEndToken(models.WithDrainsAfterStop(gctx), "rec-1")
	client := httpclient.New(zap.NewNop(), nil, cfg)

	within := func(what string, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("%s: the stream did not end at its marker", what)
		}
	}

	mocks, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gotMocks []string
	within("mocks", func() {
		for m := range mocks {
			gotMocks = append(gotMocks, m.Name)
		}
	})
	if len(gotMocks) != 2 || !client.StreamEnded(models.MockStream) {
		t.Fatalf("mocks %v, ended by its marker %v; want the two mocks, and the marker seen", gotMocks, client.StreamEnded(models.MockStream))
	}

	maps, err := client.GetMappings(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gotMaps []string
	within("mappings", func() {
		for m := range maps {
			gotMaps = append(gotMaps, m.TestName)
		}
	})
	if len(gotMaps) != 2 || !client.StreamEnded(models.MappingStream) {
		t.Fatalf("mappings %v, ended by its marker %v; want the two mappings, and the marker seen", gotMaps, client.StreamEnded(models.MappingStream))
	}

	tcs, err := client.GetIncoming(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gotTCs []string
	within("test cases", func() {
		for tc := range tcs {
			gotTCs = append(gotTCs, tc.Name)
		}
	})
	if len(gotTCs) != 2 || !client.StreamEnded(models.TestCaseStream) {
		t.Fatalf("test cases %v, ended by its marker %v; want the two test cases, and the marker seen", gotTCs, client.StreamEnded(models.TestCaseStream))
	}

	for name, n := range conns {
		if got := n.Load(); got != 1 {
			t.Fatalf("the %s stream was opened %d times: a stream ended at its marker is not resumed", name, got)
		}
		v, _ := headers.Load(name)
		h, _ := v.(http.Header)
		if got := h.Get(models.StreamEndTokenHeader); got != "rec-1" {
			t.Fatalf("the %s stream named its recording %q, want %q: the request to end the recording's streams would not reach it", name, got, "rec-1")
		}
		if h.Get(models.DrainsAfterStopHeader) != "" {
			t.Fatalf("the %s stream asked an agent this client did not start to hold it after a stop", name)
		}
	}
}

// The client asks the agent to end its streams with a POST to the agent's
// route, and says when the agent does not take it.
func TestEndRecordingStreamsPostsToTheAgent(t *testing.T) {
	var asked atomic.Int32
	status := http.StatusNoContent
	var token atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == models.StreamEndPath {
			token.Store(r.URL.Query().Get("token"))
			asked.Add(1)
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL
	client := httpclient.New(zap.NewNop(), nil, cfg)
	if err := client.EndRecordingStreams(context.Background(), "rec/1 +"); err != nil || asked.Load() != 1 {
		t.Fatalf("the request: %v, asked %d times", err, asked.Load())
	}
	if got, _ := token.Load().(string); got != "rec/1 +" {
		t.Fatalf("the request named the recording %q, want %q", got, "rec/1 +")
	}
	status = http.StatusNotFound
	if err := client.EndRecordingStreams(context.Background(), "rec-1"); err == nil {
		t.Fatal("an agent that did not take the request was not said to")
	}
}
