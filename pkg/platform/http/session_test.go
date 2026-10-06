package http

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// newSession is a fresh session for a test, under a key no other test uses,
// gone with the test.
func newSession(t *testing.T, restartable bool) *agentSession {
	t.Helper()
	return storeSession(t, "test://"+t.Name(), restartable)
}

// storeSession is a fresh session at key, gone with the test.
func storeSession(t *testing.T, key string, restartable bool) *agentSession {
	t.Helper()
	s := newAgentSession()
	s.configure(restartable)
	sessions.Store(key, s)
	t.Cleanup(func() { sessions.Delete(key) })
	return s
}

func noteStep(got *[]string, mu *sync.Mutex, name string) func(context.Context) error {
	return func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, name)
		return nil
	}
}

// A later call of a kind replaces the earlier one where it stood; the agent is
// made ready last, whatever order the calls came in (enterprise's headless
// test readies it before storing mocks).
func TestSessionReappliesInOrderReadinessLast(t *testing.T) {
	s := newSession(t, true)
	var mu sync.Mutex
	var got []string
	s.record(stepAgentReady, noteStep(&got, &mu, "ready#1"))
	s.record(stepMockOutgoing, noteStep(&got, &mu, "outgoing#1"))
	s.record(stepStoreMocks, noteStep(&got, &mu, "store#1"))
	s.record(stepAgentReady, noteStep(&got, &mu, "ready#2"))
	s.record(stepMockParams, noteStep(&got, &mu, "params#1"))
	s.record(stepMockOutgoing, noteStep(&got, &mu, "outgoing#2"))
	s.record(stepMockParams, noteStep(&got, &mu, "params#2"))

	if err := s.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if want := "outgoing#2 store#1 params#2 ready#2"; strings.Join(got, " ") != want {
		t.Fatalf("set up again with %v, want %s", got, want)
	}
}

// Nothing is kept outside compose, where nothing sets an agent up again, and
// nothing outlives the run: the stored mocks a step holds are let go.
func TestSessionKeepsNothingItWillNotUse(t *testing.T) {
	var mu sync.Mutex
	var got []string
	native := newSession(t, false)
	native.record(stepStoreMocks, noteStep(&got, &mu, "store"))
	if len(native.steps) != 0 {
		t.Fatal("recorded a step outside compose")
	}

	if native.recording() {
		t.Fatal("recording outside compose: callers copy what they would record")
	}

	s := storeSession(t, "test://"+t.Name()+"/compose", true)
	s.record(stepStoreMocks, noteStep(&got, &mu, "store"))
	s.end()
	if len(s.steps) != 0 {
		t.Fatal("the run's steps outlived it")
	}
}

// The replayer goes on changing the consumed-mocks map it hands over; the
// copy recorded is the call as it was made.
func TestCopyMockParamsSharesNothing(t *testing.T) {
	params := models.MockFilterParams{
		TotalConsumedMocks: map[string]models.MockState{"a": {}},
		MockMapping:        []string{"m1"},
		RecordedWindows:    []models.TestWindow{{}},
	}
	c := copyMockParams(params)
	params.TotalConsumedMocks["b"] = models.MockState{}
	params.MockMapping[0] = "changed"
	params.RecordedWindows[0].Start = time.Now()
	if len(c.TotalConsumedMocks) != 1 || c.MockMapping[0] != "m1" || !c.RecordedWindows[0].Start.IsZero() {
		t.Fatalf("the copy changed with the original: %+v", c)
	}
}

// Streams were opened before the agent was made ready, so they reconnect first;
// a stream that gives up instead releases the wait at once, and one opened to
// the new agent is not waited for.
func TestSessionReapplyWaitsForStreamsFirst(t *testing.T) {
	s := newSession(t, true)
	reconnecting := s.openStream()
	givingUp := s.openStream()
	var ready atomic.Bool
	s.record(stepAgentReady, func(context.Context) error { ready.Store(true); return nil })

	done := make(chan error, 1)
	go func() { done <- s.reapply(context.Background(), zap.NewNop()) }()

	if !restarts(t, reconnecting) || !restarts(t, givingUp) {
		t.Fatal("a stream was not told to reconnect")
	}
	defer s.openStream().close() // connected to the new agent
	time.Sleep(50 * time.Millisecond)
	if ready.Load() {
		t.Fatal("the agent was made ready before the streams reconnected")
	}
	reconnecting.caughtUp(reconnecting.connect(context.Background()))
	defer reconnecting.close()
	time.Sleep(50 * time.Millisecond)
	if ready.Load() {
		t.Fatal("the agent was made ready with a stream neither reconnected nor gone")
	}
	start := time.Now()
	givingUp.close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reapply waited on a stream that gave up")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("reapply took %s after the last stream settled", waited)
	}
	if !ready.Load() {
		t.Fatal("the agent was not made ready again")
	}
}

