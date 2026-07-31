package proxy

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestOpportunisticTLSIntercept_NonTLS_ReturnsBoundedWhenPeerIdle reproduces
// the hang from issue #4398: when the src side reports a non-TLS verdict
// (here, by erroring out because the client closed its connection) while the
// dst side is still idle inside sniffAndRelayLoop, opportunisticTLSIntercept
// must still return in bounded time. Before the fix, the non-TLS branch
// waited for the idle peer goroutine to exit *before* cancelling the context
// that would tell it to stop, so this call hung indefinitely instead of
// timing out after opportunisticPeekChunkTimeout.
func TestOpportunisticTLSIntercept_NonTLS_ReturnsBoundedWhenPeerIdle(t *testing.T) {
	// Silent upstream: accepts the dial but never writes or closes, so the
	// dst-side sniffAndRelayLoop stays blocked in its read-timeout loop
	// unless it's told to stop.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		<-t.Context().Done()
		_ = conn.Close()
	}()

	srcConn, client := net.Pipe()
	defer client.Close()

	p := &Proxy{logger: testLogger()}

	resCh := make(chan error, 1)
	go func() {
		resCh <- p.opportunisticTLSIntercept(context.Background(), srcConn, ln.Addr().String(), time.Time{})
	}()

	// Client sends a plaintext (non-TLS) chunk, then closes — this makes
	// the src-side sniffAndRelayLoop error out (closed pipe) and report a
	// non-TLS result, while the dst side is still idle.
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("client close: %v", err)
	}

	// Bounded by one opportunisticPeekChunkTimeout (the idle dst side's
	// own read-timeout window) plus margin — not "forever", which is what
	// the pre-fix code did.
	select {
	case <-resCh:
	case <-time.After(opportunisticPeekChunkTimeout + 3*time.Second):
		t.Fatalf("opportunisticTLSIntercept did not return within %s of the client closing "+
			"while the peer side was idle — the non-TLS branch is not cancelling the idle "+
			"peer before waiting on it", opportunisticPeekChunkTimeout+3*time.Second)
	}
}

// TestOpportunisticTLSIntercept_NonTLS_BothBudgetExhausted_FallsThroughToPlainRelay
// guards against a regression the naive fix for #4398 would introduce: if the
// non-TLS branch forced an already-expired read deadline on both connections
// (instead of just cancelling relayCtx), it would abort the *other* side's
// in-flight Read even while it was still actively relaying real bytes toward
// its own budget completion — turning a legitimate pass-through into a
// spurious timeout error instead of reaching continuePlainRelay. This test
// sends a small amount of ordinary (non-TLS) traffic on the src side and
// confirms the connection is still relaying normally afterward, i.e. neither
// side was cut off mid-flight.
func TestOpportunisticTLSIntercept_NonTLS_BothBudgetExhausted_FallsThroughToPlainRelay(t *testing.T) {
	upstreamAccepted := make(chan net.Conn, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		upstreamAccepted <- conn
	}()

	srcConn, client := net.Pipe()
	defer client.Close()
	defer srcConn.Close()

	p := &Proxy{logger: testLogger()}

	resCh := make(chan error, 1)
	go func() {
		resCh <- p.opportunisticTLSIntercept(context.Background(), srcConn, ln.Addr().String(), time.Time{})
	}()

	upstream := <-upstreamAccepted
	defer upstream.Close()

	// A plaintext chunk on each side — neither hits the TLS pattern nor
	// the byte budget, so both sniffAndRelayLoop goroutines are still
	// alive and well past their first read when the other one finishes.
	// This proves an in-flight, actively-relaying peer isn't aborted by
	// the fix: if it were, the write below would never arrive.
	if _, err := client.Write([]byte("hello-from-client")); err != nil {
		t.Fatalf("client write: %v", err)
	}

	buf := make([]byte, 64)
	_ = upstream.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := upstream.Read(buf)
	if err != nil {
		t.Fatalf("upstream did not receive the relayed chunk: %v", err)
	}
	if got := string(buf[:n]); got != "hello-from-client" {
		t.Fatalf("upstream got %q, want %q", got, "hello-from-client")
	}

	// Clean shutdown: closing the client should let the call return
	// (via the error-close path, since res.err will be non-nil here) —
	// just confirming nothing is left hanging.
	_ = client.Close()
	select {
	case <-resCh:
	case <-time.After(opportunisticPeekChunkTimeout + 3*time.Second):
		t.Fatal("opportunisticTLSIntercept did not return after the connection closed")
	}
}
