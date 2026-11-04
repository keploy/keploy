package supervisor

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

func TestMarkPeerEndedKeepsEachSidesFirstEndAndWhichEndedFirst(t *testing.T) {
	s := &Session{}
	if _, ok := s.FirstPeerEnded(); ok {
		t.Fatal("a new session has a side ended")
	}
	if s.PeerEnded(fakeconn.FromClient) != PeerOpen || s.PeerEnded(fakeconn.FromDest) != PeerOpen {
		t.Fatal("a new session has a side's end marked")
	}
	s.MarkPeerEnded(fakeconn.FromDest, true)
	s.MarkPeerEnded(fakeconn.FromDest, false) // a second end of the same side is ignored
	s.MarkPeerEnded(fakeconn.FromClient, false)
	if got := s.PeerEnded(fakeconn.FromDest); got != PeerReset {
		t.Errorf("destination's end = %d, want PeerReset (its first)", got)
	}
	if got := s.PeerEnded(fakeconn.FromClient); got != PeerClosed {
		t.Errorf("client's end = %d, want PeerClosed", got)
	}
	if dir, ok := s.FirstPeerEnded(); !ok || dir != fakeconn.FromDest {
		t.Errorf("first ended = %d, %v; want FromDest, true", dir, ok)
	}

	c := &Session{}
	c.MarkPeerEnded(fakeconn.FromClient, false)
	c.MarkPeerEnded(fakeconn.FromDest, false)
	if dir, ok := c.FirstPeerEnded(); !ok || dir != fakeconn.FromClient {
		t.Errorf("first ended = %d, %v; want FromClient, true", dir, ok)
	}
	if got := c.PeerEnded(fakeconn.FromDest); got != PeerClosed {
		t.Errorf("destination's end = %d, want PeerClosed", got)
	}

	var none *Session
	none.MarkPeerEnded(fakeconn.FromDest, true)
	if _, ok := none.FirstPeerEnded(); ok || none.PeerEnded(fakeconn.FromDest) != PeerOpen {
		t.Error("a nil session reports a side ended")
	}
	fresh := &Session{}
	fresh.MarkPeerEnded(fakeconn.Direction(2), true) // no such direction: ignored
	if _, ok := fresh.FirstPeerEnded(); ok {
		t.Error("a mark of an unknown direction ended a side")
	}
	s.MarkPeerEnded(fakeconn.Direction(2), true)
	if got := s.PeerEnded(fakeconn.Direction(2)); got != PeerOpen {
		t.Errorf("an unknown direction's end = %d, want PeerOpen", got)
	}
}

