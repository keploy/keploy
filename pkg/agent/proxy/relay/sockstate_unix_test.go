//go:build linux || darwin

package relay

import "golang.org/x/sys/unix"

// setReceiveBuffer sets the receive buffer of the socket fd to n bytes
// (SO_RCVBUF).
func setReceiveBuffer(fd uintptr, n int) error {
	return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, n)
}

// receiveBufferSizeOf is the receive buffer of the socket fd as it reads it
// back (SO_RCVBUF).
func receiveBufferSizeOf(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
}

// ackedThrough is how many of the written bytes written to the socket fd its
// peer has acknowledged: those less the bytes the socket still holds
// (queuedOf).
func ackedThrough(fd uintptr, written uint64) (uint64, error) {
	queued, err := queuedOf(fd)
	if err != nil {
		return 0, err
	}
	return written - uint64(queued), nil
}

// probeResentOf is (false, false): Linux's and macOS's rows do not ask
// whether a window probe was sent again; their counts do not take one in.
func probeResentOf(uintptr) (resent, known bool) { return false, false }
