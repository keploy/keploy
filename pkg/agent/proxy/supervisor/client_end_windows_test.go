//go:build windows

package supervisor

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The numbers client_end_codes.go gives the Windows errors are Windows' own.
func TestTheWindowsSocketErrorsAreNumberedAsWindowsNumbersThem(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		ours, its syscall.Errno
	}{
		{"ERROR_NETNAME_DELETED", winErrorNetnameDeleted, windows.ERROR_NETNAME_DELETED},
		{"ERROR_BROKEN_PIPE", winErrorBrokenPipe, windows.ERROR_BROKEN_PIPE},
		{"ERROR_NO_DATA", winErrorNoData, windows.ERROR_NO_DATA},
		{"ERROR_CONNECTION_ABORTED", winErrorConnectionAborted, windows.ERROR_CONNECTION_ABORTED},
		{"WSAECONNABORTED", winWSAEConnAborted, windows.WSAECONNABORTED},
		{"WSAECONNRESET", winWSAEConnReset, windows.WSAECONNRESET},
		{"WSAESHUTDOWN", winWSAEShutdown, windows.WSAESHUTDOWN},
	} {
		if c.ours != c.its {
			t.Errorf("%s is %d here; Windows numbers it %d", c.name, c.ours, c.its)
		}
	}
}

// resetPair is a loopback connection whose far end was closed with a reset
// (SO_LINGER 0): the near end is what the relay holds of a client that reset.
func resetPair(t *testing.T) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	near, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = near.Close() })
	far, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if err := far.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := far.Close(); err != nil {
		t.Fatal(err)
	}
	return near
}

// The errors a Windows socket gives for a client that reset the connection,
// and for a write past the end of one keploy shut down, are kept as Go's
// ECONNRESET and EPIPE: what a parser asks of the session.
func TestOnWindowsTheClientSocketsResetIsKeptAsECONNRESET(t *testing.T) {
	t.Parallel()
	t.Run("a write to a client that reset", func(t *testing.T) {
		t.Parallel()
		near := resetPair(t)
		deadline := time.Now().Add(5 * time.Second)
		var werr error
		for werr == nil && time.Now().Before(deadline) {
			_, werr = near.Write([]byte("an answer"))
			time.Sleep(10 * time.Millisecond)
		}
		if werr == nil {
			t.Fatal("no write to the reset connection failed in 5s")
		}
		s := &Session{}
		s.MarkClientEndError(werr)
		if err := s.ClientEndError(); !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("the write failed with %v (%#v); the session keeps %v, want ECONNRESET", werr, werr, err)
		}
	})
	t.Run("a read of a client that reset", func(t *testing.T) {
		t.Parallel()
		near := resetPair(t)
		_ = near.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, rerr := near.Read(make([]byte, 16))
		var ne net.Error
		if rerr == nil || (errors.As(rerr, &ne) && ne.Timeout()) {
			t.Fatalf("the read of the reset connection gave %v; want its reset", rerr)
		}
		s := &Session{}
		s.MarkClientEndError(rerr)
		if err := s.ClientEndError(); !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("the read failed with %v (%#v); the session keeps %v, want ECONNRESET", rerr, rerr, err)
		}
	})
	t.Run("a write after keploy shut its sending side", func(t *testing.T) {
		t.Parallel()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		near, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = near.Close() })
		far, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = far.Close() })
		if err := near.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		_, werr := near.Write([]byte("an answer"))
		if werr == nil {
			t.Fatal("a write after CloseWrite succeeded")
		}
		s := &Session{}
		s.MarkClientEndError(werr)
		if err := s.ClientEndError(); !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("the write failed with %v (%#v); the session keeps %v, want EPIPE", werr, werr, err)
		}
	})
	t.Run("an error no reset says nothing", func(t *testing.T) {
		t.Parallel()
		s := &Session{}
		s.MarkClientEndError(windows.WSAETIMEDOUT)
		s.MarkClientEndError(syscall.Errno(255))
		if err := s.ClientEndError(); err != nil {
			t.Fatalf("after WSAETIMEDOUT and errno 255: %v, want nil", err)
		}
		s.MarkClientEndError(windows.WSAECONNABORTED)
		if err := s.ClientEndError(); !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("after WSAECONNABORTED: %v, want ECONNRESET", err)
		}
	})
}
