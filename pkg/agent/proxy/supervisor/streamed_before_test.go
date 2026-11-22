package supervisor

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// A session whose producer cannot say how far it has put its streams (no
// Order) says neither that a stream is through a number nor what to wait on;
// one whose producer can says what it says.
func TestSession_StreamedBefore(t *testing.T) {
	t.Parallel()
	var nilSess *Session
	for _, s := range []*Session{nilSess, {}} {
		if end, ok, changed := s.StreamedBefore(fakeconn.FromDest, 3); end != 0 || ok || changed != nil {
			t.Fatalf("no producer order: got %d, %v, %v; want 0, false, nil", end, ok, changed)
		}
	}
	wait := make(chan struct{})
	s := &Session{Order: func(dir fakeconn.Direction, seq uint32) (int64, bool, <-chan struct{}) {
		if dir == fakeconn.FromDest && seq == 3 {
			return 0, false, wait
		}
		return 42, true, nil
	}}
	if _, ok, changed := s.StreamedBefore(fakeconn.FromDest, 3); ok || changed != (<-chan struct{})(wait) {
		t.Fatal("the producer's wait was not passed on")
	}
	if end, ok, _ := s.StreamedBefore(fakeconn.FromClient, 3); !ok || end != 42 {
		t.Fatalf("got %d, %v; want the producer's 42, true", end, ok)
	}
}
