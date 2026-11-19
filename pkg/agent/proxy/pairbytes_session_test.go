//go:build linux || darwin

package proxy

import (
	"errors"
	"net"
	"net/netip"
	"slices"
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

// readsAtSession makes each session move read pc's socket as holding nothing
// unread, so what the application had written is what the proxy read of it
// (pairedConn.read), for the connections a row makes by hand.
func readsAtSession(t *testing.T) {
	t.Helper()
	was := unreadAtSession
	unreadAtSession = func(*pairedConn) (int, error) { return 0, nil }
	t.Cleanup(func() { unreadAtSession = was })
}

// A close's loss is stamped with the newest agent's session of: the one the
// proxy last moved bytes on its connection in (or accepted it in, when it
// moved none), and the one whose mark the lost bytes reach past
// (PairedBytesSession, sessionsOfLocked): bytes the application wrote after
// a session began may be that session's, whether or not the proxy read any
// of them. Never merely the session open as the close is counted.
func TestACloseIsStampedWithTheSessionItsConnectionWasUsedIn(t *testing.T) {
	readsAtSession(t)
	p := &Proxy{}
	p.PairedBytesSession(1)
	usedIn1 := heldInSession(p, "50001")   // moves bytes in session 1 only
	idle := heldInSession(p, "50002")      // accepted in session 1, never used
	usedLater := heldInSession(p, "50003") // accepted in session 1, used in session 3
	usedIn1.used.Add(1)
	usedIn1.read.Add(5)
	p.PairedBytesSession(2)
	p.PairedBytesSession(3)
	acceptedIn3 := heldInSession(p, "50004") // accepted in session 3, never used
	usedLater.used.Add(1)

	if want := []sessionMark{{1, 1}, {3, 6}}; !slices.Equal(usedIn1.marks, want) {
		t.Fatalf("usedIn1's marks %v, want %v: session 2, in which it was written nothing, owns no bytes", usedIn1.marks, want)
	}
	p.chargeOpen(usedIn1, models.CaptureLossAppBytesLost, 1, 5)
	p.chargeOpen(idle, models.CaptureLossAppBytesLost, 1, 3)
	p.chargeOpen(usedLater, models.CaptureLossAppBytesLost, 1, 1)
	p.chargeOpen(acceptedIn3, models.CaptureLossAppBytesLost, 1, 1)
	want := map[string]uint64{
		"from 127.0.0.1:50001": 1, // its bytes and its use were in session 1, though sessions 2 and 3 followed
		"from 127.0.0.1:50002": 3, // accepted in session 1, but every byte it was written is past session 3's mark
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
	// A loss of a byte past session 3's mark on the connection used in
	// session 1 is session 3's, split at that mark: the bytes before it
	// are session 1's, a share apart.
	p.chargeOpen(usedIn1, models.CaptureLossAppBytesLost, 5, 6)
	got = p.TakeOwnedLosses()
	if len(got) != 2 || got[0].Session != 1 || got[1].Session != 3 {
		t.Fatalf("owned %+v, want session 1's share, then session 3's", got)
	}
}

// Where a loss's bytes are split (sessionsOfLocked): a loss is the newest
// session whose mark a byte of it reaches past, or that the proxy moved bytes
// in, and is split at that session's mark only when it reaches bytes before
// it, with the session whose mark is just before as the earlier share's.
func TestALossIsSplitAtItsSessionsMark(t *testing.T) {
	s := &pairedConns{}
	s.session.Store(2)
	pc := &pairedConn{session: 1, marks: []sessionMark{{1, 1}, {2, 6}}}
	for _, c := range []struct {
		low, top      uint64
		newest, older uint64
		split         bool
	}{
		{1, 5, 1, 0, false}, // every byte before session 2's mark
		{1, 6, 2, 1, true},  // the first byte past it, and those before
		{6, 6, 2, 1, false}, // none before it
		{6, 9, 2, 1, false},
	} {
		newest, older, split := s.sessionsOfLocked(pc, c.low, c.top)
		if newest != c.newest || split != c.split || (split && older != c.older) {
			t.Errorf("bytes %d..%d: newest %d, older %d, split %v; want %d, %d, %v", c.low, c.top, newest, older, split, c.newest, c.older, c.split)
		}
	}
	// The proxy moved bytes on it in session 2: a loss is session 2's,
	// still split at its mark.
	pc.used.Add(1)
	if newest, older, split := s.sessionsOfLocked(pc, 1, 9); newest != 2 || !split || older != 1 {
		t.Errorf("used in session 2, bytes 1..9: newest %d, older %d, split %v; want 2, 1, true", newest, older, split)
	}
	if newest, _, split := s.sessionsOfLocked(pc, 1, 5); newest != 2 || split {
		t.Errorf("used in session 2, bytes 1..5: newest %d, split %v; want 2, unsplit", newest, split)
	}
}

// A session move that cannot read what the connection's socket holds unread
// cannot tell where the new session's bytes begin: the new session owns every
// byte of the session before (fail closed), never an earlier session a byte
// the new one may have been written. The marks a connection keeps are
// bounded, the oldest session's bytes going to the next oldest, and the first
// mark always begins at byte 1.
func TestASessionMarkThatCannotReadGivesTheNewSessionTheBytesBefore(t *testing.T) {
	p := &Proxy{}
	p.PairedBytesSession(1)
	pc := heldInSession(p, "50006")
	pc.read.Add(5)
	was := unreadAtSession
	t.Cleanup(func() { unreadAtSession = was })
	unreadAtSession = func(*pairedConn) (int, error) { return 0, nil }
	p.PairedBytesSession(2)
	unreadAtSession = func(*pairedConn) (int, error) { return 0, errors.New("no socket") }
	p.PairedBytesSession(3)
	if want := []sessionMark{{1, 1}, {3, 6}}; !slices.Equal(pc.marks, want) {
		t.Fatalf("marks %v, want %v: session 3 owns session 2's bytes, which it cannot tell apart", pc.marks, want)
	}
	unreadAtSession = func(*pairedConn) (int, error) { return 2, nil }
	for i := range uint64(20) {
		pc.read.Add(1)
		p.PairedBytesSession(4 + i)
	}
	if len(pc.marks) != maxSessionMarks || pc.marks[0].from != 1 || pc.marks[len(pc.marks)-1] != (sessionMark{23, 5 + 20 + 2 + 1}) {
		t.Fatalf("marks %v, want the newest %d, the first from byte 1, the last session 23's from byte 28", pc.marks, maxSessionMarks)
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

// The share of a loss an end cut short on the connection owns is that end's
// (lostByCuts): only the bytes past the last such end are split at a
// session's mark, so bytes the cut end owns never make a share of the session
// before.
func TestALossIsSplitOnlyPastItsCutEnds(t *testing.T) {
	p := &Proxy{}
	p.PairedBytesSession(2)
	pc := heldInSession(p, "50007")
	pc.marks = []sessionMark{{1, 1}, {2, 10}}
	pc.cuts = []cutEnd{{want: 9, test: "test-1", session: 1}}
	p.chargeOpen(pc, models.CaptureLossAppBytesLost, 6, 12)
	if got := p.TakeOwnedLosses(); len(got) != 1 || got[0].Session != 2 || !got[0].Open {
		t.Fatalf("owned %+v, want only session 2's share of bytes 10..12: bytes 6..9 are test-1's", got)
	}
}
