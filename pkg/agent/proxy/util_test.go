package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// passThrough runs globalPassThrough between client and dest as a
// connection's handler does: with an errgroup and the connection ids in its
// context and, once it returns, both connections closed and its readers
// waited for. ended delivers what it returned, after all that.
func passThrough(t *testing.T, client, dest net.Conn) (cancel context.CancelFunc, ended <-chan error) {
	t.Helper()
	cancel, ended, _ = passThroughLimited(t, client, dest, -1)
	return cancel, ended
}

// passThroughLimited is passThrough with the errgroup the readers run in,
// whose number of goroutines is limited to limit (-1: no limit): with a limit
// of the two readers, a goroutine the test adds to g runs only once one of
// them has returned, so it tells, by a count, that a reader has ended.
func passThroughLimited(t *testing.T, client, dest net.Conn, limit int) (cancel context.CancelFunc, ended <-chan error, g *errgroup.Group) {
	t.Helper()
	return passThroughBy(t, p0(), client, dest, limit)
}

// passThroughBy is passThroughLimited by p, whose logger the test reads.
func passThroughBy(t *testing.T, p *Proxy, client, dest net.Conn, limit int) (cancel context.CancelFunc, ended <-chan error, g *errgroup.Group) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "1")
	gctx = context.WithValue(gctx, models.DestConnectionIDKey, "2")
	out := make(chan error, 1)
	go func() {
		err := p.globalPassThrough(gctx, client, dest)
		_ = client.Close()
		_ = dest.Close()
		cancel()
		_ = g.Wait()
		out <- err
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = dest.Close()
	})
	return cancel, out, g
}

// loopbackPair is a connected pair of TCP connections, closed when t ends.
func loopbackPair(t *testing.T) (dialed, accepted net.Conn) {
	t.Helper()
	dialed, accepted, cleanup := tcpConnPair(t)
	t.Cleanup(cleanup)
	return dialed, accepted
}

// counted is n bytes that tell their own place: a byte lost, repeated or
// moved shows.
func counted(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ byte(i>>8) ^ seed
	}
	return b
}

// halfCloseOf half-closes c's sending side as its owner would.
func halfCloseOf(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
}

// readAll reads c to its end in the background.
func readAll(c net.Conn) <-chan []byte {
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(c)
		got <- b
	}()
	return got
}

