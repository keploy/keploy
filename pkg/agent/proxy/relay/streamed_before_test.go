package relay

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// streamedThrough asks StreamedBefore until it says every chunk of dir
// captured before seq is on the stream, waiting on what it gives between asks.
func streamedThrough(t *testing.T, r *Relay, dir fakeconn.Direction, seq uint32) int64 {
	t.Helper()
	for {
		end, ok, changed := r.StreamedBefore(dir, seq)
		if ok {
			return end
		}
		if changed == nil {
			t.Fatalf("StreamedBefore(%v, %d) is neither ok nor gives a channel to wait on", dir, seq)
		}
		select {
		case <-changed:
		case <-time.After(10 * time.Second): // a hang guard, not a verdict
			t.Fatalf("StreamedBefore(%v, %d) was never told of a change", dir, seq)
		}
	}
}

// gatedAbove is a layer over the destination's Upstream that, as each Read
// begins, says so (reading) and waits for the gate to let it on.
type gatedAbove struct {
	*connseq.Upstream
	reading chan struct{}
	gate    chan struct{}
}

func (g *gatedAbove) Read(p []byte) (int, error) {
	g.reading <- struct{}{}
	<-g.gate
	return g.Upstream.Read(p)
}

// NetConn exposes the Upstream beneath, as a TLS conn does (connseq.Of).
func (g *gatedAbove) NetConn() net.Conn { return g.Upstream }

// The server's answer is read and numbered, and held in the forwarder's hand
// (its tee held back) while the client's next chunk is captured after it. The
// answer is captured before that chunk and not on the stream yet: StreamedBefore
// says so, and is told once the answer is teed. The destination then goes
// quiet, and nothing of it is left before the client's chunk: the answer was
// read to a cut the client's chunk placed, and a layer above the conn beneath
// may still hand on a chunk that ends there until a Read on the conn beneath
// begins. Here the forwarder's next Read is held above it: StreamedBefore waits
// until the gate lets it on, and is told then.
func TestRelay_StreamedBeforeWaitsOnTheAnswerInHandThenNotOnAQuietDestination(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	gated := &gatedAbove{reading: make(chan struct{}, 8), gate: make(chan struct{})}
	h := newHarnessOverBoth(t, Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromDest {
			<-release
		}
	}}, nil, func(u *connseq.Upstream) net.Conn {
		gated.Upstream = u
		return gated
	})
	defer h.shutdown()
	<-gated.reading
	gated.gate <- struct{}{} // the first Read goes on

	h.writeClient([]byte("req"))
	h.readDest(3)
	h.writeDest([]byte("resp"))
	h.readClient(4) // written on, and held before its tee
	h.writeClient([]byte("rst"))
	h.readDest(3)
	readChunk(t, h.r.ClientStream())
	rst := readChunk(t, h.r.ClientStream())

	end, ok, changed := h.r.StreamedBefore(fakeconn.FromDest, rst.ConnSeq)
	if ok || changed == nil {
		t.Fatalf("StreamedBefore(dest, %d) = %d, %v with the answer in the forwarder's hand: want it to wait", rst.ConnSeq, end, ok)
	}
	select {
	case <-changed:
		t.Fatal("told of a change while the answer is still held")
	default:
	}
	close(release)
	<-gated.reading // teed and let go of; the next Read waits above the conn beneath
	if h.r.teeD2C.acceptedBytes() != 4 {
		t.Fatalf("the answer is not teed before the next Read: the row proves nothing")
	}
	end, ok, changed = h.r.StreamedBefore(fakeconn.FromDest, rst.ConnSeq)
	if ok || changed == nil {
		t.Fatalf("StreamedBefore(dest, %d) = %d, %v before a Read on the conn beneath: a layer above may still hold a chunk ending at the cut", rst.ConnSeq, end, ok)
	}
	close(gated.gate)
	select {
	case <-changed:
	case <-time.After(10 * time.Second): // a hang guard, not a verdict
		t.Fatal("a Read on the conn beneath began, and StreamedBefore was not told")
	}
	if end := streamedThrough(t, h.r, fakeconn.FromDest, rst.ConnSeq); end != 4 {
		t.Fatalf("every server chunk before the client's is %d bytes into the stream, want 4", end)
	}
	resp := readChunk(t, h.r.DestStream())
	if !resp.CapturedBefore(rst.ConnSeq) {
		t.Fatalf("the answer is numbered %d, the client's chunk %d: want the answer first", resp.ConnSeq, rst.ConnSeq)
	}
	if got := h.r.DestStream().Consumed(); got != 4 {
		t.Fatalf("the destination's stream is read through %d, want 4", got)
	}
}

