package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// peerEndProcRoot is where peerEndMarker reads the client's process: the
// agent's own /proc, where the eBPF redirect named it (SrcNsPid). A var for
// the rows.
var peerEndProcRoot = "/proc"

// peerEndMarker marks, for a recorded connection, how each side ended its side
// (Session.MarkPeerEndedStopping), and whether the connection's first end is
// the application's stop, which records no fault (Session.EndFollowedStop),
// rather than the application's giving up or the upstream's fault:
//   - any end read once the recording's stop began (Proxy.StopBegun): keploy
//     record and keploy mock record say so before they stop the application;
//   - the client's end when the client's process (clientPid, in the agent's
//     PID namespace) is exiting or gone: its exit closed the socket, a stop of
//     keploy's or the command's own exit alike. The kernel flags a process
//     exiting (PF_EXITING) before it closes the process's files, so an exit's
//     close is never read while the process still shows as running. A client
//     keploy cannot read (clientPid 0, a process outside the agent's PID
//     namespace) is taken as one: a call it gave up on is not recorded as a
//     fault the run never showed. The client's end while its process runs on
//     is its own giving up on the call.
func (p *Proxy) peerEndMarker(sess *supervisor.Session, clientPid uint32) func(fakeconn.Direction, bool) {
	return func(dir fakeconn.Direction, reset bool) {
		stopped := p.StopBegun()
		if !stopped && dir == fakeconn.FromClient {
			stopped = learn.ProcExiting(peerEndProcRoot, clientPid)
		}
		sess.MarkPeerEndedStopping(dir, reset, stopped)
	}
}
