//go:build linux

package relay

// unresetError is 0 on Linux: the kernel keeps every reset's error on the
// socket (tcp_reset sets sk_err in every state but CLOSE), so SO_ERROR says
// each one (client_end_unix.go).
func (*tcpClientSocket) unresetError(uintptr, bool) int { return 0 }

// endGiven is nothing on Linux: SO_ERROR alone gives the connection's error.
func (*tcpClientSocket) endGiven() {}
