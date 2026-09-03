package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.uber.org/zap"
)

// These tests cover Config.HoldClientWrites against the choreography it
// was built for: MySQL's CLIENT_SSL upgrade.
//
// MySQL is server-speaks-first. The upgrade is: server greets, client
// replies with a 36-byte SSLRequest, and then — with no further server
// turn — the client starts its TLS handshake on the SAME connection. So
// exactly the SSLRequest, and nothing after it, may reach the upstream
// in cleartext. The ClientHello that follows must not be forwarded,
// because by the time it is written keploy is the one terminating that
// handshake.
//
// TestClientWriteHold_NoHoldLeaksClientHello is the negative control and
// is the most important test in this file: it asserts that WITHOUT the
// hold the ClientHello does leak. Two earlier drafts of these tests
// passed against unfixed code — once because net.Pipe's unbuffered
// Write serialises the application behind the relay and makes the race
// impossible to stage, and once because the byte counter stopped at the
// TLS upgrade while the leak actually lands just after it. A positive
// test alone cannot tell "the hold works" from "the harness never
// reproduced the race"; the control is what separates them.

const (
	// sslRequestLen is the fixed width of MySQL's SSLRequest packet:
	// a 4-byte header plus the 32-byte truncated HandshakeResponse41.
	sslRequestLen = 36
	// clientHelloLen is a plausible TLS ClientHello size. The exact
	// value only has to be distinguishable from sslRequestLen.
	clientHelloLen = 120
)

func mysqlSSLRequest() []byte {
	p := make([]byte, sslRequestLen)
	p[0] = byte(sslRequestLen - 4) // payload length, 3-byte LE
	p[3] = 1                       // sequence id
	for i := 4; i < sslRequestLen; i++ {
		p[i] = byte(i)
	}
	return p
}

func tlsClientHello() []byte {
	p := make([]byte, clientHelloLen)
	// Record type 0x16 (handshake), TLS 1.0 record version.
	p[0], p[1], p[2] = 0x16, 0x03, 0x01
	return p
}

// tcpHarness is newHarness over REAL TCP sockets instead of net.Pipe.
//
// The distinction is load-bearing. net.Pipe is synchronous and
// unbuffered: a client Write blocks until the relay reads it, which
// accidentally serialises the application behind the relay and hides the
// race entirely. A real socket has kernel buffers, so the application's
// ClientHello lands in the buffer the instant it is written and the
// forwarder picks it up on its next read — exactly as in production.
type tcpHarness struct {
	clientApp net.Conn
	destSvc   net.Conn
	r         *Relay
	cancel    context.CancelFunc
	done      chan struct{}

	upMu     sync.Mutex
	upstream []byte
}

func newTCPHarness(t *testing.T, cfg Config) *tcpHarness {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	pair := func() (a, b net.Conn) {
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
	h := &tcpHarness{clientApp: clientApp, destSvc: destSvc, r: r, cancel: cancel, done: make(chan struct{})}
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
		case <-time.After(2 * time.Second):
		}
	})
	return h
}

