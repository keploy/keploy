package supervisor

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// MarkEndedAtHole says, per direction, that the stream ended at a capture
// hole, and why: the first reason is kept, the other direction is untouched,
// and a nil session or a direction out of range is a no-op.
func TestSession_EndedAtHoleIsPerDirectionAndKeepsTheFirstReason(t *testing.T) {
	t.Parallel()
	s := &Session{}
	for _, d := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest} {
		if why, ok := s.EndedAtHole(d); ok || why != "" {
			t.Fatalf("a fresh session says %v ended at a hole, for %q", d, why)
		}
	}
	s.MarkEndedAtHole(fakeconn.FromDest, "per_conn_cap")
	s.MarkEndedAtHole(fakeconn.FromDest, "memory_pressure")
	if why, ok := s.EndedAtHole(fakeconn.FromDest); !ok || why != "per_conn_cap" {
		t.Fatalf("EndedAtHole(FromDest) = %q, %v; want per_conn_cap, true: the first reason is kept", why, ok)
	}
	if why, ok := s.EndedAtHole(fakeconn.FromClient); ok {
		t.Fatalf("EndedAtHole(FromClient) = %q, true; only the server's stream ended at a hole", why)
	}
	if s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("a stream that ended at a hole did not end with its connection")
	}

	s.MarkEndedAtHole(fakeconn.Direction(2), "per_conn_cap")
	if _, ok := s.EndedAtHole(fakeconn.Direction(2)); ok {
		t.Fatal("a direction out of range ended at a hole")
	}
	var nilSess *Session
	nilSess.MarkEndedAtHole(fakeconn.FromClient, "per_conn_cap")
	if _, ok := nilSess.EndedAtHole(fakeconn.FromClient); ok {
		t.Fatal("a nil session says a stream ended at a hole")
	}
}

// MarkReceiverGone says, per direction, that the stream ended because the
// peer its bytes are relayed to went away, and how many bytes at its end never
// reached it. The failed write of the stream's last chunk says how many, and
// is kept over a reset seen first (none undelivered); a reset after it does
// not undo it. The other direction is untouched, and a nil session or a
// direction out of range is a no-op.
func TestSession_ReceiverGoneKeepsTheFailedWritesUndeliveredBytes(t *testing.T) {
	t.Parallel()
	s := &Session{}
	if n, ok := s.ReceiverGone(fakeconn.FromDest); ok || n != 0 {
		t.Fatalf("a fresh session says the client left, %d bytes undelivered", n)
	}
	s.MarkReceiverGone(fakeconn.FromDest, 0) // the client's reset
	if n, ok := s.ReceiverGone(fakeconn.FromDest); !ok || n != 0 {
		t.Fatalf("after a reset: %d, %v; want 0, true", n, ok)
	}
	s.MarkReceiverGone(fakeconn.FromDest, 17) // the write that failed
	s.MarkReceiverGone(fakeconn.FromDest, 0)
	if n, ok := s.ReceiverGone(fakeconn.FromDest); !ok || n != 17 {
		t.Fatalf("after the failed write: %d, %v; want 17, true", n, ok)
	}
	if _, ok := s.ReceiverGone(fakeconn.FromClient); ok {
		t.Fatal("the client's stream says its receiver went away")
	}
	if s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("a stream whose receiver went away is not one an observe-only capture ended with its connection")
	}

	// The two marks of how a stream ended are kept apart, in either order.
	for _, withConnFirst := range []bool{true, false} {
		b := &Session{}
		if withConnFirst {
			b.MarkEndedWithConnection(fakeconn.FromDest)
		}
		b.MarkReceiverGone(fakeconn.FromDest, 3)
		if !withConnFirst {
			b.MarkEndedWithConnection(fakeconn.FromDest)
		}
		if n, ok := b.ReceiverGone(fakeconn.FromDest); !ok || n != 3 || !b.EndedWithConnection(fakeconn.FromDest) {
			t.Fatalf("with-connection mark first %v: ReceiverGone %d, %v, EndedWithConnection %v; want 3, true, true",
				withConnFirst, n, ok, b.EndedWithConnection(fakeconn.FromDest))
		}
	}

	s.MarkReceiverGone(fakeconn.Direction(2), 1)
	if _, ok := s.ReceiverGone(fakeconn.Direction(2)); ok {
		t.Fatal("a direction out of range says its receiver went away")
	}
	var nilSess *Session
	nilSess.MarkReceiverGone(fakeconn.FromDest, 1)
	if _, ok := nilSess.ReceiverGone(fakeconn.FromDest); ok {
		t.Fatal("a nil session says a receiver went away")
	}
}
