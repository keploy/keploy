package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// relayHarness wires a Relay to two net.Pipe pairs and starts Run in
// a background goroutine. The caller writes to clientApp to simulate
// the real client writing to the proxy, and reads from destSvc to
// simulate the real destination receiving bytes.
//
//	clientApp <-> srcProxy  ---(Relay)---  dstProxy <-> destSvc
type relayHarness struct {
	t *testing.T

	clientApp net.Conn // user's app writes here (simulating real client)
	srcProxy  net.Conn // proxy's view of the client-side pipe (the real CLIENT-side socket)
	dstProxy  net.Conn // proxy's view of the dest-side pipe (the real DEST-side socket)
	destSvc   net.Conn // destination service reads here

	r *Relay

	runCtx    context.Context
	runCancel context.CancelFunc
	runDone   chan struct{}
	runErr    error
}

func newHarness(t *testing.T, cfg Config) *relayHarness {
	t.Helper()
	clientApp, srcProxy := net.Pipe()
	dstProxy, destSvc := net.Pipe()

	// Default to a no-op logger so tests are quiet.
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	r := New(cfg, srcProxy, dstProxy)
	ctx, cancel := context.WithCancel(context.Background())

	h := &relayHarness{
		t:         t,
		clientApp: clientApp,
		srcProxy:  srcProxy,
		dstProxy:  dstProxy,
		destSvc:   destSvc,
		r:         r,
		runCtx:    ctx,
		runCancel: cancel,
		runDone:   make(chan struct{}),
	}
	go func() {
		defer close(h.runDone)
		h.runErr = r.Run(ctx)
	}()
	t.Cleanup(h.shutdown)
	return h
}

// shutdown tears down the harness in the same sequence the production
// caller will: cancel ctx, wait for Run to return, close real sockets.
func (h *relayHarness) shutdown() {
	h.runCancel()
	// Close the pipes from the user side to help Run unblock on any
	// remaining reads.
	_ = h.clientApp.Close()
	_ = h.destSvc.Close()

	select {
	case <-h.runDone:
	case <-time.After(5 * time.Second):
		h.t.Errorf("Run did not return within 5s")
	}
	// Close the proxy-side pipes last; the relay never closes these
	// itself, so the test harness must.
	_ = h.srcProxy.Close()
	_ = h.dstProxy.Close()
}

// writeClient writes p from the user's app and asserts the write succeeds.
func (h *relayHarness) writeClient(p []byte) {
	h.t.Helper()
	n, err := h.clientApp.Write(p)
	if err != nil {
		h.t.Fatalf("clientApp.Write: %v", err)
	}
	if n != len(p) {
		h.t.Fatalf("clientApp.Write: short %d/%d", n, len(p))
	}
}

// readDest reads exactly n bytes from the destination service side with
// a bounded timeout. Returns the bytes received.
func (h *relayHarness) readDest(n int) []byte {
	h.t.Helper()
	return readExact(h.t, h.destSvc, n)
}

func (h *relayHarness) writeDest(p []byte) {
	h.t.Helper()
	n, err := h.destSvc.Write(p)
	if err != nil {
		h.t.Fatalf("destSvc.Write: %v", err)
	}
	if n != len(p) {
		h.t.Fatalf("destSvc.Write: short %d/%d", n, len(p))
	}
}

func (h *relayHarness) readClient(n int) []byte {
	h.t.Helper()
	return readExact(h.t, h.clientApp, n)
}

func readExact(t *testing.T, r net.Conn, n int) []byte {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	out := make([]byte, n)
	total := 0
	for total < n {
		k, err := r.Read(out[total:])
		total += k
		if err != nil {
			if total == n {
				break
			}
			t.Fatalf("read: got %d/%d, err=%v", total, n, err)
		}
	}
	return out
}

