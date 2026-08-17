package proxy

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
		resCh <- p.opportunisticTLSIntercept(context.Background(), srcConn, ln.Addr().String(), time.Time{}, models.OutgoingOptions{})
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

	// Two properties in one bound. "Returns at all" is the #4398 fix
	// (cancel before waiting). "Returns in well under
	// opportunisticPeekChunkTimeout" is the error-path deadline poke: this
	// side already failed, so both connections are going to be closed
	// regardless and there is no reason to hold two goroutines and two
	// sockets for a further 5 s. Teardown is sub-millisecond in practice,
	// so 2 s is ~1000x headroom while still failing if either property is
	// lost.
	const bound = 2 * time.Second
	select {
	case <-resCh:
	case <-time.After(bound):
		t.Fatalf("opportunisticTLSIntercept did not return within %s of the client closing "+
			"while the peer side was idle. Either the non-TLS branch stopped cancelling the "+
			"idle peer before waiting on it (#4398), or it stopped poking an expired read "+
			"deadline on the error path and is now waiting out a full %s chunk timeout",
			bound, opportunisticPeekChunkTimeout)
	}
}

// TestOpportunisticTLSIntercept_NonTLS_BudgetExhaustedWithIdlePeer_KeepsRelaying
// pins the case the #4398 fix alone gets wrong.
//
// One side exhausts opportunisticPeekMaxBytes of plaintext without ever
// seeing a ClientHello, which is a clean "not TLS" verdict, while the peer
// sits idle. The documented intent is to fall through to continuePlainRelay
// and keep the connection alive.
//
// Reordering cancelRelay ahead of waitForOther is not enough on its own. The
// idle peer is still parked in Read; when its own deadline fires,
// isTimeoutErr(err) && ctx.Err() == nil is false because the context was just
// cancelled, so it used to publish sniffResult{err: i/o timeout}. waitForOther
// reads that as "the peer errored" and the branch closes both sockets instead
// of relaying. Whether it happened at all was a coin flip: sniffCh is
// buffered, so pushSignal's select had a ready send AND a ready ctx.Done()
// and picked at random — measured at 6 tear-downs in 8 runs.
//
// Correct code never tears the connection down here, so this test passes
// deterministically; broken code fails it most of the time. The iteration
// count is what turns "most of the time" into a dependable signal.
func TestOpportunisticTLSIntercept_NonTLS_BudgetExhaustedWithIdlePeer_KeepsRelaying(t *testing.T) {
	// Whether the broken form tears a given connection down is a coin
	// flip (see the doc comment above), so one connection proves nothing.
	// These run concurrently rather than in sequence: each spends a full
	// opportunisticPeekChunkTimeout waiting out the idle peer, so 16
	// sequential runs would cost ~2 minutes while 16 concurrent ones cost
	// one timeout window. At 16 independent trials a regression escapes
	// with probability ~2^-16.
	const conns = 16

	type outcome struct {
		idx  int
		err  error // non-nil: connection was torn down; nil: still relaying
		fail string
	}
	results := make(chan outcome, conns)

	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Silent upstream: never writes back, so the dst-side loop
			// stays idle in its read-timeout loop. This goroutine is the
			// only reader, so nothing races it for the sentinel.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				results <- outcome{idx: idx, fail: "listen: " + err.Error()}
				return
			}
			defer ln.Close()

			upCh := make(chan net.Conn, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				upCh <- conn
			}()
			// On any early-return path below, a connection may already be
			// parked in upCh with nobody left to close it.
			defer func() {
				select {
				case c := <-upCh:
					_ = c.Close()
				default:
				}
			}()

			srcConn, client := net.Pipe()
			defer client.Close()
			defer srcConn.Close()

			p := &Proxy{logger: testLogger()}
			resCh := make(chan error, 1)
			go func() {
				resCh <- p.opportunisticTLSIntercept(context.Background(), srcConn,
					ln.Addr().String(), time.Time{}, models.OutgoingOptions{})
			}()

			var upstream net.Conn
			select {
			case upstream = <-upCh:
			case <-time.After(10 * time.Second):
				results <- outcome{idx: idx, fail: "upstream never accepted"}
				return
			}
			defer upstream.Close()

			// Spend exactly the peek budget: the loop forwards every chunk
			// it reads and stops once relayed >= opportunisticPeekMaxBytes,
			// so upstream receives precisely this many bytes during the
			// peek window and the sentinel below is the next thing on the
			// wire.
			go func() {
				payload := make([]byte, 4096)
				for sent := 0; sent < opportunisticPeekMaxBytes; sent += len(payload) {
					if _, err := client.Write(payload); err != nil {
						return
					}
				}
			}()

			_ = upstream.SetReadDeadline(time.Now().Add(20 * time.Second))
			if _, err := io.CopyN(io.Discard, upstream, opportunisticPeekMaxBytes); err != nil {
				results <- outcome{idx: idx, fail: "upstream did not receive the peek-window bytes: " + err.Error()}
				return
			}

			// The expired-deadline poke wakes the idle peer immediately,
			// so a tear-down lands in milliseconds rather than after a
			// full opportunisticPeekChunkTimeout — no need to wait one
			// out here. Should the poke ever be lost (it is best-effort),
			// the sentinel read below still has its own generous deadline.
			select {
			case err := <-resCh:
				results <- outcome{idx: idx, err: err}
				return
			case <-time.After(2 * time.Second):
			}

			// Still running is necessary but not sufficient — prove the
			// relay is actually live by pushing a sentinel through it.
			go func() { _, _ = client.Write([]byte("sentinel")) }()

			buf := make([]byte, len("sentinel"))
			_ = upstream.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(upstream, buf); err != nil {
				results <- outcome{idx: idx, fail: "sentinel never reached upstream: " + err.Error() +
					" — continuePlainRelay is not running"}
				return
			}
			if got := string(buf); got != "sentinel" {
				results <- outcome{idx: idx, fail: "upstream got " + got + ", want sentinel"}
				return
			}
			results <- outcome{idx: idx}
		}(i)
	}
	wg.Wait()
	close(results)

	tornDown := 0
	for r := range results {
		if r.fail != "" {
			t.Errorf("connection %d: %s", r.idx, r.fail)
			continue
		}
		if r.err != nil {
			tornDown++
			if tornDown == 1 {
				t.Errorf("connection %d was torn down (err=%v), want it still relaying. "+
					"The idle peer's cancellation is being reported as a peer error, "+
					"flipping a clean budget-exhaustion verdict onto the "+
					"close-both-connections path", r.idx, r.err)
			}
		}
	}
	if tornDown > 0 {
		t.Errorf("%d of %d healthy plaintext connections were torn down after budget exhaustion", tornDown, conns)
	}
}
