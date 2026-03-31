package http

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

func (m *mockConn) Write(b []byte) (n int, err error) { return len(b), nil }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(time.Time) error        { return nil }
func (m *mockConn) SetReadDeadline(time.Time) error    { return nil }
func (m *mockConn) SetWriteDeadline(time.Time) error   { return nil }

// newTestHTTP creates an HTTP handler with a no-op logger for testing.
func newTestHTTP() *HTTP { return &HTTP{Logger: zap.NewNop()} }

// TestChunkedResponseExitsOnEOF tests that chunkedResponse properly exits the loop
// when it receives EOF with no data.
func TestChunkedResponseExitsOnEOF(t *testing.T) {
	h := newTestHTTP()

	// clientConn receives the proxied response
	clientConn := &mockConn{}
	// destConn simulates a server that sends some chunked data then closes
	destConn := &mockConn{
		data: []byte("5\r\nhello\r\n0\r\n\r\n"), // Valid chunked response with terminator
	}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	var finalResp []byte

	done := make(chan error, 1)
	go func() {
		var streamRef *models.StreamRef
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn, false, &streamRef, models.OutgoingOptions{})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("chunkedResponse did not exit in time")
	}
}

// TestChunkedResponseEmptyBody tests the specific case where the server closes
// the connection immediately (no body). Also verifies we don't do excessive reads.
func TestChunkedResponseEmptyBody(t *testing.T) {
	h := newTestHTTP()

	clientConn := &mockConn{}
	// destConn simulates a server that immediately returns EOF (connection closed)
	destConn := &mockConn{data: nil}

	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	var finalResp []byte

	done := make(chan error, 1)
	go func() {
		var streamRef *models.StreamRef
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn, false, &streamRef, models.OutgoingOptions{})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if destConn.readCount > maxAcceptableReads {
			t.Errorf("too many reads from destConn: %d (expected <= %d)", destConn.readCount, maxAcceptableReads)
		}
	case <-time.After(testTimeout):
		t.Fatalf("chunkedResponse stuck in loop after %d reads", destConn.readCount)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// scriptedReadConn returns the provided chunks (one per Read call) and then returns a timeout.
// This models a keep-alive socket where no more data arrives after the terminating chunk.
type scriptedReadConn struct {
	chunks [][]byte
	idx    int
}

func (c *scriptedReadConn) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, timeoutErr{}
	}
	ch := c.chunks[c.idx]
	c.idx++
	if len(ch) > len(p) {
		copy(p, ch[:len(p)])
		return len(p), nil
	}
	copy(p, ch)
	return len(ch), nil
}

func (c *scriptedReadConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *scriptedReadConn) Close() error                { return nil }
func (c *scriptedReadConn) LocalAddr() net.Addr         { return &net.TCPAddr{} }
func (c *scriptedReadConn) RemoteAddr() net.Addr        { return &net.TCPAddr{} }
func (c *scriptedReadConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedReadConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedReadConn) SetWriteDeadline(time.Time) error { return nil }

func TestChunkedResponse_EndMarkerNotIsolated_DoesNotOverread(t *testing.T) {
	h := newTestHTTP()

	// Pretend the response headers were already read and buffered by handleChunkedResponses.
	finalResp := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")

	// Last read contains *both* body bytes and the terminating chunk marker. Older code expected
	// resp == "0\r\n\r\n" and would overread and hit the timeout error.
	destConn := &scriptedReadConn{
		chunks: [][]byte{
			[]byte("5\r\nhello\r\n0\r\n\r\n"),
		},
	}

	clientConn := &mockConn{}
	var streamRef *models.StreamRef
	err := h.chunkedResponse(context.Background(), &finalResp, clientConn, destConn, false, &streamRef, models.OutgoingOptions{})
	if err != nil {
		t.Fatalf("chunkedResponse returned error: %v", err)
	}
	if streamRef != nil {
		t.Fatalf("expected nil streamRef, got: %#v", streamRef)
	}
	if !bytes.HasSuffix(finalResp, []byte("0\r\n\r\n")) {
		t.Fatalf("finalResp does not end with end marker; len=%d tail=%q", len(finalResp), finalResp[max(0, len(finalResp)-32):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

