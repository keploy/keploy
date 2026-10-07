package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// agentSession is what the CLI has set up on the agent at one address during
// one run of the application: the calls that configure it (the test-set hook,
// its mocks and their filters, the scope table, its readiness) and the streams
// it records through.
//
// Under docker compose the agent is a service in the user's project, and
// compose can start it again: keploy's retry of an `up` whose dependency
// crashed runs `up` again, and compose starts again an agent it stopped (every
// service attached, so the crash was an attached container's exit) or
// recreates one (a recreate flag). The agent it starts answers at the same
// address with none of that setup, and its healthcheck, which the app's start
// waits on, never passes. The application client sees the agent's container
// change (App.SetOnAgentRestart) and has the session put the setup back: the
// streams reconnect, and the calls are made again, readiness last.
//
// It is keyed by the agent's address rather than held by one AgentClient,
// because one run can configure its agent through more than one: enterprise's
// headless test starts the app through one and stores mocks through another.
type agentSession struct {
	mu sync.Mutex
	// restartable says the agent can be started again behind the CLI's back:
	// a compose run. Nothing is recorded otherwise, and a stream that ends
	// has ended.
	restartable bool
	steps       []sessionStep
	// gen counts the agents compose started again that setting up began on;
	// a stream whose connection is to an agent before gen is stale.
	gen int
	// ends counts the runs that ended. A stream belongs to the run that was
	// on when it opened, and stops waiting for a restarted agent once that
	// run is over.
	ends    int
	streams map[*sessionStream]struct{}
	// changed is closed, and replaced, on every change waiters care about.
	changed chan struct{}
	// reapplying lets one re-application run at a time.
	reapplying sync.Mutex
}

// sessionStep is one configuring call, made again by apply.
type sessionStep struct {
	kind  string
	apply func(ctx context.Context) error
}

// Step kinds. A later call of a kind replaces the earlier one in place: each
// configures the agent wholesale (a test set's mocks replace the last set's).
// They are made again in the order the kinds were first made in, except
// readiness, which releases the app and so goes last.
const (
	stepTestSetHook  = "before-test-set-compose"
	stepMockOutgoing = "mock-outgoing"
	stepStoreMocks   = "store-mocks"
	stepMockParams   = "update-mock-params"
	stepScopeTable   = "scope-table"
	stepScopeGate    = "scope-gate"
	stepAgentReady   = "agent-ready"
)

// staleStreamGrace is how long setting a restarted agent up waits for the
// streams to see their old agent gone (its connection closing) before it cuts
// the connections that have not; streamReconnectTimeout, how long it then
// waits for them to reconnect before it goes on regardless.
var (
	staleStreamGrace       = 5 * time.Second
	streamReconnectTimeout = 30 * time.Second
)

var sessions sync.Map // agent URI -> *agentSession

// sessionFor is the session of the agent at uri.
func sessionFor(uri string) *agentSession {
	if s, ok := sessions.Load(uri); ok {
		return s.(*agentSession)
	}
	s, _ := sessions.LoadOrStore(uri, newAgentSession())
	return s.(*agentSession)
}

func newAgentSession() *agentSession {
	return &agentSession{changed: make(chan struct{}), streams: map[*sessionStream]struct{}{}}
}

// notify wakes every waiter; with s.mu held.
func (s *agentSession) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// configure says whether this agent can be started again behind the CLI's back.
func (s *agentSession) configure(restartable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartable = restartable
}

// end ends a run of the application: its streams stop waiting for a restarted
// agent, and what was recorded (the stored mocks among it) is let go. The next
// run's calls are recorded afresh, whenever they come.
func (s *agentSession) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ends++
	s.steps = nil
	s.notify()
}

// recording reports whether configuring calls are recorded: a caller copies
// what it hands record only then.
func (s *agentSession) recording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restartable
}

// record notes a configuring call that succeeded, to be made again on a
// restarted agent. apply must not share memory its caller goes on changing.
func (s *agentSession) record(kind string, apply func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.restartable {
		return
	}
	for i := range s.steps {
		if s.steps[i].kind == kind {
			s.steps[i].apply = apply
			return
		}
	}
	s.steps = append(s.steps, sessionStep{kind: kind, apply: apply})
}

// sessionStream is one stream's place in its session: the run it belongs to,
// the agent (gen) its connection is to, and that connection.
type sessionStream struct {
	s    *agentSession
	run  int
	gen  int
	conn *streamConn
}

