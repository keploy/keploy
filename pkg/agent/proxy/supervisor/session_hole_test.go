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