// An end the relay reads once the recording's stop has begun is the stop's
// (EndFollowedStop): the first end decides it, as it is read, and a stop
// that begins after it, or a second end after the stop, does not.
func TestEndFollowedStopIsTheFirstEndReadAfterTheStopBegan(t *testing.T) {
	for _, tc := range []struct {
		name string
		// why[i]: EndAtStop where the stop had begun when end i was read.
		ends []fakeconn.Direction
		why  []NoFaultEnd
		want bool
	}{
		{"the app closed after the stop began", []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest}, []NoFaultEnd{EndAtStop, EndAtStop}, true},
		{"the upstream reset after the stop began", []fakeconn.Direction{fakeconn.FromDest}, []NoFaultEnd{EndAtStop}, true},
		{"the app gave up before the stop", []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest}, []NoFaultEnd{EndMayBeFault, EndAtStop}, false},
		{"no stop", []fakeconn.Direction{fakeconn.FromClient}, []NoFaultEnd{EndMayBeFault}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{}
			for i, dir := range tc.ends {
				s.MarkPeerEndedBy(dir, false, tc.why[i])
			}
			if got := s.EndFollowedStop(); got != tc.want {
				t.Errorf("EndFollowedStop = %v, want %v", got, tc.want)
			}
		})
	}
	if (&Session{}).EndFollowedStop() {
		t.Error("a session with no ends followed a stop")
	}
	never := &Session{}
	never.MarkPeerEnded(fakeconn.FromClient, false)
	if never.EndFollowedStop() {
		t.Error("an end marked with no stop (MarkPeerEnded) followed one")
	}
	var none *Session
	if none.EndFollowedStop() {
		t.Error("a nil session's end followed a stop")
	}
	// The mark has a bit of its own in peerEnds, apart from the server's
	// unasked bytes (ServerSpokeUnasked: set by the floor, cleared by each
	// request), from a stream that ended with its connection
	// (EndedWithConnection: in the same word), and from an end where the
	// client stopped taking the stream (EndedWhereClientStopped): none sets
	// it, the stop sets none, and a request's clear leaves it.
	for _, other := range []struct {
		name string
		set  func(*Session)
		read func(*Session) bool
	}{
		{"the server spoke unasked", func(s *Session) { s.dropped(fakeconn.Dropped{}) }, (*Session).ServerSpokeUnasked},
		{"the client's stream ended with its connection", func(s *Session) { s.MarkEndedWithConnection(fakeconn.FromClient) }, func(s *Session) bool { return s.EndedWithConnection(fakeconn.FromClient) }},
		{"the destination's stream ended with its connection", func(s *Session) { s.MarkEndedWithConnection(fakeconn.FromDest) }, func(s *Session) bool { return s.EndedWithConnection(fakeconn.FromDest) }},
		{"the stream ended where the client stopped", func(s *Session) { s.MarkEndedWhereClientStopped() }, (*Session).EndedWhereClientStopped},
		{"the client ended its side before the destination's end reached it", func(s *Session) { s.MarkClientEndedBeforeDest() }, (*Session).ClientEndedBeforeDest},
		{"the client read after its FIN", func(s *Session) { s.MarkClientReadAfterFIN() }, (*Session).ClientReadAfterFIN},
	} {
		s := &Session{}
		other.set(s)
		s.MarkPeerEndedBy(fakeconn.FromClient, false, EndMayBeFault)
		if !other.read(s) || s.EndFollowedStop() {
			t.Errorf("%s, then an end with no stop: that mark %v, EndFollowedStop %v; want true, false", other.name, other.read(s), s.EndFollowedStop())
		}
		stopped := &Session{}
		stopped.MarkPeerEndedBy(fakeconn.FromClient, false, EndAtStop)
		if other.read(stopped) {
			t.Errorf("an end after the stop says %s", other.name)
		}
	}
	stopped := &Session{}
	stopped.MarkPeerEndedBy(fakeconn.FromClient, false, EndAtStop)
	stopped.peerEnds.And(^uint32(spokeUnaskedBit)) // as NextRequest clears it for the next request
	if !stopped.EndFollowedStop() {
		t.Error("a request's clear of the unasked mark took the stop's mark away")
	}
}

