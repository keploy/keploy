package connseq

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// A client chunk not numbered yet is numbered after every number given, so
// none is pending before a given number; before a number not given yet, every
// chunk may be.
func TestPendingBeforeAClientChunkOnlyForANumberNotGiven(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{})
	if !u.PendingBefore(fakeconn.FromClient, 1) || !u.PendingBefore(fakeconn.FromDest, 1) {
		t.Fatal("nothing is numbered yet: a chunk of either direction may still be numbered before 1")
	}
	q := u.FromClient()
	if u.PendingBefore(fakeconn.FromClient, q) {
		t.Fatalf("a client chunk is pending before %d, the last number given", q)
	}
	if !u.PendingBefore(fakeconn.FromClient, q+1) || !u.PendingBefore(fakeconn.FromDest, q+1) {
		t.Fatalf("%d is not given yet: a chunk of either direction may still be numbered before it", q+1)
	}
}

// A destination chunk is pending before a client chunk's number while the bytes
// the destination had sent when the client chunk was numbered are still to be
// read, and once they are read, until a Read on the conn beneath begins: up to
// then a layer above may hand on a chunk that ends at the cut, and it takes the
// cut's number. A cut not before the number holds nothing back.
func TestPendingBeforeADestChunkUntilItsCutIsBehindARead(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{})
	u.rest = make([]byte, 5) // what the destination has sent: read, held back
	q1 := u.FromClient()     // places a cut at 5, numbered before q1
	q2 := u.FromClient()     // nothing more sent: no cut
	u.rest = nil
	if len(u.cuts) != 1 || u.cuts[0].at != 5 {
		t.Fatalf("cuts %+v, want one at 5", u.cuts)
	}
	for _, step := range []struct {
		read    uint64
		reading bool
		want    bool
		what    string
	}{
		{3, false, true, "bytes up to the cut are still to be read"},
		{3, true, true, "bytes up to the cut are still to be read, with a Read in flight"},
		{5, false, true, "every byte up to the cut is read, and a layer above may still hand on a chunk ending there"},
		{5, true, false, "a Read on the conn beneath has begun: every chunk ending at the cut was handed on"},
		{6, false, false, "a byte past the cut is read: the cut is passed"},
	} {
		u.read, u.reading = step.read, step.reading
		if got := u.PendingBefore(fakeconn.FromDest, q1); got != step.want {
			t.Fatalf("read %d, reading %v: PendingBefore(dest, %d) = %v, want %v: %s", step.read, step.reading, q1, got, step.want, step.what)
		}
		if got := u.PendingBefore(fakeconn.FromDest, q2); got != step.want {
			t.Fatalf("read %d, reading %v: PendingBefore(dest, %d) = %v, want %v: %s", step.read, step.reading, q2, got, step.want, step.what)
		}
		if u.PendingBefore(fakeconn.FromClient, q2) {
			t.Fatalf("read %d, reading %v: a client chunk is pending before %d: the cuts hold back only the destination's bytes", step.read, step.reading, q2)
		}
	}
	u.read, u.reading = 3, false
	if cut := fakeconn.ConnSeqOf(u.cuts[0].n); u.PendingBefore(fakeconn.FromDest, cut) {
		t.Fatalf("PendingBefore(dest, %d), the cut's own number: nothing is numbered before it", cut)
	}
}

// Cuts ascend: the first one not before the number ends the look, and one
// before it still holding bytes back is found past cuts that hold none.
func TestPendingBeforeADestChunkLooksAtEveryCutBeforeTheNumber(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{})
	u.rest = make([]byte, 2)
	u.FromClient() // a cut at 2
	u.rest = make([]byte, 4)
	q := u.FromClient() // a cut at 4
	u.rest = make([]byte, 9)
	later := u.FromClient() // a cut at 9, after q
	u.rest = nil
	if len(u.cuts) != 3 {
		t.Fatalf("cuts %+v, want three", u.cuts)
	}
	u.read, u.reading = 3, true // the first cut is passed, the second is not
	if !u.PendingBefore(fakeconn.FromDest, q) {
		t.Fatalf("bytes up to 4 are still to be read: a chunk may be numbered before %d", q)
	}
	u.read = 4 // the second cut is reached, with a Read in flight
	if u.PendingBefore(fakeconn.FromDest, q) {
		t.Fatalf("every cut before %d is behind a Read: the cut at 9, after it, holds nothing back before it", q)
	}
	if !u.PendingBefore(fakeconn.FromDest, later) {
		t.Fatalf("bytes up to 9 are still to be read: a chunk may be numbered before %d", later)
	}
}

// A Read on the conn beneath is in flight from when it begins, which OnRead's
// func is told, until it returns; what it returns passes the cut it reached.
func TestReadInFlightIsToldAndEndsWithTheRead(t *testing.T) {
	t.Parallel()
	u, dest := pipeUpstream(t)
	begun := make(chan struct{}, 4)
	u.OnRead(func() { begun <- struct{}{} })
	u.mu.Lock()
	u.cuts = []cut{{at: 0, n: 1}}
	u.n = 2
	u.mu.Unlock()
	q := fakeconn.ConnSeqOf(2)
	if !u.PendingBefore(fakeconn.FromDest, q) {
		t.Fatal("no Read in flight at the cut: a layer above may still hand on a chunk ending there")
	}
	done := make(chan error, 1)
	go func() {
		_, err := u.Read(make([]byte, 8))
		done <- err
	}()
	<-begun
	if u.PendingBefore(fakeconn.FromDest, q) {
		t.Fatal("a Read on the conn beneath has begun: nothing is left to be numbered at the cut")
	}
	if _, err := dest.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	reading := u.reading
	u.mu.Unlock()
	if reading {
		t.Fatal("the Read returned: it is not in flight")
	}
}
