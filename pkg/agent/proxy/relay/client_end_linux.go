package relay

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// pendingError is the error the proxy's end of the client connection holds
// (SO_ERROR), which the kernel clears as it gives it (client_end.go): nil when
// it holds none, or the socket cannot say (keploy closed it under the relay).
func (s *tcpClientSocket) pendingError() error {
	soErr := 0 // and 0 where either call fails
	_ = s.rc.Control(func(fd uintptr) {
		soErr, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_ERROR)
	})
	if soErr == 0 {
		return nil
	}
	return syscall.Errno(soErr)
}
