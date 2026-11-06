package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func clientOf(uri string) *AgentClient {
	cfg := &config.Config{}
	cfg.Agent.AgentURI = uri
	return New(zap.NewNop(), nil, cfg)
}

// A stream's request is made anew each time, for a restarted agent: a POST with
// its JSON body whole each time, a GET with neither body nor Content-Type.
func TestStreamRequestOfMakesTheSameRequestEachTime(t *testing.T) {
	ctx := context.Background()
	a := clientOf("http://agent")
	newRequest := a.streamRequestOf(http.MethodPost, "/outgoing", []byte(`{"a":1}`))
	for i := 0; i < 2; i++ {
		req, err := newRequest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if req.Method != http.MethodPost || req.URL.String() != "http://agent/outgoing" {
			t.Errorf("request %d: %s %s", i, req.Method, req.URL)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("request %d: Content-Type = %q", i, got)
		}
		b, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != `{"a":1}` {
			t.Errorf("request %d: body = %q", i, b)
		}
	}

	req, err := a.streamRequestOf(http.MethodGet, "/pcap", nil)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := req.Header["Content-Type"]; ok {
		t.Errorf("a GET's Content-Type = %q", got)
	}
	if req.Body != nil && req.Body != http.NoBody {
		t.Errorf("a GET has a body: %v", req.Body)
	}
}

// A request that cannot be made is an error, said as such, not a request that
// goes out without what it asks.
func TestIncomingRequestFailsAsItsRequestDoes(t *testing.T) {
	a := clientOf("http://bad host")
	if req, err := a.incomingRequest([]byte("{}"))(context.Background()); err == nil {
		t.Fatalf("no error, request %v", req)
	}
	_, err := a.GetIncoming(context.Background(), models.IncomingOptions{})
	if err == nil || !strings.Contains(err.Error(), "error creating request for incoming request") {
		t.Fatalf("GetIncoming: %v", err)
	}
}

// A request that cannot be made will not be made by trying again, unlike one
// that met an agent mid-start: reopenStream says so at once.
func TestReopenStreamDoesNotRetryARequestItCannotMake(t *testing.T) {
	boom := errors.New("no request")
	calls := 0
	_, err := clientOf("http://agent").reopenStream(context.Background(), "incoming", func(context.Context) (*http.Request, error) {
		calls++
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("tried %d times", calls)
	}
}

// Each attempt to reopen a stream is a request of its own: an agent that dropped
// the first attempt gets the next with its body whole and what the stream asks.
// One request kept across attempts would go out with its body already read.
func TestReopenStreamMakesItsRequestAgainForEachAttempt(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the body: %v", err)
		}
		mu.Lock()
		got = append(got, r.Header.Get(models.BodyEncodingHeader)+" "+string(b))
		n := len(got)
		mu.Unlock()
		if n > 1 {
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("drop the first attempt: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	a := clientOf(srv.URL)
	res, err := a.reopenStream(context.Background(), "incoming", a.incomingRequest([]byte(`{"a":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	mu.Lock()
	defer mu.Unlock()
	if want := `base64 {"a":1}`; len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("attempts = %q, want two of %q", got, want)
	}
}

// A restarted agent gets the options of /outgoing as the first one did: they say
// what the agent is to record and pass through.
func TestRestartedAgentGetsTheOutgoingOptions(t *testing.T) {
	agent := &restartingAgent{conns: map[string]int{}, refuse: map[string]int{}, hang: map[string]int{}, slow: map[string]int{}}
	inner := agent.handler(t)
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/outgoing" {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the body: %v", err)
			}
			mu.Lock()
			got = append(got, string(b))
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL + "/agent"
	client := New(zap.NewNop(), nil, cfg)
	storeSession(t, cfg.Agent.AgentURI, true)
	s := client.session()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{MongoPassword: "the-password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MakeAgentReadyForDockerCompose(gctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil {
		t.Fatal("no mock on the first connection")
	}
	if err := s.reapply(gctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil {
		t.Fatal("no mock on the second connection")
	}
	s.end()

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 || !strings.Contains(got[0], "the-password") || got[1] != got[0] {
		t.Errorf("/outgoing bodies per connection: %q", got)
	}
}
