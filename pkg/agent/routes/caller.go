package routes

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
)

// callerPID is the process that owns the client end of the request's TCP
// connection: the socket whose local address is the request's remote address
// and whose remote address is the server's own. It is read from the kernel's
// socket tables (tcpOwner, one per system), never from what the caller says,
// so a caller that does not carry keploy's shim, such as a hardened binary,
// is identified like any other.
func callerPID(r *http.Request) (int, error) {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return 0, fmt.Errorf("the connection's remote address %q: %w", r.RemoteAddr, err)
	}
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || la == nil {
		return 0, errors.New("the connection's local address is unknown")
	}
	local, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return 0, fmt.Errorf("the connection's local address %q: %w", la.String(), err)
	}
	return tcpOwner(remote, local)
}

// sameEndpoint compares two endpoints with IPv4-mapped IPv6 read as IPv4.
func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}
