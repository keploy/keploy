package relay

import (
	"fmt"
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
// stamped it, and layer while the layer under it stamps a read. The stamp, once
// taken, is no earlier than when the mark was set, so a look that read the
// clock before it saw the mark clear can tell that no chunk the forwarder, or
// the layer, holds is from before then.
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
// stamp (heldUnstamped).
func (t *tee) noteReading() { t.progress.held.Store(heldUnstamped) }

// noteReleased says the forwarder holds no chunk: it queued, refused,
// stashed or set aside (tee.await) the one it read.
func (t *tee) noteReleased() { t.progress.held.Store(0) }

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
// through. ok is false when there is none; unstamped is true when the only
// one is in the forwarder's or the layer's hand, not yet stamped (it can be
// from any time from now on).
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
func (t *tee) oldestUnconsumed() (at int64, unstamped, ok bool) {
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
		case m == heldUnstamped:
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
	at, unstamped, ok := t.oldestUnconsumed()
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
		// Nothing read yet: whatever it reads is stamped from now on.
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
// enterprise capture watermark makes of such a parser.
//
// A chunk in a forwarder's hand not stamped yet is taken as view says.
//
// When it is not done it says why (for what holds the watermark).
func (r *Relay) passedThrough(t int64, view stampView) (bool, string) {
	for _, tt := range [2]*tee{r.teeC2D, r.teeD2C} {
		at, unstamped, ok := tt.oldestUnconsumed()
		if ok && (unstamped && view == unstampedInAnyWindow || !unstamped && at <= t) {
			return false, tt.directionState()
		}
	}
	last := max(r.clientStream.LastReadTime().UnixNano(), r.destStream.LastReadTime().UnixNano())
	if last > t {
		return true, ""
	}
	if r.caughtUp(view) {
		return true, ""
	}
	return false, fmt.Sprintf("parser is working on what it took (last read %s); %s; %s",
		time.Unix(0, last).Format(time.RFC3339Nano), r.teeC2D.directionState(), r.teeD2C.directionState())
}

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
