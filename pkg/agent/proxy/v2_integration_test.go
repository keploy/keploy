package proxy

// End-to-end integration tests for the V2 record architecture.
//
// These tests wire up Relay + Supervisor + FakeConn with test-only
// parsers (happy / panic / hang) and drive real bytes through a
// net.Pipe()-based proxy. They prove the safety invariants from
// PLAN.md §1 are upheld in an integrated setting, independent of
// any real protocol parser.

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// Two independent net.Pipes: one for the (app-client, proxy-client-side)
// leg, one for the (proxy-dest-side, real-destination) leg. The relay
// sits in the middle owning proxySrc and proxyDst.
type pipePair struct {
	clientApp net.Conn
	proxySrc  net.Conn
	proxyDst  net.Conn
	destSrv   net.Conn
}

func newPipePair(t *testing.T) *pipePair {
	t.Helper()
	clientApp, proxySrc := netPipe()
	proxyDst, destSrv := netPipe()
	t.Cleanup(func() {
		_ = clientApp.Close()
		_ = proxySrc.Close()
		_ = proxyDst.Close()
		_ = destSrv.Close()
	})
	return &pipePair{clientApp, proxySrc, proxyDst, destSrv}
}

// mustStartRelay starts a relay in its own goroutine with a context
// cancelled on test cleanup. Returns the relay.
func mustStartRelay(t *testing.T, pp *pipePair, bump func()) *relay.Relay {
	t.Helper()
	log := zaptest.NewLogger(t)
	r := relay.New(relay.Config{
		Logger:       log,
		BumpActivity: bump,
	}, pp.proxySrc, pp.proxyDst)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("relay did not exit within 2s of cancel")
		}
	})
	return r
}

// mustBuildSession returns a supervisor.Session wired to r's streams
// plus a fresh mocks channel. The supervisor's SessionOnAbort is
// configured to close the FakeConns so parser reads unblock on abort.
func mustBuildSession(t *testing.T, sv *supervisor.Supervisor, r *relay.Relay) (*supervisor.Session, chan *models.Mock) {
	t.Helper()
	mocks := make(chan *models.Mock, 8)
	sess := &supervisor.Session{
		ClientStream: r.ClientStream(),
		DestStream:   r.DestStream(),
		Directives:   r.Directives(),
		Acks:         r.Acks(),
		Mocks:        mocks,
		Logger:       zaptest.NewLogger(t),
	}
	sv.SessionOnAbort = func() {
		_ = r.ClientStream().Close()
		_ = r.DestStream().Close()
	}
	return sess, mocks
}

// -------- Happy-path parser --------

// happyParser reads one request chunk from ClientStream and one
// response chunk from DestStream, then emits a mock anchored to the
// chunk timestamps. Exits cleanly; supervisor should report StatusOK.
func happyParser(_ context.Context, sess *supervisor.Session) error {
	c, err := sess.ClientStream.ReadChunk()
	if err != nil {
		return err
	}
	r, err := sess.DestStream.ReadChunk()
	if err != nil {
		return err
	}
	mock := &models.Mock{
		Name: "happy",
		Spec: models.MockSpec{
			ReqTimestampMock: c.ReadAt,
			ResTimestampMock: r.WrittenAt,
			Metadata: map[string]string{
				"req":  string(c.Bytes),
				"resp": string(r.Bytes),
			},
		},
	}
	return sess.EmitMock(mock)
}

