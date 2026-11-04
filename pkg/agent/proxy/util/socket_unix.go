//go:build linux || darwin

package util

import (
	"net"
	"syscall"
)

// socketOf is the socket under conn, looked for through wrappers that give
// the connection beneath (NetConn), at most 8 deep so a cycle cannot hang,
// with the connection found to give it: the first of conn's chain (conn
// itself, then each NetConn) that is a syscall.Conn whose SyscallConn gives
// one. A wrapper whose SyscallConn fails (one that holds no socket of its
// own) is looked under, as one that is no syscall.Conn is. Its own deadlines
// are the socket's, where a wrapper above it may make them no-ops.
func socketOf(conn net.Conn) (net.Conn, syscall.RawConn, bool) {
	for i := 0; ; i++ {
		if sc, ok := conn.(syscall.Conn); ok {
			if rc, err := sc.SyscallConn(); err == nil {
				return conn, rc, true
			}
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
