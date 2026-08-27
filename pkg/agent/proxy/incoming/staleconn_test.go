package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// isStaleConnError decides whether an idempotent request is redialed and
// replayed or turned into a 502 for the caller's application. Nothing covered
// it before, which is how it went unnoticed that on Windows it answered false
// for every real connection reset.
func TestIsStaleConnError(t *testing.T) {
	timeoutErr := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if !timeoutErr.Timeout() {
		t.Fatalf("harness: expected a timeout error, got %v", timeoutErr)
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"wrapped eof", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, true},
		{"net.ErrClosed", &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}, true},
		{"conn reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{"broken pipe", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, true},
		{"conn aborted", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNABORTED)}, true},

		// The exclusions are the load-bearing half. A timeout means the
		// upstream may still be processing the request, so replaying would
		// double-charge it; a parse failure is deterministic and replay would
		// only re-trigger it.
		{"timeout", timeoutErr, false},
		{"conn refused", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, false},
		{"protocol parse error", errors.New("malformed HTTP response \"garbage\""), false},
	}

	for _, c := range cases {
		if got := isStaleConnError(c.err); got != c.want {
			t.Errorf("isStaleConnError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// The table above builds errno values by hand, which is exactly the kind of
// test that passed on Windows while production failed: a hand-made
// syscall.ECONNRESET, EPIPE or ECONNABORTED matches itself on every platform,
// but Winsock produces none of those values. Note in particular that the
// "conn aborted" row asserts behaviour no Windows error can reach — Winsock's
// 10053 is deliberately unmapped (see pkg/neterr) — so that row proves the
// POSIX contract and nothing about this platform. This drives a real
// connection instead.
func TestIsStaleConnError_RealPeerReset(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	served := make(chan struct{})
	go func() {
		defer close(served)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // abortive close: RST, not FIN
		}
		_ = c.Close()
	}()

	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-served

	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 512)
	var readErr error
	for {
		if _, readErr = c.Read(buf); readErr != nil {
			break
		}
	}

	if errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Fatalf("harness did not produce a reset: read deadline expired. err=%v", readErr)
	}
	if errors.Is(readErr, io.EOF) {
		// A graceful close is also replayable, so this is not a failure of
		// the predicate — but it means the reset path went untested.
		t.Fatalf("harness did not produce a reset: peer closed gracefully. err=%v", readErr)
	}
	if !isStaleConnError(readErr) {
		var errno syscall.Errno
		errors.As(readErr, &errno)
		t.Fatalf("isStaleConnError(%v) = false for a real peer reset; unwrapped errno = %d. "+
			"An idempotent request that hits this gets a 502 instead of a redial and replay.",
			readErr, uintptr(errno))
	}
}
