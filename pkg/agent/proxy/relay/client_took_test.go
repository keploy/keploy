package relay

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// scriptedSocket answers the relay's looks at the client's socket from a
// script, in order; the last answer repeats.
type scriptedSocket struct {
	mu     sync.Mutex
	script []clientLook
	asked  int
}

func (s *scriptedSocket) look() clientLook {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.asked, len(s.script)-1)
	s.asked++
	return s.script[i]
}

func (s *scriptedSocket) looks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func (s *scriptedSocket) set(script ...clientLook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script, s.asked = script, 0
}

// The answers a look gets, with how many bytes the client's stack had
// acknowledged in all.
func open(acked uint64) clientLook   { return clientLook{said: clientOpen, acked: acked} }
func finned(acked uint64) clientLook { return clientLook{said: clientFinned, acked: acked} }
func closed(acked uint64) clientLook { return clientLook{said: clientClosed, acked: acked} }

var unknown = clientLook{said: clientUnknown}

// What a look at the client's socket says of chunks written after the look
// that found its FIN (clientFINAcked 100): a byte acknowledged since decides
// first, and says the client reads on, whatever the socket says next; a closed
// connection with nothing acknowledged since refuses; a client that has
// answered neither way is waited for, unless the look is the last; a socket
// that cannot say refuses nothing.
func TestVerdictOfALookAtTheClientsSocket(t *testing.T) {
	r := &Relay{clientFINAcked: 100}
	for _, row := range []struct {
		l    clientLook
		last bool
		want clientVerdict
	}{
		{finned(100), false, verdictNone},
		{finned(100), true, verdictAsBefore},
		{finned(101), false, verdictReadsOn},
		{closed(100), false, verdictRefused},
		{closed(100), true, verdictRefused},
		{closed(130), false, verdictReadsOn},
		{unknown, false, verdictAsBefore},
		{open(100), false, verdictAsBefore},
	} {
		if got := r.verdictOf(row.l, row.last); got != row.want {
			t.Errorf("verdictOf(%+v, last %v) = %d, want %d", row.l, row.last, got, row.want)
		}
	}
}

// tookHarness is a relay asking a scripted client socket, as the destination's
// forwarder asks it, with the destination's tee (no drain: what is queued stays
// on its queue).
type tookHarness struct {
	r     *Relay
	sock  *scriptedSocket
	t     *tee
	seq   atomic.Uint64
	stops atomic.Int32
	mu    sync.Mutex
	marks []string
	// carried is what the relay told of the chunks it handed the tee
	// (Config.OnCarried), and pushing what the capture watermark found as
	// each push began (oldestUnconsumed, from the tee's memCheck hook,
	// which runs inside push before the chunk is queued).
	carried []carriedChunk
	pushing []int64
}

// carriedChunk is one Config.OnCarried call.
type carriedChunk struct {
	at      time.Time
	teed    bool
	through int64
}

func newTookHarness(script ...clientLook) *tookHarness {
	h := &tookHarness{sock: &scriptedSocket{script: script}}
	h.t = newTee(fakeconn.FromDest, 1<<20, 16, time.Second, func() bool {
		at, _, ok := h.t.oldestUnconsumed()
		if !ok {
			at = 0
		}
		h.mu.Lock()
		h.pushing = append(h.pushing, at)
		h.mu.Unlock()
		return false
	}, nil, nil)
	h.r = &Relay{client: h.sock, lastLook: make(chan struct{}), cfg: Config{
		ClientStopped: func() { h.stops.Add(1) },
		OnMarkMockIncomplete: func(reason string) {
			h.mu.Lock()
			h.marks = append(h.marks, reason)
			h.mu.Unlock()
		},
		OnCarried: func(_ fakeconn.Direction, at time.Time, teed bool, through int64) {
			h.mu.Lock()
			h.carried = append(h.carried, carriedChunk{at, teed, through})
			h.mu.Unlock()
		},
	}}
	return h
}