// A side that ends what it sends has its end forwarded to the other side, and
// the direction still open runs on to its own end: that is what passing a
// connection through means. Each row is red with the end not forwarded (the
// relay ended on the first side's EOF, and its caller closed both).
func TestGlobalPassThroughForwardsEachSidesEnd(t *testing.T) {
	t.Run("a client that half-closes gets the server's whole answer", func(t *testing.T) {
		app, client := loopbackPair(t)
		dest, server := loopbackPair(t)
		request, answer := counted(300<<10, 1), counted(500<<10, 2)
		heard := make(chan []byte, 1)
		go func() {
			// It answers only once the request has ended.
			got, _ := io.ReadAll(server)
			heard <- got
			_, _ = server.Write(answer)
			_ = server.Close()
		}()
		_, ended := passThrough(t, client, dest)
		got := readAll(app)
		if _, err := app.Write(request); err != nil {
			t.Fatal(err)
		}
		halfCloseOf(t, app)
		if h := awaitEvent(t, heard, "the server reading the request to its end"); !bytes.Equal(h, request) {
			t.Fatalf("the server got %d bytes, want the client's %d", len(h), len(request))
		}
		if a := awaitEvent(t, got, "the client reading the answer to its end"); !bytes.Equal(a, answer) {
			t.Fatalf("the client got %d bytes of the answer, want %d", len(a), len(answer))
		}
		if err := awaitEvent(t, ended, "the relay ending"); err != nil {
			t.Fatalf("the relay ended with %v, want nil: both sides ended cleanly", err)
		}
	})

	t.Run("a server that half-closes is heard as one, and still hears the client", func(t *testing.T) {
		app, client := loopbackPair(t)
		dest, server := loopbackPair(t)
		heard := make(chan []byte, 1)
		go func() {
			if _, err := server.Write([]byte("answer")); err != nil {
				return
			}
			_ = server.(*net.TCPConn).CloseWrite()
			got, _ := io.ReadAll(server)
			heard <- got
		}()
		_, ended := passThrough(t, client, dest)
		if a := awaitEvent(t, readAll(app), "the client reading the server's answer to its end"); string(a) != "answer" {
			t.Fatalf("the client got %q, want the server's %q", a, "answer")
		}
		if _, err := app.Write([]byte("after")); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
		if h := awaitEvent(t, heard, "the server reading the client to its end"); string(h) != "after" {
			t.Fatalf("the server got %q after its half-close, want the client's %q", h, "after")
		}
		if err := awaitEvent(t, ended, "the relay ending"); err != nil {
			t.Fatalf("the relay ended with %v, want nil", err)
		}
	})

	t.Run("a relay held half-open by a server that neither answers nor closes ends with its context", func(t *testing.T) {
		app, client := loopbackPair(t)
		dest, server := loopbackPair(t)
		heard := make(chan []byte, 1)
		go func() {
			got, _ := io.ReadAll(server)
			heard <- got // and it holds the connection open
		}()
		cancel, ended := passThrough(t, client, dest)
		if _, err := app.Write([]byte("request")); err != nil {
			t.Fatal(err)
		}
		halfCloseOf(t, app)
		if h := awaitEvent(t, heard, "the server reading the request to its end"); string(h) != "request" {
			t.Fatalf("the server got %q, want %q", h, "request")
		}
		cancel()
		if err := awaitEvent(t, ended, "the half-open relay ending with its context"); !errors.Is(err, context.Canceled) {
			t.Fatalf("the relay ended with %v, want its context's end", err)
		}
	})

	t.Run("over TLS: the client's close_notify reaches the server as one, and the answer comes back whole", func(t *testing.T) {
		cert := selfSignedForDispatch(t)
		appRaw, clientRaw := loopbackPair(t)
		destRaw, serverRaw := loopbackPair(t)
		app := tls.Client(appRaw, &tls.Config{InsecureSkipVerify: true})
		client := tls.Server(clientRaw, &tls.Config{Certificates: []tls.Certificate{cert}})
		dest := tls.Client(destRaw, &tls.Config{InsecureSkipVerify: true})
		server := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{cert}})
		handshake(t, app, client, dest, server)
		answer := counted(100<<10, 3)
		heard := make(chan []byte, 1)
		go func() {
			got, _ := io.ReadAll(server)
			heard <- got
			_, _ = server.Write(answer)
			_ = server.Close()
		}()
		_, ended := passThrough(t, client, dest)
		got := readAll(app)
		if _, err := app.Write([]byte("request")); err != nil {
			t.Fatal(err)
		}
		halfCloseOf(t, app)
		if h := awaitEvent(t, heard, "the TLS server reading the request to its end"); string(h) != "request" {
			t.Fatalf("the server got %q, want %q", h, "request")
		}
		if a := awaitEvent(t, got, "the TLS client reading the answer to its end"); !bytes.Equal(a, answer) {
			t.Fatalf("the client got %d bytes of the answer, want %d", len(a), len(answer))
		}
		if err := awaitEvent(t, ended, "the relay ending"); err != nil {
			t.Fatalf("the relay ended with %v, want nil", err)
		}
	})

	// However it is wrapped: handleConnection hands the relay a pUtil.Conn (or
	// a SafeConn, a trackedConn, ...), whose CloseWrite answers for the conn
	// it wraps. And where a CloseWrite fails, as a *tls.Conn's does when its
	// close_notify cannot be written. Red with a wrapper's CloseWrite taken
	// for a half-close when the conn beneath has none, with CloseWrite's
	// error ignored, or with only pUtil.ErrCannotHalfClose taken for a
	// failure: the client, never told the server's end, never sees its own,
	// and the relay waits for an EOF the client was never asked to send.
	for _, tc := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"bare", func(c net.Conn) net.Conn { return noCloseWriteConn{c} }},
		{"in a pUtil.Conn, as handleConnection hands it", func(c net.Conn) net.Conn {
			l := noCloseWriteConn{c}
			return &pUtil.Conn{Conn: l, Reader: l, Logger: zap.NewNop()}
		}},
		{"in a SafeConn in a pUtil.Conn", func(c net.Conn) net.Conn {
			s := proxyOwned{SafeConn: pUtil.NewSafeConn(noCloseWriteConn{c}, zap.NewNop()), leaf: c}
			return &pUtil.Conn{Conn: s, Reader: s, Logger: zap.NewNop()}
		}},
		{"its CloseWrite fails", func(c net.Conn) net.Conn { return failingCloseWrite{c} }},
	} {
		t.Run("a side that cannot half-close is closed on the other's end, as before: "+tc.name, func(t *testing.T) {
			app, client := loopbackPair(t)
			dest, server := loopbackPair(t)
			go func() {
				_, _ = server.Write([]byte("answer"))
				_ = server.Close()
			}()
			// The client holds its connection open, and reads.
			_, ended := passThrough(t, tc.wrap(client), dest)
			if a := awaitEvent(t, readAll(app), "the client reading to its end"); string(a) != "answer" {
				t.Fatalf("the client got %q, want %q", a, "answer")
			}
			if err := awaitEvent(t, ended, "the relay ending"); err != nil {
				t.Fatalf("the relay ended with %v, want nil", err)
			}
		})
	}

	// And the other way: where the destination's CloseWrite fails, the
	// client's end ends the relay and the caller closes both, so the server
	// hears the end. Red with only pUtil.ErrCannotHalfClose taken for a
	// failure there: the server, never told, waits for more of the request.
	t.Run("a destination whose CloseWrite fails is closed on the client's end, as before", func(t *testing.T) {
		app, client := loopbackPair(t)
		dest, server := loopbackPair(t)
		heard := make(chan []byte, 1)
		go func() {
			got, _ := io.ReadAll(server)
			heard <- got
		}()
		_, ended := passThrough(t, client, failingCloseWrite{dest})
		if _, err := app.Write([]byte("request")); err != nil {
			t.Fatal(err)
		}
		halfCloseOf(t, app)
		if h := awaitEvent(t, heard, "the server reading the client to its end"); string(h) != "request" {
			t.Fatalf("the server got %q, want %q", h, "request")
		}
		if err := awaitEvent(t, ended, "the relay ending"); err != nil {
			t.Fatalf("the relay ended with %v, want nil", err)
		}
	})

	// A wrapper with no CloseWrite of its own is half-closed through the conn
	// it wraps (NetConn, Unwrap). Red with that look-through gone: the relay
	// ends on the server's end, and the server never hears the client again.
	for _, tc := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"NetConn", func(c net.Conn) net.Conn { return netConnOnly{c} }},
		{"Unwrap", func(c net.Conn) net.Conn { return unwrapOnly{c} }},
	} {
		t.Run("a wrapper with no CloseWrite is half-closed through the conn it wraps: "+tc.name, func(t *testing.T) {
			app, client := loopbackPair(t)
			dest, server := loopbackPair(t)
			heard := make(chan []byte, 1)
			go func() {
				if _, err := server.Write([]byte("answer")); err != nil {
					return
				}
				_ = server.(*net.TCPConn).CloseWrite()
				got, _ := io.ReadAll(server)
				heard <- got
			}()
			_, ended := passThrough(t, tc.wrap(client), dest)
			if a := awaitEvent(t, readAll(app), "the client reading the server's answer to its end"); string(a) != "answer" {
				t.Fatalf("the client got %q, want the server's %q", a, "answer")
			}
			if _, err := app.Write([]byte("after")); err != nil {
				t.Fatal(err)
			}
			halfCloseOf(t, app)
			if h := awaitEvent(t, heard, "the server reading the client to its end"); string(h) != "after" {
				t.Fatalf("the server got %q after its half-close, want the client's %q", h, "after")
			}
			if err := awaitEvent(t, ended, "the relay ending"); err != nil {
				t.Fatalf("the relay ended with %v, want nil", err)
			}
		})
	}
}