// A stream has nothing to reconnect to once ctx is done (checked first: a stale
// stream at a recording's stop must not reconnect), where the agent cannot be
// started again behind the CLI's back, or once the run is over.
func TestAwaitRestartGivesUp(t *testing.T) {
	shortenStreamWaits(t)
	s := newSession(t, true)
	stale := s.openStream()
	if err := s.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if stale.awaitRestart(ctx) {
		t.Fatal("a stale stream reconnected after ctx was done")
	}
	stale.close()

	native := storeSession(t, "test://"+t.Name()+"/native", false)
	if restarts(t, native.openStream()) {
		t.Fatal("waited for a restart outside compose")
	}

	ended := storeSession(t, "test://"+t.Name()+"/ended", true)
	st := ended.openStream()
	defer st.close()
	go func() { time.Sleep(20 * time.Millisecond); ended.end() }()
	if restarts(t, st) {
		t.Fatal("waited for a restart after the run ended")
	}
	// However soon the next run sets a restarted agent up, a stream of the
	// run before does not come back for it.
	if err := ended.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if restarts(t, st) {
		t.Fatal("a stream of a run that ended reconnected in the next")
	}
	next := ended.openStream()
	defer next.close()
	if err := ended.reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if !restarts(t, next) {
		t.Fatal("a stream of the next run did not reconnect")
	}
}

// restarts is st.awaitRestart's answer, failing the test, rather than hanging
// it, when none comes in time.
func restarts(t *testing.T, st *sessionStream) bool {
	t.Helper()
	got := make(chan bool, 1)
	go func() { got <- st.awaitRestart(context.Background()) }()
	select {
	case ok := <-got:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatal("awaitRestart gave no answer in time")
		return false
	}
}

// recv is the next value on a stream's channel, failing the test, rather than
// hanging it, when none comes in time.
func recv[T any](t *testing.T, ch <-chan T) (T, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	case <-time.After(5 * time.Second):
		t.Fatal("nothing came on the stream in time")
		var zero T
		return zero, false
	}
}

// shortenStreamWaits makes setting a restarted agent up wait briefly for the
// streams, for the test.
func shortenStreamWaits(t *testing.T) {
	t.Helper()
	grace, timeout := staleStreamGrace, streamReconnectTimeout
	staleStreamGrace, streamReconnectTimeout = 50*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { staleStreamGrace, streamReconnectTimeout = grace, timeout })
}

