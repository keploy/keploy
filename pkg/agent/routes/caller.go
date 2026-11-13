package routes

import "net/http"

// CallerPID is the process that sent r: the ID, in the reader's PID namespace,
// of the process that holds the client end of r's TCP connection, read from
// the kernel on Linux and macOS, never from what the caller says. It is how
// keploy's agent tells which test process claims an instance or begins a
// test; a server that answers the agent's routes in its place (such as a
// conformance check of a test-framework integration) reads callers the same
// way.
//
// It is an error:
//   - on platforms other than Linux and macOS;
//   - when the connection's addresses are unknown;
//   - when the socket is not in the kernel's tables the reader sees: the
//     caller is in another network namespace, or the connection is not TCP;
//   - when the reader cannot see the process that holds the socket: another
//     user's process, on Linux and macOS;
//   - when no one process holds the socket: processes share it and none of
//     them is the one every other descends from (a socket passed between
//     processes, or kept by two children whose parent closed it). A child
//     between its fork and its exec is not such a case: its parent, which
//     every other holder descends from, is the caller.
func CallerPID(r *http.Request) (int, error) {
	return callerPID(r)
}