func TestV2_HappyPath_ChunkTimestampsCarried(t *testing.T) {
	t.Parallel()
	pp := newPipePair(t)

	sv := supervisor.New(supervisor.Config{Logger: zaptest.NewLogger(t)})
	defer sv.Close()

	r := mustStartRelay(t, pp, sv.BumpActivity)
	sess, mocks := mustBuildSession(t, sv, r)

	// Destination: echo with "reply:" prefix.
	go func() {
		buf := make([]byte, 64)
		n, err := pp.destSrv.Read(buf)
		if err != nil {
			return
		}
		_, _ = pp.destSrv.Write(append([]byte("reply:"), buf[:n]...))
	}()

	clientGot := make(chan []byte, 1)
	go func() {
		_, _ = pp.clientApp.Write([]byte("ping"))
		b := make([]byte, 64)
		n, _ := pp.clientApp.Read(b)
		clientGot <- b[:n]
	}()

	resCh := make(chan supervisor.Result, 1)
	go func() { resCh <- sv.Run(context.Background(), happyParser, sess) }()

	// Traffic goes end-to-end via the relay:
	select {
	case got := <-clientGot:
		if !bytes.Equal(got, []byte("reply:ping")) {
			t.Errorf("client got %q, want %q", got, "reply:ping")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive reply within 2s")
	}

	var res supervisor.Result
	select {
	case res = <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("recordViaSupervisor did not return within 3s — parser/supervisor may be stuck")
	}
	if res.Status != supervisor.StatusOK {
		t.Errorf("status = %v, want StatusOK; err=%v", res.Status, res.Err)
	}
	if res.FallthroughToPassthrough {
		t.Errorf("happy path must not request passthrough")
	}

	// Drain at least one mock with chunk-derived timestamps.
	select {
	case m := <-mocks:
		if m.Spec.ReqTimestampMock.IsZero() {
			t.Error("ReqTimestampMock zero — chunk timestamp not carried through")
		}
		if m.Spec.ResTimestampMock.IsZero() {
			t.Error("ResTimestampMock zero")
		}
		if m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
			t.Errorf("Res (%v) before Req (%v)", m.Spec.ResTimestampMock, m.Spec.ReqTimestampMock)
		}
		if m.Spec.Metadata["req"] != "ping" || m.Spec.Metadata["resp"] != "reply:ping" {
			t.Errorf("metadata = %+v", m.Spec.Metadata)
		}
	case <-time.After(time.Second):
		t.Fatal("no mock emitted")
	}
}

// -------- Panic parser --------

// panicParser panics on its first chunk read. Must be caught by the
// supervisor and must NOT prevent the relay from delivering bytes
// end-to-end.
func panicParser(_ context.Context, sess *supervisor.Session) error {
	_, _ = sess.ClientStream.ReadChunk()
	panic("synthetic parser panic")
}

