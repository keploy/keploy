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
		// stopBefore[i]: the stop had begun when end i was read.
		ends       []fakeconn.Direction
		stopBefore []bool
		want       bool
	}{
		{"the app closed after the stop began", []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest}, []bool{true, true}, true},
		{"the upstream reset after the stop began", []fakeconn.Direction{fakeconn.FromDest}, []bool{true}, true},
		{"the app gave up before the stop", []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest}, []bool{false, true}, false},
		{"no stop", []fakeconn.Direction{fakeconn.FromClient}, []bool{false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{}
			for i, dir := range tc.ends {
				s.MarkPeerEndedStopping(dir, false, tc.stopBefore[i])
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
}
