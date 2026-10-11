package relay

import (
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// collect reads every chunk of fc until it ends.
func collect(fc *fakeconn.FakeConn) chan []fakeconn.Chunk {
	out := make(chan []fakeconn.Chunk, 1)
	go func() {
		var cs []fakeconn.Chunk
		for {
			c, err := fc.ReadChunk()
			if err != nil {
				out <- cs
				return
			}
			cs = append(cs, c)
		}
	}()
	return out
}

// The relay numbers a chunk as its Read returns, not as it is teed. Here the
// client direction's tee is held back (beforeTee) until the destination's
// answer to the request has been read, written to the client and teed: the
// answer is teed first, and it is still numbered after the request. Numbered
// as they are teed, the two would swap, and the parser would take the answer
// for one captured before the request, and drop it.
func TestRelay_NumbersAtReadNotAtTee(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	h := newHarness(t, Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromClient {
			<-release
		}
	}})
	clientChunks, destChunks := collect(h.r.ClientStream()), collect(h.r.DestStream())

	go func() { _, _ = h.clientApp.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(h.destSvc, buf); err != nil { // the request is written on; its tee is held
		t.Fatal(err)
	}
	go func() { _, _ = h.destSvc.Write([]byte("pong")) }()
	if _, err := io.ReadFull(h.clientApp, buf); err != nil { // the answer is relayed
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.r.teeD2C.acceptedBytes() == 4 }) // and teed, first
	if h.r.teeC2D.acceptedBytes() != 0 {
		t.Fatal("the request was teed before the hook released it: the test proves nothing")
	}
	close(release)
	waitFor(t, func() bool { return h.r.teeC2D.acceptedBytes() == 4 })
	h.shutdown()
	cs, ds := <-clientChunks, <-destChunks
	if len(cs) != 1 || len(ds) != 1 {
		t.Fatalf("got %d client and %d server chunks, want one each", len(cs), len(ds))
	}
	if cs[0].ConnSeq == 0 || !cs[0].CapturedBefore(ds[0].ConnSeq) {
		t.Fatalf("the request is numbered %d, its answer %d: the answer must come after", cs[0].ConnSeq, ds[0].ConnSeq)
	}
}

// Not in lockstep: the client and the destination each write their next
// message as soon as the other's has arrived, while every client chunk's tee is
// held back a random while, so the two directions are teed in any order. Every
// answer is still numbered after its request and before the next request, and
// the numbers are the connection's: unique, and never 0.
func TestRelay_NumbersBothDirectionsFromOneCounterWhateverTheTeeOrder(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	delays := make(chan time.Duration, 512)
	for i := 0; i < cap(delays); i++ {
		delays <- time.Duration(rng.Intn(3000)) * time.Microsecond
	}
	h := newHarness(t, Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromClient {
			select {
			case d := <-delays:
				time.Sleep(d)
			default:
			}
		}
	}})
	clientChunks, destChunks := collect(h.r.ClientStream()), collect(h.r.DestStream())
	const rounds = 200
	serverDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		for i := 0; i < rounds; i++ {
			if _, err := io.ReadFull(h.destSvc, buf); err != nil {
				serverDone <- err
				return
			}
			if _, err := h.destSvc.Write([]byte(fmt.Sprintf("pong-%03d", i))); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	buf := make([]byte, 8)
	for i := 0; i < rounds; i++ {
		h.writeClient([]byte(fmt.Sprintf("ping-%03d", i)))
		if _, err := io.ReadFull(h.clientApp, buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return h.r.teeC2D.acceptedBytes() == 8*rounds && h.r.teeD2C.acceptedBytes() == 8*rounds
	})
	h.shutdown()
	cs, ds := <-clientChunks, <-destChunks
	if len(cs) != rounds || len(ds) != rounds {
		t.Fatalf("got %d client and %d server chunks, want %d each (one per write)", len(cs), len(ds), rounds)
	}
	seen := map[uint32]bool{}
	for i := 0; i < rounds; i++ {
		q, a := cs[i], ds[i]
		if q.ConnSeq == 0 || a.ConnSeq == 0 || seen[q.ConnSeq] || seen[a.ConnSeq] {
			t.Fatalf("round %d: numbers %d and %d are not unique, non-zero numbers", i, q.ConnSeq, a.ConnSeq)
		}
		seen[q.ConnSeq], seen[a.ConnSeq] = true, true
		if !q.CapturedBefore(a.ConnSeq) {
			t.Fatalf("round %d: the request is numbered %d, its answer %d", i, q.ConnSeq, a.ConnSeq)
		}
		if i+1 < rounds && !a.CapturedBefore(cs[i+1].ConnSeq) {
			t.Fatalf("round %d: the answer is numbered %d, the next request %d", i, a.ConnSeq, cs[i+1].ConnSeq)
		}
	}
}

// Every chunk is numbered, and only the destination's connseq.Upstream, at
// its socket under any TLS, can say what the destination had sent when a
// client chunk was read: a relay for a destination not read through one is
// refused, not run with chunks numbered some other way.
func TestNew_RefusesADestinationNotReadThroughAnUpstream(t *testing.T) {
	t.Parallel()
	src, app := net.Pipe()
	dst, dest := net.Pipe()
	t.Cleanup(func() { _ = src.Close(); _ = app.Close(); _ = dst.Close(); _ = dest.Close() })
	defer func() {
		if recover() == nil {
			t.Fatal("New ran a relay for a destination with no connseq.Upstream")
		}
	}()
	New(Config{Logger: zap.NewNop()}, src, tls.Client(dst, &tls.Config{}))
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for limit := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(limit) {
			t.Fatal("condition not met in 10s")
		}
		time.Sleep(time.Millisecond)
	}
}
