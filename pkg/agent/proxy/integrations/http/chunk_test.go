package http

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

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

// mockConn is a mock net.Conn that simulates a connection returning EOF.
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
