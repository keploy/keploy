package http

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// mockConn is a mock net.Conn that simulates a connection returning EOF
type mockConn struct {
	readCount    int
	maxReads     int
	data         []byte
	dataReturned bool
}

func (m *mockConn) Read(b []byte) (n int, err error) {
	m.readCount++
	// First read returns data if we have any
	if !m.dataReturned && len(m.data) > 0 {
		n = copy(b, m.data)
		m.dataReturned = true
		return n, nil
	}
	// Subsequent reads return EOF with no data (simulating closed connection)
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

// TestChunkedResponseExitsOnEOF tests that chunkedResponse properly exits the loop
// when it receives EOF with no data. This test will timeout if the break statement
// only exits the select block instead of the for loop (which is the bug).
func TestChunkedResponseExitsOnEOF(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := &HTTP{Logger: logger}

	// Create mock connections
	// clientConn receives the proxied response
	clientConn := &mockConn{}
	// destConn simulates a server that sends some chunked data then closes
	destConn := &mockConn{
		data: []byte("5\r\nhello\r\n0\r\n\r\n"), // Valid chunked response with terminator
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var finalResp []byte

	// This should complete quickly, not timeout
	done := make(chan error, 1)
	go func() {
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn)
	}()

	select {
	case err := <-done:
		if err != nil && err != context.DeadlineExceeded {
			t.Logf("chunkedResponse returned error (expected): %v", err)
		}
		t.Logf("chunkedResponse completed successfully in time")
	case <-time.After(1 * time.Second):
		t.Fatal("chunkedResponse did not exit within 1 second - the break statement is not exiting the for loop!")
	}
}

// TestChunkedResponseExitsOnEOFWithEmptyResponse tests the specific case where
// the server closes the connection immediately after headers (no body).
// This reproduces the bug seen with Playwright where the proxy gets stuck in a loop.
func TestChunkedResponseExitsOnEOFWithEmptyResponse(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := &HTTP{Logger: logger}

	// Create mock connections
	clientConn := &mockConn{}
	// destConn simulates a server that immediately returns EOF (connection closed)
	destConn := &mockConn{
		data: nil, // No data, just EOF
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var finalResp []byte

	// This should complete quickly when receiving EOF, not loop forever
	done := make(chan error, 1)
	go func() {
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn)
	}()

	select {
	case err := <-done:
		// We expect it to return (possibly with an error, but it should return)
		t.Logf("chunkedResponse returned: %v (this is expected behavior)", err)
	case <-time.After(1 * time.Second):
		t.Fatal("BUG REPRODUCED: chunkedResponse did not exit within 1 second - " +
			"the 'break' inside select only exits select, not the for loop!")
	}
}

// TestChunkedResponseMultipleEOFReads verifies that we don't get stuck
// reading EOF repeatedly in the loop
func TestChunkedResponseMultipleEOFReads(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := &HTTP{Logger: logger}

	clientConn := &mockConn{}
	destConn := &mockConn{
		data:     nil,
		maxReads: 100, // Track how many reads happen
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var finalResp []byte

	done := make(chan error, 1)
	go func() {
		done <- h.chunkedResponse(ctx, &finalResp, clientConn, destConn)
	}()

	select {
	case <-done:
		// Check that we didn't do excessive reads
		if destConn.readCount > 10 {
			t.Errorf("Too many reads from destConn: %d (expected < 10). "+
				"This suggests the loop is not exiting properly on EOF", destConn.readCount)
		} else {
			t.Logf("Read count: %d (acceptable)", destConn.readCount)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("BUG: chunkedResponse stuck in loop after %d reads", destConn.readCount)
	}
}