// MarkPeerEndedBy keeps which end that records no fault the client's end was
// (ClientEndNoFault), first or not, and sets EndFollowedStop for each such
// end that came first; the other side's end, whatever it was, changes
// neither. It shares no bit with the word's other marks: none sets it, and it
// sets none.
func TestClientEndNoFaultIsTheClientsOwn(t *testing.T) {
	for _, why := range []NoFaultEnd{EndMayBeFault, EndAtStop, EndAtExit, EndUnjudged} {
		later := EndAtStop
		if why == EndAtStop {
			later = EndUnjudged
		}
		for _, dir := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest} {
			s := &Session{}
			s.MarkPeerEndedBy(dir, false, why)
			s.MarkPeerEndedBy(1-dir, true, later)
			client := later
			if dir == fakeconn.FromClient {
				client = why
			}
			if got := s.ClientEndNoFault(); got != client {
				t.Errorf("first end %d marked %d, then the other side's %d: ClientEndNoFault = %d, want %d", dir, why, later, got, client)
			}
			if got, want := s.EndFollowedStop(), why != EndMayBeFault; got != want {
				t.Errorf("first end %d marked %d: EndFollowedStop = %v, want %v", dir, why, got, want)
			}
		}
	}
	for _, other := range []struct {
		name string
		set  func(*Session)
		read func(*Session) bool
	}{
		{"the server spoke unasked", func(s *Session) { s.dropped(fakeconn.Dropped{}) }, (*Session).ServerSpokeUnasked},
		{"the client's stream ended with its connection", func(s *Session) { s.MarkEndedWithConnection(fakeconn.FromClient) }, func(s *Session) bool { return s.EndedWithConnection(fakeconn.FromClient) }},
		{"the destination's stream ended with its connection", func(s *Session) { s.MarkEndedWithConnection(fakeconn.FromDest) }, func(s *Session) bool { return s.EndedWithConnection(fakeconn.FromDest) }},
		{"the stream ended where the client stopped", func(s *Session) { s.MarkEndedWhereClientStopped() }, (*Session).EndedWhereClientStopped},
		{"the client ended its side before the destination's end reached it", func(s *Session) { s.MarkClientEndedBeforeDest() }, (*Session).ClientEndedBeforeDest},
		{"the client read after its FIN", func(s *Session) { s.MarkClientReadAfterFIN() }, (*Session).ClientReadAfterFIN},
	} {
		s := &Session{}
		other.set(s)
		s.MarkPeerEndedBy(fakeconn.FromClient, false, EndMayBeFault)
		if got := s.ClientEndNoFault(); got != EndMayBeFault || s.EndFollowedStop() {
			t.Errorf("%s, then an end that may be a fault: ClientEndNoFault = %d, EndFollowedStop = %v", other.name, got, s.EndFollowedStop())
		}
		for _, why := range []NoFaultEnd{EndAtStop, EndAtExit, EndUnjudged} {
			marked := &Session{}
			marked.MarkPeerEndedBy(fakeconn.FromClient, false, why)
			if other.read(marked) {
				t.Errorf("an end marked %d says %s", why, other.name)
			}
		}
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("a nil session's ClientEndNoFault panicked: %v", r)
			}
		}()
		if got := (*Session)(nil).ClientEndNoFault(); got != EndMayBeFault {
			t.Errorf("a nil session's ClientEndNoFault = %d", got)
		}
	}()
}

// ClientEndNoFault is the client's own end's kind, whichever side the relay
// read first: a client's end marked after the destination's keeps its own
// kind, and the destination's end, first or after, never gives the client
// its kind. The client's first mark is kept. It is EndMayBeFault while the
// client's end is unmarked, and for a nil session.
func TestClientEndNoFaultIsTheClientsOwnWhicheverEndIsFirst(t *testing.T) {
	for _, why := range []NoFaultEnd{EndMayBeFault, EndAtStop, EndAtExit, EndUnjudged} {
		for _, dest := range []NoFaultEnd{EndMayBeFault, EndAtStop} {
			for _, clientFirst := range []bool{true, false} {
				s := &Session{}
				if clientFirst {
					s.MarkPeerEndedBy(fakeconn.FromClient, false, why)
					s.MarkPeerEndedBy(fakeconn.FromDest, true, dest)
				} else {
					s.MarkPeerEndedBy(fakeconn.FromDest, false, dest)
					s.MarkPeerEndedBy(fakeconn.FromClient, true, why)
				}
				s.MarkPeerEndedBy(fakeconn.FromClient, false, EndUnjudged^why) // a second end of the client's is ignored
				if got := s.ClientEndNoFault(); got != why {
					t.Errorf("the client's end %d, the destination's %d, the client's first: %v: ClientEndNoFault = %d, want %d",
						why, dest, clientFirst, got, why)
				}
				first := dest
				if clientFirst {
					first = why
				}
				if got, want := s.EndFollowedStop(), first != EndMayBeFault; got != want {
					t.Errorf("the client's end %d, the destination's %d, the client's first: %v: EndFollowedStop = %v, want %v",
						why, dest, clientFirst, got, want)
				}
			}
		}
	}
	s := &Session{}
	s.MarkPeerEndedBy(fakeconn.FromDest, false, EndAtStop)
	if got := s.ClientEndNoFault(); got != EndMayBeFault {
		t.Errorf("only the destination's end marked, at the stop: ClientEndNoFault = %d, want %d", got, EndMayBeFault)
	}
}

