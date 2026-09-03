package relay

import (
	"bytes"
	"context"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A client that half-closes and then waits for the reply is a normal,
// widely used shape — Node's socket.end(data), Python's
// sock.shutdown(SHUT_WR), any request/EOF/response protocol. It works
// unproxied. Before the relay honoured it, routing such a connection
// through keploy discarded the reply: the client's FIN was read as
// "connection over" and BOTH directions were torn down, so the server's
// answer never came back.
//
// That is silent, protocol-level data loss for the recorded
// application, not merely a lost mock. It is what fails the
// node-dependency-matrix `deps-generic` scenario in k8s-proxy's
// playwright-kube-regression lanes — the one scenario of ten that
// half-closes — with "generic socket closed before response" and an
// HTTP 500 from the app.

// halfCloseHarness wires a relay between two real TCP socket pairs.
// Real sockets, not net.Pipe: net.Pipe has no notion of FIN, so it
// cannot express half-close at all.
type halfCloseHarness struct {
	clientApp net.Conn
	destSvc   net.Conn
	r         *Relay
	done      chan struct{}
}

func newHalfCloseHarness(t *testing.T, cfg Config) *halfCloseHarness {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	pair := func() (net.Conn, net.Conn) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		type res struct {
			c   net.Conn
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := ln.Accept()
			ch <- res{c, err}
		}()
		dial, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		got := <-ch
		if got.err != nil {
			t.Fatalf("accept: %v", got.err)
		}
		return dial, got.c
	}

	clientApp, srcProxy := pair()
	dstProxy, destSvc := pair()

	r := New(cfg, srcProxy, dstProxy)
	ctx, cancel := context.WithCancel(context.Background())
	h := &halfCloseHarness{clientApp: clientApp, destSvc: destSvc, r: r, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientApp.Close()
		_ = destSvc.Close()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
		}
	})
	return h
}

// TestHalfClose_ClientShutdownStillReceivesReply is the regression test
// for the deps-generic failure.
func TestHalfClose_ClientShutdownStillReceivesReply(t *testing.T) {
	h := newHalfCloseHarness(t, Config{})

	request := []byte("matrix-generic\n")
	reply := []byte("fixture-ack:matrix-generic\n")

	// The service answers only after it has seen EOF on its read side,
	// which is exactly what "the client finished its request" means for
	// an EOF-delimited protocol.
	srvDone := make(chan error, 1)
	go func() {
		got, err := io.ReadAll(h.destSvc)
		if err != nil {
			srvDone <- err
			return
		}
		if !bytes.Equal(got, request) {
			srvDone <- io.ErrUnexpectedEOF
			return
		}
		_, err = h.destSvc.Write(reply)
		srvDone <- err
	}()

	if _, err := h.clientApp.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// shutdown(SHUT_WR): done writing, still reading.
	tcp, ok := h.clientApp.(*net.TCPConn)
	if !ok {
		t.Fatalf("harness client is %T, want *net.TCPConn", h.clientApp)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	// Bounded, not a bare receive. The relay never closes the real
	// sockets (that ownership is proxy.go's), so if the FIN is not
	// forwarded the service's ReadAll blocks forever and this test
	// HANGS to the package timeout instead of reporting the defect. A
	// test that hangs on a regression is barely better than one that
	// misses it.
	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("the upstream never saw the complete request: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream never saw EOF: the relay did not forward the client's FIN, so an " +
			"EOF-delimited protocol never learns the request ended and never replies")
	}

	_ = h.clientApp.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(h.clientApp, got); err != nil {
		t.Fatalf("client never received the reply after half-closing: %v.\n"+
			"A clean EOF on one direction means \"this side finished WRITING\", not \"the "+
			"connection is over\". Tearing down the opposite direction discards the peer's "+
			"answer, and the recorded application sees its connection close before the reply "+
			"arrives — silent data loss, not just a lost mock.", err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("client received % x, want % x", got, reply)
	}
}

// The bound is what keeps the original protection: a peer that answers
// with neither data nor a FIN must not park the surviving forwarder in
// Read forever (the ~60s hang the unconditional teardown prevented).
func TestHalfClose_GraceBoundsASilentPeer(t *testing.T) {
	h := newHalfCloseHarness(t, Config{HalfCloseGrace: 300 * time.Millisecond})

	if _, err := h.clientApp.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tcp := h.clientApp.(*net.TCPConn)
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	// The service deliberately never answers and never closes.

	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay never returned after a half-close with a silent peer; the grace bound " +
			"is what stops this becoming the ~60s hang the old teardown prevented")
	}
}