// noCloseWriteConn is a conn that cannot half-close: no CloseWrite, and
// nothing beneath it to look through to.
type noCloseWriteConn struct{ net.Conn }

// failingCloseWrite is a conn whose CloseWrite fails with an error other than
// pUtil.ErrCannotHalfClose, as a *tls.Conn's does when its close_notify
// cannot be written: no FIN was sent.
type failingCloseWrite struct{ net.Conn }

func (failingCloseWrite) CloseWrite() error { return errors.New("shutdown: broken pipe") }

// proxyOwned is a SafeConn closed as the proxy, which owns the conn beneath,
// closes it: a SafeConn's own Close does nothing.
type proxyOwned struct {
	*pUtil.SafeConn
	leaf net.Conn
}

func (c proxyOwned) Close() error { return c.leaf.Close() }

// netConnOnly and unwrapOnly wrap a conn with no CloseWrite of their own,
// telling the conn beneath by NetConn or Unwrap.
type netConnOnly struct{ net.Conn }

func (c netConnOnly) NetConn() net.Conn { return c.Conn }

type unwrapOnly struct{ net.Conn }

func (c unwrapOnly) Unwrap() net.Conn { return c.Conn }

// A side that sent its EOF and then closed whole takes no more of the other
// side's bytes: the write to it fails, and that ends the relay with no error,
// for the side had ended what it would hear (and nothing is logged at Error).
// The other side writes on until the relay ends, so the relay's write to the
// closed side comes, whenever the reset that closes it arrives: a count of
// writes, no clock. Red with that write taken for a failure (logged at Error,
// and the relay's error).
func TestGlobalPassThroughEndsQuietlyOnAWriteToASideThatEndedAndClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// ender is the side that sends its EOF and then closes whole, and
		// writer the side that writes on; each is given the test's ends in
		// (client, server) order.
		ender, writer func(app, server net.Conn) net.Conn
	}{
		{"the server", func(_, server net.Conn) net.Conn { return server }, func(app, _ net.Conn) net.Conn { return app }},
		{"the client", func(app, _ net.Conn) net.Conn { return app }, func(_, server net.Conn) net.Conn { return server }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, client := loopbackPair(t)
			dest, server := loopbackPair(t)
			core, logs := observer.New(zap.ErrorLevel)
			_, ended, _ := passThroughBy(t, &Proxy{logger: zap.New(core)}, client, dest, -1)
			ender, writer := tc.ender(app, server), tc.writer(app, server)
			halfCloseOf(t, ender)
			// The relay forwarded its end: the writer reads its own.
			if got := awaitEvent(t, readAll(writer), "the other side reading the end forwarded to it"); len(got) != 0 {
				t.Fatalf("the other side read %q, want only its end", got)
			}
			_ = ender.Close()
			chunk := counted(pUtil.WaitReadSize, 5)
			var err error
		writing:
			for {
				select {
				case err = <-ended:
					break writing
				default:
				}
				if _, werr := writer.Write(chunk); werr != nil { // the relay ended, and its caller closed the conns
					err = awaitEvent(t, ended, "the relay ending")
					break
				}
			}
			if err != nil {
				t.Errorf("the relay ended with %v, want nil: the side it could not write to had ended and closed", err)
			}
			for _, e := range logs.All() {
				t.Errorf("the proxy logged %s %q (%v), want nothing at Error", e.Level, e.Message, e.ContextMap())
			}
		})
	}
}

