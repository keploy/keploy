//go:build linux || darwin

package proxy

import (
	"net"
	"net/netip"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// sessionConn is a key for a paired connection the test makes by hand.
type sessionConn struct{ net.Conn }

// heldInSession makes p hold a worker's paired connection accepted in the
// session open now, as add does.
func heldInSession(p *Proxy, name string) *pairedConn {
	s := &p.paired
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byConn == nil {
		s.byConn = map[net.Conn]*pairedConn{}
	}
	pc := &pairedConn{instance: 1, app: netip.MustParseAddrPort("127.0.0.1:" + name), done: make(chan struct{}),
		sessions: &s.session, session: s.session.Load()}
	s.byConn[&sessionConn{}] = pc
	return pc
}

// A close's loss is stamped with the agent's session its connection was used
// in (PairedBytesSession, sessionOfLocked): the session the proxy last moved
// bytes on it in, or that accepted it when it moved none, never merely the
// session open as the close is counted, nor one it lived through unused.
func TestACloseIsStampedWithTheSessionItsConnectionWasUsedIn(t *testing.T) {
	p := &Proxy{}
	p.PairedBytesSession(1)
	usedIn1 := heldInSession(p, "50001")   // moves bytes in session 1 only
	idle := heldInSession(p, "50002")      // accepted in session 1, never used
	usedLater := heldInSession(p, "50003") // accepted in session 1, used in session 3
	usedIn1.used.Add(1)
	p.PairedBytesSession(2)
	p.PairedBytesSession(3)
	acceptedIn3 := heldInSession(p, "50004") // accepted in session 3, never used
	usedLater.used.Add(1)

	for _, pc := range []*pairedConn{usedIn1, idle, usedLater, acceptedIn3} {
		p.chargeOpen(pc, models.CaptureLossAppBytesLost)
	}
	want := map[string]uint64{
		"from 127.0.0.1:50001": 1, // its use was in session 1, though sessions 2 and 3 followed
		"from 127.0.0.1:50002": 1, // accepted in session 1, unused since
		"from 127.0.0.1:50003": 3, // used in the session open at its close
		"from 127.0.0.1:50004": 3, // accepted in the session open at its close
	}
	got := p.TakeOwnedLosses()
	if len(got) != len(want) {
		t.Fatalf("owned %+v, want one loss for each of %d connections", got, len(want))
	}
	for _, o := range got {
		if w, ok := want[o.Loss.What]; !ok || o.Session != w || !o.Open || o.Worker != 1 {
			t.Errorf("loss %+v, want worker 1's open test's, stamped with session %d", o, w)
		}
	}
}

// A cut-short end's loss is stamped with the session of that end, not the
// session open when its connection's close is counted; a connection no add
// made stamps session 0.
func TestACutEndsLossIsStampedWithItsEndsSession(t *testing.T) {
	p := &Proxy{}
	p.PairedBytesSession(4)
	pc := heldInSession(p, "50005")
	pc.cutShort("test-1", 3)
	p.PairedBytesSession(5)
	cuts, _ := pc.lostByCuts(1, 3)
	if len(cuts) != 1 || cuts[0].test != "test-1" || cuts[0].session != 4 {
		t.Fatalf("cuts %+v, want test-1's, stamped with session 4", cuts)
	}
	p.paired.own(cuts[0], pc.app)
	if got := p.TakeOwnedLosses(); len(got) != 1 || got[0].Session != 4 || got[0].Test != "test-1" {
		t.Fatalf("owned %+v, want test-1's loss stamped with session 4", got)
	}

	var bare pairedConn
	bare.cutShort("test-2", 1)
	if len(bare.cuts) != 1 || bare.cuts[0].session != 0 {
		t.Fatalf("cuts %+v, want test-2's in session 0", bare.cuts)
	}
}

// Past the bound, the run's entry for a kind keeps each session apart and
// carries its session: a later session's loss is never counted into an
// earlier session's entry, which charges no later run.
func TestOwnedLossesPastTheBoundKeepEachSessionApart(t *testing.T) {
	var s pairedConns
	app := netip.MustParseAddrPort("127.0.0.1:50912")
	for i := range maxOwned {
		s.own(cutEnd{test: "test-" + strconv.Itoa(i+1), session: 1}, app)
	}
	s.own(cutEnd{test: "test-past-1", session: 1}, app)
	s.own(cutEnd{test: "test-past-2", session: 2}, app)
	s.own(cutEnd{test: "test-past-3", session: 2}, app)
	if len(s.owned) != maxOwned+2 {
		t.Fatalf("%d owned losses kept, want %d: the bound's own and one of the run's for each session", len(s.owned), maxOwned+2)
	}
	for i, want := range []agent.OwnedLoss{
		{Session: 1, Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: agent.OwnedLossesPastTheBound}},
		{Session: 2, Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 2, What: agent.OwnedLossesPastTheBound}},
	} {
		if got := s.owned[maxOwned+i]; got.Test != "" || got.Session != want.Session || got.Loss.Kind != want.Loss.Kind || got.Loss.Count != want.Loss.Count || got.Loss.What != want.Loss.What {
			t.Errorf("the run's owned loss %d = %+v, want %+v", i, got, want)
		}
	}
}
