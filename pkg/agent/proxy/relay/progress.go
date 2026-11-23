package relay

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// progress.go follows how far each direction of a connection's capture has
// got, for the capture watermark (watermark.go).
//
// A chunk's time is its ReadAt: when the relay read it off the socket (or, for
// TLS, when the bytes under it were read). The relay forwards in-line, so a
// chunk is read before its bytes reach the peer, and a test case that used the
// bytes completed after that. From the read on, a chunk is in one of six
// places until its parser has consumed it whole:
//
//   - in the layer the forwarder reads through, read off the socket and not
//     yet handed on (layer): a TLS layer holds the bytes under a record from
//     the read that brought them in (readTrackingConn) until it hands the
//     record to the forwarder, and can hold more records than it hands on in
//     one Read;
//   - in the forwarder's hand, from the read until the forwarder's next
//     iteration, past the push (held);
//   - set aside: a destination chunk written to a client after its FIN, which
//     waits for the client's answer before it is pushed or refused
//     (tee.awaiting, client_took.go);
//   - on the tee's queue (tee.q);
//   - delivered: taken off the queue by the drain, in the drain's hand, on the
//     FakeConn-facing channel, or in the FakeConn partly read (delivered);
//   - consumed: the FakeConn has handed every byte of it on (or swallowed it
//     for the parser).
//
// The oldest chunk of a direction not yet consumed is how far its parser has
// got through the traffic. Each place is looked at in an order that cannot
// miss a chunk moving between two of them (oldestUnconsumed).

// heldUnstamped is held while the forwarder has a chunk in hand and has not yet
// stamped it, and layer while the layer under it stamps a read. A stamp, once
// taken, is no earlier than when the first mark over its bytes was set: a
// record a TLS layer hands on takes the stamp of the read under it, taken
// after the layer set its mark and before the forwarder sets held (the stamp
// of a stash the handshake reads first is never a record's: readTrackingConn),
// and the forwarder's own stamp, from the clock, is taken after it set held.
// So a look that read the clock before it saw both marks clear can tell that
// no chunk the forwarder, or the layer, holds is from before then; one that
// saw only held clear cannot, as the layer's mark may hold the stamp of a
// record the forwarder takes next.
const heldUnstamped int64 = -1

// teeProgress is one direction's capture progress. held, first and last are
// the forwarder's; delivered and deliveredBytes are guarded by tee.mu.
type teeProgress struct {
	// held is the stamp (UnixNano) of the chunk the forwarder has read and not
	// let go of yet: set from its Read, and cleared at the top of the
	// forwarder's next iteration, once it has queued, refused, stashed or set
	// aside the chunk and told of it. 0 when it holds none, heldUnstamped
	// while it is stamping one.
	held atomic.Int64
	// layer is the stamp (UnixNano) of the read under the layer the forwarder
	// reads through (a TLS layer over a readTrackingConn), when that layer
	// may hold bytes from it it has not handed on: 0 when it holds none,
	// heldUnstamped while that read is being stamped. The layer's reads keep
	// it (readTrackingConn.Read); it is cleared as the layer reads again
	// (which it does only once it has handed on every whole record it held),
	// when its handshake fails, and when the forwarder stops.
	layer atomic.Int64
	// first and last are the stamps of the first and the newest chunk the
	// forwarder read on this direction (0: none yet): what the connection
	// carried, whether the tee queued it or not.
	first, last atomic.Int64
	// heldSeq is the number (fakeconn.Chunk.ConnSeq) of the chunk held: set
	// with held, numbering while the forwarder has read one and not numbered
	// it yet, and 0 when it holds none (Relay.StreamedBefore).
	heldSeq atomic.Uint64
	// changed is told whenever what Relay.StreamedBefore says of this
	// direction may change though no chunk comes on its stream.
	changed waker
	// afterLook, when set, runs in Relay.StreamedBefore after it looks at
	// each place a chunk of this direction is in before its stream. Tests
	// set it to move a chunk on between two looks.
	afterLook func(place)

	// delivered is the chunks the drain took off the queue that the parser
	// has not consumed whole, oldest first, by where each ends in the stream.
	// There are at most TeeChanBuf+2 of them: the channel's, the one in the
	// drain's hand and the one the FakeConn is in the middle of.
	delivered []deliveredChunk
	// deliveredBytes is how many bytes the drain has taken off the queue: the
	// stream offset the next chunk it takes starts at.
	deliveredBytes int64
}