// A stream whose connection does not end with its agent (the agent vanished
// without closing it) is cut and reconnected, and its reader sees the stream
// end, not fail.
func TestSessionCutsAStreamStillOnItsOldAgent(t *testing.T) {
	shortenStreamWaits(t)
	s := newSession(t, true)
	st := s.openStream()
	defer st.close()
	conn := st.connect(context.Background())
	body := conn.body(io.NopCloser(&blockingReader{ctx: conn.ctx}))

	done := make(chan error, 1)
	go func() { done <- s.reapply(context.Background(), zap.NewNop()) }()
	read := make(chan error, 1)
	go func() { _, err := body.Read(make([]byte, 1)); read <- err }()
	select {
	case err := <-read:
		if err != io.EOF {
			t.Fatalf("read on the cut connection: %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection on the old agent was not cut")
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close of the cut connection: %v", err)
	}
	if !restarts(t, st) {
		t.Fatal("the cut stream was not told to reconnect")
	}
	st.caughtUp(st.connect(context.Background()))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// blockingReader reads nothing until ctx is done, as a connection to an agent
// that vanished without closing it.
type blockingReader struct{ ctx context.Context }

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// restartingAgent is an agent whose stream routes serve one connection per
// process: each connection hands over one item and ends, as compose stops the
// agent; once the session is set up again the streams reconnect.
type restartingAgent struct {
	mu    sync.Mutex
	conns map[string]int
	ready int
	// down drops every connection, as the published port of an agent whose
	// container runs but whose server does not listen yet; downCalls counts
	// the calls other than health checks that met it.
	down      bool
	downCalls int
	// refuse answers a path's nth connection 503; hang keeps it open after
	// its item, as an agent that vanished without closing it, until release
	// is closed; slow answers it slowBy late.
	refuse, hang, slow map[string]int
	release            chan struct{}
	slowBy             time.Duration
}

func (r *restartingAgent) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if r.down {
			if req.URL.Path != "/agent/health" {
				r.downCalls++
			}
			r.mu.Unlock()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		r.conns[req.URL.Path]++
		n := r.conns[req.URL.Path]
		if req.URL.Path == "/agent/agent/ready" { // the agent's /agent/ready, under its /agent prefix
			r.ready++
		}
		r.mu.Unlock()
		switch req.URL.Path {
		case "/agent/agent/ready", "/agent/health":
			w.WriteHeader(http.StatusOK)
		case "/agent/outgoing":
			if r.refuse[req.URL.Path] == n {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if r.slow[req.URL.Path] == n {
				time.Sleep(r.slowBy)
			}
			_ = gob.NewEncoder(w).Encode(models.Mock{Name: fmt.Sprintf("mock-%d", n), Kind: models.HTTP})
			if r.hang[req.URL.Path] == n {
				w.(http.Flusher).Flush()
				select {
				case <-req.Context().Done():
				case <-r.release:
				}
			}
		case "/agent/incoming":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(models.TestCase{Name: fmt.Sprintf("test-%d", n)})
		case "/agent/mappings":
			_ = json.NewEncoder(w).Encode(models.TestMockMapping{TestName: fmt.Sprintf("mapping-%d", n)})
		case "/agent/pcap/traffic", "/agent/pcap/keylog":
			_, _ = fmt.Fprintf(w, "%s#%d\n", req.URL.Path, n)
		default:
			t.Errorf("unexpected request %s", req.URL.Path)
		}
	})
}

func newRestartingClient(t *testing.T, restartable bool) (*AgentClient, *restartingAgent) {
	t.Helper()
	agent := &restartingAgent{conns: map[string]int{}, refuse: map[string]int{}, hang: map[string]int{}, slow: map[string]int{}}
	srv := httptest.NewServer(agent.handler(t))
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL + "/agent"
	client := New(zap.NewNop(), nil, cfg)
	storeSession(t, cfg.Agent.AgentURI, restartable)
	return client, agent
}

// A record stream does not end with the agent compose stopped: once the
// session is set up on the agent compose started again, it reconnects and goes
// on feeding the same channel, which closes when the run ends.
func TestStreamsReconnectToARestartedAgent(t *testing.T) {
	client, agent := newRestartingClient(t, true)
	s := client.session()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := client.GetIncoming(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := client.GetMappings(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MakeAgentReadyForDockerCompose(gctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-1" {
		t.Fatalf("first mock: %+v", m)
	}
	if tc, _ := recv(t, incoming); tc == nil || tc.Name != "test-1" {
		t.Fatalf("first test case: %+v", tc)
	}
	if mp, _ := recv(t, mappings); mp.TestName != "mapping-1" {
		t.Fatalf("first mapping: %+v", mp)
	}

	if err := s.reapply(gctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-2" {
		t.Fatalf("mock after the restart: %+v", m)
	}
	if tc, _ := recv(t, incoming); tc == nil || tc.Name != "test-2" {
		t.Fatalf("test case after the restart: %+v", tc)
	}
	if mp, _ := recv(t, mappings); mp.TestName != "mapping-2" {
		t.Fatalf("mapping after the restart: %+v", mp)
	}
	agent.mu.Lock()
	ready := agent.ready
	agent.mu.Unlock()
	if ready != 2 {
		t.Fatalf("the agent was made ready %d times, want 2: once, and again after the restart", ready)
	}

	s.end()
	if _, open := recv(t, outgoing); open {
		t.Fatal("the outgoing stream did not close when the run ended")
	}
	if _, open := recv(t, incoming); open {
		t.Fatal("the incoming stream did not close when the run ended")
	}
	if _, open := recv(t, mappings); open {
		t.Fatal("the mappings stream did not close when the run ended")
	}
}

// The agent's container runs before its server listens: nothing is reconnected
// to it or made again on it until it answers, and then all of it is.
func TestRestartedAgentIsSetUpOnceItAnswers(t *testing.T) {
	client, agent := newRestartingClient(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MakeAgentReadyForDockerCompose(gctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-1" {
		t.Fatalf("first mock: %+v", m)
	}

	agent.mu.Lock()
	agent.down = true
	agent.mu.Unlock()
	setUp := make(chan error, 1)
	go func() { setUp <- client.setUpRestartedAgent(gctx) }()
	time.Sleep(time.Second)
	agent.mu.Lock()
	agent.down = false
	downCalls := agent.downCalls
	agent.mu.Unlock()
	if downCalls != 0 {
		t.Fatalf("%d call(s) made to the restarted agent before it answered", downCalls)
	}
	if err := <-setUp; err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-2" {
		t.Fatalf("mock after the restart: %+v", m)
	}
	agent.mu.Lock()
	ready := agent.ready
	agent.mu.Unlock()
	if ready != 2 {
		t.Fatalf("the agent was made ready %d times, want 2", ready)
	}
	client.session().end()
}

// Outside compose a stream that ends has ended, as it always had.
func TestStreamsEndOutsideCompose(t *testing.T) {
	client, _ := newRestartingClient(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recv(t, outgoing)
	select {
	case _, open := <-outgoing:
		if open {
			t.Fatal("a second mock outside compose")
		}
	case <-ctx.Done():
		t.Fatal("the stream did not close when its connection ended")
	}
}

// The packet capture goes on after a restart: the traffic in a capture file of
// its own (each a valid capture), the key log in the same file.
func TestPcapStreamsReconnectToARestartedAgent(t *testing.T) {
	client, _ := newRestartingClient(t, true)
	s := client.session()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.StreamPcapArtifacts(ctx, dir) }()

	waitFor := func(path, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s holds %q, want %q", path, b, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor(filepath.Join(dir, "traffic.pcap"), "/agent/pcap/traffic#1")
	waitFor(filepath.Join(dir, "sslkeys.log"), "/agent/pcap/keylog#1")
	if err := s.reapply(ctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	waitFor(filepath.Join(dir, "traffic.2.pcap"), "/agent/pcap/traffic#2")
	waitFor(filepath.Join(dir, "sslkeys.log"), "/agent/pcap/keylog#1\n/agent/pcap/keylog#2")
	s.end()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pcap streams did not end with the run")
	}
}

// A stream that cannot reconnect to the agent compose started again waits for
// the next one, rather than ending for the rest of the run.
func TestStreamThatCannotReconnectWaitsForTheNextAgent(t *testing.T) {
	shortenStreamWaits(t)
	retries := reopenRetries
	reopenRetries = 1
	t.Cleanup(func() { reopenRetries = retries })
	client, agent := newRestartingClient(t, true)
	agent.refuse["/agent/outgoing"] = 2
	s := client.session()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-1" {
		t.Fatalf("first mock: %+v", m)
	}
	if err := s.reapply(gctx, zap.NewNop()); err != nil { // refused
		t.Fatal(err)
	}
	if err := s.reapply(gctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-3" {
		t.Fatalf("mock from the agent after the one it could not reconnect to: %+v", m)
	}
	s.end()
	if _, open := recv(t, outgoing); open {
		t.Fatal("the outgoing stream did not close when the run ended")
	}
}

// A stream whose agent vanished without closing its connection is cut once
// the session is set up on the next, and reconnects to it.
func TestStreamOnAVanishedAgentReconnects(t *testing.T) {
	shortenStreamWaits(t)
	client, agent := newRestartingClient(t, true)
	agent.hang["/agent/outgoing"] = 1
	s := client.session()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-1" {
		t.Fatalf("first mock: %+v", m)
	}
	if err := s.reapply(gctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-2" {
		t.Fatalf("mock after the restart: %+v", m)
	}
	s.end()
	if _, open := recv(t, outgoing); open {
		t.Fatal("the outgoing stream did not close when the run ended")
	}
}

// A stream that sees its old agent gone late, just before the connections
// still to it are cut, keeps the connection it is making to the new agent.
func TestStreamReconnectingLateIsNotCut(t *testing.T) {
	shortenStreamWaits(t) // the cut 50ms in
	client, agent := newRestartingClient(t, true)
	agent.hang["/agent/outgoing"] = 1
	agent.release = make(chan struct{})
	agent.slow["/agent/outgoing"] = 2
	agent.slowBy = 100 * time.Millisecond
	s := client.session()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-1" {
		t.Fatalf("first mock: %+v", m)
	}
	done := make(chan error, 1)
	go func() { done <- s.reapply(gctx, zap.NewNop()) }()
	time.Sleep(30 * time.Millisecond)
	close(agent.release) // the old connection ends, 20ms before the cut
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-2" {
		t.Fatalf("mock after the restart: %+v", m)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.end()
	if _, open := recv(t, outgoing); open {
		t.Fatal("the outgoing stream did not close when the run ended")
	}
}

// A stream that reconnected to an agent compose replaced while it did is
// still behind the newest one, and reconnects again.
func TestStreamCaughtUpOnlyToTheAgentItReached(t *testing.T) {
	s := newSession(t, true)
	st := s.openStream()
	defer st.close()
	bump := func() {
		s.mu.Lock()
		s.gen++
		s.notify()
		s.mu.Unlock()
	}
	bump()
	conn := st.connect(context.Background()) // to the agent after the first restart
	bump()                                   // and another restart comes meanwhile
	st.caughtUp(conn)
	if !restarts(t, st) {
		t.Fatal("a stream on a replaced agent did not reconnect to the newest")
	}
}