// streamConn is one connection of a stream, which setting a restarted agent
// up cuts when it is still to the agent before: gen is the agent it was made
// to. A connection made once setting up began is to the new agent, however
// late the stream noticed the old one was gone.
type streamConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	gen    int
	cut    atomic.Bool
}

// body is the connection's response body. A cut ends it as the agent ending
// the stream would: the read returns io.EOF, and the close nothing.
func (c *streamConn) body(b io.ReadCloser) io.ReadCloser {
	return &streamBody{ReadCloser: b, conn: c}
}

type streamBody struct {
	io.ReadCloser
	conn *streamConn
}

func (b *streamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.conn.cut.Load() {
		err = io.EOF
	}
	return n, err
}

func (b *streamBody) Close() error {
	err := b.ReadCloser.Close()
	if b.conn.cut.Load() {
		return nil
	}
	return err
}

// openStream registers a stream before it connects, so a restart that comes
// while it does still counts it.
func (s *agentSession) openStream() *sessionStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &sessionStream{s: s, run: s.ends, gen: s.gen}
	s.streams[st] = struct{}{}
	return st
}

// connect starts a connection of the stream: its request is made with the
// connection's ctx, and its response read through the connection's body.
func (st *sessionStream) connect(ctx context.Context) *streamConn {
	c := &streamConn{}
	c.ctx, c.cancel = context.WithCancel(ctx)
	s := st.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c.gen = s.gen
	if st.conn != nil {
		st.conn.cancel()
	}
	st.conn = c
	return c
}

// close deregisters the stream for good.
func (st *sessionStream) close() {
	s := st.s
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, st)
	if st.conn != nil {
		st.conn.cancel()
	}
	s.notify()
}

// awaitRestart is called when the stream's connection ended. It reports
// whether to reconnect: the session was set up on a restarted agent. It does
// not when there is nothing to reconnect to: ctx is done, the agent cannot be
// restarted behind the CLI's back (not a compose run), or the stream's run is
// over.
func (st *sessionStream) awaitRestart(ctx context.Context) bool {
	s := st.s
	for {
		if ctx.Err() != nil {
			return false
		}
		s.mu.Lock()
		if !s.restartable || s.ends != st.run {
			s.mu.Unlock()
			return false
		}
		if s.gen > st.gen {
			s.mu.Unlock()
			return true
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// caughtUp says the stream is done with the agents before the one conn was
// made to: it reconnected to it, or could not and waits for the next. Should
// yet another agent have come since, the stream is still behind it.
func (st *sessionStream) caughtUp(conn *streamConn) {
	s := st.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.gen < conn.gen {
		st.gen = conn.gen
		s.notify()
	}
}

// stale is how many streams have not caught up; with s.mu held.
func (s *agentSession) stale() int {
	n := 0
	for st := range s.streams {
		if st.gen < s.gen {
			n++
		}
	}
	return n
}

// cutStale cuts the connections, still to an agent before the current one, of
// the streams that have not caught up: that agent is gone, but the connection
// has not said so. A stream reconnecting to the current agent keeps its new
// connection.
func (s *agentSession) cutStale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for st := range s.streams {
		if st.gen < s.gen && st.conn != nil && st.conn.gen < s.gen {
			st.conn.cut.Store(true)
			st.conn.cancel()
		}
	}
}

// awaitStreams waits up to d for every stream to catch up. It reports whether
// they did; err is ctx's when it is done first.
func (s *agentSession) awaitStreams(ctx context.Context, d time.Duration) (bool, error) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		stale, changed := s.stale(), s.changed
		s.mu.Unlock()
		if stale == 0 {
			return true, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		}
	}
}

