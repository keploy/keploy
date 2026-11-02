package proxy

import "context"

// ClientTLSGate is a build's part in the TLS handshakes keploy's proxy makes
// as the server of the application's processes: every MITM handshake
// (handleConnection's, the opportunistic upgrade's, the relay's, a record
// upgrader's, and the one a replayer makes itself in band: MySQL's and
// Postgres's, on an SSLRequest, through the context MockOutgoing is given),
// each told through its clientTLS. p is the client's process: its KernelPid
// as the kernel's initial PID namespace numbers it (the eBPF redirect's,
// models.OutgoingOptions.SrcPid), and its Pid in the agent's own PID
// namespace (SrcNsPid), by which an agent in a container's namespace reads it
// in its /proc; Pid is 0 for a process outside that namespace. The proxy calls
// neither method for a connection whose process it does not know (KernelPid 0).
type ClientTLSGate interface {
	// Ready returns once keploy's proxy may answer the process's handshake,
	// its certificate next: a build that must prepare the process first
	// (keploy's TLS pin bypass, which has to be in place before the client
	// checks keploy's certificate) does so here, and returns when that is
	// done, when it cannot be done, or when ctx ends.
	//
	// realOrPassthrough is the destinations the run reaches live for this
	// process (realOrPassthroughLabels): its mock.real entries, its session's
	// bypassRules, the replay options that relay calls to their real servers,
	// or a global pass-through marker; peers whose real certificates the
	// process still checks itself. It is empty when the run reaches only
	// keploy's own mocks. A gate that readies the process by
	// turning its certificate checks off (keploy's TLS pin bypass) must not do
	// so when this is non-empty — the same process-wide switch would stop the
	// process verifying those real servers too — and should leave the process's
	// TLS handshakes unprepared, so a pinned one is charged as a capture loss
	// with a Why that names the destination, not silently mocked with
	// verification off.
	Ready(ctx context.Context, p Process, realOrPassthrough []string)
	// Why is what the build knows of why a handshake of the process failed
	// (the pin bypass could not attach to it, and why), for the capture loss
	// the failure is charged as; "" when it knows nothing.
	Why(p Process) string
}

// OnClientTLS, when a build sets it, is consulted on each TLS handshake
// keploy's proxy makes with a process it knows (ClientTLSGate): Ready before
// the proxy answers the client, Why when the handshake is charged as a
// capture loss. Set it before the proxy starts: it is read without
// synchronisation, as OnProcess is.
var OnClientTLS ClientTLSGate