// The client's request is written on and held in the forwarder's hand while
// the server's answer to it is captured and teed. The request is captured
// before the answer and not on the stream yet: StreamedBefore says so until it
// is teed.
func TestRelay_StreamedBeforeWaitsOnTheRequestInHand(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	h := newHarness(t, Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromClient {
			<-release
		}
	}})
	defer h.shutdown()

	h.writeClient([]byte("req"))
	h.readDest(3) // written on, and held before its tee
	h.writeDest([]byte("resp"))
	h.readClient(4)
	resp := readChunk(t, h.r.DestStream())

	if end, ok, changed := h.r.StreamedBefore(fakeconn.FromClient, resp.ConnSeq); ok || changed == nil {
		t.Fatalf("StreamedBefore(client, %d) = %d, %v with the request in the forwarder's hand: want it to wait", resp.ConnSeq, end, ok)
	}
	close(release)
	if end := streamedThrough(t, h.r, fakeconn.FromClient, resp.ConnSeq); end != 3 {
		t.Fatalf("every client chunk before the answer is %d bytes into the stream, want 3", end)
	}
	if req := readChunk(t, h.r.ClientStream()); !req.CapturedBefore(resp.ConnSeq) {
		t.Fatalf("the request is numbered %d, the answer %d: want the request first", req.ConnSeq, resp.ConnSeq)
	}
}

// A chunk in the forwarder's hand not numbered yet may be numbered before any
// number; one numbered is before the numbers after its own.
func TestTee_HeldBefore(t *testing.T) {
	t.Parallel()
	tt := newTee(fakeconn.FromDest, 1<<20, 4, time.Second, nil, zap.NewNop())
	if tt.heldBefore(5) {
		t.Fatal("nothing held: nothing is before 5")
	}
	tt.noteReading()
	if !tt.heldBefore(1) || !tt.heldBefore(1<<31+1) {
		t.Fatal("a chunk held and not numbered yet may be numbered before any number")
	}
	tt.noteNumbered(5)
	if !tt.heldBefore(6) || tt.heldBefore(5) || tt.heldBefore(4) {
		t.Fatal("a chunk held, numbered 5: want it before 6 only")
	}
	tt.noteReleased()
	if tt.heldBefore(6) {
		t.Fatal("released: nothing is held before 6")
	}
}

// A chunk set aside for the client's answer is before the numbers after its own;
// one let go of, refused, tells a reader waiting on it.
func TestTee_AwaitingBeforeAndToldWhenLetGo(t *testing.T) {
	t.Parallel()
	tt := newTee(fakeconn.FromDest, 1<<20, 4, time.Second, nil, zap.NewNop())
	tt.await(fakeconn.Chunk{ConnSeq: 7, Bytes: []byte("x")})
	tt.await(fakeconn.Chunk{ConnSeq: 9, Bytes: []byte("y")})
	if !tt.awaitingBefore(8) || tt.awaitingBefore(7) {
		t.Fatal("chunks 7 and 9 set aside: want one before 8 and none before 7")
	}
	changed := tt.progress.changed.wait()
	tt.doneAwaiting()
	select {
	case <-changed:
	default:
		t.Fatal("a chunk set aside was let go of: a reader waiting on it is not told")
	}
	if tt.awaitingBefore(8) || !tt.awaitingBefore(10) {
		t.Fatal("chunk 9 set aside: want it before 10 only")
	}
}

