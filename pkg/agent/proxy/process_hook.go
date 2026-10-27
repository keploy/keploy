package proxy

import "go.keploy.io/server/v3/pkg/provenswitch"

// OnProcess, when a build sets it, is told each process of the run the proxy
// sees, under the proven switch: the process of every connection it accepts,
// and the owner of a listener of the run's own process tree that a connection
// is addressed to, whether it is then relayed to it or answered from mocks
// (a dependency the tests started; starts.Default.Dependency tells them
// apart). A build that freezes the run's clock checks there, once per
// process, that each one reads it, and lists those that do not (design v14
// §3.4, detection).
//
// The pid is the process's kernel PID, in the root PID namespace (the eBPF
// redirect's, and listenerOwner's /proc scan). An agent that runs natively
// reads it in its own /proc; an agent in a container sees other numbers, or
// none, there.
//
// Set it before the proxy starts: it is read without synchronisation. It is
// called on the connection's own goroutine, so it must return at once and do
// its work elsewhere.
var OnProcess func(pid uint32)

// seeProcess tells OnProcess about pid; 0 is no process.
func seeProcess(pid uint32) {
	if f := OnProcess; f != nil && pid != 0 && provenswitch.On() {
		f(pid)
	}
}