// write is the destination's forwarder writing chunk b to the client, whose
// Write earned mark: what becomes of it.
func (h *tookHarness) write(ctx context.Context, b string, mark string) chunkFate {
	finBefore := h.r.clientFINBefore()
	return h.r.destWritten(ctx, h.t, &h.seq, fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte(b), ReadAt: time.Now()}, finBefore, mark)
}

// settled waits until no resolve waits for the client's answer.
func (h *tookHarness) settled(t *testing.T) {
	t.Helper()
	h.r.verdictMu.Lock()
	done := h.r.resolverDone
	h.r.verdictMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the client's answer was not settled: resolve waits on a client that has answered, or on nothing")
	}
}

// queued is what the tee has queued for the parser, in order.
func (h *tookHarness) queued() []string {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	var q []string
	for _, c := range h.t.q {
		q = append(q, string(c.Bytes))
	}
	return q
}

func (h *tookHarness) gotMarks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.marks...)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// What the relay does with each destination chunk from what the client's
// socket says (client_took.go). Until a look before a write finds the client's
// FIN, every chunk is teed at once, whatever the socket says after. A chunk
// written once the FIN was found is set aside, and so is every chunk after it,
// until the client's answer, waited for off the forwarding path, decides them
// all, in order: teed if the client acknowledged a byte since that look
// (and every later chunk teed at once), refused if the connection closed with
// none (and every later chunk refused at once, the relay saying so once). The
// relay's end or the recording's stop makes the next look the last, and a
// client that has answered neither way by then, or a socket that cannot say,
// refuses nothing. The incomplete-mock mark of a chunk set aside is set only
// if it is teed.
func TestClientTookDecidesFromWhatTheClientsSocketSays(t *testing.T) {
	bg := context.Background()
	t.Run("no FIN before the write: teed at once, one look each, whatever comes after", func(t *testing.T) {
		h := newTookHarness(open(0), closed(0))
		if f := h.write(bg, "a", ""); f != fateTee || h.sock.looks() != 1 || h.stops.Load() != 0 {
			t.Fatalf("fate %d, %d looks, %d stops; want teed on the one look before the write", f, h.sock.looks(), h.stops.Load())
		}
	})
	t.Run("a reset with no FIN before it refuses nothing", func(t *testing.T) {
		// The client read part of what it was sent and closed with the rest
		// unread: its stack resets instead of sending a FIN, and the part it
		// read may be in this very chunk.
		h := newTookHarness(closed(0))
		if f := h.write(bg, "a", "write_error"); f != fateTee || h.stops.Load() != 0 {
			t.Fatalf("fate %d, %d stops; want teed (and the forwarder marks it)", f, h.stops.Load())
		}
	})
	t.Run("FIN before the write, then closed with nothing acknowledged: refused, and every later chunk", func(t *testing.T) {
		h := newTookHarness(finned(100), finned(100), finned(100), closed(100))
		if f := h.write(bg, "first", ""); f != fateAwaiting {
			t.Fatalf("the first chunk after the FIN: fate %d, want set aside", f)
		}
		h.settled(t)
		if q := h.queued(); len(q) != 0 || h.t.awaitingLen() != 0 || h.stops.Load() != 1 || h.r.notTakenBytes.Load() != 5 {
			t.Fatalf("queued %q, %d still aside, %d stops, %d bytes not taken; want nothing queued, once, 5", q, h.t.awaitingLen(), h.stops.Load(), h.r.notTakenBytes.Load())
		}
		asked := h.sock.looks()
		if f := h.write(bg, "later", "write_error"); f != fateRefused || h.sock.looks() != asked || h.stops.Load() != 1 {
			t.Fatalf("a later chunk: fate %d, the socket asked (%d -> %d), %d stops; want refused, unasked, once", f, asked, h.sock.looks(), h.stops.Load())
		}
		if m := h.gotMarks(); len(m) != 0 {
			t.Fatalf("marked %q for chunks the client refused, want nothing", m)
		}
	})
	t.Run("a chunk set aside whose Write failed, then refused: no mark", func(t *testing.T) {
		// The client's reset came in between the look that found its FIN and
		// the Write: the Write fails, and the chunk waits for the answer
		// like any other written after the FIN. Refused, it marks nothing.
		h := newTookHarness(finned(100), closed(100))
		if f := h.write(bg, "first", "write_error"); f != fateAwaiting {
			t.Fatalf("fate %d, want set aside", f)
		}
		h.settled(t)
		if q := h.queued(); len(q) != 0 || h.stops.Load() != 1 {
			t.Fatalf("queued %q, %d stops; want refused", q, h.stops.Load())
		}
		if m := h.gotMarks(); len(m) != 0 {
			t.Fatalf("marked %q for a chunk the client refused, want nothing", m)
		}
	})
	t.Run("FIN before the write, then a byte acknowledged: teed, and every later chunk at once", func(t *testing.T) {
		h := newTookHarness(finned(100), finned(100), finned(101))
		if f := h.write(bg, "first", ""); f != fateAwaiting {
			t.Fatalf("fate %d, want set aside", f)
		}
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"first"}) || !h.r.clientReadsOn.Load() || h.stops.Load() != 0 {
			t.Fatalf("queued %q, reads on %v, %d stops", q, h.r.clientReadsOn.Load(), h.stops.Load())
		}
		h.sock.set(closed(101))
		if f := h.write(bg, "later", ""); f != fateTee || h.sock.looks() != 0 {
			t.Fatalf("after the client read on: fate %d with %d looks, want teed at once with no look", f, h.sock.looks())
		}
	})
	t.Run("acknowledged since the FIN, then closed: never refused", func(t *testing.T) {
		h := newTookHarness(finned(100), closed(130))
		h.write(bg, "first", "")
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"first"}) || !h.r.clientReadsOn.Load() || h.stops.Load() != 0 {
			t.Fatalf("queued %q, reads on %v, %d stops; want teed", q, h.r.clientReadsOn.Load(), h.stops.Load())
		}
	})
	t.Run("what the FIN itself acknowledged is not reading on", func(t *testing.T) {
		// A first chunk written before the FIN; the FIN carries the
		// acknowledgement of all the client had received, and the look that
		// finds it counts from there.
		h := newTookHarness(open(0), finned(40), finned(40), closed(40))
		if f := h.write(bg, "before", ""); f != fateTee {
			t.Fatalf("a chunk written before the FIN: fate %d, want teed", f)
		}
		h.write(bg, "after", "")
		h.settled(t)
		if h.stops.Load() != 1 || h.r.notTakenBytes.Load() != 5 {
			t.Fatalf("%d stops, %d bytes not taken; want the chunk after the FIN refused", h.stops.Load(), h.r.notTakenBytes.Load())
		}
	})
	t.Run("chunks written while the answer is awaited are set aside behind, and settled in order", func(t *testing.T) {
		// Each settled chunk is numbered (SeqNo) and told of (OnCarried) in
		// order, as the forwarder does for one it tees at once, and leaves
		// the set-aside list only once it is on the queue: the capture
		// watermark finds the oldest of them as each push begins.
		h := newTookHarness(finned(0))
		h.write(bg, "one", "")
		h.t.mu.Lock()
		oldest := h.t.awaiting[0].ReadAt
		h.t.mu.Unlock()
		if f := h.write(bg, "two", ""); f != fateAwaiting {
			t.Fatalf("a chunk behind one set aside: fate %d, want set aside", f)
		}
		if f := h.write(bg, "three", "write_error"); f != fateAwaiting {
			t.Fatalf("fate %d, want set aside", f)
		}
		if q := h.queued(); len(q) != 0 || h.t.awaitingLen() != 3 {
			t.Fatalf("queued %q with %d aside while the client has not answered, want nothing queued, 3 aside", q, h.t.awaitingLen())
		}
		h.sock.set(finned(1))
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"one", "two", "three"}) || h.t.awaitingLen() != 0 {
			t.Fatalf("queued %q, %d aside; want all three, in order", q, h.t.awaitingLen())
		}
		if m := h.gotMarks(); !equal(m, []string{"write_error"}) {
			t.Fatalf("marked %q, want the last chunk's write_error, once it was teed", m)
		}
		h.t.mu.Lock()
		for i, c := range h.t.q {
			if c.SeqNo != uint64(i+1) {
				t.Errorf("chunk %q numbered %d, want %d", c.Bytes, c.SeqNo, i+1)
			}
		}
		h.t.mu.Unlock()
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.carried) != 3 {
			t.Fatalf("told of %d chunks, want the 3 settled", len(h.carried))
		}
		for i, through := range []int64{3, 6, 11} {
			if c := h.carried[i]; !c.teed || c.through != through {
				t.Errorf("chunk %d told of as teed %v through %d, want teed through %d", i+1, c.teed, c.through, through)
			}
		}
		if len(h.pushing) != 3 {
			t.Fatalf("%d pushes, want 3", len(h.pushing))
		}
		for i, at := range h.pushing {
			if at != oldest.UnixNano() {
				t.Errorf("as push %d began the watermark's oldest unconsumed chunk was %d, want the first set aside (%d): a chunk between the two lists is lost to it", i+1, at, oldest.UnixNano())
			}
		}
	})
	t.Run("no answer by the relay's end: teed as before, the mark with it", func(t *testing.T) {
		h := newTookHarness(finned(0))
		h.write(bg, "one", "short_write")
		close(h.r.lastLook)
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"one"}) || h.stops.Load() != 0 || h.r.clientReadsOn.Load() {
			t.Fatalf("queued %q, %d stops, reads on %v; want teed as before, not reading on", q, h.stops.Load(), h.r.clientReadsOn.Load())
		}
		if m := h.gotMarks(); !equal(m, []string{"short_write"}) {
			t.Fatalf("marked %q, want short_write", m)
		}
	})
	t.Run("no answer by the recording's stop: teed as before", func(t *testing.T) {
		h := newTookHarness(finned(0))
		ctx, cancel := context.WithCancel(bg)
		h.write(ctx, "one", "")
		cancel()
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"one"}) || h.stops.Load() != 0 {
			t.Fatalf("queued %q, %d stops; want teed as before", q, h.stops.Load())
		}
	})
	t.Run("the relay's end, the client having reset meanwhile: refused on the last look", func(t *testing.T) {
		h := newTookHarness(finned(0))
		h.write(bg, "one", "")
		h.sock.set(closed(0))
		close(h.r.lastLook)
		h.settled(t)
		if q := h.queued(); len(q) != 0 || h.stops.Load() != 1 {
			t.Fatalf("queued %q, %d stops; want refused", q, h.stops.Load())
		}
	})
	t.Run("the socket cannot say: teed, nothing stops", func(t *testing.T) {
		h := newTookHarness(unknown)
		if f := h.write(bg, "a", ""); f != fateTee {
			t.Fatalf("no FIN found: fate %d, want teed", f)
		}
		h = newTookHarness(finned(0), unknown)
		h.write(bg, "a", "")
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"a"}) || h.stops.Load() != 0 {
			t.Fatalf("queued %q, %d stops; want teed", q, h.stops.Load())
		}
	})
	t.Run("after a verdict as before, the next chunk waits for the client's answer again, from the same look", func(t *testing.T) {
		h := newTookHarness(finned(20), unknown)
		h.write(bg, "a", "")
		h.settled(t)
		h.sock.set(finned(20), closed(20))
		if f := h.write(bg, "b", ""); f != fateAwaiting {
			t.Fatalf("fate %d, want set aside", f)
		}
		h.settled(t)
		if q := h.queued(); !equal(q, []string{"a"}) || h.stops.Load() != 1 {
			t.Fatalf("queued %q, %d stops; want a teed as before, b refused", q, h.stops.Load())
		}
	})
	t.Run("no socket to ask: teed", func(t *testing.T) {
		r := &Relay{}
		if fin := r.clientFINBefore(); fin || r.destWritten(bg, nil, nil, fakeconn.Chunk{}, fin, "") != fateTee {
			t.Fatal("not teed")
		}
	})
}