// A TLS 1.2 server's last answer, read together with the close_notify that
// follows it (crypto/tls returns them as one read: the bytes and io.EOF),
// reaches the client: a relay reader that dropped the bytes that came with
// its read's error lost the answer of every server that answers and closes.
func TestGlobalPassThroughHandsOnATLSServersLastAnswerReadWithItsClose(t *testing.T) {
	cert := selfSignedForDispatch(t)
	app, client := loopbackPair(t)
	destRaw, serverRaw := loopbackPair(t)
	whole := &readWhole{Conn: destRaw}
	dest := tls.Client(whole, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	server := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{cert}})
	handshake(t, dest, server)
	go func() {
		_, _ = server.Write([]byte("last answer"))
		_ = server.Close()
	}()
	// From here the relay's first read of dest gets the server's answer and
	// its close_notify at once.
	whole.arm()
	_, ended := passThrough(t, client, dest)
	if a := awaitEvent(t, readAll(app), "the client reading to its end"); string(a) != "last answer" {
		t.Fatalf("the client got %q, want the server's last answer %q", a, "last answer")
	}
	_ = app.Close()
	if err := awaitEvent(t, ended, "the relay ending"); err != nil {
		t.Fatalf("the relay ended with %v, want nil", err)
	}
}

// readWhole, once armed, reads its connection to its end before it hands on
// any of it, so a TLS layer above reads what its peer sent last in one read.
type readWhole struct {
	net.Conn
	mu    sync.Mutex
	armed bool
	rest  *bytes.Reader
}

func (c *readWhole) arm() { c.mu.Lock(); c.armed = true; c.mu.Unlock() }

func (c *readWhole) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return c.Conn.Read(b)
	}
	if c.rest == nil {
		all, err := io.ReadAll(c.Conn)
		if err != nil {
			return 0, err
		}
		c.rest = bytes.NewReader(all)
	}
	return c.rest.Read(b)
}

