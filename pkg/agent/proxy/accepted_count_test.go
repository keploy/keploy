package proxy

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
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

// The accept loop puts the macOS and Windows count of what is written to the client
// (relay.CountClientWrites) over the accepted socket, under the tracking.
// Every site that keys or reads the accepted socket itself finds it under the
// count: the pairing's key (pairKey, for countAccept's store, pairedDest and
// every lookup of the paired connections: holds, servedWithoutSession,
// closing, remove), the walk to the TCP socket (tcpConnOf: countAccept,
// the real destinations' resets), and the tracking's own read of the socket
// (trackTold). A site that took the count for the socket would key the
// pairing by another conn, find no TCP socket, or read nothing, and fail
// here. Elsewhere the count is the socket itself, as before.
func TestTheAcceptedSocketIsFoundUnderTheCountOfWrites(t *testing.T) {
	t.Parallel()
	_, raw := acceptedSocket(t)
	// A proxy that keeps its connections, in either mode of the switch, and
	// that the kernel paired this one with an application connection.
	p := &Proxy{settleReads: true}
	p.countedFromStart.Store(true)
	p.paired.add(raw, 1, 0)
	conn, tracked := p.acceptedConn(context.Background(), raw)
	if tracked == nil || conn != net.Conn(tracked) {
		t.Fatalf("the accepted connection is handled through %T, not its tracking", conn)
	}
	if tracked.beforeClose == nil {
		t.Error("the paired connection's close is not read before it happens")
	}
	t.Cleanup(func() { p.inflight.forget(tracked) })
	counted := tracked.Conn
	countsOver(t, counted, raw, "the tracking")
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
	// The paired connection is found under each of them: the no-session
	// mark (handed the tracking), the close's read and the removal.
	for _, c := range []net.Conn{raw, counted, tracked} {
		if !p.paired.holds(c) {
			t.Errorf("the pairing does not hold the accepted socket under %T", c)
		}
	}
	p.paired.servedWithoutSession(tracked)
	p.paired.mu.Lock()
	pc := p.paired.byConn[raw]
	p.paired.mu.Unlock()
	if pc == nil || !pc.noSession.Load() {
		t.Error("serving the tracking with no session did not mark its paired connection")
	}
	if got, _, _ := p.paired.closing(counted, nil); got != pc || pc == nil {
		t.Error("the close of the count did not read its paired connection")
	}
	p.paired.remove(tracked)
	if p.paired.holds(raw) {
		t.Error("removing the tracking did not forget its paired connection")
	}
}

// countsOver checks that c, what the proxy hands on of the accepted socket raw
// (through what), is the count of writes over raw on macOS and Windows, and
// raw itself elsewhere.
func countsOver(t *testing.T, c net.Conn, raw *net.TCPConn, what string) {
	t.Helper()
	counted := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if counted == (c == net.Conn(raw)) {
		t.Fatalf("%s keeps %T on %s: want a count over the socket on macOS and Windows only", what, c, runtime.GOOS)
	}
	if u, ok := c.(interface{ NetConn() net.Conn }); counted && (!ok || u.NetConn() != net.Conn(raw)) {
		t.Fatalf("the count %s keeps is not over the accepted socket", what)
	}
}

// The count is put over every accepted socket, whether the proxy keeps the
// connection or not, and whether the kernel paired it or not: a proxy that
// neither keeps nor pairs it hands on the count itself, untracked, and one
// that pairs but does not keep it tracks it over the count. With the switch
// off a proxy that settles no reads keeps no connection (keepsConnections),
// which is how users record; with it on every proxy keeps them. This
// package's rows start with the switch off whatever KEPLOY_PROVEN the process
// inherited (switch_main_test.go), so each case sets it itself, and every
// case runs in either mode. A count put only where the connection is kept, or
// only where it is paired, or a tracking over the socket instead of the
// count, would leave a client recorded over TLS uncounted.
func TestEveryAcceptedSocketIsCountedWhetherKeptOrPaired(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, paired := range []bool{false, true} {
			t.Run(fmt.Sprintf("switch on %v, paired %v", on, paired), func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				}
				acceptedCountedWhetherKeptOrPaired(t, on, paired)
			})
		}
	}
}

func acceptedCountedWhetherKeptOrPaired(t *testing.T, on, paired bool) {
	_, raw := acceptedSocket(t)
	p := &Proxy{}
	if keeps := p.keepsConnections(); keeps != on || provenswitch.On() != on {
		t.Fatalf("a proxy that settles no reads keeps its connections: %v, want %v (the switch on: %v)", keeps, on, provenswitch.On())
	}
	if paired {
		p.countedFromStart.Store(true)
		p.paired.add(raw, 1, 0)
	}
	kept := p.keepsConnections()
	conn, tracked := p.acceptedConn(context.Background(), raw)
	if !kept && !paired {
		if tracked != nil {
			t.Fatal("a connection the proxy neither keeps nor pairs is tracked")
		}
		countsOver(t, conn, raw, "the accept")
		return
	}
	if tracked == nil || conn != net.Conn(tracked) {
		t.Fatalf("a connection the proxy keeps (%v) or pairs (%v) is handed on as %T, not its tracking", kept, paired, conn)
	}
	t.Cleanup(func() { p.inflight.forget(tracked) })
	if paired && tracked.beforeClose == nil {
		t.Error("the paired connection's close is not read before it happens")
	}
	countsOver(t, tracked.Conn, raw, "the tracking")
}