// deliveredChunk is a delivered chunk's end, as a stream offset, and its stamp.
type deliveredChunk struct {
	end, at int64
}

// noteCarried says the forwarder stamped the chunk it read at at, and holds it
// until it is queued, refused or stashed (noteReleased). noteReading came
// before the stamp was taken.
func (t *tee) noteCarried(at time.Time) {
	ns := at.UnixNano()
	t.progress.held.Store(ns)
	t.progress.first.CompareAndSwap(0, ns)
	for {
		old := t.progress.last.Load()
		if ns <= old || t.progress.last.CompareAndSwap(old, ns) {
			return
		}
	}
}

// noteReading says the forwarder's Read returned bytes, which it is about to
// stamp (heldUnstamped) and number (numbering).
func (t *tee) noteReading() {
	t.progress.heldSeq.Store(numbering)
	t.progress.held.Store(heldUnstamped)
}

// noteNumbered says the chunk the forwarder holds is numbered seq. A reader
// that waits on it while it was not numbered is told (changed): numbered after
// the number it asked of, it holds nothing back.
func (t *tee) noteNumbered(seq uint32) {
	t.progress.heldSeq.Store(uint64(seq))
	t.progress.changed.wake()
}

// noteReleased says the forwarder holds no chunk: it queued, refused,
// stashed or set aside (tee.await) the one it read. One it refused or stashed
// never comes on the stream, so a reader waiting on it is told (changed).
func (t *tee) noteReleased() {
	t.progress.held.Store(0)
	t.progress.heldSeq.Store(0)
	t.progress.changed.wake()
}

// numbering is heldSeq while the forwarder holds a chunk it has not numbered:
// above every ConnSeq.
const numbering = uint64(1) << 32

// waker tells whoever asked (wait) that something they wait on may have
// changed.
type waker struct {
	mu sync.Mutex
	ch chan struct{}
	// woken, when set, runs at each wake before a reader is told, so it sees
	// what that reader, told, looks at. Set it only on a bare tee (newTee,
	// in no Relay), where only the test's own goroutine wakes w: wake reads
	// it without w.mu, so setting it where another goroutine may wake (any
	// tee in a Relay) is a data race. It runs with w.mu not held, but with
	// whatever lock the waking caller holds, so a woken func that takes that
	// lock deadlocks.
	woken func()
}

// wait returns a channel closed at the next wake. Asked before the state it
// waits on is looked at, a change after the look closes it.
func (w *waker) wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = make(chan struct{})
	}
	return w.ch
}

// wake closes the channel wait gave, if any.
func (w *waker) wake() {
	if w.woken != nil {
		w.woken()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch != nil {
		close(w.ch)
		w.ch = nil
	}
}

// heldBefore reports whether the forwarder holds a chunk numbered before seq,
// or one it has not numbered yet.
func (t *tee) heldBefore(seq uint32) bool {
	h := t.progress.heldSeq.Load()
	return h == numbering || h != 0 && (fakeconn.Chunk{ConnSeq: uint32(h)}).CapturedBefore(seq)
}

// awaitingBefore reports whether a chunk set aside for the client's answer
// (tee.await) is numbered before seq.
func (t *tee) awaitingBefore(seq uint32) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.awaiting {
		if c.CapturedBefore(seq) {
			return true
		}
	}
	return false
}

// StreamedBefore reports whether every chunk of dir captured before the chunk
// numbered seq, a number given on this connection, has gone on dir's stream,
// or never will (refused, stashed): end is then how far into the stream they
// all are (fakeconn.FakeConn.Consumed counts in the same bytes), and ok true.
// While one may still go on it, ok is false and changed is closed once that
// may have changed with nothing more on the stream. Each place a chunk is in
// before its stream is looked at in the order chunks move through them: not
// numbered yet (connseq.Upstream.PendingBefore), in the forwarder's hand, set
// aside, and then queued (acceptedBytes), so none is missed as it moves. A
// chunk the relay stashes across a pause and hands on later is not followed:
// a pause is a directive's (a TLS upgrade), which a parser that asks this does
// not send. It is supervisor.Session.Order.
func (r *Relay) StreamedBefore(dir fakeconn.Direction, seq uint32) (end int64, ok bool, changed <-chan struct{}) {
	t := r.teeC2D
	if dir == fakeconn.FromDest {
		t = r.teeD2C
	}
	changed = t.progress.changed.wait()
	if t.looked(placeNotNumbered, r.order.PendingBefore(dir, seq)) ||
		t.looked(placeInHand, t.heldBefore(seq)) ||
		t.looked(placeSetAside, t.awaitingBefore(seq)) {
		return 0, false, changed
	}
	return t.acceptedBytes(), true, nil
}