// handshake completes each TLS connection's handshake, together, as each
// pair's two ends need.
func handshake(t *testing.T, conns ...*tls.Conn) {
	t.Helper()
	errs := make(chan error, len(conns))
	for _, c := range conns {
		go func() { errs <- c.Handshake() }()
	}
	for range conns {
		if err := awaitEvent(t, errs, "a TLS handshake"); err != nil {
			t.Fatalf("handshake: %v", err)
		}
	}
}

// streamingConn is a client that sends data and then its EOF, telling the size
// of each Read's buffer.
type streamingConn struct {
	net.Conn
	data  []byte
	sizes chan int
	// endWithLast: its last bytes come with its EOF, in one Read.
	endWithLast bool
	// rest, when set, holds every Read after the first until it is closed.
	rest  chan struct{}
	reads int
}

func (c *streamingConn) Read(b []byte) (int, error) {
	c.sizes <- len(b)
	if c.reads++; c.reads > 1 && c.rest != nil {
		<-c.rest
	}
	n := copy(b, c.data)
	c.data = c.data[n:]
	if len(c.data) == 0 && c.endWithLast {
		return n, io.EOF
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// heldWriteConn is a destination that cannot half-close, keeps what is written to
// it, tells the size of each write, and holds the first write until release
// is closed. With err set, a write released fails with it instead.
type heldWriteConn struct {
	net.Conn
	writes  chan int
	release chan struct{}
	err     error
	mu      sync.Mutex
	got     []byte
}

func (c *heldWriteConn) Write(b []byte) (int, error) {
	c.writes <- len(b)
	<-c.release
	if c.err != nil {
		return 0, c.err
	}
	c.mu.Lock()
	c.got = append(c.got, b...)
	c.mu.Unlock()
	return len(b), nil
}

// A pass-through's reads are small, for each can wait for hours and holds its
// buffer while it does (pUtil.WaitReadSize, as the old loop read), and the
// reads that queue while the other side is written are gathered into one
// write of up to pUtil.ReadChunkSize, so a stream costs a write a few reads,
// not one a read. The client's last reads, queued behind its EOF, are written
// before its end ends the relay. No clock: the first write is held until the
// client's reads have filled the queue and its reader waits on it.
func TestGlobalPassThroughReadsSmallAndGathersWhatQueues(t *testing.T) {
	const chunks = 80
	app, cp := net.Pipe()
	dp, srv := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	want := counted(chunks*pUtil.WaitReadSize, 7)
	client := &streamingConn{Conn: cp, data: want, sizes: make(chan int, 2*chunks)}
	dest := &heldWriteConn{Conn: dp, writes: make(chan int, 2*chunks), release: make(chan struct{})}
	_, ended := passThrough(t, client, dest)

	first := awaitEvent(t, dest.writes, "the first write to the destination")
	// The reads in the first write, the queue's, and the one its reader holds.
	for i := 0; i < first/pUtil.WaitReadSize+passThroughQueue+1; i++ {
		if got := awaitEvent(t, client.sizes, "a read of the client"); got != pUtil.WaitReadSize {
			t.Fatalf("the client's read %d had a %d-byte buffer, want pUtil.WaitReadSize (%d)", i, got, pUtil.WaitReadSize)
		}
	}
	close(dest.release)
	if got := awaitEvent(t, dest.writes, "the second write to the destination"); got != pUtil.ReadChunkSize {
		t.Fatalf("with %d reads queued, the next write took %d bytes, want them gathered into pUtil.ReadChunkSize (%d)",
			passThroughQueue, got, pUtil.ReadChunkSize)
	}
	if err := awaitEvent(t, ended, "the pass-through's end on the client's EOF"); err != nil {
		t.Fatalf("globalPassThrough = %v, want nil on the client's end", err)
	}
	dest.mu.Lock()
	defer dest.mu.Unlock()
	if !bytes.Equal(dest.got, want) {
		t.Fatalf("the destination got %d bytes, want the client's %d in order", len(dest.got), len(want))
	}
	for len(client.sizes) > 0 {
		if got := <-client.sizes; got != pUtil.WaitReadSize {
			t.Fatalf("a read of the client had a %d-byte buffer, want pUtil.WaitReadSize (%d)", got, pUtil.WaitReadSize)
		}
	}
}

// A side's reads queued behind its EOF are written to the other side before
// that EOF ends the relay: the relay takes a side's error from its own channel,
// and one taken first would end the relay with them unwritten. The client's
// first read is written alone, and that write is held until the client's
// reader has returned (a goroutine added to its errgroup, limited to the two
// readers, runs only then), so its last reads and its EOF both wait for the
// relay when it next selects, and it takes either first. With the queued reads
// dropped at the end, each round fails one time in two, so the row runs the
// round 64 times: the drop goes unseen once in 2^64. A count, no clock.
func TestGlobalPassThroughWritesASidesQueuedReadsBeforeItsEnd(t *testing.T) {
	for round := 0; round < 64; round++ {
		if !writesQueuedReadsBeforeItsEnd(t, round) {
			return
		}
	}
}

// writesQueuedReadsBeforeItsEnd is one round of
// TestGlobalPassThroughWritesASidesQueuedReadsBeforeItsEnd, and reports whether
// it passed.
func writesQueuedReadsBeforeItsEnd(t *testing.T, round int) bool {
	t.Helper()
	const chunks = 20 // fewer than passThroughQueue: all of them queue
	app, cp := net.Pipe()
	dp, srv := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	want := counted(chunks*pUtil.WaitReadSize, byte(round))
	client := &streamingConn{Conn: cp, data: want, sizes: make(chan int, 2*chunks), endWithLast: true, rest: make(chan struct{})}
	dest := &heldWriteConn{Conn: dp, writes: make(chan int, 2*chunks), release: make(chan struct{})}
	_, ended, g := passThroughLimited(t, client, dest, 2)

	// Both readers run by the first write: the relay starts them first.
	if got := awaitEvent(t, dest.writes, "the first write to the destination"); got != pUtil.WaitReadSize {
		t.Errorf("round %d: the first write took %d bytes, want the client's first read alone (%d)", round, got, pUtil.WaitReadSize)
		return false
	}
	close(client.rest) // its last read comes with its EOF
	clientReaderEnded := make(chan struct{})
	go g.Go(func() error { close(clientReaderEnded); return nil })
	awaitEvent(t, clientReaderEnded, "the client's reader returning, its last reads and its EOF queued")
	close(dest.release)
	if err := awaitEvent(t, ended, "the pass-through's end on the client's EOF"); err != nil {
		t.Errorf("round %d: globalPassThrough = %v, want nil on the client's end", round, err)
		return false
	}
	dest.mu.Lock()
	defer dest.mu.Unlock()
	if !bytes.Equal(dest.got, want) {
		t.Errorf("round %d: the destination got %d bytes, want the client's %d in order: its last reads were dropped at its end", round, len(dest.got), len(want))
		return false
	}
	return true
}

// A pass-through's readers end with it, while its context lasts: a reader
// holding a read the ended relay will never take returns once the relay does.
// Here the relay ends on a write to the destination that fails, while the
// client streams on, so its reader fills its queue and holds a read more.
// handleConnection ends the context its readers run under before it waits for
// them, so it does not show there: the errgroup here is waited for with its
// context still open, and the readers must still end. Red with the readers'
// context the call's own: the client's reader waits on its full queue forever.
func TestGlobalPassThroughEndsItsReadersWithIt(t *testing.T) {
	app, cp := net.Pipe()
	dp, srv := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	client := &endlessConn{Conn: cp}
	dest := &failingWriteConn{Conn: dp}
	g, gctx := errgroup.WithContext(context.Background())
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "1")
	gctx = context.WithValue(gctx, models.DestConnectionIDKey, "2")
	returned := make(chan error, 1)
	go func() { returned <- p0().globalPassThrough(gctx, client, dest) }()
	if err := awaitEvent(t, returned, "the pass-through's end on its failed write"); err == nil {
		t.Fatal("globalPassThrough = nil, want the failed write's error")
	}
	// Closed, as its caller closes them: the destination's reader, in its
	// read, returns on that; the client's never reads an error.
	_ = client.Close()
	_ = dest.Close()
	waited := make(chan error, 1)
	go func() { waited <- g.Wait() }()
	_ = awaitEvent(t, waited, "the pass-through's readers ending with it, their context still open")
}

// A reader holding its side's last bytes, read with its EOF, returns with the
// relay while its queue is full: the hand-off of the bytes that came with a
// read's error waits for a taker only while the relay's context lasts, as
// every hand-off does. The relay is held in its first write to the
// destination until the client's reader, its queue full, holds that last
// read (its goroutine parked in the hand-off); then the write fails, and the
// relay ends and will take nothing more. Events and a count, no clock. Red
// with that hand-off a plain send: the reader waits for a taker forever, and
// the connection's wait for its goroutines never returns.
func TestGlobalPassThroughEndsAReaderHoldingItsSidesLastBytes(t *testing.T) {
	app, cp := net.Pipe()
	dp, srv := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	// The read the first write takes, the queue's, and the one held.
	const reads = 1 + passThroughQueue + 1
	client := &streamingConn{Conn: cp, data: counted(reads*pUtil.WaitReadSize, 9), sizes: make(chan int, reads+1),
		endWithLast: true, rest: make(chan struct{})}
	dest := &heldWriteConn{Conn: dp, writes: make(chan int, 1), release: make(chan struct{}),
		err: errors.New("the destination reset the connection")}
	_, ended := passThrough(t, client, dest)

	// The client's reads after its first wait for rest, so that first one is
	// written alone, and the write holds the relay.
	awaitEvent(t, dest.writes, "the first write to the destination")
	close(client.rest)
	for i := 0; i < reads; i++ {
		awaitEvent(t, client.sizes, "a read of the client")
	}
	// Its last read began after the one before it was queued; once the
	// reader parks, it holds that read's bytes, with the queue full.
	for !parkedInAHandOff() {
		runtime.Gosched()
	}
	close(dest.release)
	if err := awaitEvent(t, ended, "the pass-through and its readers ending"); err == nil {
		t.Fatal("globalPassThrough = nil, want the failed write's error")
	}
}

// parkedInAHandOff reports whether a relay reader (a goroutine in
// ReadBuffConn) waits on a channel send or a select in package pUtil itself,
// outside readBytes: handing on what it read, in ReadBuffConn or in any
// helper of pUtil's it hands on through. Only relay readers count, so a
// goroutine an earlier test left in a pUtil wait does not. A reader blocked
// in its read waits in the conn's read (net.(*pipe)'s here, or the test's
// streamingConn's), outside pUtil; one in readBytes waits in its read or
// before it retries an empty EOF, and is handing nothing on.
func parkedInAHandOff() bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		lines := strings.Split(g, "\n")
		if !strings.Contains(lines[0], "[chan send") && !strings.Contains(lines[0], "[select") {
			continue
		}
		// A relay reader, and not in its read: readBytes waits in package
		// pUtil too, before it retries an empty EOF.
		if !strings.Contains(g, "go.keploy.io/server/v3/pkg/agent/proxy/util.ReadBuffConn(") ||
			strings.Contains(g, "go.keploy.io/server/v3/pkg/agent/proxy/util.readBytes(") {
			continue
		}
		for _, f := range lines[1:] { // its first frame past package runtime's
			if f == "" || strings.HasPrefix(f, "\t") || strings.HasPrefix(f, "runtime.") {
				continue
			}
			if strings.HasPrefix(f, "go.keploy.io/server/v3/pkg/agent/proxy/util.") {
				return true
			}
			break
		}
	}
	return false
}