// drainUpstream records every byte the real destination service
// receives. It counts them ALL, not just the ones before the TLS
// upgrade: the leak this file is about lands just AFTER the upgrade
// completes, because the forwarder is still running and flushes what the
// application already wrote.
func (h *tcpHarness) drainUpstream(t *testing.T) {
	t.Helper()
	go func() {
		buf := make([]byte, 4096)
		for {
			_ = h.destSvc.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := h.destSvc.Read(buf)
			if n > 0 {
				h.upMu.Lock()
				h.upstream = append(h.upstream, buf[:n]...)
				h.upMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

// upstreamBytes returns a copy of everything the destination has seen.
// Callers settle first so a leak in flight is not mistaken for absence.
func (h *tcpHarness) upstreamBytes() []byte {
	h.upMu.Lock()
	defer h.upMu.Unlock()
	return append([]byte(nil), h.upstream...)
}

func awaitTCPAck(t *testing.T, h *tcpHarness) directive.Ack {
	t.Helper()
	select {
	case ack := <-h.r.Acks():
		return ack
	case <-time.After(3 * time.Second):
		t.Fatal("no ack from the directive")
		return directive.Ack{}
	}
}

// greetAndAwait plays the server's MySQL greeting and waits for the real
// client to receive it, establishing that D2C forwarding is unaffected
// by a client-side hold.
func (h *tcpHarness) greetAndAwait(t *testing.T) {
	t.Helper()
	greeting := []byte{0x0a, 'm', 'y', 's', 'q', 'l', 0x00}
	if _, err := h.destSvc.Write(greeting); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	got := make([]byte, len(greeting))
	_ = h.clientApp.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(h.clientApp, got); err != nil {
		t.Fatalf("client never saw the greeting (a client hold must not stall dest->client): %v", err)
	}
}

// awaitParserSees consumes exactly n bytes off the parser's client
// stream and returns once it has them, mirroring a parser that reads a
// fixed-width packet before deciding what to do about it.
func (h *tcpHarness) awaitParserSees(t *testing.T, n int) []byte {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(h.r.ClientStream(), buf)
		ch <- res{buf, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("parser never saw %d bytes on the teed client stream: %v", n, got.err)
		}
		return got.b
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the parser to see %d bytes on the teed client stream", n)
		return nil
	}
}

// awaitForwarderTeed blocks until the C2D forwarder has read and teed at
// least n client bytes. It CONSUMES NOTHING — it observes the tee's
// accepted-byte counter, which is the same stream position the relay
// itself names when it discards handshake bytes.
//
// It exists because the two jobs the old single awaitParserSees call was
// doing are in direct conflict.
//
// Job one is staging the production ordering: the forwarder is parked in
// Read on the client socket, so it holds the ClientHello long before the
// parser — which has to be scheduled, decode a packet and decide — can
// say anything about it. The forwarder being AHEAD of the parser is
// precisely why the leak exists, and a test that leaves it to a race
// stops reproducing it silently (an earlier draft did exactly that).
//
// Job two was reading the ClientHello off the parser's stream, and that
// is not staging at all — it is the assertion's own subject being
// quietly cleaned up. A real MySQL parser reads the 36-byte SSLRequest
// and stops; the ClientHello behind it stays in its FakeConn, which is
// the entire defect. Draining it in the shared helper made every test in
// this file blind to it.
//
// So the ordering is established here, without touching the stream, and
// the parser's read is spelled out separately as the fixed-width packet
// read it models.
func (h *tcpHarness) awaitForwarderTeed(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.r.teeC2D.acceptedBytes() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the forwarder to tee %d client bytes; it teed %d. Without "+
		"the forwarder demonstrably ahead of the parser these tests are not staging the race "+
		"they exist to reproduce.", n, h.r.teeC2D.acceptedBytes())
}

// runSSLRequestUpgrade drives the MySQL CLIENT_SSL exchange and returns
// what the upstream received in cleartext. coalesced controls whether
// the client writes the SSLRequest and the ClientHello as one write —
// which real clients do intermittently, and which is why the flush has
// to split by byte count rather than by chunk.
func runSSLRequestUpgrade(t *testing.T, cfg Config, flushBytes int, coalesced bool) (*tcpHarness, []byte, directive.Ack, []byte) {
	t.Helper()

	sslRequest := mysqlSSLRequest()
	clientHello := tlsClientHello()

	// The client-side upgrade reads the ClientHello off whatever conn
	// the relay hands it. That is the real proof the held remainder is
	// not lost: it arrives either prepended from the stash or straight
	// off the socket, and either way the handshake must be able to read
	// it. Captured so the assertions can check it is the ClientHello and
	// not, say, the SSLRequest replayed by mistake.
	var helloMu sync.Mutex
	var helloSeen []byte
	fn := func(_ context.Context, conn net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
		if !isClient {
			seen := make([]byte, clientHelloLen)
			// Short deadline on purpose: in the no-hold control the
			// ClientHello has already gone upstream, so this read is
			// meant to come up empty rather than stall the test.
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(conn, seen); err == nil {
				helloMu.Lock()
				helloSeen = seen
				helloMu.Unlock()
			}
			_ = conn.SetReadDeadline(time.Time{})
		}
		return &closeTrackingConn{Conn: conn}, nil
	}
	cfg.TLSUpgradeFn = fn

	h := newTCPHarness(t, cfg)
	h.drainUpstream(t)
	h.greetAndAwait(t)

	// Ordering is staged as a fact rather than left to a race — see
	// awaitForwarderTeed for why, and for why the wait must not consume
	// anything off the parser's stream.
	if coalesced {
		// One write, both messages. The forwarder sees them as a single
		// read, so nothing chunk-shaped can separate them.
		if _, err := h.clientApp.Write(append(append([]byte(nil), sslRequest...), clientHello...)); err != nil {
			t.Fatalf("write coalesced SSLRequest+ClientHello: %v", err)
		}
	} else {
		if _, err := h.clientApp.Write(sslRequest); err != nil {
			t.Fatalf("write SSLRequest: %v", err)
		}
		h.awaitForwarderTeed(t, sslRequestLen)
		// The application's TLS stack now writes its ClientHello
		// immediately. Nothing asks it to wait, because on a real
		// connection nothing can.
		if _, err := h.clientApp.Write(clientHello); err != nil {
			t.Fatalf("write ClientHello: %v", err)
		}
	}
	// Both messages are now through the forwarder. In the no-hold case
	// this is the moment the leak has already happened; in the hold case
	// it is the moment both are sitting in the stash awaiting the split.
	h.awaitForwarderTeed(t, sslRequestLen+clientHelloLen)

	// Now the parser does what a MySQL parser does: read exactly the
	// SSLRequest packet, decide on it, and dispatch. It does NOT read
	// the ClientHello — nothing in the protocol would make it, and the
	// bytes are not its to consume.
	h.awaitParserSees(t, sslRequestLen)

	h.r.Directives() <- directive.Directive{
		Kind: directive.KindUpgradeTLS,
		TLS: &directive.UpgradeTLSParams{
			DestTLSConfig:    &tls.Config{},
			ClientTLSConfig:  &tls.Config{},
			ClientFlushBytes: flushBytes,
		},
		Reason: "mysql client_ssl",
	}

	ack := awaitTCPAck(t, h)
	// Let anything still in flight land, so a leak a moment behind the
	// ack is not mistaken for no leak at all.
	time.Sleep(300 * time.Millisecond)

	helloMu.Lock()
	hello := helloSeen
	helloMu.Unlock()
	return h, h.upstreamBytes(), ack, hello
}

// TestClientWriteHold_NoHoldLeaksClientHello is the negative control:
// with no hold, the ClientHello reaches the real server in cleartext.
// If this test ever passes-by-not-leaking, the harness has stopped
// reproducing the race and every other test in this file is vacuous.
func TestClientWriteHold_NoHoldLeaksClientHello(t *testing.T) {
	_, upstream, ack, _ := runSSLRequestUpgrade(t, Config{}, 0, false)
	if !ack.OK {
		t.Fatalf("TLS upgrade failed: %+v", ack)
	}
	if len(upstream) <= sslRequestLen {
		t.Fatalf("negative control did not reproduce the leak: upstream saw %d cleartext bytes, "+
			"expected more than the %d-byte SSLRequest. Without a demonstrated leak here, the "+
			"hold tests below cannot distinguish a working hold from a harness that never staged "+
			"the race.", len(upstream), sslRequestLen)
	}
}

// TestClientWriteHold_SSLRequestPrefixOnly is the fix: the hold keeps
// the ClientHello off the wire while still delivering the SSLRequest the
// server needs in order to switch to TLS.
func TestClientWriteHold_SSLRequestPrefixOnly(t *testing.T) {
	cfg := Config{HoldClientWrites: true}
	_, upstream, ack, hello := runSSLRequestUpgrade(t, cfg, sslRequestLen, false)

	if !ack.OK {
		t.Fatalf("TLS upgrade failed: %+v", ack)
	}
	if len(upstream) != sslRequestLen {
		t.Fatalf("upstream received %d cleartext bytes, want exactly %d (the SSLRequest and "+
			"nothing else). The surplus is the application's ClientHello: the real MySQL server "+
			"reads it as protocol, and keploy's own destination handshake then runs against a "+
			"desynchronised connection.", len(upstream), sslRequestLen)
	}
	if !bytes.Equal(upstream, mysqlSSLRequest()) {
		t.Fatalf("upstream received %d bytes but not the SSLRequest: % x", len(upstream), upstream)
	}
	if len(hello) == 0 || hello[0] != 0x16 {
		t.Fatalf("the client-side handshake never read the ClientHello (got % x); holding it back "+
			"from the destination must not lose it — it belongs to keploy's own handshake", hello)
	}
}

// TestClientWriteHold_CoalescedSSLRequestAndClientHello is the same
// exchange with both messages in a single client write. Real clients do
// this intermittently, and it is why the flush splits by byte count: no
// chunk boundary separates the two here.
func TestClientWriteHold_CoalescedSSLRequestAndClientHello(t *testing.T) {
	cfg := Config{HoldClientWrites: true}
	_, upstream, ack, hello := runSSLRequestUpgrade(t, cfg, sslRequestLen, true)

	if !ack.OK {
		t.Fatalf("TLS upgrade failed: %+v", ack)
	}
	if len(upstream) != sslRequestLen {
		t.Fatalf("coalesced write: upstream received %d cleartext bytes, want exactly %d. A "+
			"chunk-shaped flush forwards the whole read and leaks the ClientHello with it; the "+
			"split has to be by byte count.", len(upstream), sslRequestLen)
	}
	if !bytes.Equal(upstream, mysqlSSLRequest()) {
		t.Fatalf("coalesced write: upstream got %d bytes but not the SSLRequest: % x", len(upstream), upstream)
	}
	if len(hello) == 0 || hello[0] != 0x16 {
		t.Fatalf("coalesced write: the client-side handshake never read the ClientHello (got % x)", hello)
	}
}

// postTLSHandshakeResponse is a stand-in for the HandshakeResponse41 a
// MySQL client sends once TLS is up. Only two things matter: it is not
// the ClientHello, and its first byte is not 0x16, so a parser that got
// handed the ClientHello instead fails on content rather than on length.
func postTLSHandshakeResponse() []byte {
	p := []byte{0x20, 0x00, 0x00, 0x01}
	return append(p, []byte("post-tls-handshake-response")...)
}

// assertParserResumesAfterUpgrade writes a post-TLS message from the
// client and asserts the parser's next bytes are exactly that message.
//
// In production the relay hands the parser plaintext lifted out of the
// upgraded tls.Conn; here the harness's TLSUpgradeFn is a passthrough,
// so a plain socket write after the ack is the same event.
func assertParserResumesAfterUpgrade(t *testing.T, h *tcpHarness) {
	t.Helper()
	post := postTLSHandshakeResponse()
	if _, err := h.clientApp.Write(post); err != nil {
		t.Fatalf("write post-TLS message: %v", err)
	}
	got := h.awaitParserSees(t, len(post))
	if !bytes.Equal(got, post) {
		t.Fatalf("after the TLS upgrade the parser read % x, want % x.\n"+
			"The relay consumed the held ClientHello for its own client-side handshake but a "+
			"copy of it was still teed into the parser's stream, so the parser reads TLS "+
			"handshake bytes where the post-TLS HandshakeResponse41 should be. A MySQL header "+
			"of 16 03 01 00 declares payloadLength=66326, so ReadRequiredBytes blocks until the "+
			"hang watchdog retires the parser and the connection falls through to passthrough — "+
			"zero mocks on every MySQL TLS connection.", got, post)
	}
}

// TestClientWriteHold_ParserDoesNotSeeClientHello is the other half of
// the hold's contract, and the one the wire-level tests above cannot
// see: bytes the relay CONSUMES for the handshake must not also reach
// the parser as stream content.
//
// Keeping the ClientHello off the wire and leaving a copy of it in the
// parser's stream trades a leak for a hang, which is strictly worse —
// the leak corrupted one upstream connection, the hang costs every mock
// on every MySQL TLS connection.
func TestClientWriteHold_ParserDoesNotSeeClientHello(t *testing.T) {
	h, upstream, ack, hello := runSSLRequestUpgrade(t, Config{HoldClientWrites: true}, sslRequestLen, false)
	if !ack.OK {
		t.Fatalf("TLS upgrade failed: %+v", ack)
	}
	if len(upstream) != sslRequestLen {
		t.Fatalf("upstream received %d cleartext bytes, want exactly %d", len(upstream), sslRequestLen)
	}
	if len(hello) == 0 || hello[0] != 0x16 {
		t.Fatalf("the client-side handshake never read the ClientHello (got % x); this test is "+
			"only meaningful if the handshake really did consume it", hello)
	}
	assertParserResumesAfterUpgrade(t, h)
}

// TestClientWriteHold_ParserDoesNotSeeCoalescedClientHello is the same
// assertion when both messages arrive in one read — the case where the
// ClientHello is not a chunk of its own but the tail of the chunk whose
// head the parser legitimately consumed. Any fix that works by dropping
// whole pending chunks passes the test above and fails this one.
func TestClientWriteHold_ParserDoesNotSeeCoalescedClientHello(t *testing.T) {
	h, upstream, ack, hello := runSSLRequestUpgrade(t, Config{HoldClientWrites: true}, sslRequestLen, true)
	if !ack.OK {
		t.Fatalf("TLS upgrade failed: %+v", ack)
	}
	if len(upstream) != sslRequestLen {
		t.Fatalf("coalesced: upstream received %d cleartext bytes, want exactly %d", len(upstream), sslRequestLen)
	}
	if len(hello) == 0 || hello[0] != 0x16 {
		t.Fatalf("coalesced: the client-side handshake never read the ClientHello (got % x)", hello)
	}
	assertParserResumesAfterUpgrade(t, h)
}

// TestClientWriteHold_ReleaseFlushesEverything covers the no-TLS branch:
// a MySQL client that sends a plaintext HandshakeResponse. The parser
// releases the hold and every held byte is delivered, in order, followed
// by normal forwarding.
func TestClientWriteHold_ReleaseFlushesEverything(t *testing.T) {
	h := newTCPHarness(t, Config{HoldClientWrites: true})
	h.drainUpstream(t)
	h.greetAndAwait(t)

	plainAuth := bytes.Repeat([]byte{0xAB}, 64)
	if _, err := h.clientApp.Write(plainAuth); err != nil {
		t.Fatalf("write HandshakeResponse: %v", err)
	}
	h.awaitParserSees(t, len(plainAuth))

	// Nothing has reached the server yet: that is the hold doing its job.
	if got := h.upstreamBytes(); len(got) != 0 {
		t.Fatalf("upstream saw %d bytes while the hold was up, want 0", len(got))
	}

	h.r.Directives() <- directive.ReleaseClient("mysql plaintext auth")
	if ack := awaitTCPAck(t, h); !ack.OK {
		t.Fatalf("release-client failed: %+v", ack)
	}

	// The held bytes are delivered...
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(h.upstreamBytes()) < len(plainAuth) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstreamBytes(); !bytes.Equal(got, plainAuth) {
		t.Fatalf("release flushed %d bytes, want the %d held bytes verbatim", len(got), len(plainAuth))
	}

	// ...and forwarding resumes for everything after.
	query := []byte("SELECT 1")
	if _, err := h.clientApp.Write(query); err != nil {
		t.Fatalf("write post-release query: %v", err)
	}
	want := append(append([]byte(nil), plainAuth...), query...)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(h.upstreamBytes()) < len(want) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstreamBytes(); !bytes.Equal(got, want) {
		t.Fatalf("after release the forwarder delivered %d bytes, want %d — the hold must come "+
			"off for good, not just for the flush", len(got), len(want))
	}
}

// TestClientWriteHold_CapBreachReleases covers a parser that arms a hold
// and then never answers. The relay must choose the application over the
// recording: flush, resume, and report the mock incomplete.
func TestClientWriteHold_CapBreachReleases(t *testing.T) {
	var incMu sync.Mutex
	var reasons []string
	cfg := Config{
		HoldClientWrites: true,
		ClientHoldCap:    1024,
		OnMarkMockIncomplete: func(reason string) {
			incMu.Lock()
			reasons = append(reasons, reason)
			incMu.Unlock()
		},
	}
	h := newTCPHarness(t, cfg)
	h.drainUpstream(t)
	h.greetAndAwait(t)

	// Overrun the cap without the parser ever issuing a directive.
	payload := bytes.Repeat([]byte{0xCD}, 4096)
	if _, err := h.clientApp.Write(payload); err != nil {
		t.Fatalf("write oversized payload: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(h.upstreamBytes()) < len(payload) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstreamBytes(); len(got) != len(payload) {
		t.Fatalf("cap breach delivered %d of %d bytes; the relay must not strand the client's "+
			"request when it gives up on the hold", len(got), len(payload))
	}

	incMu.Lock()
	got := append([]string(nil), reasons...)
	incMu.Unlock()
	var found bool
	for _, reason := range got {
		if reason == "client_hold_cap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cap breach reported %v, want a client_hold_cap mark — a silently degraded "+
			"recording is worse than a loud one", got)
	}
}

// TestClientWriteHold_ShortFlushIsRejected: a parser asking to forward
// more than it ever saw is a bug in the parser. Forwarding the short
// prefix would leave the server blocked on the rest of a message that is
// never coming, so the directive fails instead.
func TestClientWriteHold_ShortFlushIsRejected(t *testing.T) {
	cfg := Config{HoldClientWrites: true}
	_, upstream, ack, _ := runSSLRequestUpgrade(t, cfg, sslRequestLen+clientHelloLen+1, false)

	if ack.OK {
		t.Fatal("upgrade acked OK with a flush request larger than the stash; a byte-exact " +
			"split that cannot be performed must be refused, not approximated")
	}
	// The refusal is loud, and nothing is silently destroyed: no TLS
	// came up, so every held byte is delivered.
	//
	// The alternative — dropping them with the pause — was worse in
	// both directions. takeStashedPrefix mutates, so validating after
	// claiming ate the prefix and abandoned it; the server was then
	// left waiting for a message whose head the relay had swallowed,
	// while passthrough resumed and fed it the continuation. Two hangs
	// and a desynchronised stream, from a parser bug.
	//
	// Delivering does mean that on a CLIENT_SSL connection the
	// ClientHello reaches the server in cleartext — the very thing the
	// hold prevents. That is accepted here and only here: reaching this
	// path means the parser asked for a split that its own view of the
	// stream cannot justify, so the connection is already unsalvageable
	// (the client is committed to a TLS handshake nobody will answer).
	// Given a choice between two broken outcomes, the server sees a
	// protocol error and closes promptly, instead of both peers
	// blocking until their own timeouts.
	if len(upstream) != sslRequestLen+clientHelloLen {
		t.Fatalf("a refused flush delivered %d of %d held bytes; a refusal must not silently "+
			"destroy what the client already sent", len(upstream), sslRequestLen+clientHelloLen)
	}
}

// TestReleaseClient_WithoutHoldIsRejected: the directive installs a
// pause and nudges read deadlines into the past. Acting on it with no
// hold up would leave those deadlines stuck there, spinning both
// forwarders on EAGAIN for the rest of the connection.
func TestReleaseClient_WithoutHoldIsRejected(t *testing.T) {
	h := newTCPHarness(t, Config{})
	h.drainUpstream(t)

	h.r.Directives() <- directive.ReleaseClient("no hold armed")
	ack := awaitTCPAck(t, h)
	if ack.OK {
		t.Fatal("release-client acked OK with no hold active; it must be refused")
	}
	if ack.Err == nil {
		t.Fatal("release-client refusal carried no error")
	}

	// The connection must still work.
	msg := []byte("still alive")
	if _, err := h.clientApp.Write(msg); err != nil {
		t.Fatalf("write after refused release: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(h.upstreamBytes()) < len(msg) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstreamBytes(); !bytes.Equal(got, msg) {
		t.Fatalf("forwarding broke after a refused release: upstream got % x, want % x", got, msg)
	}
}

// awaitUpstream waits until the destination has received at least n
// bytes, then returns everything it has.
func (h *tcpHarness) awaitUpstream(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.upstreamBytes(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h.upstreamBytes()
}

// TestClientWriteHold_ReleasedOnParserAbort covers the supervisor's
// abort path: hang, panic, memory cap and cancellation all retire a
// parser without a releasing directive, and the dispatcher's answer to
// every one of them is to leave the relay forwarding raw bytes so user
// traffic survives.
//
// A hold that outlives its parser falsifies that promise outright — the
// client direction stays blackholed for the rest of the connection, and
// ClientHoldCap cannot rescue it, because a client blocked waiting for
// a reply to the request we are sitting on never sends the further
// bytes a cap would count.
func TestClientWriteHold_ReleasedOnParserAbort(t *testing.T) {
	h := newTCPHarness(t, Config{HoldClientWrites: true})
	h.drainUpstream(t)
	h.greetAndAwait(t)

	held := bytes.Repeat([]byte{0x5A}, 48)
	if _, err := h.clientApp.Write(held); err != nil {
		t.Fatalf("write held payload: %v", err)
	}
	h.awaitParserSees(t, len(held))
	if got := h.upstreamBytes(); len(got) != 0 {
		t.Fatalf("upstream saw %d bytes while the hold was up, want 0", len(got))
	}

	// Exactly what proxy_v2.go's SessionOnAbort does — and nothing
	// more. The release is deliberately NOT called here: PauseTees owns
	// it, so that forgetting it at a call site is impossible rather
	// than merely discouraged. If someone decouples them, this test
	// fails instead of a customer's connection hanging.
	h.r.PauseTees()
	_ = h.r.ClientStream().Close()
	_ = h.r.DestStream().Close()

	if got := h.awaitUpstream(t, len(held)); !bytes.Equal(got, held) {
		t.Fatalf("abort delivered %d of %d held bytes; the relay promises user traffic is "+
			"unaffected when a parser is retired", len(got), len(held))
	}

	// And the connection keeps working with no parser attached.
	after := []byte("post-abort")
	if _, err := h.clientApp.Write(after); err != nil {
		t.Fatalf("write after abort: %v", err)
	}
	want := append(append([]byte(nil), held...), after...)
	if got := h.awaitUpstream(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("forwarding did not resume after abort: got %d bytes, want %d", len(got), len(want))
	}
}

// TestClientWriteHold_NoUpgraderStillDelivers: a relay with no
// TLSUpgradeFn refuses the directive. That refusal used to return above
// every hold-clearing path, leaving the connection blackholed for good.
func TestClientWriteHold_NoUpgraderStillDelivers(t *testing.T) {
	h := newTCPHarness(t, Config{HoldClientWrites: true})
	h.drainUpstream(t)
	h.greetAndAwait(t)

	held := mysqlSSLRequest()
	if _, err := h.clientApp.Write(held); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	h.awaitParserSees(t, len(held))

	h.r.Directives() <- directive.Directive{
		Kind:   directive.KindUpgradeTLS,
		TLS:    &directive.UpgradeTLSParams{ClientFlushBytes: len(held)},
		Reason: "no upgrader configured",
	}
	ack := awaitTCPAck(t, h)
	if ack.OK {
		t.Fatal("upgrade acked OK with no TLSUpgradeFn configured")
	}
	if got := h.awaitUpstream(t, len(held)); !bytes.Equal(got, held) {
		t.Fatalf("a refused upgrade stranded %d of %d held bytes; the hold must not outlive "+
			"the directive that failed to end it", len(got), len(held))
	}
}

// TestClientWriteHold_FlushWithoutHoldIsRefused: ClientFlushBytes only
// means something under a hold. Honouring it without one would tell the
// parser its byte-exact split had been performed on a stream that was
// forwarded in real time — the leak, with a green light on it.
func TestClientWriteHold_FlushWithoutHoldIsRefused(t *testing.T) {
	fn := func(_ context.Context, conn net.Conn, _ bool, _ *tls.Config) (net.Conn, error) {
		return &closeTrackingConn{Conn: conn}, nil
	}
	h := newTCPHarness(t, Config{TLSUpgradeFn: fn}) // no hold
	h.drainUpstream(t)

	h.r.Directives() <- directive.Directive{
		Kind:   directive.KindUpgradeTLS,
		TLS:    &directive.UpgradeTLSParams{ClientFlushBytes: 36},
		Reason: "flush without a hold",
	}
	ack := awaitTCPAck(t, h)
	if ack.OK {
		t.Fatal("upgrade acked OK for a byte-exact flush with no hold to split")
	}
	if ack.Err == nil {
		t.Fatal("refusal carried no error")
	}
}

// TestClientWriteHold_PreambleMismatchDeliversHeldBytes: the documented
// "server declined TLS, record the cleartext path" outcome acks OK=true
// with TLSUpgraded=false. No handshake ran, so the held remainder is
// ordinary cleartext the server is still waiting for — and endPause
// discards stashes unconditionally, so it used to vanish under a
// success ack.
func TestClientWriteHold_PreambleMismatchDeliversHeldBytes(t *testing.T) {
	fn := func(_ context.Context, conn net.Conn, _ bool, _ *tls.Config) (net.Conn, error) {
		return &closeTrackingConn{Conn: conn}, nil
	}
	h := newTCPHarness(t, Config{HoldClientWrites: true, TLSUpgradeFn: fn})
	h.drainUpstream(t)

	head := bytes.Repeat([]byte{0x11}, 20)
	tail := bytes.Repeat([]byte{0x22}, 30)
	if _, err := h.clientApp.Write(append(append([]byte(nil), head...), tail...)); err != nil {
		t.Fatalf("write client payload: %v", err)
	}
	h.awaitParserSees(t, len(head)+len(tail))

	// The server answers 'N' where the parser required 'S' — written
	// AFTER the directive, so the pause barrier is up and the handler
	// owns the socket. Writing it earlier lets the D2C forwarder
	// consume it and forward it to the client, and the handler's
	// preamble read then blocks on a byte that has already gone.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = h.destSvc.Write([]byte{'N'})
	}()

	h.r.Directives() <- directive.Directive{
		Kind: directive.KindUpgradeTLS,
		TLS: &directive.UpgradeTLSParams{
			DestTLSConfig:        &tls.Config{},
			ClientTLSConfig:      &tls.Config{},
			ClientFlushBytes:     len(head),
			PreambleReadFromDest: 1,
			ProceedOnPreamble:    []byte{'S'},
		},
		Reason: "preamble gate",
	}
	ack := awaitTCPAck(t, h)
	if !ack.OK || ack.TLSUpgraded {
		t.Fatalf("want OK=true TLSUpgraded=false on a preamble mismatch, got %+v", ack)
	}

	want := append(append([]byte(nil), head...), tail...)
	if got := h.awaitUpstream(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("preamble mismatch delivered %d of %d held bytes. No TLS came up, so the "+
			"remainder is plain client data — dropping it truncates the client's message "+
			"mid-stream while acking success.", len(got), len(want))
	}
}

// TestClientWriteHold_CapAccumulatesAcrossReads exercises the cap the
// way it actually trips in production — many small client writes rather
// than one oversized one.
func TestClientWriteHold_CapAccumulatesAcrossReads(t *testing.T) {
	var incMu sync.Mutex
	var reasons []string
	h := newTCPHarness(t, Config{
		HoldClientWrites: true,
		ClientHoldCap:    512,
		OnMarkMockIncomplete: func(reason string) {
			incMu.Lock()
			reasons = append(reasons, reason)
			incMu.Unlock()
		},
	})
	h.drainUpstream(t)
	h.greetAndAwait(t)

	chunk := bytes.Repeat([]byte{0x33}, 64)
	total := 0
	for i := 0; i < 12; i++ {
		if _, err := h.clientApp.Write(chunk); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
		total += len(chunk)
		time.Sleep(5 * time.Millisecond)
	}

	if got := h.awaitUpstream(t, total); len(got) != total {
		t.Fatalf("cap breach across many reads delivered %d of %d bytes", len(got), total)
	}

	incMu.Lock()
	got := append([]string(nil), reasons...)
	incMu.Unlock()
	var found bool
	for _, reason := range got {
		if reason == "client_hold_cap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cap breach reported %v, want client_hold_cap", got)
	}
}

// TestClientWriteHold_MutuallyExclusiveWithPreDispatch: arming both
// silently disables the hold's byte accounting and lets
// resume-pre-dispatch ack OK on a still-held connection. Run refuses
// the combination and keeps the hold.
func TestClientWriteHold_MutuallyExclusiveWithPreDispatch(t *testing.T) {
	h := newTCPHarness(t, Config{HoldClientWrites: true, PreDispatchPause: true})
	h.drainUpstream(t)
	h.greetAndAwait(t)

	held := bytes.Repeat([]byte{0x44}, 24)
	if _, err := h.clientApp.Write(held); err != nil {
		t.Fatalf("write held payload: %v", err)
	}
	h.awaitParserSees(t, len(held))

	// The hold is in force, not pre-dispatch: nothing reached the server
	// and the bytes are counted against the cap rather than parked in
	// the pause stash.
	if got := h.upstreamBytes(); len(got) != 0 {
		t.Fatalf("upstream saw %d bytes; the hold should be the active brake", len(got))
	}

	h.r.Directives() <- directive.ReleaseClient("release under both flags")
	if ack := awaitTCPAck(t, h); !ack.OK {
		t.Fatalf("release-client failed: %+v", ack)
	}
	if got := h.awaitUpstream(t, len(held)); !bytes.Equal(got, held) {
		t.Fatalf("release delivered %d of %d held bytes", len(got), len(held))
	}
}
