package proxy

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// opaqueNetErr carries a real errno behind a message that matches none of the
// substrings isShutdownError greps for. This is the Windows shape: the errno is
// there, the text is not the Linux text.
type opaqueNetErr struct{ err error }

func (o opaqueNetErr) Error() string { return "connection ended" }
func (o opaqueNetErr) Unwrap() error { return o.err }

func TestIsShutdownError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context canceled", context.Canceled, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"use of closed network connection", errors.New("use of closed network connection"), true},
		{"connection reset by peer text", errors.New("read tcp: connection reset by peer"), true},
		{"wsarecv text", errors.New("read tcp: wsarecv: An existing connection was forcibly closed by the remote host."), true},

		// The errno is present but nothing in the message is. Without the
		// errno classification this returns false and the shutdown is logged
		// at Error level as if it were a real failure.
		{"reset errno, opaque message", opaqueNetErr{&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, true},
		{"aborted errno, opaque message", opaqueNetErr{&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNABORTED)}}, true},

		{"unrelated", errors.New("malformed frame header"), false},
		{"refused is not a shutdown", opaqueNetErr{&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}, false},
	}

	for _, c := range cases {
		if got := isShutdownError(c.err); got != c.want {
			t.Errorf("isShutdownError(%s) = %v, want %v (err=%v)", c.name, got, c.want, c.err)
		}
	}
}