// A negative grace restores the original behaviour exactly.
//
// Operators reach it as record.recordBuffer.halfCloseGrace in keploy.yml
// or the hidden --half-close-grace flag, so turning it off in the field
// needs no new build — and specifically does not need
// KEPLOY_NEW_RELAY=off, which routes to the legacy path where a
// half-closing client hangs instead.
func TestHalfClose_NegativeGraceOptsOut(t *testing.T) {
	h := newHalfCloseHarness(t, Config{HalfCloseGrace: -1})

	if _, err := h.clientApp.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tcp := h.clientApp.(*net.TCPConn)
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("with half-close disabled the relay must tear down on the first EOF, as before")
	}
}

// TestHalfClose_FullCloseEndsPromptlyWhenThePeerAlsoCloses pins the
// common case, and with it the cost of this change.
//
// A full close and a half-close are the SAME event on the wire: both
// send FIN, and the relay reads io.EOF for either. It cannot tell them
// apart from the read side, so it must assume the peer may still answer
// — which is why the grace exists at all.
//
// The cost is therefore paid only by a connection whose peer neither
// answers NOR closes; that one waits out the grace. In the ordinary
// case the peer sees EOF and closes too, the surviving forwarder reads
// its own EOF, and teardown is immediate. This test is what keeps that
// true: if it ever starts taking the full grace, every closed
// connection is holding a goroutine and its tee buffers for 30s.
func TestHalfClose_FullCloseEndsPromptlyWhenThePeerAlsoCloses(t *testing.T) {
	h := newHalfCloseHarness(t, Config{})

	// The service mirrors a real peer: it reads to EOF, then closes.
	go func() {
		_, _ = io.ReadAll(h.destSvc)
		_ = h.destSvc.Close()
	}()

	if _, err := h.clientApp.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = h.clientApp.Close()

	start := time.Now()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay never returned after both peers closed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("teardown took %v once both peers had closed. The grace must only be paid by "+
			"a peer that neither answers nor closes; charging it to every closed connection "+
			"holds a goroutine and its tee buffers for the whole window.", elapsed)
	}
}

// TestHalfClose_SurvivesTheTLSReadTimeWrapper pins the wrapper that the
// TLS upgrade stores into Relay.src / Relay.dst.
//
// readTimeReportingConn embeds net.Conn as an INTERFACE, so Go promotes
// only net.Conn's method set and CloseWrite is not in it. Without an
// explicit delegation, half-close is dead on every TLS-upgraded
// connection — which is to say dead on exactly the MITM'd traffic
// keploy exists to record, while every plaintext test here still
// passes. The same blind spot is already documented in
// directive_proc.go, where RealCertHook must run BEFORE this wrapper
// because unwrapToTLSConn cannot see through it either.
func TestHalfClose_SurvivesTheTLSReadTimeWrapper(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = raw.Close() }()
	peer := <-accepted
	defer func() { _ = peer.Close() }()

	// Exactly what directive_proc.go stores after a TLS upgrade.
	tracked := newReadTrackingConn(raw)
	wrapped := newReadTimeReportingConn(tracked, tracked)
	var asConn net.Conn = wrapped

	if !halfCloseWrite(&asConn) {
		t.Fatal("halfCloseWrite failed through readTimeReportingConn: every TLS-upgraded " +
			"connection silently loses half-close, and the plaintext tests cannot see it")
	}

	// The peer must observe EOF, which is what "the FIN arrived" means.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := peer.Read(buf); err != io.EOF {
		t.Fatalf("peer read = %v, want io.EOF — the FIN did not reach it", err)
	}
}

