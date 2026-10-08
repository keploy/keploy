//go:build linux

package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// The tests here run on real sockets and not in lockstep: the bytes each
// direction carries wait in the kernel until the relay's forwarders read them,
// which they do in whatever order the scheduler runs them. Numbered as each
// Read returns, a 408 that reached the proxy before a request was numbered
// after it in tens of rounds out of a few hundred; numbered by what the
// destination had sent when the request was read (connseq.Upstream), never.

// loopbackPair returns the two ends of a loopback TCP connection.
func loopbackPair(t *testing.T) (dialed, accepted net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		got <- c
	}()
	dialed, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if accepted = <-got; accepted == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.Close() })
	return dialed, accepted
}

// received is the socket c's count of the bytes it has received.
func received(t *testing.T, c net.Conn) uint64 {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got uint64
	if err := rc.Control(func(fd uintptr) {
		info, gerr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if gerr != nil {
			t.Error(gerr)
			return
		}
		got = info.Bytes_received
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// awaitReceived waits until the socket c has received n bytes in all.
func awaitReceived(t *testing.T, c net.Conn, n uint64) {
	t.Helper()
	for limit := time.Now().Add(5 * time.Second); received(t, c) < n; {
		if time.Now().After(limit) {
			t.Fatalf("the socket received %d bytes in 5s, want %d", received(t, c), n)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

const (
	idle408 = "HTTP/1.1 408 Request Timeout\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
	nextReq = "GET /orders/8 HTTP/1.1\r\nHost: upstream\r\n\r\n"
)

// A connection the proxy dialled while it held the application's handshake can
// hold the destination's bytes before the relay starts: a 408 for a
// connection that idled past the server's request timeout, sent before the
// application's first request reached the proxy. The relay starts with both
// waiting, and its forwarders read them in either order; the 408 is still
// numbered before the request.
func TestRelay_NumbersWhatTheDestinationSentBeforeTheRelayStartedBeforeTheFirstRequest(t *testing.T) {
	t.Parallel()
	const rounds = 200
	wrong := 0
	for i := 0; i < rounds; i++ {
		if !firstRequestAfterAWaiting408(t) {
			wrong++
		}
	}
	if wrong != 0 {
		t.Fatalf("the 408 the destination sent before the first request was numbered after it in %d of %d rounds", wrong, rounds)
	}
}

// firstRequestAfterAWaiting408 reports whether, with a 408 and the first
// request both waiting when the relay starts, the 408 is numbered first.
func firstRequestAfterAWaiting408(t *testing.T) bool {
	t.Helper()
	clientApp, srcProxy := loopbackPair(t)
	dstProxy, destSvc := loopbackPair(t)
	if _, err := destSvc.Write([]byte(idle408)); err != nil {
		t.Fatal(err)
	}
	if _, err := clientApp.Write([]byte(nextReq)); err != nil {
		t.Fatal(err)
	}
	awaitReceived(t, dstProxy, uint64(len(idle408)))
	awaitReceived(t, srcProxy, uint64(len(nextReq)))

	r := New(Config{Logger: zap.NewNop()}, srcProxy, connseq.NewUpstream(dstProxy))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	req, rerr := r.ClientStream().ReadChunk()
	resp, derr := r.DestStream().ReadChunk()
	cancel()
	_ = clientApp.Close()
	_ = destSvc.Close()
	<-done
	if rerr != nil || derr != nil {
		t.Fatalf("reading the first chunks: %v, %v", rerr, derr)
	}
	return resp.CapturedBefore(req.ConnSeq)
}

// The keep-alive idle-close race, mid-connection: with both forwarders parked
// in Read, the destination's 408 reaches the proxy just before the client's
// next request does. Whichever forwarder the scheduler runs first, the 408 is
// numbered before the request: at the default GOMAXPROCS, at 2, where the two
// forwarders contend the most, and with TLS to the destination, where what
// the socket received is counted in records under the TLS the relay reads.
func TestRelay_NumbersWhatTheDestinationSentBeforeTheNextRequestBeforeIt(t *testing.T) {
	t.Run("default GOMAXPROCS", func(t *testing.T) { nextRequestAfterA408(t, 300, false) })
	t.Run("GOMAXPROCS=2", func(t *testing.T) {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
		nextRequestAfterA408(t, 300, false)
	})
	t.Run("TLS to the destination", func(t *testing.T) { nextRequestAfterA408(t, 300, true) })
}

func nextRequestAfterA408(t *testing.T, rounds int, overTLS bool) {
	clientApp, srcProxy := loopbackPair(t)
	dstProxy, destSvc := loopbackPair(t)
	var dst net.Conn = connseq.NewUpstream(dstProxy)
	// What a record adds to its plaintext on the socket: TLS 1.3's content
	// type byte, 5 byte header and 16 byte tag.
	var sent, overhead uint64
	if overTLS {
		server := tls.Server(destSvc, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)},
			MinVersion: tls.VersionTLS13, SessionTicketsDisabled: true})
		client := tls.Client(dst, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // test server
		handshook := make(chan error, 1)
		go func() { handshook <- server.Handshake() }()
		if err := client.Handshake(); err != nil {
			t.Fatal(err)
		}
		if err := <-handshook; err != nil {
			t.Fatal(err)
		}
		destSvc, dst, overhead = server, client, 22
		sent = received(t, dstProxy)
	}
	r := New(Config{Logger: zap.NewNop()}, srcProxy, dst)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	clientChunks, destChunks := collect(r.ClientStream()), collect(r.DestStream())

	for i := 0; i < rounds; i++ {
		early, req := fmt.Sprintf("S%03d", i), fmt.Sprintf("C%03d", i)
		if _, err := destSvc.Write([]byte(early)); err != nil {
			t.Fatal(err)
		}
		sent += uint64(len(early)) + overhead
		awaitReceived(t, dstProxy, sent) // the 408 is at the proxy, read or not
		if _, err := clientApp.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		if got := string(readExact(t, clientApp, len(early))); got != early {
			t.Fatalf("round %d: the client got %q, want %q", i, got, early)
		}
		if got := string(readExact(t, destSvc, len(req))); got != req {
			t.Fatalf("round %d: the destination got %q, want %q", i, got, req)
		}
	}
	waitFor(t, func() bool {
		return r.teeC2D.acceptedBytes() == int64(4*rounds) && r.teeD2C.acceptedBytes() == int64(4*rounds)
	})
	cancel()
	_ = clientApp.Close()
	_ = destSvc.Close()
	<-done
	cs, ds := <-clientChunks, <-destChunks
	if len(cs) != rounds || len(ds) != rounds {
		t.Fatalf("got %d client and %d server chunks, want %d each", len(cs), len(ds), rounds)
	}
	wrong := 0
	for i := 0; i < rounds; i++ {
		if !ds[i].CapturedBefore(cs[i].ConnSeq) {
			wrong++
		}
		if i > 0 && !cs[i-1].CapturedBefore(ds[i].ConnSeq) {
			t.Fatalf("round %d: the 408 is numbered %d, the previous request %d: it reached the proxy after it",
				i, ds[i].ConnSeq, cs[i-1].ConnSeq)
		}
	}
	if wrong != 0 {
		t.Fatalf("the 408 that reached the proxy before the next request was numbered after it in %d of %d rounds",
			wrong, rounds)
	}
}