func TestForwardsBothDirections(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})

	// Client → Dest.
	payload := []byte("hello world")
	go h.writeClient(payload)
	got := h.readDest(len(payload))
	if string(got) != string(payload) {
		t.Fatalf("dest got %q, want %q", got, payload)
	}

	// Dest → Client.
	reply := []byte("hi back")
	go h.writeDest(reply)
	rec := h.readClient(len(reply))
	if string(rec) != string(reply) {
		t.Fatalf("client got %q, want %q", rec, reply)
	}

	// FakeConns should have received both chunks with correct Dir.
	c, err := h.r.ClientStream().ReadChunk()
	if err != nil {
		t.Fatalf("ClientStream.ReadChunk: %v", err)
	}
	if c.Dir != fakeconn.FromClient {
		t.Fatalf("ClientStream chunk Dir=%v, want FromClient", c.Dir)
	}
	if string(c.Bytes) != string(payload) {
		t.Fatalf("ClientStream chunk bytes=%q, want %q", c.Bytes, payload)
	}
	if c.ReadAt.IsZero() || c.WrittenAt.IsZero() {
		t.Fatalf("ClientStream chunk missing timestamps: %+v", c)
	}

	d, err := h.r.DestStream().ReadChunk()
	if err != nil {
		t.Fatalf("DestStream.ReadChunk: %v", err)
	}
	if d.Dir != fakeconn.FromDest {
		t.Fatalf("DestStream chunk Dir=%v, want FromDest", d.Dir)
	}
	if string(d.Bytes) != string(reply) {
		t.Fatalf("DestStream chunk bytes=%q, want %q", d.Bytes, reply)
	}
}

func TestTimestampsStampedAtRealBoundary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})

	before := time.Now()
	go h.writeClient([]byte("Z"))
	_ = h.readDest(1)
	after := time.Now()

	c, err := h.r.ClientStream().ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if c.ReadAt.Before(before) || c.ReadAt.After(after) {
		t.Fatalf("ReadAt %v outside window [%v, %v]", c.ReadAt, before, after)
	}
	if !c.WrittenAt.After(c.ReadAt) && !c.WrittenAt.Equal(c.ReadAt) {
		// WrittenAt must be >= ReadAt; in fast machines they can be
		// equal because time.Now() has monotonic-clock granularity.
		t.Fatalf("WrittenAt %v must be >= ReadAt %v", c.WrittenAt, c.ReadAt)
	}
	if c.SeqNo == 0 {
		t.Fatalf("expected non-zero SeqNo, got 0")
	}
}

func TestTeeDropOnMemoryGuard(t *testing.T) {
	t.Parallel()
	var pressure atomic.Bool
	pressure.Store(true)
	drops := newDropSink()

	h := newHarness(t, Config{
		MemoryGuardCheck:     pressure.Load,
		OnMarkMockIncomplete: drops.record,
	})

	// Forward traffic under memory pressure. Real traffic still flows.
	go h.writeClient([]byte("visible"))
	got := h.readDest(len("visible"))
	if string(got) != "visible" {
		t.Fatalf("dest got %q, want %q", got, "visible")
	}

	// But the FakeConn does not receive the chunk.
	_ = h.r.ClientStream().SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := h.r.ClientStream().ReadChunk()
	if err == nil {
		t.Fatalf("ClientStream should have timed out, got chunk")
	}
	if !errors.Is(err, fakeconn.ErrDeadlineExceeded) && err.(net.Error).Timeout() != true {
		t.Fatalf("expected deadline error, got %v", err)
	}

	if drops.count(DropMemoryPressure) == 0 {
		t.Fatalf("expected memory_pressure drop reason, got %v", drops.snapshot())
	}
}

func TestTeeDropOnPerConnCap(t *testing.T) {
	t.Parallel()
	drops := newDropSink()

	h := newHarness(t, Config{
		PerConnCap:           4,
		TeeChanBuf:           64,
		OnMarkMockIncomplete: drops.record,
	})

	// Don't drain the FakeConn so bytes accumulate in staging and hit
	// the cap. Write 10 bytes total in several pieces.
	payload := []byte("0123456789")
	doneW := make(chan error, 1)
	go func() {
		_, err := h.clientApp.Write(payload)
		doneW <- err
	}()
	// Drain at destination so forwarding isn't blocked.
	_ = h.readDest(len(payload))
	if err := <-doneW; err != nil {
		t.Fatalf("writeClient error: %v", err)
	}

	// Give the tee a moment to process.
	time.Sleep(50 * time.Millisecond)

	if drops.count(DropPerConnCap) == 0 {
		t.Fatalf("expected per_conn_cap drop, got reasons %v (c2d drops=%d)",
			drops.snapshot(), h.r.teeC2D.dropCount())
	}
}

