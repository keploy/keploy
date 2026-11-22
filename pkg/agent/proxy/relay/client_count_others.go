//go:build !darwin && !windows

package relay

import "net"

// countFromAccept does nothing: only macOS and Windows count the bytes written
// to a client's socket (client_count_accept.go). Linux's socket counts the
// bytes its client acknowledged and those its send buffer holds itself
// (tcpi_bytes_acked, SIOCOUTQ), and elsewhere no client socket is asked.
func countFromAccept(net.Conn, clientSocket) {}

// CountClientWrites is c: only macOS and Windows count the bytes written to
// an accepted client's socket (client_count_accept.go).
func CountClientWrites(c net.Conn) net.Conn { return c }

// AcceptedConn is c: only macOS and Windows put a count over an accepted
// client's socket (CountClientWrites).
func AcceptedConn(c net.Conn) net.Conn { return c }