// A pass-through that ends because its context did ends quietly: the
// interrupt that puts both connections' deadlines in the past runs only once
// the context its relay and readers judge an interrupted read or write by
// reads as ended, so they take the interruption for that end, the relay
// returns the context's error, and nothing is logged at Error.
//
// Which runs first is otherwise down to the order a context's end reaches
// what waits on it, which the context package leaves open (its waits are a
// map's). The caller's context here (orderedCtx) fixes it: it runs what
// waits on its end one at a time, in the order each began to wait or in the
// reverse of it, and before the next lets what the last one set off settle:
// here, until the pass-through has returned. The row runs both orders. An
// interrupt that waits on the caller's context beside the relay's own context
// made from it (as one did), whichever began to wait first, then fires while
// the relay's context is still open in one of them, on every run: the
// readers' interrupted reads are logged as failures, and the relay returns
// their i/o timeout. A stated limit: an interrupt run by a goroutine that
// waits on the caller's Done() itself, not by an AfterFunc, is not ordered by
// this row.
func TestGlobalPassThroughInterruptsOnlyOnceItsContextHasEnded(t *testing.T) {
	for _, order := range []struct {
		name    string
		reverse bool
	}{{"in the order they began to wait", false}, {"in the reverse of it", true}} {
		t.Run(order.name, func(t *testing.T) { interruptsOnlyOnceItsContextHasEnded(t, order.reverse) })
	}
}

