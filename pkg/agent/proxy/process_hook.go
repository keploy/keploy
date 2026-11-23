package proxy

import "go.keploy.io/server/v3/pkg/provenswitch"

// Process is a process of the run the proxy sees (OnProcess).
type Process struct {
	// Pid is the process's number in the agent's own PID namespace, by which
	// the agent reads it in its own /proc: the eBPF redirect's agentNsPid (the
	// kernel finds it by the namespace's inode the agent registered), or
	// listenerOwner's scan of the agent's /proc. It is the kernel's PID for an
	// agent in the initial PID namespace, another number for one in a
	// container. 0 when the agent cannot name the process: one outside the
	// agent's PID namespace (a sibling container's, the host's), which a build
	// that checks the run's processes then cannot check.
	Pid uint32
	// KernelPid is the process's number in the initial PID namespace, the
	// kernel's (the eBPF redirect's kernelPid); 0 when the proxy did not learn
	// it (a listener's owner, read from the agent's /proc).
	KernelPid uint32
}

// OnProcess, when a build sets it, is told each process of the run the proxy
// sees, under the proven switch: the process of every connection it accepts,
// and the owner of a listener of the run's own process tree that a connection
// is addressed to, whether it is then relayed to it or answered from mocks
// (a dependency the tests started; starts.Default.Dependency tells them
// apart). A build that freezes the run's clock checks there, once per
// process, that each one reads it, and lists those that do not (design v14
// §3.4, detection).
//
// It is told a process the agent cannot name too (Pid 0, KernelPid set), so
// that such a build can say it could not check it rather than pass it.
//
// Set it before the proxy starts: it is read without synchronisation. It is
// called on the connection's own goroutine, so it must return at once and do
// its work elsewhere.
var OnProcess func(Process)

// seeProcess tells OnProcess about p; one with no number at all (a
// connection the kernel did not attribute) is no process.
func seeProcess(p Process) {
	if f := OnProcess; f != nil && (p.Pid != 0 || p.KernelPid != 0) && provenswitch.On() {
		f(p)
	}
}