// awaitClientVerdict, as the relay ends, gives a client that has not answered
// HalfCloseGrace more, then the last look: here the client answers within it.
func TestTheRelaysEndWaitsForTheClientsAnswer(t *testing.T) {
	h := newTookHarness(finned(0))
	h.r.cfg.HalfCloseGrace = 10 * time.Second
	h.write(context.Background(), "one", "")
	ended := make(chan struct{})
	go func() { h.r.awaitClientVerdict(); close(ended) }()
	select {
	case <-ended:
		t.Fatal("the relay ended before the client answered, within the grace")
	case <-time.After(20 * time.Millisecond):
	}
	h.sock.set(finned(5))
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end once the client answered")
	}
	if q := h.queued(); !equal(q, []string{"one"}) || !h.r.clientReadsOn.Load() {
		t.Fatalf("queued %q, reads on %v; want teed, reading on", q, h.r.clientReadsOn.Load())
	}
}

// A chunk set aside for the client's answer is one its parser has not
// consumed, so the capture watermark waits for it (oldestUnconsumed): from the
// moment it is set aside, through its move onto the tee's queue as it is
// teed. A refused chunk is no longer waited for.
func TestAChunkSetAsideHoldsTheCaptureWatermark(t *testing.T) {
	bg := context.Background()
	stamp := time.Unix(100, 0)
	write := func(h *tookHarness) {
		finBefore := h.r.clientFINBefore()
		if f := h.r.destWritten(bg, h.t, &h.seq, fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("one"), ReadAt: stamp}, finBefore, ""); f != fateAwaiting {
			t.Fatalf("fate %d, want set aside", f)
		}
	}
	oldest := func(h *tookHarness) (int64, bool) {
		at, unstamped, ok := h.t.oldestUnconsumed()
		if unstamped {
			t.Fatal("the oldest unconsumed chunk is unstamped, want the one set aside")
		}
		return at, ok
	}
	h := newTookHarness(finned(0))
	write(h)
	if at, ok := oldest(h); !ok || at != stamp.UnixNano() {
		t.Fatalf("oldest unconsumed (%d, %v) while the chunk waits, want its stamp %d", at, ok, stamp.UnixNano())
	}
	h.sock.set(finned(1))
	h.settled(t)
	if at, ok := oldest(h); !ok || at != stamp.UnixNano() {
		t.Fatalf("oldest unconsumed (%d, %v) once the chunk was teed, want its stamp %d (on the queue)", at, ok, stamp.UnixNano())
	}

	h = newTookHarness(finned(0))
	write(h)
	h.sock.set(closed(0))
	h.settled(t)
	if at, ok := oldest(h); ok {
		t.Fatalf("oldest unconsumed %d once the chunk was refused, want none", at)
	}
}

