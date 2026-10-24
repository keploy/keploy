package http

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const (
	// testTimeout is the maximum time a test should wait for chunkedResponse to complete.
	// If it takes longer, the test fails (indicating the loop didn't exit properly).
	testTimeout = 1 * time.Second
	// contextTimeout is the context deadline for the chunkedResponse call.
	contextTimeout = 2 * time.Second
	// maxAcceptableReads is the threshold for detecting excessive EOF reads.
	// ReadBytes in util.go returns EOF after 5 consecutive empty reads, so we allow some margin.
	maxAcceptableReads = 10
)

// mockConn is a mock net.Conn that returns test data on first read, then EOF on subsequent reads.
type mockConn struct {
	readCount    int
	data         []byte
	dataReturned bool
}

func (m *mockConn) Read(b []byte) (n int, err error) {
	m.readCount++
	// NOTE: This mock is intentionally simplified. In production, util.ReadBytes
	// returns EOF after 5 consecutive empty reads (with 100ms sleep between each).
	// Here we return EOF immediately to keep tests fast and deterministic.
	//
	// First read returns data if we have any.
	if !m.dataReturned && len(m.data) > 0 {
		n = copy(b, m.data)
		m.dataReturned = true
		return n, nil
	}
	// Subsequent reads return EOF with no data (simulating closed connection).
	return 0, io.EOF
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	return len(b), nil
}

func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

// newTestHTTP creates an HTTP handler with a no-op logger for testing.
func newTestHTTP() *HTTP {
	return &HTTP{Logger: zap.NewNop()}
}

// TestChunkedResponseExitsOnEOF tests that chunkedResponse properly exits the loop
// when it receives EOF with no data. This test will timeout if the break statement
// only exits the select block instead of the for loop (which is the bug).
func TestChunkedResponseExitsOnEOF(t *testing.T) {
	h := newTestHTTP()

	// Create mock connections
	// clientConn receives the proxied response
	clientConn := &mockConn{}
	// destConn simulates a server that sends some chunked data then closes
	destConn := &mockConn{
		data: []byte("5\r\nhello\r\n0\r\n\r\n"), // Valid chunked response with terminator
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	var finalResp []byte

	// This should complete quickly, not timeout
	done := make(chan error, 1)
	go func() {
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("chunkedResponse did not exit in time - the break statement is not exiting the for loop!")
	}
}

// TestChunkedResponseExitsOnSuffixedTerminator reproduces the SAP /360 bug:
// pUtil.ReadBytes returns a buffer shaped like "<body>0\r\n\r\n" (last-chunk
// terminator concatenated with the preceding body bytes in the same TLS
// record) rather than the terminator in a dedicated read. The old strict
// equality check `string(resp) == "0\r\n\r\n"` missed this shape, so the
// loop blocked on a second ReadBytes until the upstream's keep-alive timed
// out (~60s observed). The response's chunked framing now ends the loop
// (chunkedBody), so it must exit promptly — we assert completion well under
// the keep-alive idle window by enforcing a tight 100 ms budget (the
// happy-path return takes microseconds; anything over 100 ms means the
// terminator was not recognised).
func TestChunkedResponseExitsOnSuffixedTerminator(t *testing.T) {
	h := newTestHTTP()

	clientConn := &mockConn{}
	// Buggy shape: body tail + last-chunk marker in one TLS record. The
	// headers and the start of the chunk were read before chunkedResponse
	// runs, as handleChunkedResponses leaves them in finalResp.
	destConn := &mockConn{
		data: []byte("def\r\n0\r\n\r\n"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	finalResp := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nabc")

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn)
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if elapsed > 100*time.Millisecond {
			t.Fatalf("chunkedResponse took %v (> 100ms) — suggests a second "+
				"ReadBytes call on an idle conn; the terminator was not recognised",
				elapsed)
		}
		if destConn.readCount != 1 {
			t.Errorf("expected exactly 1 read (exit on the last-chunk), got %d "+
				"— a second read means the terminator was not recognised",
				destConn.readCount)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("chunkedResponse stuck past 100ms on buffer \"<body>0\\r\\n\\r\\n\" "+
			"after %d reads — the terminator was not recognised", destConn.readCount)
	}
}

// TestChunkedResponseEmptyBody tests the specific case where the server closes
// the connection immediately (no body). This reproduces the bug seen with Playwright
// where the proxy gets stuck in a loop. Also verifies we don't do excessive reads.
func TestChunkedResponseEmptyBody(t *testing.T) {
	h := newTestHTTP()

	clientConn := &mockConn{}
	// destConn simulates a server that immediately returns EOF (connection closed)
	destConn := &mockConn{
		data: nil, // No data, just EOF
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	var finalResp []byte

	done := make(chan error, 1)
	go func() {
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		// Verify we didn't do excessive reads (indicates loop not exiting properly)
		if destConn.readCount > maxAcceptableReads {
			t.Errorf("Too many reads from destConn: %d (expected <= %d). "+
				"This suggests the loop is not exiting properly on EOF",
				destConn.readCount, maxAcceptableReads)
		}
	case <-time.After(testTimeout):
		t.Fatalf("chunkedResponse stuck in loop after %d reads", destConn.readCount)
	}
}

// endsAsItsDeadlineIsSet is a connection whose context ends as a read deadline
// still to come is first set on it: it cancels the context then, and sets the
// deadline only once the context's end has set its own, one already passed
// (pUtil.InterruptOnDone), so that it replaces that one. It sets it as no
// deadline: a read waits for data, the close, or a deadline already passed,
// never for the clock.
type endsAsItsDeadlineIsSet struct {
	net.Conn
	cancel      context.CancelFunc
	interrupted chan struct{} // closed once a deadline already passed is set
	interrupt   sync.Once
	end         sync.Once
}

func (c *endsAsItsDeadlineIsSet) SetDeadline(t time.Time) error {
	err := c.Conn.SetDeadline(t)
	if !t.IsZero() && !t.After(time.Now()) {
		c.interrupt.Do(func() { close(c.interrupted) })
	}
	return err
}

func (c *endsAsItsDeadlineIsSet) SetReadDeadline(t time.Time) error {
	if t.IsZero() || !t.After(time.Now()) {
		return c.Conn.SetReadDeadline(t)
	}
	c.end.Do(func() {
		c.cancel()
		<-c.interrupted
	})
	return c.Conn.SetReadDeadline(time.Time{})
}

// A request body read whose context ends just before it sets its read
// deadline still ends: the deadline it sets replaces the one the context's end
// set to end its read (pUtil.InterruptOnDone), so it looks at the context after
// it sets it. It looked before, and then read on until its own deadline (20 s)
// had passed, and held decodeHTTP, which waits for it, as long. Nothing writes
// the body here, so only the context's end can end the read.
func TestABodyReadEndsWithAContextThatEndsAsItsDeadlineIsSet(t *testing.T) {
	_, proxy := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &endsAsItsDeadlineIsSet{Conn: proxy, cancel: cancel, interrupted: make(chan struct{})}
	defer pUtil.InterruptOnDone(ctx, conn)()
	finalReq := []byte("POST /u HTTP/1.1\r\nHost: api.example\r\nContent-Length: 5\r\n\r\n")
	returned := make(chan error, 1)
	go func() {
		returned <- newTestHTTP().contentLengthRequest(ctx, &finalReq, conn, nil, 5)
	}()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("contentLengthRequest = %v, want the context's error", err)
		}
	case <-time.After(time.Minute): // the bound only turns a hang into a failure
		t.Fatal("the body read went on reading after its context had ended")
	}
}

// closedAtTheEndConn is a connection the context's end closes, as the proxy's
// connection closer closes the client's at the call's end, while the deadline
// the end sets (pUtil.InterruptOnDone) ends the read in progress: once a
// deadline already passed is set on it, no read deadline can be set any more
// (net.ErrClosed). Each read that starts is told on reading, when nothing
// waits there already.
type closedAtTheEndConn struct {
	net.Conn
	closed  atomic.Bool
	reading chan struct{}
}

func (c *closedAtTheEndConn) SetDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		c.closed.Store(true) // before the deadline wakes the read
	}
	return c.Conn.SetDeadline(t)
}

