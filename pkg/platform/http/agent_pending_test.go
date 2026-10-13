package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// BacklogBefore asks the agent's /record/pending, with the time in Unix
// nanoseconds, and reads its answer into one of three verdicts, or a fourth for
// an agent that can never tell (501: no watermark; 404: it predates the
// route). An answer that did not come (an error status, a body that does not
// decode, a timeout) is "cannot tell", with the error: never "clear".
func TestAgentClient_BacklogBefore(t *testing.T) {
	var status int
	var body, gotBefore string
	var stall chan struct{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/record/pending" {
			http.NotFound(w, r)
			return
		}
		gotBefore = r.URL.Query().Get("before")
		if stall != nil {
			select {
			case <-stall:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	before := time.Unix(0, 1234567890)
	for _, tc := range []struct {
		status  int
		body    string
		want    models.AgentBacklog
		wantErr bool
	}{
		{http.StatusOK, `{"pending":true,"progress":42,"waitingOn":"connection 7"}`, models.AgentBacklog{Verdict: models.BacklogPending, Progress: 42, WaitingOn: "connection 7"}, false},
		{http.StatusOK, `{"pending":true}`, models.AgentBacklog{Verdict: models.BacklogPending}, false},
		{http.StatusOK, `{"pending":false,"progress":9}`, models.AgentBacklog{Verdict: models.BacklogClear, Progress: 9}, false},
		{http.StatusNotImplemented, "cannot tell", models.AgentBacklog{Verdict: models.BacklogUnsupported}, false},
		{http.StatusNotFound, "", models.AgentBacklog{Verdict: models.BacklogUnsupported}, false},
		{http.StatusInternalServerError, "boom", models.AgentBacklog{Verdict: models.BacklogCannotTell}, true},
		{http.StatusOK, `{"pending":`, models.AgentBacklog{Verdict: models.BacklogCannotTell}, true},
	} {
		status, body = tc.status, tc.body
		got, err := a.BacklogBefore(context.Background(), before)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("status %d %q: (%+v, %v), want %+v (error %v)", tc.status, tc.body, got, err, tc.want, tc.wantErr)
		}
	}
	if gotBefore != "1234567890" {
		t.Fatalf("asked before=%q, want the stop in Unix nanoseconds", gotBefore)
	}

	// An agent that does not answer within the look's bound: cannot tell.
	stall = make(chan struct{})
	defer close(stall)
	got, err := a.BacklogBefore(context.Background(), before)
	if err == nil || got.Verdict != models.BacklogCannotTell {
		t.Fatalf("a look the agent did not answer: (%+v, %v), want cannot tell with its error", got, err)
	}
}

// The frame streams a recording opens on a context marked
// models.WithDrainsAfterStop carry DrainsAfterStopHeader, so the agent holds a
// stop signal for them; others (keploy mock's) do not.
func TestAgentClient_StreamsSayTheyDrainAfterTheStop(t *testing.T) {
	seen := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path + "=" + r.Header.Get(models.DrainsAfterStopHeader)
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	a.startedAgent.Store(true) // this client started the agent
	open := func(ctx context.Context) []string {
		grp, gctx := errgroup.WithContext(ctx)
		ctx = context.WithValue(gctx, models.ErrGroupKey, grp)
		_, _ = a.GetIncoming(ctx, models.IncomingOptions{})
		_, _ = a.GetOutgoing(ctx, models.OutgoingOptions{})
		if ch, err := a.GetMappings(ctx, models.IncomingOptions{}); err == nil {
			for range ch {
			}
		}
		var got []string
		for len(seen) > 0 {
			got = append(got, <-seen)
		}
		return got
	}
	for _, g := range open(models.WithDrainsAfterStop(context.Background())) {
		if !strings.HasSuffix(g, "=true") {
			t.Errorf("a recording's stream went without the header: %s", g)
		}
	}
	plain := open(context.Background())
	if len(plain) == 0 {
		t.Fatal("no stream reached the agent")
	}
	for _, g := range plain {
		if !strings.HasSuffix(g, "=") {
			t.Errorf("a stream not marked to drain carried the header: %s", g)
		}
	}

	// An agent something else started (docker compose, a k8s sidecar) is
	// stopped by that, not by this client: asked to hold for the client, it
	// would wait out that stop's timeout and be killed.
	a.startedAgent.Store(false)
	for _, g := range open(models.WithDrainsAfterStop(context.Background())) {
		if !strings.HasSuffix(g, "=") {
			t.Errorf("a stream to an agent this client did not start carried the header: %s", g)
		}
	}
	a.startedAgent.Store(true)

	// A stream that reconnects to a restarted agent asks it to hold for it
	// again; one not marked does not.
	reopen := func(ctx context.Context) string {
		if _, err := a.reopenStream(ctx, "incoming", http.MethodPost, "/incoming", []byte("{}")); err == nil {
			t.Fatal("the agent answered 404, yet the stream reopened")
		}
		return <-seen
	}
	if g := reopen(models.WithDrainsAfterStop(context.Background())); g != "/incoming=true" {
		t.Errorf("a recording's stream reconnected without the header: %s", g)
	}
	if g := reopen(context.Background()); g != "/incoming=" {
		t.Errorf("a stream not marked to drain reconnected with the header: %s", g)
	}
}