func TestTeeDropOnChannelFull(t *testing.T) {
	t.Parallel()
	drops := newDropSink()

	h := newHarness(t, Config{
		TeeChanBuf:           1,
		PerConnCap:           1 << 30, // large, so only channel full triggers
		OnMarkMockIncomplete: drops.record,
	})

	// Send many small chunks while NOT draining FakeConn.
	for i := 0; i < 32; i++ {
		go h.writeClient([]byte("x"))
		_ = h.readDest(1)
	}

	// Give goroutines a moment.
	time.Sleep(100 * time.Millisecond)

	if drops.count(DropChannelFull) == 0 {
		t.Fatalf("expected channel_full drop, got %v", drops.snapshot())
	}
}

func TestDirectiveAbortMock(t *testing.T) {
	t.Parallel()
	drops := newDropSink()

	h := newHarness(t, Config{OnMarkMockIncomplete: drops.record})

	h.r.Directives() <- directive.AbortMock("parser-confused")

	// Wait for ack.
	select {
	case a := <-h.r.Acks():
		if a.Kind != directive.KindAbortMock || !a.OK {
			t.Fatalf("bad ack: %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no ack for AbortMock")
	}

	if drops.count("parser-confused") == 0 {
		t.Fatalf("expected parser-confused reason in drop sink, got %v", drops.snapshot())
	}

	// Forwarders should still be running: a subsequent write lands.
	go h.writeClient([]byte("post-abort"))
	got := h.readDest(len("post-abort"))
	if string(got) != "post-abort" {
		t.Fatalf("dest got %q, want %q", got, "post-abort")
	}
}

func TestDirectivePauseResume(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})

	h.r.Directives() <- directive.Pause(fakeconn.FromClient, "parser-pause")
	select {
	case a := <-h.r.Acks():
		if a.Kind != directive.KindPauseDir || !a.OK {
			t.Fatalf("bad pause ack: %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no pause ack")
	}

	// Bytes still flow end-to-end.
	go h.writeClient([]byte("while-paused"))
	got := h.readDest(len("while-paused"))
	if string(got) != "while-paused" {
		t.Fatalf("dest got %q, want %q", got, "while-paused")
	}

	// But FakeConn sees nothing.
	_ = h.r.ClientStream().SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := h.r.ClientStream().ReadChunk(); err == nil {
		t.Fatalf("ClientStream should have timed out while paused")
	}
	_ = h.r.ClientStream().SetReadDeadline(time.Time{})

	// Resume and confirm new bytes come through.
	h.r.Directives() <- directive.Resume(fakeconn.FromClient, "parser-resume")
	select {
	case <-h.r.Acks():
	case <-time.After(2 * time.Second):
		t.Fatalf("no resume ack")
	}

	go h.writeClient([]byte("post-resume"))
	_ = h.readDest(len("post-resume"))

	c, err := h.r.ClientStream().ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk after resume: %v", err)
	}
	if string(c.Bytes) != "post-resume" {
		t.Fatalf("got %q, want post-resume", c.Bytes)
	}
}

// fakeTLSUpgrader returns a TLSUpgradeFn that wraps the underlying
// conn with a trivial xor-based transformer so post-upgrade bytes can
// be distinguished from pre-upgrade bytes. failDest/failClient make
// the corresponding side return an error.
type xorConn struct {
	net.Conn
	mask byte
}

func (x xorConn) Read(p []byte) (int, error) {
	n, err := x.Conn.Read(p)
	for i := 0; i < n; i++ {
		p[i] ^= x.mask
	}
	return n, err
}
func (x xorConn) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	for i := range p {
		buf[i] = p[i] ^ x.mask
	}
	return x.Conn.Write(buf)
}

