//go:build linux

package proxy

import (
	"context"
	"net/netip"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
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

// The connection's first end is the application's stop, which records no
// fault (Session.EndFollowedStop), when the recording's stop had begun, or
// when it is the client's end and the kernel saw the client's process release
// that connection as it exited on its own. The client's end while its
// process ran on (it gave up), as a signal killed it, or with no record of
// the release (hooks that make none, or none for this connection) is a
// fault; and the upstream's end before the stop is the upstream's, however
// the client's process ended.
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
		{"the hooks say nothing of releases", false, true, nil, fakeconn.FromClient, false},
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

// The recording's stop has begun once keploy mock record's client says so
// (NoteStopBegun) or keploy record's graceful-shutdown notice arrives, and a
// new recording or mocking session starts with it not begun.
func TestStopBegunIsSetByEitherNoticeAndClearedByTheNextSession(t *testing.T) {
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	if p.StopBegun() {
		t.Fatal("a new proxy has its stop begun")
	}
	if err := p.SetGracefulShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.StopBegun() {
		t.Fatal("keploy record's graceful-shutdown notice did not begin the stop")
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	if p.StopBegun() {
		t.Fatal("a new recording started with the last one's stop begun")
	}
	p.NoteStopBegun()
	if !p.StopBegun() {
		t.Fatal("keploy mock record's stop notice did not begin the stop")
	}
	if p.IsGracefulShutdown() {
		t.Fatal("keploy mock record's stop notice set the graceful-shutdown flag, whose effects belong after its drain")
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if p.StopBegun() {
		t.Fatal("a new mocking session started with the last recording's stop begun")
	}
}
