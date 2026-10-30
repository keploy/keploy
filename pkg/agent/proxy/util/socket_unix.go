//go:build linux || darwin

package util

import (
	"net"
	"syscall"
)

// rawSocket is the socket under conn, looked for through wrappers that give
// the connection beneath (NetConn), at most 8 deep so a cycle cannot hang.
func rawSocket(conn net.Conn) (syscall.RawConn, bool) {
	_, rc, ok := socketOf(conn)
	return rc, ok
}

// socketOf is rawSocket, with the connection found to give it: the first of
// conn's chain (conn itself, then each NetConn) that is a syscall.Conn. Its
// own deadlines are the socket's, where a wrapper above it may make them
// no-ops.
func socketOf(conn net.Conn) (net.Conn, syscall.RawConn, bool) {
	for i := 0; ; i++ {
		if sc, ok := conn.(syscall.Conn); ok {
			rc, err := sc.SyscallConn()
			return conn, rc, err == nil
		}
		if i == 8 || conn == nil {
			return nil, nil, false
		}
		u, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			return nil, nil, false
		}
		conn = u.NetConn()
	}
}