func (c *closedAtTheEndConn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	return c.Conn.SetReadDeadline(t)
}

func (c *closedAtTheEndConn) Read(p []byte) (int, error) {
	select {
	case c.reading <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}

// A request body read that the context's end stops returns the context's
// error and logs no ERROR, the connection closed at the end too: the read
// that sets a deadline before each read (Content-Length) failed to set the
// next one and logged it, and the one that frames chunks logged its read's
// end with no error, so not as the context's. Nothing writes the body here,
// so only the context's end can end the read.
func TestABodyReadTheContextsEndStopsLogsNoError(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
		read func(h *HTTP, ctx context.Context, finalReq *[]byte, conn net.Conn) error
	}{
		{"Content-Length", "POST /u HTTP/1.1\r\nHost: api.example\r\nContent-Length: 5\r\n\r\n",
			func(h *HTTP, ctx context.Context, finalReq *[]byte, conn net.Conn) error {
				return h.contentLengthRequest(ctx, finalReq, conn, nil, 5)
			}},
		{"chunked", "POST /u HTTP/1.1\r\nHost: api.example\r\nTransfer-Encoding: chunked\r\n\r\n",
			func(h *HTTP, ctx context.Context, finalReq *[]byte, conn net.Conn) error {
				return h.chunkedRequest(ctx, finalReq, conn, nil)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, proxy := tcpPair(t)
			conn := &closedAtTheEndConn{Conn: proxy, reading: make(chan struct{}, 1)}
			core, logs := observer.New(zapcore.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer pUtil.InterruptOnDone(ctx, conn)()
			finalReq := []byte(tc.head)
			returned := make(chan error, 1)
			go func() { returned <- tc.read(h, ctx, &finalReq, conn) }()
			// The bounds below only turn a hang into a failure.
			select {
			case <-conn.reading:
			case <-time.After(time.Minute):
				t.Fatal("the body was not read")
			}
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("the body read = %v, want the context's error", err)
				}
			case <-time.After(time.Minute):
				t.Fatal("the body read went on reading after its context had ended")
			}
			for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
				t.Errorf("logged an ERROR at the context's end: %s %v", e.Message, e.Context)
			}
		})
	}
}
