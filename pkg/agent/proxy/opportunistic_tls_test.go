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

// waitForOther used to return only res.err, throwing away isTLS and the
// buffered ClientHello with it. That mattered because only the src side
// detects TLS: when dst reports first (budget or error) and src lands an
// isTLS verdict a moment later, the caller saw err == nil, fell through to
// continuePlainRelay, and the handshake bytes — which sniffAndRelayLoop had
// deliberately withheld from the upstream so a hijack could replay them —
// were gone. The relay stayed up but the two ends were permanently out of
// sync: the app waited for a ServerHello that could never arrive.
//
// The race window is now microseconds wide (the parent's deadline poke
// retires the peer almost immediately), which makes it impractical to drive
// from an integration test. This pins the contract the parent depends on
// instead; the dispatch that consumes it is the `other.isTLS` branch in
// opportunisticTLSIntercept.
func TestWaitForOther_PreservesTLSVerdictAndBufferedBytes(t *testing.T) {
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01}

	ch := make(chan sniffResult, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch <- sniffResult{side: "src", isTLS: true, buffered: hello}
	}()

	got := waitForOther(context.Background(), ch, &wg)

	if !got.isTLS {
		t.Errorf("isTLS = false, want true — the TLS verdict was dropped and the caller " +
			"will plain-relay a connection whose ClientHello has been swallowed")
	}
	if string(got.buffered) != string(hello) {
		t.Errorf("buffered = %x, want %x — the withheld ClientHello must survive so "+
			"hijackAndMITM can replay it", got.buffered, hello)
	}
	if got.err != nil {
		t.Errorf("err = %v, want nil", got.err)
	}
}

// A peer blocked in to.Write() is the #4398 leak from the other end. Nothing
// used to set a write deadline, and cancelRelay is invisible to a blocked
// Write — the parent's poke sets a READ deadline — so wg.Wait() never
// returned and the connection's two goroutines and two sockets were pinned
// for the life of the process.
//
// Here the client stops reading while the upstream is still sending, so the
// dst-side loop is parked in Write when the src side exhausts its budget and
// the parent cancels. The call must still come back.
func TestOpportunisticTLSIntercept_NonTLS_PeerBlockedInWriteStillReturns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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

	srcConn, client := net.Pipe()
	defer client.Close()
	defer srcConn.Close()

	p := &Proxy{logger: testLogger()}
	resCh := make(chan error, 1)
	go func() {
		resCh <- p.opportunisticTLSIntercept(context.Background(), srcConn,
			ln.Addr().String(), time.Time{}, models.OutgoingOptions{})
	}()

	upstream := <-upCh
	defer upstream.Close()
	defer func() {
		select {
		case c := <-upCh:
			_ = c.Close()
		default:
		}
	}()

	// Upstream sends first, and we then WAIT for proof the dst-side loop
	// has taken those bytes and parked in Write before doing anything
	// else. Without that wait the premise is merely likely: under load
	// the dst loop can still be in Read when the parent cancels, in which
	// case it returns via the cancelled-read path, the connection plain-
	// relays forever, and the test fails with a message blaming the write
	// deadline — indistinguishable in CI from a real regression.
	//
	// net.Pipe is unbuffered, so reading a single byte here proves the
	// loop's 4096-byte Write is underway with the remainder outstanding.
	// This client reads nothing after that, so the loop stays parked.
	go func() { _, _ = upstream.Write(make([]byte, 4096)) }()
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
		t.Fatalf("dst-side loop never reached its Write: %v", err)
	}
	_ = client.SetReadDeadline(time.Time{})

	// Spend the src budget so the parent takes the non-TLS branch.
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
		t.Fatalf("upstream did not receive the peek-window bytes: %v", err)
	}

	// Deliberately nothing here unsticks the dst loop — no close, no
	// drain. Any such nudge would release the pre-fix code too and the
	// test would pass either way. Only its own write deadline can.
	//
	// Once it fires, the partially-unwritten chunk is reported rather
	// than dropped, so the parent closes both sockets and this call
	// returns. Before the fix a blocked Write had no deadline to expire
	// and could not see cancelRelay, so waitForOther never completed and
	// the call hung with two goroutines and two sockets pinned.
	const bound = opportunisticPeekChunkTimeout + 15*time.Second
	select {
	case <-resCh:
	case <-time.After(bound):
		t.Fatalf("opportunisticTLSIntercept did not return within %s while the peer was "+
			"parked in Write — the write deadline is not bounding it and the connection "+
			"leaks", bound)
	}
}

