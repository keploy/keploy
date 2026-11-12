package routes

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
)

// CallerPID is the process that holds the client end of r's TCP connection,
// read from the kernel on Linux, macOS and Windows, never from what the
// caller says: on Linux its ID in the PID namespace of the /proc it reads
// (the reader's own, unless /proc was mounted from another). It is how
// keploy's agent tells which test process claims an instance or begins a
// test; a server that answers the agent's routes in its place (such as a
// conformance check of a test-framework integration) reads callers the same
// way.
//
// The holder is not always the sender. When a process and its descendants
// hold the socket, it is the ancestor, whichever of them wrote (a child
// between its fork and its exec, or one given the socket, holds a copy). A
// proxy in the path holds the connection, so the proxy is named, not the
// process that sent through it. A caller that closed its write side
// (CloseWrite) after its request, and keeps the connection, still holds it
// and is named. On Windows the holder is the process the kernel's TCP table
// names as the socket's owner (GetExtendedTcpTable): the one that opened it,
// whichever process it was later given to.
//
// It is an error:
//   - on platforms other than Linux, macOS and Windows;
//   - when the connection's addresses are unknown: r came over no network
//     connection, or over a unix socket;
//   - when the socket is not in the kernel's tables the reader sees: the
//     caller is in another network namespace, the connection is not TCP, or
//     no process holds it any more (the caller closed it or exited);
//   - when the reader cannot see the process that holds the socket: another
//     user's process; a process that is not dumpable (setuid, or one that set
//     PR_SET_DUMPABLE to 0), whose open files only a reader allowed to trace
//     it can list; or a process in another PID namespace that shares the
//     reader's network namespace (a Kubernetes sidecar without
//     shareProcessNamespace, or a container started with --network
//     container:X and its own PID namespace). On Linux the error then says no
//     process holds the socket;
//   - when no one process holds the socket: processes share it and none of
//     them is the one every other descends from (a socket passed to a process
//     outside the holder's descendants, or kept by two children whose parent
//     closed it).
func CallerPID(r *http.Request) (int, error) {
	return callerPID(r)
}

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

// sameEndpoint compares two endpoints with IPv4-mapped IPv6 read as IPv4 and
// with no IPv6 zone: a link-local caller's address carries one (fe80::1%eth0)
// while the kernel's socket tables carry none.
func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap().WithZone("") == b.Addr().Unmap().WithZone("")
}
