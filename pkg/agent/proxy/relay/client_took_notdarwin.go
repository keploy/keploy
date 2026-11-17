//go:build !darwin

package relay

import "net"

// countFromAccept does nothing: only macOS counts the bytes written to a
// client's socket (client_took_darwin.go). Linux's socket counts the bytes its
// client acknowledged itself (tcpi_bytes_acked), and elsewhere no client
// socket is asked.
func countFromAccept(net.Conn, clientSocket) {}

// CountClientWrites is c: only macOS counts the bytes written to an accepted
// client's socket (client_took_darwin.go).
func CountClientWrites(c net.Conn) net.Conn { return c }

// AcceptedConn is c: only macOS puts a count over an accepted client's
// socket (CountClientWrites).
func AcceptedConn(c net.Conn) net.Conn { return c }