func TestV2_PanicDoesNotBlockTraffic(t *testing.T) {
	t.Parallel()
	pp := newPipePair(t)

	var reports atomic.Int32
	sv := supervisor.New(supervisor.Config{
		Logger: zaptest.NewLogger(t),
		PanicReporter: func(_ any, _ []byte) {
			reports.Add(1)
		},
	})
	defer sv.Close()

	r := mustStartRelay(t, pp, sv.BumpActivity)
	sess, mocks := mustBuildSession(t, sv, r)

	// Destination: keep echoing forever.
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := pp.destSrv.Read(buf)
			if err != nil {
				return
			}
			_, werr := pp.destSrv.Write(append([]byte("reply:"), buf[:n]...))
			if werr != nil {
				return
			}
		}
	}()

	// Client sends one request; that's all we need to prove the
	// byte-path survives a parser panic.
	clientGot := make(chan []byte, 1)
	go func() {
		_, _ = pp.clientApp.Write([]byte("hello"))
		b := make([]byte, 64)
		n, _ := pp.clientApp.Read(b)
		clientGot <- b[:n]
	}()

	resCh := make(chan supervisor.Result, 1)
	go func() { resCh <- sv.Run(context.Background(), panicParser, sess) }()

	// Byte path survives the panic.
	select {
	case got := <-clientGot:
		if !bytes.Equal(got, []byte("reply:hello")) {
			t.Errorf("got %q, want reply:hello", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive reply despite parser panic")
	}

	var res supervisor.Result
	select {
	case res = <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("recordViaSupervisor did not return within 3s — supervisor panic-to-passthrough may be stuck")
	}
	if res.Status != supervisor.StatusPanicked {
		t.Errorf("status = %v, want StatusPanicked", res.Status)
	}
	if !res.FallthroughToPassthrough {
		t.Error("panic result must request passthrough")
	}
	if reports.Load() != 1 {
		t.Errorf("PanicReporter calls = %d, want 1", reports.Load())
	}
	if len(mocks) != 0 {
		t.Errorf("panic produced %d mocks, want 0 (partial mocks must be dropped)", len(mocks))
	}
}

// -------- Error-return parser --------

// errorReturnParser reads one client chunk, then returns a non-nil
// error without panicking. Mirrors the path real V2 parsers take when
// they hit a decode failure (invalid Content-Length, gzip header
// mismatch, malformed wire frame, etc.). The supervisor must classify
// this as StatusError AND request passthrough so the relay keeps
// forwarding bytes end-to-end — same invariant the panic test
// asserts, but for the much more common error-return path.
func errorReturnParser(_ context.Context, sess *supervisor.Session) error {
	_, _ = sess.ClientStream.ReadChunk()
	return errParserDecode{}
}

type errParserDecode struct{}

func (errParserDecode) Error() string { return "synthetic parser decode error" }

// TestV2_ErrorDoesNotBlockTraffic mirrors TestV2_PanicDoesNotBlockTraffic
// for the clean error-return path. A real parser returning an error
// (e.g. http recordv2 on "invalid content-length: ...") must not tear
// down the application's TCP connection. Confirmed empirically against
// the V2 HTTP parser via /api/stalls/v2gap/http?case=bad-cl in the
// sample-app validators; this is the unit-level regression guard.
func TestV2_ErrorDoesNotBlockTraffic(t *testing.T) {
	t.Parallel()
	pp := newPipePair(t)

	sv := supervisor.New(supervisor.Config{Logger: zaptest.NewLogger(t)})
	defer sv.Close()

	r := mustStartRelay(t, pp, sv.BumpActivity)
	sess, mocks := mustBuildSession(t, sv, r)

	// Destination: keep echoing forever so we can probe the byte
	// path AFTER the parser returns.
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := pp.destSrv.Read(buf)
			if err != nil {
				return
			}
			_, werr := pp.destSrv.Write(append([]byte("reply:"), buf[:n]...))
			if werr != nil {
				return
			}
		}
	}()

	clientGot := make(chan []byte, 1)
	go func() {
		_, _ = pp.clientApp.Write([]byte("hello"))
		b := make([]byte, 64)
		n, _ := pp.clientApp.Read(b)
		clientGot <- b[:n]
	}()

	resCh := make(chan supervisor.Result, 1)
	go func() { resCh <- sv.Run(context.Background(), errorReturnParser, sess) }()

	// Byte path survives the parser's error return.
	select {
	case got := <-clientGot:
		if !bytes.Equal(got, []byte("reply:hello")) {
			t.Errorf("got %q, want reply:hello", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive reply despite parser returning an error")
	}

	var res supervisor.Result
	select {
	case res = <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not return within 3s")
	}
	if res.Status != supervisor.StatusError {
		t.Errorf("status = %v, want StatusError", res.Status)
	}
	if !res.FallthroughToPassthrough {
		t.Error("error-return must request passthrough — otherwise the dispatcher cancels the relay and tears down the application's connection")
	}
	if len(mocks) != 0 {
		t.Errorf("error-return produced %d mocks, want 0 (incomplete decode → no mock)", len(mocks))
	}
}

// -------- Hang parser --------

// hangParser marks pending work (so the watchdog arms) and blocks
// forever on its ctx. Supervisor's activity-based watchdog should
// fire once HangBudget elapses with no activity.
func makeHangParser(sv *supervisor.Supervisor) supervisor.ParserFunc {
	return func(ctx context.Context, _ *supervisor.Session) error {
		sv.MarkPendingWork()
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestV2_HangDetected(t *testing.T) {
	t.Parallel()
	pp := newPipePair(t)

	sv := supervisor.New(supervisor.Config{
		Logger:     zaptest.NewLogger(t),
		HangBudget: 50 * time.Millisecond,
	})
	defer sv.Close()
	r := mustStartRelay(t, pp, sv.BumpActivity)
	sess, _ := mustBuildSession(t, sv, r)

	resCh := make(chan supervisor.Result, 1)
	go func() { resCh <- sv.Run(context.Background(), makeHangParser(sv), sess) }()

	select {
	case res := <-resCh:
		if res.Status != supervisor.StatusHung {
			t.Errorf("status = %v, want StatusHung; err=%v", res.Status, res.Err)
		}
		if !res.FallthroughToPassthrough {
			t.Error("hang must request passthrough")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire within 2s (budget was 50ms)")
	}
}

// -------- Kill switch --------

func TestV2_KillSwitchLifecycle(t *testing.T) {
	// This test exercises a FRESH local KillSwitch constructed via
	// newLocalKillSwitch — util.DefaultKillSwitch is NOT touched,
	// so t.Parallel is technically safe. We keep the test serial
	// anyway so future additions that DO couple to the global
	// can't accidentally race against us.
	ks := newLocalKillSwitch()
	if ks.Enabled() {
		t.Fatal("fresh KillSwitch reports Enabled")
	}
	ks.Trip()
	if !ks.Enabled() {
		t.Fatal("Trip did not enable")
	}
	ks.Reset()
	if ks.Enabled() {
		t.Fatal("Reset did not disable")
	}
}
