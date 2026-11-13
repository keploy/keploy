package routes

import "net/http"

// CallerPID is the process that sent r: the owner of the client end of r's
// TCP connection, read from the kernel on Linux and macOS, never from what the
// caller says. It is an error on other platforms, when the connection's
// addresses are unknown, and when no one process holds that socket. It is how
// keploy's agent tells which test process claims an instance or begins a
// test; a server that answers the agent's routes in its place (such as a
// conformance check of a test-framework integration) reads callers the same
// way.
func CallerPID(r *http.Request) (int, error) {
	return callerPID(r)
}
