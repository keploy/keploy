package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"golang.org/x/sync/errgroup"
)

// passThrough runs globalPassThrough between client and dest as a
// connection's handler does: with an errgroup and the connection ids in its
// context and, once it returns, both connections closed and its readers
// waited for. ended delivers what it returned, after all that.
func passThrough(t *testing.T, client, dest net.Conn) (cancel context.CancelFunc, ended <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "1")
	gctx = context.WithValue(gctx, models.DestConnectionIDKey, "2")
	out := make(chan error, 1)
	go func() {
		err := p0().globalPassThrough(gctx, client, dest)
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
	return cancel, out
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

	t.Run("a side that cannot half-close is closed on the other's end, as before", func(t *testing.T) {
		app, client := loopbackPair(t)
		dest, server := loopbackPair(t)
		go func() {
			_, _ = server.Write([]byte("answer"))
			_ = server.Close()
		}()
		// The client holds its connection open, and reads.
		_, ended := passThrough(t, struct{ net.Conn }{client}, dest)
		if a := awaitEvent(t, readAll(app), "the client reading to its end"); string(a) != "answer" {
			t.Fatalf("the client got %q, want %q", a, "answer")
		}
		if err := awaitEvent(t, ended, "the relay ending"); err != nil {
			t.Fatalf("the relay ended with %v, want nil", err)
		}
	})
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
// is closed.
type heldWriteConn struct {
	net.Conn
	writes  chan int
	release chan struct{}
	mu      sync.Mutex
	got     []byte
}

func (c *heldWriteConn) Write(b []byte) (int, error) {
	c.writes <- len(b)
	<-c.release
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
// first read is written alone, and that write is held until its last read,
// which comes with its EOF, has been made, so its error waits beside its
// queued reads. The relay then takes either first (a select): with the queued
// reads dropped at the end, about one run in two fails. No clock.
func TestGlobalPassThroughWritesASidesQueuedReadsBeforeItsEnd(t *testing.T) {
	const chunks = 20 // fewer than passThroughQueue: all of them queue
	app, cp := net.Pipe()
	dp, srv := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = srv.Close() })
	want := counted(chunks*pUtil.WaitReadSize, 9)
	client := &streamingConn{Conn: cp, data: want, sizes: make(chan int, 2*chunks), endWithLast: true, rest: make(chan struct{})}
	dest := &heldWriteConn{Conn: dp, writes: make(chan int, 2*chunks), release: make(chan struct{})}
	_, ended := passThrough(t, client, dest)

	if got := awaitEvent(t, dest.writes, "the first write to the destination"); got != pUtil.WaitReadSize {
		t.Fatalf("the first write took %d bytes, want the client's first read alone (%d)", got, pUtil.WaitReadSize)
	}
	close(client.rest)
	for i := 0; i < chunks; i++ { // its last read comes with its EOF
		awaitEvent(t, client.sizes, "a read of the client")
	}
	close(dest.release)
	if err := awaitEvent(t, ended, "the pass-through's end on the client's EOF"); err != nil {
		t.Fatalf("globalPassThrough = %v, want nil on the client's end", err)
	}
	dest.mu.Lock()
	defer dest.mu.Unlock()
	if !bytes.Equal(dest.got, want) {
		t.Fatalf("the destination got %d bytes, want the client's %d in order: its last reads were dropped at its end", len(dest.got), len(want))
	}
}