// TestHalfClose_SlowResponseIsNotTruncated pins the idle-based bound.
//
// A TOTAL bound cuts the surviving direction off mid-response, and does
// it silently: proxy.go closes the client socket once Run returns, so an
// EOF-delimited protocol — the shape that motivated half-close support —
// sees a clean EOF on a truncated body and believes it complete, and
// keploy records the truncation as a mock. A loud failure would be
// better than that.
func TestHalfClose_SlowResponseIsNotTruncated(t *testing.T) {
	const (
		chunks    = 12
		chunkSize = 1024
		grace     = 300 * time.Millisecond
		interval  = 100 * time.Millisecond // well inside the grace, but the
		//                                    total run far exceeds it
	)
	h := newHalfCloseHarness(t, Config{HalfCloseGrace: grace})

	go func() {
		_, _ = io.ReadAll(h.destSvc) // wait for the client's FIN
		payload := bytes.Repeat([]byte{0x7A}, chunkSize)
		for i := 0; i < chunks; i++ {
			if _, err := h.destSvc.Write(payload); err != nil {
				return
			}
			time.Sleep(interval)
		}
		_ = h.destSvc.Close()
	}()

	if _, err := h.clientApp.Write([]byte("request\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := h.clientApp.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	want := chunks * chunkSize
	// ReadFull, not ReadAll: on a truncated stream the relay has stopped
	// forwarding but nothing closes the client socket in this harness
	// (that is proxy.go's job in production), so ReadAll would hang to
	// the deadline and report a timeout instead of the byte count that
	// names the defect.
	_ = h.clientApp.SetReadDeadline(time.Now().Add(8 * time.Second))
	got := make([]byte, want)
	n, err := io.ReadFull(h.clientApp, got)
	got = got[:n]
	if err != nil && len(got) == want {
		t.Fatalf("read response: %v", err)
	}
	if len(got) != want {
		t.Fatalf("client received %d of %d bytes. The response took %v to stream, longer than "+
			"the %v grace — so a TOTAL bound truncates it, and the client sees a clean EOF on "+
			"a half body rather than an error. The bound must be on IDLE time.",
			len(got), want, time.Duration(chunks)*interval, grace)
	}
}

// TestHalfCloseGrace_NegativeSurvivesDefaulting pins the one value that
// is easy to lose in plumbing.
//
// Zero and negative both carry meaning here — "use the default" and
// "disable half-close" — so every layer that forwards this value has to
// use != 0 rather than the > 0 its siblings use, and withDefaults has to
// resolve only the zero. A layer that drops the negative silently leaves
// half-close ON for an operator who asked for it off, which is the
// opposite of what they configured and impossible to see from the
// outside.
func TestHalfCloseGrace_NegativeSurvivesDefaulting(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero resolves to the default", 0, DefaultHalfCloseGrace},
		{"negative is preserved as disabled", -1, -1},
		{"explicit value is preserved", 250 * time.Millisecond, 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Config{HalfCloseGrace: tc.in}.withDefaults().HalfCloseGrace
			if got != tc.want {
				t.Fatalf("HalfCloseGrace %v resolved to %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestHalfClose_SurvivesTheProductionConnWrappers is the test that
// should have existed from the start.
//
// Three times now, a half-close fix has been written against raw
// *net.TCPConn and shipped inert, because every wrapper between the
// proxy and the real socket embeds net.Conn as an INTERFACE — which
// promotes net.Conn's method set and nothing else. CloseWrite is not in
// that set, so the capability assertion just fails and the FIN is
// silently dropped.
//
// The wrappers that actually reach THIS relay:
//   - util.Conn — what handleConnection hands in as the client side, so
//     it is on every V2 connection
//   - readTimeReportingConn / readTrackingConn — installed by the TLS
//     upgrade, so they are on every MITM'd connection
//
// A test that drives raw sockets proves nothing about either. (util.SafeConn
// has the same defect but never reaches the relay — it wraps the LEGACY
// record session, and is covered where that path is fixed.)
func TestHalfClose_SurvivesTheProductionConnWrappers(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"util.Conn (client side of every V2 relay)", func(c net.Conn) net.Conn {
			return &proxyutil.Conn{Conn: c, Reader: c, Logger: zap.NewNop()}
		}},
		{"readTimeReportingConn (installed by the TLS upgrade)", func(c net.Conn) net.Conn {
			tracked := newReadTrackingConn(c)
			return newReadTimeReportingConn(tracked, tracked)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer func() { _ = ln.Close() }()
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					accepted <- c
				}
			}()
			raw, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = raw.Close() }()
			peer := <-accepted
			defer func() { _ = peer.Close() }()

			wrapped := tc.wrap(raw)
			if !halfCloseWrite(&wrapped) {
				t.Fatalf("halfCloseWrite failed through %T: the FIN is silently dropped and "+
					"half-close is dead on this path, while raw-socket tests stay green", wrapped)
			}

			_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 1)
			if _, err := peer.Read(buf); err != io.EOF {
				t.Fatalf("peer read = %v, want io.EOF — the FIN did not reach it through %T",
					err, wrapped)
			}
		})
	}
}
