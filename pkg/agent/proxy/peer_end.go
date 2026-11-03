package proxy

import (
	"net/netip"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
)

// peerEndMarker marks, for a recorded connection, how each side ended its side
// (Session.MarkPeerEndedStopping), and whether the connection's first end is
// the application's stop, which records no fault (Session.EndFollowedStop),
// rather than the application's giving up or the upstream's fault:
//   - any end read once the recording's stop began (Proxy.StopBegun): keploy
//     record and keploy mock record say so before they stop the application;
//   - the client's end when the kernel saw the client's process release its
//     end of the connection (app, its address and port) as it exited on its
//     own (appEndedByItsExit): the command's own exit, or a test process's,
//     with the call still open, which is the run's end.
//
// The client's end while its process ran (it closed the socket: it gave up on
// the call), or as a signal ended it while the run went on, is a fault: a
// signal that killed it, or a stop signal another process sent it that it
// handled and exited from (a JVM's or .NET's default). So is one the hooks
// hold no record of (agent.ConnEndInfo), a client that shut its end down
// before it exited included: nothing says it was the run's end, and a call
// the application never got an answer to is recorded as one it gave up on.
func (p *Proxy) peerEndMarker(sess *supervisor.Session, app netip.AddrPort) func(fakeconn.Direction, bool) {
	return func(dir fakeconn.Direction, reset bool) {
		stopped := p.StopBegun()
		if !stopped && dir == fakeconn.FromClient {
			stopped = p.appEndedByItsExit(app)
		}
		sess.MarkPeerEndedStopping(dir, reset, stopped)
	}
}

// appEndedByItsExit reports whether the kernel saw the application release its
// end app of a connection as its process exited on its own, ended by no
// signal (agent.AppConnEnd); false where the hooks hold no record of it.
func (p *Proxy) appEndedByItsExit(app netip.AddrPort) bool {
	ce, ok := p.DestInfo.(agent.ConnEndInfo)
	if !ok {
		return false
	}
	end, ok := ce.TakeAppConnEnd(app)
	return ok && end.Exiting && end.Signal == 0
}
