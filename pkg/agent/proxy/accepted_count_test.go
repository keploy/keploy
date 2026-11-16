package proxy

import (
	"context"
	"net"
	"runtime"
	"testing"
)

// acceptedSocket is an accepted loopback TCP connection, as the proxy's
// listener gives one, and the client's end of it.
func acceptedSocket(t *testing.T) (app net.Conn, accepted *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); got <- c }()
	if app, err = net.Dial("tcp", ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = app.Close(); _ = c.Close() })
	return app, c.(*net.TCPConn)
}

// The accept loop puts the macOS count of what is written to the client
// (relay.CountClientWrites) over the accepted socket, under the tracking.
// Every site that keys or reads the accepted socket itself finds it under the
// count: the pairing's key (pairKey, for countAccept's store, pairedDest and
// servedWithoutSession), the walk to the TCP socket (tcpConnOf: countAccept,
// the real destinations' resets), and the tracking's own read of the socket
// (trackTold). A site that took the count for the socket would key the
// pairing by another conn, find no TCP socket, or read nothing, and fail
// here. Elsewhere the count is the socket itself, as before.
func TestTheAcceptedSocketIsFoundUnderTheCountOfWrites(t *testing.T) {
	t.Parallel()
	_, raw := acceptedSocket(t)
	// A proxy that keeps its connections, in either mode of the switch.
	p := &Proxy{settleReads: true}
	p.countedFromStart.Store(true)
	conn, tracked := p.acceptedConn(context.Background(), raw)
	if tracked == nil || conn != net.Conn(tracked) {
		t.Fatalf("the accepted connection is handled through %T, not its tracking", conn)
	}
	t.Cleanup(func() { p.inflight.forget(tracked) })
	counted := tracked.Conn
	if onMac := runtime.GOOS == "darwin"; onMac == (counted == net.Conn(raw)) {
		t.Fatalf("the tracking keeps %T on %s: want a count over the socket on macOS only", counted, runtime.GOOS)
	}
	if c, ok := counted.(interface{ NetConn() net.Conn }); runtime.GOOS == "darwin" && (!ok || c.NetConn() != net.Conn(raw)) {
		t.Fatal("the count under the tracking is not over the accepted socket")
	}
	for _, c := range []net.Conn{raw, counted, tracked} {
		if k := pairKey(c); k != net.Conn(raw) {
			t.Errorf("pairKey(%T) = %T, want the accepted socket", c, k)
		}
		if tc, ok := tcpConnOf(c); !ok || tc != raw {
			t.Errorf("tcpConnOf(%T) did not find the accepted socket (%v)", c, ok)
		}
	}
	if tracked.raw == nil {
		t.Error("the tracking cannot read the accepted socket under the count")
	}
}
