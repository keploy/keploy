package proxy

import (
	"net/netip"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
)

// peerEndMarker marks, for a recorded connection, how each side ended its side
// (Session.MarkPeerEndedBy), and whether the connection's first end records no
// fault (Session.EndFollowedStop), and the client's end, first or not, which
// such end it is (Session.ClientEndNoFault), rather than being the
// application's giving up or the upstream's fault:
//   - any end read once the recording's stop began (Proxy.StopBegun): keploy
//     record and keploy mock record say so before they stop the application
//     (supervisor.EndAtStop);
//   - the client's end when the kernel saw the client's process release its
//     end of the connection (app, its address and port) as it exited on its
//     own: the command's own exit, or a test process's, with the call still
//     open, which is the run's end (supervisor.EndAtExit);
//   - the client's end where the hooks make no record of how the application
//     releases its connections (not an agent.ConnEndInfo: the macOS and
//     Windows hooks): nothing tells its giving up from its process's exit,
//     and a fault is recorded only on what says it was one
//     (supervisor.EndUnjudged). A call such an application gives up on
//     records no fault: one that got no answer is not recorded, as before
//     faults were recorded, and one it closed partway through the answer is
//     left out, as nothing tells a whole answer from a cut one.
//
// The client's end while its process ran (it closed the socket: it gave up on
// the call), or as a signal ended it while the run went on, is a fault: a
// signal that killed it, or a stop signal another process sent it that it
// handled and exited from (a JVM's or .NET's default). So is one hooks that
// make such records hold no record of, a client that shut its end down before
// it exited included: nothing says it was the run's end, and a call the
// application never got an answer to is recorded as one it gave up on. The
// upstream's end before the stop is the upstream's, whatever the hooks say.
func (p *Proxy) peerEndMarker(sess *supervisor.Session, app netip.AddrPort) func(fakeconn.Direction, bool) {
	return func(dir fakeconn.Direction, reset bool) {
		why := supervisor.EndMayBeFault
		if p.StopBegun() {
			why = supervisor.EndAtStop
		} else if dir == fakeconn.FromClient {
			why = p.clientEndNoFault(app)
		}
		sess.MarkPeerEndedBy(dir, reset, why)
	}
}

// clientEndNoFault reports which end that records no fault the client's end of
// the connection whose application end is app is: EndUnjudged where the hooks
// make no record of how the application releases its connections (not an
// agent.ConnEndInfo), EndAtExit where the kernel saw it release app as its
// process exited on its own, ended by no signal (agent.AppConnEnd), and
// EndMayBeFault where hooks that make such records hold none of it.
func (p *Proxy) clientEndNoFault(app netip.AddrPort) supervisor.NoFaultEnd {
	ce, ok := p.DestInfo.(agent.ConnEndInfo)
	if !ok {
		return supervisor.EndUnjudged
	}
	if end, ok := ce.TakeAppConnEnd(app); ok && end.Exiting && end.Signal == 0 {
		return supervisor.EndAtExit
	}
	return supervisor.EndMayBeFault
}