func fakeTLSUpgrader(failDest, failClient bool) TLSUpgradeFn {
	return func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		if isClient && failDest {
			return nil, errors.New("dest upgrade failed (simulated)")
		}
		if !isClient && failClient {
			return nil, errors.New("client upgrade failed (simulated)")
		}
		// Both sides use the same mask so bytes survive the round
		// trip: client writes plaintext → forwarder reads with
		// xor-client → writes with xor-dest → dest reads with xor
		// on its end. In this test we only upgrade the proxy's two
		// sockets; the user-side pipes remain cleartext so the xor
		// is symmetric only on the proxy boundary. To avoid garbling
		// end-to-end traffic after upgrade we use mask=0 here and
		// assert on Ack shape rather than on byte transformation.
		return xorConn{Conn: conn, mask: 0}, nil
	}
}

// countingConn counts Read and Write calls so we can prove that the
// relay started using the upgraded net.Conn after a successful TLS
// upgrade directive.
type countingConn struct {
	net.Conn
	reads, writes atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error)  { c.reads.Add(1); return c.Conn.Read(p) }
func (c *countingConn) Write(p []byte) (int, error) { c.writes.Add(1); return c.Conn.Write(p) }

func TestDirectiveUpgradeTLS_UsesUpgradedConn(t *testing.T) {
	t.Parallel()
	var upgraded countingConn
	upgraderFn := func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		if !isClient {
			// Client side only: wrap with counter. Dest returns
			// unwrapped to keep the test focused.
			upgraded.Conn = conn
			return &upgraded, nil
		}
		return conn, nil
	}

	h := newHarness(t, Config{TLSUpgradeFn: upgraderFn})

	h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
	select {
	case ack := <-h.r.Acks():
		if !ack.OK {
			t.Fatalf("TLS upgrade failed: %+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no ack")
	}

	// Dest→client traffic now traverses upgraded.Read on the way out
	// (client side) — i.e. the dst→src forwarder reads dst, writes src,
	// and src is the upgraded conn.
	go h.writeDest([]byte("post"))
	_ = h.readClient(4)

	if upgraded.writes.Load() == 0 {
		t.Fatalf("upgraded conn never Written; pointer swap did not take effect")
	}
}

func TestDirectiveUpgradeTLS_Success(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{TLSUpgradeFn: fakeTLSUpgrader(false, false)})

	before := time.Now()
	h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "parser-prelude-done")

	var ack directive.Ack
	select {
	case ack = <-h.r.Acks():
	case <-time.After(2 * time.Second):
		t.Fatalf("no TLS ack")
	}
	after := time.Now()

	if !ack.OK {
		t.Fatalf("TLS upgrade failed: %+v", ack)
	}
	if ack.BoundaryReadAt.Before(before) || ack.BoundaryReadAt.After(after) {
		t.Fatalf("BoundaryReadAt %v outside window", ack.BoundaryReadAt)
	}
	if ack.BoundaryWrittenAt.Before(ack.BoundaryReadAt) {
		t.Fatalf("BoundaryWrittenAt %v before BoundaryReadAt %v",
			ack.BoundaryWrittenAt, ack.BoundaryReadAt)
	}

	// Post-upgrade bytes still flow (xor mask=0 in the fake).
	go h.writeClient([]byte("post-tls"))
	got := h.readDest(len("post-tls"))
	if string(got) != "post-tls" {
		t.Fatalf("post-TLS got %q, want post-tls", got)
	}
}

func TestDirectiveUpgradeTLS_FailDest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{TLSUpgradeFn: fakeTLSUpgrader(true, false)})

	h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "try-upgrade")

	var ack directive.Ack
	select {
	case ack = <-h.r.Acks():
	case <-time.After(2 * time.Second):
		t.Fatalf("no TLS ack")
	}

	if ack.OK {
		t.Fatalf("expected OK=false, got ok ack: %+v", ack)
	}
	if ack.Err == nil {
		t.Fatalf("expected ack.Err to be set")
	}

	// Forwarders keep running on the original conns.
	go h.writeClient([]byte("still-plain"))
	got := h.readDest(len("still-plain"))
	if string(got) != "still-plain" {
		t.Fatalf("after failed TLS: got %q, want still-plain", got)
	}
}