// interruptsOnlyOnceItsContextHasEnded is
// TestGlobalPassThroughInterruptsOnlyOnceItsContextHasEnded with the caller's
// context running its waits in reverse when reverse is set.
func interruptsOnlyOnceItsContextHasEnded(t *testing.T, reverse bool) {
	// The far ends stay open (until t ends) and send nothing: the relay
	// waits on its reads.
	client, _ := loopbackPair(t)
	dest, _ := loopbackPair(t)
	reading := make(chan struct{}, 2)
	core, logs := observer.New(zap.ErrorLevel)
	p := &Proxy{logger: zap.New(core)}
	g := new(errgroup.Group)
	ctx := newOrderedCtx(context.WithValue(context.WithValue(context.WithValue(context.Background(),
		models.ErrGroupKey, g),
		models.ClientConnectionIDKey, "1"),
		models.DestConnectionIDKey, "2"), reverse)
	var err error
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		err = p.globalPassThrough(ctx, &readEnteredConn{Conn: client, entered: reading}, &readEnteredConn{Conn: dest, entered: reading})
	}()
	// Both readers waiting on a read: the relay made its waits on ctx, which
	// come before it starts them.
	awaitEvent(t, reading, "the client's reader waiting on a read")
	awaitEvent(t, reading, "the destination's reader waiting on a read")
	ctx.end(func() { awaitEvent(t, returned, "the pass-through returning") })
	awaitEvent(t, returned, "the pass-through returning")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("globalPassThrough = %v, want the context's end (%v)", err, context.Canceled)
	}
	_ = client.Close()
	_ = dest.Close()
	waited := make(chan error, 1)
	go func() { waited <- g.Wait() }()
	_ = awaitEvent(t, waited, "the pass-through's readers ending")
	for _, e := range logs.All() {
		t.Errorf("the proxy logged %s %q (%v), want nothing at Error on the context's end", e.Level, e.Message, e.ContextMap())
	}
}

