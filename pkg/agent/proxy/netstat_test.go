//go:build linux

package proxy

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestForwardDelta(t *testing.T) {
	cases := []struct{ cur, prev, want uint64 }{
		{cur: 100, prev: 40, want: 60}, // normal growth
		{cur: 40, prev: 40, want: 0},   // no change
		{cur: 30, prev: 100, want: 30}, // counter went backwards (reset) → full current
		{cur: 0, prev: 0, want: 0},
	}
	for _, c := range cases {
		if got := forwardDelta(c.cur, c.prev); got != c.want {
			t.Errorf("forwardDelta(%d, %d) = %d, want %d", c.cur, c.prev, got, c.want)
		}
	}
}

// TestNetworkIO_PeriodicSamplingNoDoubleCount exercises the core of the fix: a
// tracked connection's bytes are folded in via periodic sampling AND at teardown,
// as DELTAS, so the same bytes are never counted twice. It uses a real loopback
// TCP pair because the accounting reads the kernel's TCP_INFO counters.
func TestNetworkIO_PeriodicSamplingNoDoubleCount(t *testing.T) {
	var rxTot, txTot atomic.Uint64
	SetNetworkIOSink(func(rx, tx uint64) { rxTot.Add(rx); txTot.Add(tx) })
	defer SetNetworkIOSink(nil)
	// isolate the global registry for this test
	liveConnsMu.Lock()
	liveConns = map[net.Conn]*trackedConn{}
	liveConnsMu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	type acc struct {
		c   net.Conn
		err error
	}
	accepted := make(chan acc, 1)
	go func() { c, err := ln.Accept(); accepted <- acc{c, err} }()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	a := <-accepted
	if a.err != nil {
		t.Fatal(a.err)
	}
	server := a.c // treat as the app-facing socket keploy owns
	defer func() { _ = server.Close() }()

	const n = 4096
	// client -> server: server RECEIVES these bytes, so TCP_INFO Bytes_received
	// grows → our mapping records them as tx (app egress relative to the peer).
	drained := make(chan struct{})
	go func() {
		_, _ = io.ReadFull(server, make([]byte, n))
		close(drained)
	}()
	if _, err := client.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	<-drained

	TrackConnNetworkIO(server)

	// TCP_INFO updates asynchronously; poll a few ticks for the first non-zero.
	var afterFirst uint64
	for i := 0; i < 50; i++ {
		sampleLiveConns()
		if afterFirst = txTot.Load(); afterFirst > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if afterFirst == 0 {
		t.Fatal("expected non-zero tx after sampling a connection that received data")
	}

	// No new traffic: a second sample must add nothing (the delta property).
	sampleLiveConns()
	if got := txTot.Load(); got != afterFirst {
		t.Fatalf("second sample double-counted: %d -> %d", afterFirst, got)
	}

	// Teardown takes only the final delta and deregisters.
	RecordConnNetworkIO(server)
	liveConnsMu.Lock()
	_, still := liveConns[server]
	liveConnsMu.Unlock()
	if still {
		t.Fatal("connection was not deregistered after RecordConnNetworkIO")
	}

	// The total must reflect the ~n payload bytes once — not doubled. TCP_INFO byte
	// counters are payload octets (no headers), so a generous upper bound catches a
	// gross double/triple count without being flaky.
	if got := txTot.Load(); got < n || got > n*3 {
		t.Fatalf("tx total out of expected band: got %d, want ~%d", got, n)
	}
}

// TestRecordConnNetworkIO_UntrackedFullTotal asserts an untracked connection still
// reports its full lifetime total at teardown (the original, unchanged behavior).
func TestRecordConnNetworkIO_UntrackedFullTotal(t *testing.T) {
	var txTot atomic.Uint64
	SetNetworkIOSink(func(_, tx uint64) { txTot.Add(tx) })
	defer SetNetworkIOSink(nil)
	liveConnsMu.Lock()
	liveConns = map[net.Conn]*trackedConn{}
	liveConnsMu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server := <-accepted
	defer func() { _ = server.Close() }()

	const n = 2048
	drained := make(chan struct{})
	go func() { _, _ = io.ReadFull(server, make([]byte, n)); close(drained) }()
	if _, err := client.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	<-drained

	// No TrackConnNetworkIO: teardown should report the connection's full total.
	var got uint64
	for i := 0; i < 50; i++ {
		txTot.Store(0)
		RecordConnNetworkIO(server) // untracked → absolute; safe to retry (idempotent read)
		if got = txTot.Load(); got > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got < n || got > n*3 {
		t.Fatalf("untracked total out of expected band: got %d, want ~%d", got, n)
	}
}
