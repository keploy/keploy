package supervisor

import (
	"errors"
	"syscall"
	"testing"
)

// Both OSes' codes, on every OS: the errno itself up to 255 off Windows; on
// Windows the reset and pipe classes of the socket's errors, given back as
// Go's syscall.ECONNRESET and syscall.EPIPE, and nothing else.
func TestTheClientsEndErrorIsKeptAsEachOSCanKeepIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		errno syscall.Errno
		code  uint32
		back  error
	}{
		{0, 0, nil},
		{1, 1, syscall.Errno(1)},
		{255, 255, syscall.Errno(255)},
		{256, 0, nil},
		{winWSAEConnReset, 0, nil},
	} {
		code := errnoCode(c.errno)
		if code != c.code {
			t.Errorf("off Windows, errno %d is kept as %d; want %d", c.errno, code, c.code)
		}
		if back := errnoError(code); !errors.Is(back, c.back) || (back == nil) != (c.back == nil) {
			t.Errorf("off Windows, errno %d gives back %v; want %v", c.errno, back, c.back)
		}
	}
	for _, c := range []struct {
		name  string
		errno syscall.Errno
		back  error
	}{
		{"WSAECONNRESET", winWSAEConnReset, syscall.ECONNRESET},
		{"WSAECONNABORTED", winWSAEConnAborted, syscall.ECONNRESET},
		{"ERROR_NETNAME_DELETED", winErrorNetnameDeleted, syscall.ECONNRESET},
		{"ERROR_CONNECTION_ABORTED", winErrorConnectionAborted, syscall.ECONNRESET},
		{"Go's ECONNRESET", syscall.ECONNRESET, syscall.ECONNRESET},
		{"WSAESHUTDOWN", winWSAEShutdown, syscall.EPIPE},
		{"ERROR_BROKEN_PIPE", winErrorBrokenPipe, syscall.EPIPE},
		{"ERROR_NO_DATA", winErrorNoData, syscall.EPIPE},
		{"Go's EPIPE", syscall.EPIPE, syscall.EPIPE},
		{"no errno", 0, nil},
		{"WSAETIMEDOUT", syscall.Errno(10060), nil},
		{"WSAENETRESET", syscall.Errno(10052), nil},
		{"ERROR_ACCESS_DENIED", syscall.Errno(5), nil},
		{"Go's ETIMEDOUT", syscall.ETIMEDOUT, nil},
	} {
		code := winsockCode(c.errno)
		if (code == 0) != (c.back == nil) || code > 255 {
			t.Errorf("on Windows, %s is kept as %d", c.name, code)
		}
		if back := winsockError(code); back != c.back {
			t.Errorf("on Windows, %s gives back %v; want %v", c.name, back, c.back)
		}
	}
	if winsockReset == winsockPipe {
		t.Fatal("Windows keeps a reset and a pipe error as one code")
	}
	if back := winsockError(255); back != nil {
		t.Errorf("on Windows, a code winsockCode never keeps gives back %v; want nil", back)
	}
}