// pushSignal used to select over ctx.Done() as well as the send. Once the
// parent cancelled relayCtx both cases were runnable and Go chose uniformly
// at random, so roughly half of all post-cancellation verdicts were silently
// dropped — including an isTLS verdict carrying a ClientHello the loop had
// already withheld from the upstream. The guard protected against nothing:
// sniffCh has capacity 2, exactly two goroutines run, and each returns right
// after publishing, so the send can never block.
//
// The invariant asserted here is exact rather than statistical. net.Pipe is
// unbuffered, so a client write only completes if sniffAndRelayLoop actually
// consumed those bytes. Having consumed a ClientHello, the loop owes the
// parent a verdict — those bytes exist nowhere else. Iterations where
// cancellation wins the race first are skipped, not counted as failures:
// there the loop never read anything and owes nothing.
func TestSniffAndRelayLoop_PublishesVerdictEvenWhenCancelled(t *testing.T) {
	const iterations = 250
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01}

	consumed, dropped := 0, 0
	for i := 0; i < iterations; i++ {
		srcConn, client := net.Pipe()
		toConn, toPeer := net.Pipe()

		ch := make(chan sniffResult, 2)
		ctx, cancel := context.WithCancel(context.Background())

		p := &Proxy{logger: testLogger()}
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.sniffAndRelayLoop(ctx, "src", srcConn, toConn, ch)
		}()

		// Sweep the cancellation across the read/publish window instead
		// of firing it at a fixed instant: at 0 it almost always beats
		// the first Read (nothing consumed, iteration skipped), and by a
		// few tens of microseconds it lands around the publish, which is
		// the window that used to lose verdicts.
		go func(d time.Duration) {
			time.Sleep(d)
			cancel()
		}(time.Duration(i%25) * time.Microsecond)
		_ = client.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		_, werr := client.Write(hello)

		<-done

		if werr == nil {
			// The loop took the bytes, so it must have published.
			consumed++
			select {
			case res := <-ch:
				if !res.isTLS || string(res.buffered) != string(hello) {
					t.Errorf("iteration %d: got %+v, want the isTLS verdict carrying %x", i, res, hello)
				}
			default:
				dropped++
			}
		}

		cancel()
		_ = client.Close()
		_ = srcConn.Close()
		_ = toConn.Close()
		_ = toPeer.Close()
	}

	if consumed == 0 {
		t.Fatalf("no iteration got the handshake into the loop; the test proved nothing")
	}
	if dropped > 0 {
		t.Errorf("%d of %d consumed ClientHellos were never published — the loop swallowed "+
			"handshake bytes it had already withheld from the upstream, so the parent will "+
			"plain-relay a connection whose handshake no longer exists anywhere",
			dropped, consumed)
	}
}

// replayWithheldHandshake carries the bytes a sniffAndRelayLoop withheld
// from the upstream when the parent gives up on interception after the peer
// has already caught a ClientHello. Those bytes were consumed out of the
// client socket and live only in memory, so failing to forward them corrupts
// the stream rather than merely losing an optimisation.
func TestReplayWithheldHandshake_ForwardsBytesAndBoundsTheWrite(t *testing.T) {
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01}

	t.Run("forwards the withheld bytes", func(t *testing.T) {
		dstConn, upstream := net.Pipe()
		defer dstConn.Close()
		defer upstream.Close()

		got := make(chan []byte, 1)
		go func() {
			buf := make([]byte, len(hello))
			if _, err := io.ReadFull(upstream, buf); err != nil {
				got <- nil
				return
			}
			got <- buf
		}()

		if err := replayWithheldHandshake(dstConn, hello); err != nil {
			t.Fatalf("replayWithheldHandshake: %v", err)
		}
		if b := <-got; string(b) != string(hello) {
			t.Errorf("upstream got %x, want %x", b, hello)
		}
	})

	t.Run("bounded when the upstream never reads", func(t *testing.T) {
		// This path runs only after the upstream has pushed a full
		// opportunisticPeekMaxBytes — exactly the profile of a peer that
		// may have stopped reading. An unbounded Write here would put the
		// #4398 hang back, in the parent goroutine this time.
		dstConn, upstream := net.Pipe()
		defer dstConn.Close()
		defer upstream.Close() // never read from

		done := make(chan error, 1)
		go func() { done <- replayWithheldHandshake(dstConn, hello) }()

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("want an error when the upstream never reads, got nil")
			}
		case <-time.After(opportunisticPeekChunkTimeout + 5*time.Second):
			t.Fatal("replayWithheldHandshake blocked indefinitely on an upstream that " +
				"never reads — the write is unbounded and re-creates #4398")
		}

		// The deadline must not outlive the call, or continuePlainRelay
		// inherits it and dies on its first write.
		var zero time.Time
		if err := dstConn.SetWriteDeadline(zero); err != nil {
			t.Fatalf("clearing deadline: %v", err)
		}
	})
}

// A peer that publishes and then exits leaves waitForOther with BOTH its
// result buffered and wg at zero, so the <-ch and <-doneCh cases are ready
// together and Go picks between them at random. Taking <-doneCh without
// looking discarded whatever was already in the buffer — including an isTLS
// verdict carrying a withheld ClientHello, which exists nowhere else once
// dropped. Same defect pushSignal had, one level up.
//
// This is a probabilistic detector, not a proof. Hitting the race needs
// waitForOther's internal wg.Wait goroutine to close doneCh before the select
// runs, which is uncommon — the un-drained form loses roughly 1 in 20,000
// here, hence the iteration count. A pass is therefore weak evidence; the
// argument for the drain is the reachability above, not this test. It is kept
// because it costs ~0.3s and does fail on the un-drained form.
func TestWaitForOther_DrainsResultPublishedBeforeGoroutineExit(t *testing.T) {
	const iterations = 500000
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01}

	dropped := 0
	for i := 0; i < iterations; i++ {
		ch := make(chan sniffResult, 2)
		var wg sync.WaitGroup

		// Publish and exit before waitForOther is even called, so both
		// of its cases are ready the moment it selects.
		wg.Add(1)
		func() {
			defer wg.Done()
			ch <- sniffResult{side: "src", isTLS: true, buffered: hello}
		}()

		got := waitForOther(context.Background(), ch, &wg)
		if !got.isTLS || string(got.buffered) != string(hello) {
			dropped++
		}
	}

	if dropped > 0 {
		t.Errorf("%d of %d already-published verdicts were discarded — waitForOther took its "+
			"doneCh case without draining the buffer, losing a ClientHello that exists "+
			"nowhere else", dropped, iterations)
	}
}
