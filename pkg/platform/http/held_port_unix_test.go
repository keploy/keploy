//go:build unix

package http

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// heldLoopbackPort binds a TCP port on 127.0.0.1 and does not listen on it:
// a connection to it is refused, as one to a port nothing listens on is. The
// port stays this test's all along: it is bound without SO_REUSEADDR, so no
// other socket on the host can bind it, not even a listener (Go's set it),
// until the test ends. listen makes that same socket listen and returns it
// as a listener, so the port is never released between the refusals and the
// answers, for anything else on a busy host to take.
func heldLoopbackPort(t *testing.T) (port int, listen func() (net.Listener, error)) {
	t.Helper()
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "held loopback port")
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return sa.(*syscall.SockaddrInet4).Port, func() (net.Listener, error) {
		if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
			return nil, os.NewSyscallError("listen", err)
		}
		// A listener on a copy of the socket: f's close at the test's end
		// leaves it listening until the app's server closes it.
		return net.FileListener(f)
	}
}