// reapply sets a restarted agent up as the one before it was: the streams
// reconnect first, as they were opened before the agent was made ready, then
// the configuring calls are made again in order, readiness last.
func (s *agentSession) reapply(ctx context.Context, logger *zap.Logger) error {
	s.reapplying.Lock()
	defer s.reapplying.Unlock()

	s.mu.Lock()
	s.gen++
	var steps, ready []sessionStep
	for _, step := range s.steps {
		if step.kind == stepAgentReady {
			ready = append(ready, step)
		} else {
			steps = append(steps, step)
		}
	}
	s.notify()
	s.mu.Unlock()

	caught, err := s.awaitStreams(ctx, staleStreamGrace)
	if err != nil {
		return err
	}
	if !caught {
		s.cutStale()
		if caught, err = s.awaitStreams(ctx, streamReconnectTimeout); err != nil {
			return err
		}
		if !caught {
			s.mu.Lock()
			stale := s.stale()
			s.mu.Unlock()
			logger.Warn("keploy's restarted agent: not every stream reconnected in time; setting it up regardless",
				zap.Int("streams not reconnected", stale), zap.Duration("waited", staleStreamGrace+streamReconnectTimeout))
		}
	}
	for _, step := range append(steps, ready...) {
		if err := step.apply(ctx); err != nil {
			return fmt.Errorf("failed to set keploy's restarted agent up again (%s): %w", step.kind, err)
		}
	}
	logger.Info("keploy's agent was started again by docker compose; set it up again as before",
		zap.Int("calls", len(steps)+len(ready)))
	return nil
}

// session is the session of the agent this client talks to.
func (a *AgentClient) session() *agentSession {
	return sessionFor(a.conf.Agent.AgentURI)
}

// setUpRestartedAgent sets up the agent docker compose started again, once it
// answers: its container runs before its server listens, and a stream that
// reconnected, or a call made, before then would only be refused.
func (a *AgentClient) setUpRestartedAgent(ctx context.Context) error {
	if err := a.awaitAgentAnswers(ctx); err != nil {
		return err
	}
	return a.session().reapply(ctx, a.logger)
}

// awaitAgentAnswers waits, up to the agent's readiness budget, for its health
// endpoint to answer.
func (a *AgentClient) awaitAgentAnswers(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pkg.AgentReadyTimeout())
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		probe, cancelProbe := context.WithTimeout(ctx, 500*time.Millisecond)
		req, err := http.NewRequestWithContext(probe, http.MethodGet, a.conf.Agent.AgentURI+"/health", nil)
		if err == nil {
			if res, err := a.client.Do(req); err == nil {
				_ = res.Body.Close()
				if res.StatusCode == http.StatusOK {
					cancelProbe()
					return nil
				}
			}
		}
		cancelProbe()
		select {
		case <-ctx.Done():
			return fmt.Errorf("keploy's agent, started again by docker compose, did not answer at %s: %w",
				a.conf.Agent.AgentURI, ctx.Err())
		case <-tick.C:
		}
	}
}

// copyMockParams is params sharing no memory with it: the replayer goes on
// changing the consumed-mocks map it hands over.
func copyMockParams(params models.MockFilterParams) models.MockFilterParams {
	params.TotalConsumedMocks = maps.Clone(params.TotalConsumedMocks)
	params.RecordedWindows = slices.Clone(params.RecordedWindows)
	params.MockMapping = slices.Clone(params.MockMapping)
	return params
}

// resumeStream is called when a stream's connection ended. Once the session
// is set up on a restarted agent, it makes the stream's request again, the
// same method, path and body, on a new connection, and returns its response.
// A stream that cannot reconnect waits for the next agent compose starts. It
// reports false when the stream is over (see awaitRestart).
func (a *AgentClient) resumeStream(ctx context.Context, stream *sessionStream, name, method, path string, body []byte) (*http.Response, bool) {
	for stream.awaitRestart(ctx) {
		conn := stream.connect(ctx)
		res, err := a.reopenStream(conn.ctx, name, method, path, body)
		if err == nil {
			res.Body = conn.body(res.Body)
			stream.caughtUp(conn)
			return res, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		if conn.cut.Load() {
			// Cut as stale: an agent after it was set up meanwhile.
			continue
		}
		utils.LogError(a.logger, err, "failed to reconnect a stream to keploy's restarted agent; it reconnects to the next one docker compose starts",
			zap.String("stream", name))
		stream.caughtUp(conn)
	}
	return nil, false
}

// reopenRetries and reopenBackoff bound how a stream reconnects to the agent
// the session was set up on again: it answers by then, but a connection can
// still meet it mid-start.
var (
	reopenRetries = 10
	reopenBackoff = 500 * time.Millisecond
)

// reopenStream makes a stream's request again: see resumeStream.
func (a *AgentClient) reopenStream(ctx context.Context, name, method, path string, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= reopenRetries; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, a.conf.Agent.AgentURI+path, rd)
		if err != nil {
			return nil, err
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := a.client.Do(req)
		if err == nil {
			if err := agentStreamStatus(name, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(reopenBackoff):
		}
	}
	return nil, lastErr
}