// A wake closes the channel the last wait gave, once; a wait after it gives a
// new one; a wake with no wait does nothing.
func TestWaker(t *testing.T) {
	t.Parallel()
	var w waker
	w.wake()
	a := w.wait()
	if b := w.wait(); b != a {
		t.Fatal("two waits before a wake gave two channels")
	}
	w.wake()
	select {
	case <-a:
	default:
		t.Fatal("the wake did not close the channel the wait gave")
	}
	w.wake()
	b := w.wait()
	select {
	case <-b:
		t.Fatal("a wait after the wake gave a closed channel")
	default:
	}
}

// A forwarder that lets go of the chunk in its hand tells a reader waiting on
// it: one refused or stashed never comes on the stream.
func TestTee_ToldWhenTheChunkInHandIsLetGo(t *testing.T) {
	t.Parallel()
	tt := newTee(fakeconn.FromClient, 1<<20, 4, time.Second, nil, zap.NewNop())
	tt.noteReading()
	tt.noteNumbered(3)
	changed := tt.progress.changed.wait()
	tt.noteReleased()
	select {
	case <-changed:
	default:
		t.Fatal("the chunk in hand was let go of: a reader waiting on it is not told")
	}
}

// A chunk set aside for the client's answer (tee.await) is not on the stream:
// StreamedBefore waits on one numbered before the number it is asked of, and
// is told once it is let go of.
func TestRelay_StreamedBeforeWaitsOnAChunkSetAside(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	defer h.shutdown()

	h.writeClient([]byte("req"))
	h.readDest(3)
	h.writeDest([]byte("resp"))
	h.readClient(4)
	readChunk(t, h.r.ClientStream())
	resp := readChunk(t, h.r.DestStream())
	h.writeClient([]byte("rst"))
	h.readDest(3)
	rst := readChunk(t, h.r.ClientStream())
	if end := streamedThrough(t, h.r, fakeconn.FromDest, rst.ConnSeq); end != 4 {
		t.Fatalf("every server chunk before the client's is %d bytes into the stream, want 4", end)
	}

	h.r.teeD2C.await(fakeconn.Chunk{Dir: fakeconn.FromDest, ConnSeq: resp.ConnSeq, Bytes: []byte("zz")})
	end, ok, changed := h.r.StreamedBefore(fakeconn.FromDest, rst.ConnSeq)
	if ok || changed == nil {
		t.Fatalf("StreamedBefore(dest, %d) = %d, %v with a chunk numbered %d set aside: want it to wait", rst.ConnSeq, end, ok, resp.ConnSeq)
	}
	h.r.teeD2C.doneAwaiting() // refused: it never comes on the stream
	select {
	case <-changed:
	default:
		t.Fatal("the chunk set aside was let go of, and StreamedBefore was not told")
	}
	if end, ok, _ := h.r.StreamedBefore(fakeconn.FromDest, rst.ConnSeq); !ok || end != 4 {
		t.Fatalf("StreamedBefore(dest, %d) = %d, %v once the chunk set aside is let go of: want 4, true", rst.ConnSeq, end, ok)
	}
}

// A chunk in the forwarder's hand captured after the number asked of holds
// nothing back: StreamedBefore says at once that the stream is through every
// chunk before it.
func TestRelay_StreamedBeforeIsNotHeldByAChunkInHandCapturedAfter(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var clientChunks atomic.Int32
	h := newHarness(t, Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromClient && clientChunks.Add(1) == 2 {
			<-release
		}
	}})
	defer h.shutdown()
	defer close(release)

	h.writeClient([]byte("req"))
	h.readDest(3)
	h.writeDest([]byte("resp"))
	h.readClient(4)
	readChunk(t, h.r.ClientStream())
	resp := readChunk(t, h.r.DestStream())
	h.writeClient([]byte("next"))
	h.readDest(4) // written on, and held before its tee, numbered after the answer

	end, ok, _ := h.r.StreamedBefore(fakeconn.FromClient, resp.ConnSeq)
	if !ok || end != 3 {
		t.Fatalf("StreamedBefore(client, %d) = %d, %v with only a chunk captured after it in hand: want 3, true", resp.ConnSeq, end, ok)
	}
}