// place is where a chunk is before its stream, in the order chunks move
// through them (Relay.StreamedBefore).
type place int

const (
	placeNotNumbered place = iota // connseq.Upstream.PendingBefore
	placeInHand                   // the forwarder's (heldBefore)
	placeSetAside                 // for the client's answer (awaitingBefore)
)

// looked returns found, what StreamedBefore saw at p, once afterLook has run.
func (t *tee) looked(p place, found bool) bool {
	if t.progress.afterLook != nil {
		t.progress.afterLook(p)
	}
	return found
}

// noteLayerGone says nothing the layer the forwarder read through holds will
// reach the parser: the forwarder stopped, or the layer's handshake failed and
// the relay drops it.
func (t *tee) noteLayerGone() { t.progress.layer.Store(0) }

// noteDeliveringLocked follows a chunk the drain took off the queue until its
// parser consumes it whole. Caller holds t.mu.
func (t *tee) noteDeliveringLocked(c fakeconn.Chunk) {
	t.trimDeliveredLocked()
	t.progress.deliveredBytes += int64(len(c.Bytes))
	t.progress.delivered = append(t.progress.delivered,
		deliveredChunk{end: t.progress.deliveredBytes, at: c.ReadAt.UnixNano()})
}

// trimDeliveredLocked forgets the delivered chunks the parser has consumed
// whole. Caller holds t.mu.
func (t *tee) trimDeliveredLocked() {
	if t.consumed == nil || len(t.progress.delivered) == 0 {
		return
	}
	done := t.consumed()
	i := 0
	for i < len(t.progress.delivered) && t.progress.delivered[i].end <= done {
		i++
	}
	if i == len(t.progress.delivered) {
		t.progress.delivered = t.progress.delivered[:0]
		return
	}
	t.progress.delivered = t.progress.delivered[i:]
}

// oldestUnconsumed is the stamp of the oldest chunk of this direction that its
// parser has not consumed whole: delivered, queued, waiting for the client's
// answer (tee.awaiting), in the forwarder's hand, or in the layer it reads
// through. ok is false when there is none. A mark not stamped yet
// (heldUnstamped) is taken as view says: with unstampedInAnyWindow it is the
// answer (unstamped, as it can be stamped at any time from now on); with
// stampsOnly it is passed over and the next place answers, so a record the
// forwarder is stamping does not hide the stamp of the layer's read under it.
//
// Both marks are read before the lock, which is what keeps a chunk moving on
// during the look from being missed; the order of the two loads is not. The
// layer sets layer before it takes the stamp of a read and clears it only as
// it reads again, by when the forwarder has queued every chunk from the reads
// before (a chunk it hands on over that read takes that read's stamp, or,
// when the read brought nothing, the forwarder's). The forwarder sets held
// before it stamps a chunk and clears it once it has queued, refused,
// stashed or set aside (to wait) it. So when this look read a mark clear,
// every chunk that was in that hand has left it (onto the queue, or set
// aside, which the look reads under the lock, or refused or stashed), and one
// that comes into it later is stamped no earlier than the look
// (heldUnstamped). A chunk set aside moves to the queue (a push) before it
// leaves awaiting (doneAwaiting), so a look under the lock finds it in one of
// the two. A chunk further on is older than one behind it, so the first place
// that holds one gives the oldest.
func (t *tee) oldestUnconsumed(view stampView) (at int64, unstamped, ok bool) {
	l := t.progress.layer.Load()
	h := t.progress.held.Load()
	t.mu.Lock()
	t.trimDeliveredLocked()
	switch {
	case len(t.progress.delivered) > 0:
		at, ok = t.progress.delivered[0].at, true
	case len(t.q) > 0:
		at, ok = t.q[0].ReadAt.UnixNano(), true
	case len(t.awaiting) > 0:
		at, ok = t.awaiting[0].ReadAt.UnixNano(), true
	}
	t.mu.Unlock()
	if ok {
		return at, false, true
	}
	for _, m := range [2]int64{h, l} {
		switch {
		case m == heldUnstamped && view == unstampedInAnyWindow:
			return 0, true, true
		case m > 0:
			return m, false, true
		}
	}
	return 0, false, false
}

