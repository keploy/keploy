//go:build linux || darwin

package relay

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// pendingError is the error the proxy's end of the client connection holds
// (SO_ERROR), which the kernel clears as it gives it (client_end.go), or, when
// it holds none, the reset the kernel closed it with and kept no error for
// (unresetError: macOS's, after the client's FIN, finRead or found by a
// look): nil when it holds neither, or the socket cannot say (keploy closed
// it under the relay). Each is given once: once SO_ERROR has given the
// connection's error, the connection closed on it, and unresetError gives
// nothing more (endGiven).
func (s *tcpClientSocket) pendingError(finRead bool) error {
	soErr := 0
	_ = s.rc.Control(func(fd uintptr) {
		v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_ERROR)
		switch {
		case err != nil: // the socket cannot say
		case v != 0:
			soErr = v
			s.endGiven()
		default:
			soErr = s.unresetError(fd, finRead)
		}
	})
	if soErr == 0 {
		return nil
	}
	return syscall.Errno(soErr)
}
