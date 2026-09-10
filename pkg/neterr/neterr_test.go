package neterr

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dialAndGetReset drives a real TCP connection whose peer answers with RST,
// and returns the error the read surfaced. Everything here is deliberately
// real: the whole point of this package is that synthetic syscall.Errno values
// do not tell you what the platform actually produces.
func dialAndGetReset(t *testing.T) error {
	t.Helper()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// Read what the client sent so the RST is a response to a complete
		// exchange rather than to unread data.
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
		if tc, ok := c.(*net.TCPConn); ok {
			// linger 0 makes Close send RST instead of FIN. This is the whole
			// experiment; without it the peer closes gracefully and the client
			// sees io.EOF, which is a different classification.
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	}()

	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-accepted

	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := c.Read(buf); err != nil {
			return err
		}
	}
}

func TestIsConnReset_RealPeerReset(t *testing.T) {
	err := dialAndGetReset(t)

	// Distinguish "the experiment did not run" from "classification is
	// broken". A graceful FIN surfaces as io.EOF and a lost RST as a deadline
	// expiry; neither is a classification failure, and reporting them as one
	// would send the next reader hunting for an errno bug that is not there.
	if err == nil {
		t.Fatal("read succeeded against a peer that reset the connection")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("harness did not produce a reset: peer closed gracefully, so SetLinger(0) did not take effect. err=%v", err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("harness did not produce a reset: the read deadline expired with no error from the peer. err=%v", err)
	}

	if !IsConnReset(err) {
		var errno syscall.Errno
		errors.As(err, &errno)
		t.Fatalf("IsConnReset(%v) = false; unwrapped errno = %d. "+
			"On Windows a real reset is WSAECONNRESET (10054) while syscall.ECONNRESET "+
			"is an APPLICATION_ERROR-space value, so comparing against the portable "+
			"constant alone is inert - that is the bug this package exists to prevent.",
			err, uintptr(errno))
	}
}

func TestIsConnRefused_RealDeadPort(t *testing.T) {
	// Bind and immediately release, so the port is known to have no listener.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Retry rather than skip. This is the only test of IsConnRefused
	// anywhere, so a skip would let the Windows job go green having proven
	// nothing at all.
	var dialErr error
	for attempt := 0; attempt < 5; attempt++ {
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			dialErr = err
			break
		}
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr == nil {
		t.Fatalf("nothing refused a connection to %s across 5 attempts; the harness could not produce the error under test", addr)
	}
	err = dialErr

	if !IsConnRefused(err) {
		var errno syscall.Errno
		errors.As(err, &errno)
		t.Fatalf("IsConnRefused(%v) = false; unwrapped errno = %d. "+
			"On Windows a refused connect is WSAECONNREFUSED (10061), not syscall.ECONNREFUSED.",
			err, uintptr(errno))
	}
	if IsConnReset(err) {
		t.Errorf("IsConnReset(%v) = true for a refused connect; the classes must not overlap", err)
	}
}

func TestNilAndUnrelatedErrors(t *testing.T) {
	for _, fn := range []struct {
		name string
		f    func(error) bool
	}{
		{"IsConnReset", IsConnReset},
		{"IsConnAborted", IsConnAborted},
		{"IsBrokenPipe", IsBrokenPipe},
		{"IsConnRefused", IsConnRefused},
	} {
		if fn.f(nil) {
			t.Errorf("%s(nil) = true, want false", fn.name)
		}
		if fn.f(errors.New("something else entirely")) {
			t.Errorf("%s(unrelated) = true, want false", fn.name)
		}
		if fn.f(io.EOF) {
			t.Errorf("%s(io.EOF) = true, want false; EOF is a graceful close, not a reset", fn.name)
		}
	}
}

// TestPortableConstantsStillMatch guards the portable branch on every platform.
// Note what it does NOT prove: a value built from syscall.ECONNRESET compares
// equal to itself everywhere, Windows included, so this passing on Windows says
// nothing about real sockets. That is the whole reason the tests above drive an
// actual connection.
func TestPortableConstantsStillMatch(t *testing.T) {
	wrapped := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	got := IsConnReset(wrapped)
	if !got {
		t.Fatalf("IsConnReset(wrapped syscall.ECONNRESET) = false; the portable branch regressed")
	}
	if !strings.Contains(wrapped.Error(), "read") {
		t.Fatalf("unexpected error rendering: %v", wrapped)
	}
}
