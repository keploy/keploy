package proxy

import (
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// halfCloseRecorder reports whether CloseWrite reached the wrapped conn.
type halfCloseRecorder struct {
	net.Conn
	closedWrite chan struct{}
}

func (h *halfCloseRecorder) CloseWrite() error {
	select {
	case <-h.closedWrite:
	default:
		close(h.closedWrite)
	}
	return nil
}

// A replayConn must forward CloseWrite to the conn it wraps. It embeds net.Conn
// as an interface, so without an explicit method Go promotes nothing and
// proxyutil.CloseWriteIfPossible silently does nothing — the peer's io.Copy
// then never sees a FIN. The existing forward_raw_half_close_test drives a bare
// *net.TCPConn and cannot catch this.
func TestReplayConn_ForwardsCloseWrite(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	rec := &halfCloseRecorder{Conn: c1, closedWrite: make(chan struct{})}
	rc := newReplayConn([]byte("abc"), rec)

	cw, ok := rc.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("replayConn does not implement CloseWrite, so the relay's half-close is dead")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case <-rec.closedWrite:
	case <-time.After(time.Second):
		t.Fatal("CloseWrite did not reach the wrapped connection")
	}
}

// The prefix must still replay after the method set grows.
func TestReplayConn_StillReplaysPrefix(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	go func() {
		_, _ = c2.Write([]byte("def"))
		_ = c2.Close()
	}()

	got, err := io.ReadAll(newReplayConn([]byte("abc"), c1))
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "abcdef" {
		t.Errorf("prefix replay broken: got %q want %q", got, "abcdef")
	}
}

// forwardRawTCP used to hang a watcher goroutine on the caller's context, which
// is the forwarder's lifetime context. One connection then leaked one goroutine
// (and two conn references) until StopIngress. Routing every inbound TLS
// connection through this pump would make that a per-request leak.
func TestForwardRawTCP_DoesNotLeakWatcherGoroutine(t *testing.T) {
	// A context that stays alive, exactly like the real forwarder's.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settle := func() {
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			time.Sleep(2 * time.Millisecond)
		}
	}

	settle()
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		cliA, cliB := net.Pipe()
		upA, upB := net.Pipe()
		// Close both far ends up front so each io.Copy inside the pump reads
		// EOF immediately and forwardRawTCP returns. Leaving either open
		// deadlocks the pump, which waits on BOTH directions.
		_ = cliB.Close()
		_ = upB.Close()
		forwardRawTCP(ctx, cliA, upA)
		_ = cliA.Close()
		_ = upA.Close()
	}

	settle()
	after := runtime.NumGoroutine()

	// Each leaked watcher is one goroutine that outlives its connection and
	// never returns while ctx is live. Allow a small margin for runtime noise.
	if after-before > 5 {
		t.Errorf("goroutines grew by %d across 20 connections (before=%d after=%d); the shutdown watcher is leaking",
			after-before, before, after)
	}
}
