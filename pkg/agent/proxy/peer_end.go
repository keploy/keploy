package proxy

import (
	"net/netip"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
)

// peerEndMarker marks, for a recorded connection, how each side ended its side
// (Session.MarkPeerEndedStopping), and whether the connection's first end
// records no fault (Session.EndFollowedStop), rather than being the
// application's giving up or the upstream's fault:
//   - any end read once the recording's stop began (Proxy.StopBegun): keploy
//     record and keploy mock record say so before they stop the application;
//   - the client's end when the kernel saw the client's process release its
//     end of the connection (app, its address and port) as it exited on its
//     own: the command's own exit, or a test process's, with the call still
//     open, which is the run's end;
//   - the client's end where the hooks make no record of how the application
//     releases its connections (not an agent.ConnEndInfo: the macOS and
//     Windows hooks): nothing tells its giving up from its process's exit,
//     and a fault is recorded only on what says it was one. A call such an
//     application gives up on records no fault, and is recorded as it was
//     before faults were recorded: one that got no answer is not recorded.
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
		stopped := p.StopBegun()
		if !stopped && dir == fakeconn.FromClient {
			stopped = p.clientEndIsNoFault(app)
		}
		sess.MarkPeerEndedStopping(dir, reset, stopped)
	}
}

// clientEndIsNoFault reports whether the client's end of the connection whose
// application end is app records no fault: the hooks make no record of how
// the application releases its connections (not an agent.ConnEndInfo), or
// the kernel saw it release app as its process exited on its own, ended by no
// signal (agent.AppConnEnd). False where hooks that make such records hold
// none of it.
func (p *Proxy) clientEndIsNoFault(app netip.AddrPort) bool {
	ce, ok := p.DestInfo.(agent.ConnEndInfo)
	if !ok {
		return true
	}
	end, ok := ce.TakeAppConnEnd(app)
	return ok && end.Exiting && end.Signal == 0
}