// lagDelivery holds a chunk the drain took off the queue until readerLag after
// it was read ([Config.ReaderLag]). It reports false when the parser closed its
// FakeConn meanwhile, after abandoning the chunk and the queue behind it.
func (t *tee) lagDelivery(c fakeconn.Chunk) bool {
	wait := time.Until(c.ReadAt.Add(t.readerLag))
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-t.consumerGone:
		t.abandon(c)
		return false
	}
}

// directionState is where one direction's capture is, for what holds the
// watermark.
func (t *tee) directionState() string {
	at, unstamped, ok := t.oldestUnconsumed(unstampedInAnyWindow)
	where := "nothing unconsumed"
	switch {
	case unstamped:
		where = "a chunk in the forwarder's or its TLS layer's hand, being stamped"
	case ok:
		where = "oldest unconsumed chunk read at " + time.Unix(0, at).Format(time.RFC3339Nano)
	}
	consumed := int64(-1)
	if t.consumed != nil {
		consumed = t.consumed()
	}
	layer := ""
	if l := t.progress.layer.Load(); l > 0 {
		layer = ", its TLS layer holds bytes read at " + time.Unix(0, l).Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s: %s, parser took %d of %d bytes%s", t.dir, where, consumed, t.acceptedBytes(), layer)
}

// stampView is how a look at a connection's progress takes a chunk in a
// forwarder's hand, or a read under its TLS layer, that is not stamped yet
// (heldUnstamped).
type stampView bool

const (
	// unstampedInAnyWindow: the chunk can be stamped within any window,
	// whatever else the connection carried, so it holds them all, for the
	// moment between the Read and the stamp. The verdict looks so
	// (Watermark.behind): it must not be final while a chunk can still land
	// in its window.
	unstampedInAnyWindow stampView = true
	// stampsOnly: the chunk is passed over until it is stamped, and the
	// stamps decide. Watermark.HeldProgress looks so: it counts the
	// connections the stamps show behind a window, and one that carried
	// nothing within it, or whose parser is past it, would otherwise move the
	// count each time its forwarder stamps a chunk.
	stampsOnly stampView = false
)

// carriedNothingIn reports whether this connection carried no byte stamped
// within [start, end]: its first chunk came after end, or its newest before
// start. A chunk in a forwarder's hand counts by its stamp once it has one: the
// stamp is final (noteCarried), and a window it falls in is held by
// passedThrough until the parser has the chunk. So a forwarder whose Write to a
// slow reader blocks, or that is descheduled before it queues the chunk, holds
// only the windows that chunk was stamped in, not every later one. Bytes in
// the hand of the TLS layer a forwarder reads through count the same, by the
// stamp of the read that brought them (layer), which the record the layer
// hands on takes: the layer keeps that mark while the forwarder holds the
// record, as it reads again only on the forwarder's next Read, so a TLS
// forwarder blocked in its Write holds no later window either. A mark not
// stamped yet (heldUnstamped) is taken as view says. The marks are read
// before first and last: noteCarried stores held first, and the layer clears
// its mark only as it reads again, after the forwarder has stored the stamps
// of every chunk from the read before, so a stamp this look missed in first
// or last is in a mark, or taken after the look (a record a layer hands on
// over a read it makes inside the forwarder's Read takes that read's stamp,
// or the forwarder's, both after the mark they follow was cleared and seen
// clear), and a mark's stamp counts toward the connection's
// first as well as its newest (a direction's first chunk is in held, and for
// TLS in layer, before it is in first).
func (r *Relay) carriedNothingIn(start, end int64, view stampView) bool {
	marks := [4]int64{
		r.teeC2D.progress.layer.Load(), r.teeD2C.progress.layer.Load(),
		r.teeC2D.progress.held.Load(), r.teeD2C.progress.held.Load(),
	}
	first := minNonZero(r.teeC2D.progress.first.Load(), r.teeD2C.progress.first.Load())
	newest := max(r.teeC2D.progress.last.Load(), r.teeD2C.progress.last.Load())
	for _, m := range marks {
		if m == heldUnstamped {
			if view == unstampedInAnyWindow {
				return false
			}
			continue
		}
		first = minNonZero(first, m)
		newest = max(newest, m)
	}
	if first == 0 {
		// No stamp in first or any mark. In the verdict's view
		// (unstampedInAnyWindow) no mark is unstamped here (one would have
		// answered above), so whatever chunk the relay has read or reads next
		// is stamped no earlier than this look: a forwarder's or a TLS layer's
		// read marks itself before it reads the clock (noteReading,
		// readTrackingConn), and the stash a prependingConn stamps with its
		// own, older stamp is made of chunks the forwarder stamped
		// (tee.noteCarried) before it stashed them (stashInflightFromPause),
		// so first is set by then. In stampsOnly any mark not stamped yet was
		// passed over: the read under it may have taken from the clock, before
		// this look, a stamp inside the window, and that view leaves it out
		// until the stamp is stored.
		return true
	}
	if first > end {
		return true
	}
	return newest < start
}

func minNonZero(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// passedThrough reports whether this connection's parser is done with every
// byte the connection carried stamped up to t: no chunk from then waits for it
// (delivered, queued or in a forwarder's hand), and it has either taken a
// chunk from after t, or taken everything it was given and waits for more. A
// parser that reads one exchange at a time and has moved past t has handed
// over (or reported) what it had from before it. One that reads each
// direction in a goroutine of its own and waits on one while the other is in
// the middle of a chunk is taken as done with it: the same reading the
// enterprise capture watermark makes of such a parser. A parser that reads on
// while it holds a call it parsed, its mock not handed on, says so
// (FakeConn.HoldFrom): it is not done with t while that call's first chunk
// was captured at or before t, whatever it has read since.
//
// A chunk in a forwarder's hand not stamped yet is taken as view says.
//
// When it is not done it says why (for what holds the watermark).
func (r *Relay) passedThrough(t int64, view stampView) (bool, string) {
	for _, tt := range [2]*tee{r.teeC2D, r.teeD2C} {
		at, unstamped, ok := tt.oldestUnconsumed(view)
		if ok && (unstamped || at <= t) {
			return false, tt.directionState()
		}
	}
	last := max(r.clientStream.LastReadTime().UnixNano(), r.destStream.LastReadTime().UnixNano())
	// The hold is read after last: a parser sets it before it takes the
	// chunk that moves last past the call, so a look that sees that read
	// sees the hold.
	if held := r.heldFrom(); held != 0 && held <= t {
		return false, fmt.Sprintf("parser holds a call it parsed, its mock not handed on, whose first chunk was captured at %s",
			time.Unix(0, held).Format(time.RFC3339Nano))
	}
	if last > t {
		return true, ""
	}
	if r.caughtUp(view) {
		return true, ""
	}
	return false, fmt.Sprintf("parser is working on what it took (last read %s); %s; %s",
		time.Unix(0, last).Format(time.RFC3339Nano), r.teeC2D.directionState(), r.teeD2C.directionState())
}

// heldFrom is the earliest hold the parser has set on either stream
// (FakeConn.HoldFrom), as UnixNano, 0 while it holds no call.
func (r *Relay) heldFrom() int64 {
	var held int64
	for _, fc := range [2]*fakeconn.FakeConn{r.clientStream, r.destStream} {
		if at := fc.HeldFrom(); !at.IsZero() {
			held = minNonZero(held, at.UnixNano())
		}
	}
	if h := afterHoldLook.Load(); h != nil {
		(*h)()
	}
	return held
}

// afterHoldLook, when set, runs as heldFrom has read the holds, before
// passedThrough uses them. It is for tests, which put a parser's hold and its
// read past the window there every time instead of by chance; nil outside
// them.
var afterHoldLook atomic.Pointer[func()]

// caughtUp reports whether the parser has consumed every byte the connection
// carried and is blocked waiting for more on at least one direction, and
// stayed so across the look. A chunk in a forwarder's hand not stamped yet is
// taken as view says: with stampsOnly it is not yet carried.
func (r *Relay) caughtUp(view stampView) bool {
	wc, nc := r.clientStream.Waiting()
	wd, nd := r.destStream.Waiting()
	if !wc && !wd {
		return false
	}
	for _, tt := range [2]*tee{r.teeC2D, r.teeD2C} {
		held := tt.progress.held.Load()
		if held > 0 || held == heldUnstamped && view == unstampedInAnyWindow ||
			tt.consumed == nil || tt.consumed() < tt.acceptedBytes() {
			return false
		}
	}
	wc2, nc2 := r.clientStream.Waiting()
	wd2, nd2 := r.destStream.Waiting()
	return wc == wc2 && wd == wd2 && nc == nc2 && nd == nd2
}

// consumedBytes is how many bytes the parser has taken of both directions:
// it only grows while it parses.
func (r *Relay) consumedBytes() uint64 {
	return uint64(max(r.clientStream.Consumed(), 0) + max(r.destStream.Consumed(), 0))
}
