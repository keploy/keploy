package tls

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// A client that hung up -- closed or reset its connection -- gave the
// handshake up, however the handshake wraps it; every other failure (an
// alert, a record keploy could not read, a hello it refused) is not one.
func TestIsClientGiveUp(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{io.EOF, true},
		{fmt.Errorf("handshake: %w", io.ErrUnexpectedEOF), true},
		{&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{&net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)}, true},
		{net.ErrClosed, true},
		{&net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}, false},
		{&net.OpError{Op: "remote error", Err: errors.New("tls: user canceled")}, false},
		{&net.OpError{Op: "local error", Err: errors.New("tls: bad record MAC")}, false},
		{errors.New("tls: client offered only unsupported versions: [301]"), false},
	} {
		if got := IsClientGiveUp(tc.err); got != tc.want {
			t.Errorf("IsClientGiveUp(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