func TestDirectiveUpgradeTLS_NoUpgraderConfigured(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{}) // no TLSUpgradeFn
	h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "no-upgrader")
	select {
	case ack := <-h.r.Acks():
		if ack.OK {
			t.Fatalf("expected OK=false without upgrader")
		}
		if !errors.Is(ack.Err, ErrNoTLSUpgrader) {
			t.Fatalf("expected ErrNoTLSUpgrader, got %v", ack.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no ack")
	}
}

func TestCleanShutdownOnCtxCancel(t *testing.T) {
	t.Parallel()
	// Don't use the harness's built-in shutdown timing; we want to
	// observe the cancel itself.
	clientApp, srcProxy := net.Pipe()
	dstProxy, destSvc := net.Pipe()
	t.Cleanup(func() {
		_ = clientApp.Close()
		_ = destSvc.Close()
		_ = srcProxy.Close()
		_ = dstProxy.Close()
	})

	r := New(Config{}, srcProxy, dstProxy)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Let goroutines start.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			// Benign errors like deadline-exceeded are filtered out
			// inside Run; a non-nil return here would indicate a
			// genuine forwarder error we surfaced.
			t.Fatalf("Run returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}
}

func TestFinalizeMockIsNoop(t *testing.T) {
	t.Parallel()
	drops := newDropSink()
	h := newHarness(t, Config{OnMarkMockIncomplete: drops.record})

	h.r.Directives() <- directive.FinalizeMock("commit")
	select {
	case ack := <-h.r.Acks():
		if !ack.OK || ack.Kind != directive.KindFinalizeMock {
			t.Fatalf("bad ack: %+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no ack")
	}
	if len(drops.snapshot()) != 0 {
		t.Fatalf("FinalizeMock should not mark incomplete, got %v", drops.snapshot())
	}
}

func TestSecondRunReturnsError(t *testing.T) {
	t.Parallel()
	clientApp, srcProxy := net.Pipe()
	dstProxy, destSvc := net.Pipe()
	t.Cleanup(func() {
		_ = clientApp.Close()
		_ = destSvc.Close()
		_ = srcProxy.Close()
		_ = dstProxy.Close()
	})

	r := New(Config{}, srcProxy, dstProxy)
	ctx, cancel := context.WithCancel(context.Background())

	go func() { _ = r.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Second call should refuse.
	if err := r.Run(context.Background()); !errors.Is(err, ErrRelayAlreadyRun) {
		t.Fatalf("second Run got %v, want ErrRelayAlreadyRun", err)
	}
}

func TestBumpActivityInvoked(t *testing.T) {
	t.Parallel()
	var count atomic.Int64
	h := newHarness(t, Config{BumpActivity: func() { count.Add(1) }})

	go h.writeClient([]byte("tick"))
	_ = h.readDest(4)
	go h.writeDest([]byte("tock"))
	_ = h.readClient(4)

	// Give the forwarders a moment to run the BumpActivity callback.
	time.Sleep(20 * time.Millisecond)
	if got := count.Load(); got < 2 {
		t.Fatalf("BumpActivity count=%d, want >=2", got)
	}
}

// dropSink mirrors dropRecorder in tee_test.go but lives here so the
// relay tests stay self-contained; kept separate to avoid exporting
// test helpers across files.
type dropSink struct {
	mu   sync.Mutex
	seen []string
}

func newDropSink() *dropSink { return &dropSink{} }

func (d *dropSink) record(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, reason)
}

func (d *dropSink) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.seen))
	copy(out, d.seen)
	return out
}

func (d *dropSink) count(reason string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.seen {
		if r == reason {
			n++
		}
	}
	return n
}
