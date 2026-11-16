//go:build !darwin

package relay

import "net"

// countWrites is src: only macOS counts the bytes the relay writes to a
// client's socket (client_took_darwin.go). Linux's socket counts the bytes its
// client acknowledged itself (tcpi_bytes_acked), and elsewhere no client
// socket is asked.
func countWrites(src net.Conn, _ clientSocket) net.Conn { return src }
