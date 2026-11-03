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
		{"the stream ended where the client stopped", func(s *Session) { s.MarkEndedWhereClientStopped(func() []byte { return nil }) }, (*Session).EndedWhereClientStopped},
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

// MarkPeerEndedBy keeps which end that records no fault the connection's first
// end was (FirstEndNoFault), and sets EndFollowedStop for each such end; the
// other side's later end, whatever it was, changes neither. It shares no bit
// with the word's other marks: none sets it, and it sets none.
func TestFirstEndNoFaultIsTheFirstEndsOwn(t *testing.T) {
	for _, why := range []NoFaultEnd{EndMayBeFault, EndAtStop, EndAtExit, EndUnjudged} {
		later := EndAtStop
		if why == EndAtStop {
			later = EndUnjudged
		}
		for _, dir := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest} {
			s := &Session{}
			s.MarkPeerEndedBy(dir, false, why)
			s.MarkPeerEndedBy(1-dir, true, later)
			if got := s.FirstEndNoFault(); got != why {
				t.Errorf("first end %d marked %d, then the other side's %d: FirstEndNoFault = %d, want %d", dir, why, later, got, why)
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
		{"the stream ended where the client stopped", func(s *Session) { s.MarkEndedWhereClientStopped(func() []byte { return nil }) }, (*Session).EndedWhereClientStopped},
	} {
		s := &Session{}
		other.set(s)
		s.MarkPeerEndedBy(fakeconn.FromClient, false, EndMayBeFault)
		if got := s.FirstEndNoFault(); got != EndMayBeFault {
			t.Errorf("%s, then an end that may be a fault: FirstEndNoFault = %d", other.name, got)
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
				t.Errorf("a nil session's FirstEndNoFault panicked: %v", r)
			}
		}()
		if got := (*Session)(nil).FirstEndNoFault(); got != EndMayBeFault {
			t.Errorf("a nil session's FirstEndNoFault = %d", got)
		}
	}()
}