// orderedCtx is a context whose end runs what waits on it (a context made
// from it, a context.AfterFunc on it: the context package hands a parent with
// an AfterFunc method each such wait) one at a time, in the order each began
// to wait (or, reverse, the last first), with what end is given run after
// each. Its values are its Context's, which never ends.
type orderedCtx struct {
	context.Context
	reverse bool
	done    chan struct{}
	mu      sync.Mutex
	err     error
	waiting []*orderedWait
}

// orderedWait is one wait on an orderedCtx: f, unless stopped first.
type orderedWait struct {
	f             func()
	stopped, took bool
}

func newOrderedCtx(values context.Context, reverse bool) *orderedCtx {
	return &orderedCtx{Context: values, reverse: reverse, done: make(chan struct{})}
}

func (c *orderedCtx) Done() <-chan struct{} { return c.done }

func (c *orderedCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// AfterFunc has f run at c's end, in its turn; on a c already ended, at once
// in its own goroutine, as context.AfterFunc does.
func (c *orderedCtx) AfterFunc(f func()) (stop func() bool) {
	w := &orderedWait{f: f}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		w.took = true
		go f()
	} else {
		c.waiting = append(c.waiting, w)
	}
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if w.took || w.stopped {
			return false
		}
		w.stopped = true
		return true
	}
}

// end ends c, then runs each wait not stopped by then, in the order they
// began (or its reverse), calling settle after each.
func (c *orderedCtx) end(settle func()) {
	c.mu.Lock()
	c.err = context.Canceled
	close(c.done)
	waiting := slices.Clone(c.waiting)
	c.mu.Unlock()
	if c.reverse {
		slices.Reverse(waiting)
	}
	for _, w := range waiting {
		c.mu.Lock()
		run := !w.stopped
		w.took = true
		c.mu.Unlock()
		if run {
			w.f()
			settle()
		}
	}
}

// readEnteredConn says on entered each time a Read begins.
type readEnteredConn struct {
	net.Conn
	entered chan<- struct{}
}

func (c *readEnteredConn) Read(b []byte) (int, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	return c.Conn.Read(b)
}

// endlessConn is a client that always has more to send: each Read fills its
// buffer, and none waits or fails.
type endlessConn struct{ net.Conn }

func (endlessConn) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'c'
	}
	return len(b), nil
}

// failingWriteConn is a destination whose every write fails.
type failingWriteConn struct{ net.Conn }

func (failingWriteConn) Write([]byte) (int, error) {
	return 0, errors.New("the destination reset the connection")
}
