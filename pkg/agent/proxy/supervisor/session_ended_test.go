package supervisor

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// How a session's streams ended is one word and one pointer (a Session stays
// in its size class): each stream that ended with its connection, and, where
// the client stopped taking the destination's stream, what reads the start of
// what it refused there, for a parser that reaches that end mid-message.
func TestASessionSaysHowItsStreamsEnded(t *testing.T) {
	s := &Session{}
	for _, d := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest, fakeconn.Direction(2)} {
		if s.EndedWithConnection(d) {
			t.Fatalf("a fresh session says %v ended with its connection", d)
		}
	}
	if s.EndedWhereClientStopped() || s.NotTakenWhereClientStopped() != nil {
		t.Fatalf("a fresh session says the client stopped, refusing %q", s.NotTakenWhereClientStopped())
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

	refused := "0\r\n"
	s.MarkEndedWhereClientStopped(func() []byte { return []byte(refused) })
	refused += "\r\n" // refused after the stop: read as it is now
	if !s.EndedWhereClientStopped() || string(s.NotTakenWhereClientStopped()) != "0\r\n\r\n" {
		t.Fatalf("EndedWhereClientStopped %v, refused %q; want true, the last chunk", s.EndedWhereClientStopped(), s.NotTakenWhereClientStopped())
	}
	stopped := &Session{}
	stopped.MarkEndedWhereClientStopped(nil)
	if !stopped.EndedWhereClientStopped() || stopped.NotTakenWhereClientStopped() != nil {
		t.Fatalf("stopped with nothing to read what was refused: EndedWhereClientStopped %v, refused %q; want true, nil",
			stopped.EndedWhereClientStopped(), stopped.NotTakenWhereClientStopped())
	}
	if stopped.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("a stream that ended where the client stopped says it ended with its connection")
	}

	var nilSess *Session
	nilSess.MarkEndedWithConnection(fakeconn.FromDest)
	nilSess.MarkEndedWhereClientStopped(func() []byte { return []byte("x") })
	if nilSess.EndedWithConnection(fakeconn.FromDest) || nilSess.EndedWhereClientStopped() || nilSess.NotTakenWhereClientStopped() != nil {
		t.Fatal("a nil session says how a stream ended")
	}
}
