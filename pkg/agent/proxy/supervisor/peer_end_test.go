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
