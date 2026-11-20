package supervisor

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// How a session's streams ended: each stream that ended with its connection,
// and whether the destination's ended where the client stopped taking it, for
// a parser that reaches that end mid-message.
func TestASessionSaysHowItsStreamsEnded(t *testing.T) {
	s := &Session{}
	for _, d := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest, fakeconn.Direction(2)} {
		if s.EndedWithConnection(d) {
			t.Fatalf("a fresh session says %v ended with its connection", d)
		}
	}
	if s.EndedWhereClientStopped() {
		t.Fatal("a fresh session says the client stopped")
	}

	s.MarkEndedWithConnection(fakeconn.FromDest)
	if !s.EndedWithConnection(fakeconn.FromDest) || s.EndedWithConnection(fakeconn.FromClient) {
		t.Fatalf("marked FromDest: EndedWithConnection(FromDest) %v, (FromClient) %v; want true, false",
			s.EndedWithConnection(fakeconn.FromDest), s.EndedWithConnection(fakeconn.FromClient))
	}
	s.MarkEndedWithConnection(fakeconn.FromClient)
	if !s.EndedWithConnection(fakeconn.FromClient) || !s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("marked both: one of them does not say so")
	}
	s.MarkEndedWithConnection(fakeconn.Direction(2))
	if s.EndedWithConnection(fakeconn.Direction(2)) {
		t.Fatal("a direction out of range ended with its connection")
	}
	if s.EndedWhereClientStopped() {
		t.Fatal("a stream that ended with its connection says the client stopped taking it")
	}

	s.MarkEndedWhereClientStopped()
	if !s.EndedWhereClientStopped() {
		t.Fatal("marked where the client stopped: EndedWhereClientStopped false")
	}
	stopped := &Session{}
	stopped.MarkEndedWhereClientStopped()
	if stopped.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("a stream that ended where the client stopped says it ended with its connection")
	}

	var nilSess *Session
	nilSess.MarkEndedWithConnection(fakeconn.FromDest)
	nilSess.MarkEndedWhereClientStopped()
	if nilSess.EndedWithConnection(fakeconn.FromDest) || nilSess.EndedWhereClientStopped() {
		t.Fatal("a nil session says how a stream ended")
	}
}