// The client ending its side before the destination's end reached it
// (MarkClientEndedBeforeDest) is a mark of its own: it marks no side's end,
// no first end, no stop, no end kind of the client's, and no other word of
// the relay's, and none of those marks it (above). Nil-safe.
func TestClientEndedBeforeDestIsAMarkOfItsOwn(t *testing.T) {
	s := &Session{}
	if s.ClientEndedBeforeDest() {
		t.Fatal("a fresh session says the client ended its side before the destination's end reached it")
	}
	s.MarkClientEndedBeforeDest()
	if !s.ClientEndedBeforeDest() {
		t.Fatal("marked: ClientEndedBeforeDest false")
	}
	if _, ok := s.FirstPeerEnded(); ok || s.PeerEnded(fakeconn.FromClient) != PeerOpen || s.PeerEnded(fakeconn.FromDest) != PeerOpen {
		t.Fatal("the mark marked a side's end")
	}
	if s.EndFollowedStop() || s.ClientEndNoFault() != EndMayBeFault || s.EndedWhereClientStopped() || s.ServerSpokeUnasked() ||
		s.EndedWithConnection(fakeconn.FromClient) || s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("the mark set another of the session's marks")
	}
	s.MarkPeerEndedBy(fakeconn.FromDest, true, EndAtStop)
	s.MarkPeerEndedBy(fakeconn.FromClient, false, EndAtExit)
	if !s.ClientEndedBeforeDest() {
		t.Fatal("the sides' ends took the mark away")
	}
	var nilSess *Session
	nilSess.MarkClientEndedBeforeDest()
	if nilSess.ClientEndedBeforeDest() {
		t.Fatal("a nil session says the client ended its side first")
	}
}

// The client's reading after its FIN (MarkClientReadAfterFIN) is a mark of its
// own: it marks no side's end, no first end, no stop, no end kind of the
// client's, and no other word of the relay's (the client's end before the
// destination's included), and none of those marks it (above). Nil-safe.
func TestClientReadAfterFINIsAMarkOfItsOwn(t *testing.T) {
	s := &Session{}
	if s.ClientReadAfterFIN() {
		t.Fatal("a fresh session says the client read after its FIN")
	}
	s.MarkClientEndedBeforeDest()
	s.MarkEndedWhereClientStopped()
	if s.ClientReadAfterFIN() {
		t.Fatal("the client's end before the destination's, or its stopping, says it read after its FIN")
	}
	s = &Session{}
	s.MarkClientReadAfterFIN()
	if !s.ClientReadAfterFIN() {
		t.Fatal("marked: ClientReadAfterFIN false")
	}
	if _, ok := s.FirstPeerEnded(); ok || s.PeerEnded(fakeconn.FromClient) != PeerOpen || s.PeerEnded(fakeconn.FromDest) != PeerOpen {
		t.Fatal("the mark marked a side's end")
	}
	if s.EndFollowedStop() || s.ClientEndNoFault() != EndMayBeFault || s.EndedWhereClientStopped() || s.ClientEndedBeforeDest() ||
		s.ServerSpokeUnasked() || s.EndedWithConnection(fakeconn.FromClient) || s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("the mark set another of the session's marks")
	}
	s.MarkPeerEndedBy(fakeconn.FromClient, false, EndMayBeFault)
	s.MarkPeerEndedBy(fakeconn.FromDest, false, EndAtStop)
	if !s.ClientReadAfterFIN() {
		t.Fatal("the sides' ends took the mark away")
	}
	var nilSess *Session
	nilSess.MarkClientReadAfterFIN()
	if nilSess.ClientReadAfterFIN() {
		t.Fatal("a nil session says the client read after its FIN")
	}
}
