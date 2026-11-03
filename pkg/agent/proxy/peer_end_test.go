package proxy

import (
	"net/netip"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
)

// fakeConnEnds is a DestInfo whose hooks say how the application released
// its connections (agent.ConnEndInfo), each record taken once, as the
// hooks' app_conn_ends entry is deleted as it is read.
type fakeConnEnds struct {
	fakeDestInfo
	ends map[netip.AddrPort]agent.AppConnEnd
}

func (f *fakeConnEnds) TakeAppConnEnd(app netip.AddrPort) (agent.AppConnEnd, bool) {
	e, ok := f.ends[app]
	delete(f.ends, app)
	return e, ok
}

// The connection's first end records no fault (Session.EndFollowedStop) when
// the recording's stop had begun, when it is the client's end and the kernel
// saw the client's process release that connection as it exited on its own,
// or when it is the client's end and the hooks make no record of how the
// application releases its connections (not an agent.ConnEndInfo: the macOS
// and Windows hooks), so nothing tells its giving up from its process's exit.
// The client's end while its process ran on (it gave up), as a signal killed
// it, or with no record of this connection's release from hooks that make
// such records is a fault; and the upstream's end before the stop is the
// upstream's, however the client's process ended and whatever the hooks can
// say.
func TestPeerEndMarkerTellsTheApplicationsStopFromItsGivingUp(t *testing.T) {
	app := netip.MustParseAddrPort("10.0.0.7:41000")
	other := netip.MustParseAddrPort("10.0.0.7:41001")
	running := agent.AppConnEnd{}
	ownExit := agent.AppConnEnd{Exiting: true}
	killed := agent.AppConnEnd{Exiting: true, Signal: 9}
	for _, tc := range []struct {
		name      string
		stopBegun bool
		noEnds    bool // hooks that say nothing of the release (not an agent.ConnEndInfo)
		ends      map[netip.AddrPort]agent.AppConnEnd
		first     fakeconn.Direction
		wantStop  bool
	}{
		{"the client gives up while its process runs on", false, false, map[netip.AddrPort]agent.AppConnEnd{app: running}, fakeconn.FromClient, false},
		{"the client's process exits on its own with the call open", false, false, map[netip.AddrPort]agent.AppConnEnd{app: ownExit}, fakeconn.FromClient, true},
		{"the client's process is killed by a signal while the run goes on", false, false, map[netip.AddrPort]agent.AppConnEnd{app: killed}, fakeconn.FromClient, false},
		{"the client's process is stopped by SIGTERM", false, false, map[netip.AddrPort]agent.AppConnEnd{app: {Exiting: true, Signal: 15}}, fakeconn.FromClient, false},
		{"the hooks hold no record of the client's release", false, false, nil, fakeconn.FromClient, false},
		{"the hooks hold only another connection's release", false, false, map[netip.AddrPort]agent.AppConnEnd{other: ownExit}, fakeconn.FromClient, false},
		{"the hooks say nothing of releases: the client's end", false, true, nil, fakeconn.FromClient, true},
		{"the hooks say nothing of releases: the upstream ends first", false, true, nil, fakeconn.FromDest, false},
		{"the hooks say nothing of releases: the upstream's end after the stop began", true, true, nil, fakeconn.FromDest, true},
		{"the client's end after the stop began", true, false, map[netip.AddrPort]agent.AppConnEnd{app: running}, fakeconn.FromClient, true},
		{"the upstream's end after the stop began", true, false, nil, fakeconn.FromDest, true},
		{"the upstream ends first, the client's process exiting on its own", false, false, map[netip.AddrPort]agent.AppConnEnd{app: ownExit}, fakeconn.FromDest, false},
		{"the upstream ends first, no record", false, false, nil, fakeconn.FromDest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ends := &fakeConnEnds{ends: tc.ends}
			p := &Proxy{DestInfo: ends}
			if tc.noEnds {
				p.DestInfo = fakeDestInfo{}
			}
			if tc.stopBegun {
				p.NoteStopBegun()
			}
			sess := &supervisor.Session{}
			mark := p.peerEndMarker(sess, app)
			mark(tc.first, false)
			mark(1-tc.first, false)
			if got := sess.EndFollowedStop(); got != tc.wantStop {
				t.Fatalf("EndFollowedStop = %v, want %v", got, tc.wantStop)
			}
			if dir, ok := sess.FirstPeerEnded(); !ok || dir != tc.first {
				t.Fatalf("first end = %d, %v; want %d", dir, ok, tc.first)
			}
		})
	}
}
