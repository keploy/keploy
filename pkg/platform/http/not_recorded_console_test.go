package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// lockedBuffer is a bytes.Buffer safe for the stream's reader and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A request the agent did not record as a test case comes on the test-case
// stream as a part of its own (models.NotRecordedPart): the client says it on
// keploy's console, its method and path beside the path tag the agent's line
// names it by, and why, so the user can tell /events from /poll, which the
// line names alike (GET /{}). Never in the client's log: the path can hold a
// token. The names of its query's parameters are said, never their values.
// A request said once is not said again on the stream for the same why: the
// stream's end says how many more came (a streaming endpoint a browser calls
// over and over would bury the recording's own lines).
func TestTheClientSaysANotRecordedRequestOnTheConsoleByItsTag(t *testing.T) {
	calls := []models.NotRecordedCall{
		{Method: "GET", Path: "/events", PathTag: models.PathTag(context.Background(), "GET", "/events"), Why: "its client closed its side of the connection before the app's answer ended"},
		{Method: "GET", Path: "/poll", QueryKeys: "token", PathTag: models.PathTag(context.Background(), "GET", "/poll"), Why: "its client closed its side of the connection before the app's answer ended"},
	}
	console := &lockedBuffer{}
	core, logs := observer.New(zapcore.DebugLevel)
	// /events three times, /poll once.
	streamNotRecorded(t, zap.New(core), console, []models.NotRecordedCall{calls[0], calls[1], calls[0], calls[0]})
	// The stream's end is said once the channel closes.
	want := "🐰 Keploy: not recorded as a test case: GET /events [path tag " + calls[0].PathTag + "]: its client closed its side of the connection before the app's answer ended\n" +
		"🐰 Keploy: not recorded as a test case: GET /poll?{token} [path tag " + calls[1].PathTag + "]: its client closed its side of the connection before the app's answer ended\n" +
		"🐰 Keploy: not recorded as test cases, 2 more after the one said above: GET /events [path tag " + calls[0].PathTag + "]: its client closed its side of the connection before the app's answer ended\n"
	if out := console.String(); out != want {
		t.Errorf("the console said\n%s\nwant\n%s", out, want)
	}
	for _, e := range logs.All() {
		line := e.Message
		for k, v := range e.ContextMap() {
			line += " " + k + "=" + strings.TrimSpace(strings.ReplaceAll(jsonOf(v), "\n", " "))
		}
		if strings.Contains(line, "/events") || strings.Contains(line, "/poll") {
			t.Errorf("the client's log names a not-recorded request's path: %q", line)
		}
	}
}

// A stream's console names at most notRecordedSaidMax requests not recorded
// (by method, path, tag and why): a streamed endpoint with an id in its path
// is a request of its own each time, and naming each would flood the console
// and grow the stream's map for as long as the stream lasts. Past them each
// is only counted, and the stream's end says how many came, and that the
// agent's log names each by its shape and tag.
func TestTheClientNamesAtMostSoManyNotRecordedRequests(t *testing.T) {
	var calls []models.NotRecordedCall
	for i := range notRecordedSaidMax + 7 {
		p := fmt.Sprintf("/events/%d", i)
		calls = append(calls, models.NotRecordedCall{Method: "GET", Path: p, PathTag: models.PathTag(context.Background(), "GET", p), Why: "its answer is a stream"})
	}
	console := &lockedBuffer{}
	streamNotRecorded(t, zap.NewNop(), console, append(calls, calls[0], calls[notRecordedSaidMax]))
	out := console.String()
	if n := strings.Count(out, "🐰 Keploy: not recorded as a test case: "); n != notRecordedSaidMax {
		t.Errorf("the console named %d requests, want %d:\n%s", n, notRecordedSaidMax, out)
	}
	if strings.Contains(out, fmt.Sprintf("GET /events/%d ", notRecordedSaidMax)) {
		t.Errorf("the console named a request past the first %d:\n%s", notRecordedSaidMax, out)
	}
	for _, want := range []string{
		"🐰 Keploy: not recorded as test cases, 1 more after the one said above: GET /events/0 [path tag " + calls[0].PathTag + "]: its answer is a stream\n",
		fmt.Sprintf("🐰 Keploy: not recorded as test cases, %d more requests beyond the %d named above, which this console names no more of: the agent's log names each by its shape and path tag\n", 8, notRecordedSaidMax),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the console said\n%s\nwant it to hold %q", out, want)
		}
	}
}

// streamNotRecorded runs a test-case stream that carries calls as
// not-recorded parts and its end marker, through a client logging to logger
// whose console is console (none for nil), and returns once the stream ends.
func streamNotRecorded(t *testing.T, logger *zap.Logger, console io.Writer, calls []models.NotRecordedCall) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		part := func(name string, body any) {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="`+name+`"`)
			p, _ := mw.CreatePart(h)
			if body != nil {
				_ = json.NewEncoder(p).Encode(body)
			}
		}
		for _, c := range calls {
			part(models.NotRecordedPart, c)
			part("delimiter", nil)
		}
		part(models.StreamEndPart, models.StreamEndSummary{})
		_ = mw.Close()
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL
	client := New(logger, nil, cfg)
	if console != nil {
		client.SetConsole(console)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
}

// jsonOf is v as JSON, for a log entry's field.
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
