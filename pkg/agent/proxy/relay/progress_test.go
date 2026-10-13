package relay

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.uber.org/zap"
)

// idleRelay is a Relay that is not running: no forwarder or drain touches its
// tees' progress, which a test then sets as one would.
func idleRelay(t *testing.T) *Relay {
	t.Helper()
	a, b := net.Pipe()
	c, d := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close(); _ = c.Close(); _ = d.Close() })
	return New(Config{Logger: zap.NewNop()}, a, connseq.NewUpstream(c))
}

// CapturedTo, how far the capture has got, is now unless a forwarder holds a
// chunk it has not stamped yet: that chunk is stamped no earlier than the one
// before it on its direction, which holds the horizon back to that stamp. A
// stamped chunk in hand has been told of (Config.OnCapture), and holds
// nothing back.
func TestCapturedTo_AChunkBeingStampedHoldsItBack(t *testing.T) {
	r := idleRelay(t)
	before := time.Now()
	if got := r.CapturedTo(); got.Before(before.Round(0)) {
		t.Fatalf("with no chunk in hand, CapturedTo is %v, want the clock (no earlier than %v)", got, before)
	}

	old := time.Now().Add(-10 * time.Minute) // an idle direction's last stamp
	r.teeD2C.progress.last.Store(old.UnixNano())
	r.teeD2C.progress.held.Store(heldUnstamped)
	if got := r.CapturedTo(); !got.Equal(time.Unix(0, old.UnixNano())) {
		t.Fatalf("with a chunk being stamped, CapturedTo is %v, want the stamp before it (%v): the chunk may be stamped that early", got, old)
	}

	r.teeD2C.progress.held.Store(old.UnixNano())
	if got := r.CapturedTo(); got.Before(before.Round(0)) {
		t.Fatalf("with a stamped chunk in hand, CapturedTo is %v, want the clock (no earlier than %v)", got, before)
	}
}

// A chunk is told of (Config.OnCapture) as the relay stamps it, before it is
// written on: a write blocked on a slow peer does not hold the note back. The
// unrecorded-span accounting reads the capture's horizon from these notes,
// and a chunk in hand, stamped and not told of, was a chunk the horizon had
// passed with nothing noting it.
func TestOnCapture_AChunkIsToldOfAsItIsStampedBeforeItIsWrittenOn(t *testing.T) {
	clientApp, src := net.Pipe()
	dst, destSvc := net.Pipe() // destSvc reads nothing until the test says: the write blocks
	noted := make(chan time.Time, 4)
	r := New(Config{Logger: zap.NewNop(), OnCapture: func(at time.Time) { noted <- at }}, src, connseq.NewUpstream(dst))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		// The pipes first: a write blocked on one ends only as it closes.
		cancel()
		_ = clientApp.Close()
		_ = src.Close()
		_ = dst.Close()
		_ = destSvc.Close()
		<-done
	})

	before := time.Now()
	go func() { _, _ = clientApp.Write([]byte("GET / HTTP/1.1\r\n\r\n")) }()
	var at time.Time
	select {
	case at = <-noted:
	case <-time.After(30 * time.Second):
		t.Fatal("the chunk was not told of while its write to the destination was blocked")
	}
	if at.Before(before.Round(0)) {
		t.Fatalf("the chunk was told of with the stamp %v, before it was sent (%v)", at, before)
	}
	buf := make([]byte, 64)
	if n, err := destSvc.Read(buf); err != nil || string(buf[:n]) != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("the destination read %q, %v", buf[:n], err)
	}
}