// lockedLooks is a client socket that notes, at each look, whether the relay's
// finMu was free: the forwarders' looks (clientFINRead, clientFINBefore) take
// it, so they never run at once on the socket's shared answer.
type lockedLooks struct {
	r        *Relay
	l        clientLook
	unlocked atomic.Int32
}

func (s *lockedLooks) look() clientLook {
	if s.r.finMu.TryLock() {
		s.r.finMu.Unlock()
		s.unlocked.Add(1)
	}
	return s.l
}

// The client's forwarder's look as it reads the FIN and the destination's
// forwarder's look before a write are taken under finMu, and the one that
// finds the FIN keeps what the client had acknowledged then.
func TestTheForwardersLooksForTheFINTakeItsLock(t *testing.T) {
	r := &Relay{lastLook: make(chan struct{})}
	s := &lockedLooks{r: r, l: open(3)}
	r.client = s
	r.clientFINRead()
	if r.clientFINBefore() {
		t.Fatal("the FIN found while the socket said the client had sent none")
	}
	s.l = finned(5)
	r.clientFINRead()
	if !r.clientFINBefore() || r.clientFINAcked != 5 {
		t.Fatalf("FIN found %v with %d acknowledged, want found with 5", r.clientFIN.Load(), r.clientFINAcked)
	}
	if n := s.unlocked.Load(); n != 0 {
		t.Fatalf("%d of the forwarders' looks ran without finMu", n)
	}
}
