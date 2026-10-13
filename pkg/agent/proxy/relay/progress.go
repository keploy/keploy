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
// bytes completed after that. From the read on, a chunk is in one of four
// places until its parser has consumed it whole:
//
//   - in the forwarder's hand, between the read and the push (held);
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
// stamped it. Its stamp, once taken, is no earlier than when held was set, so a
// look that read the clock before it saw held clear can tell that no chunk the
// forwarder holds is from before then.
const heldUnstamped int64 = -1

// teeProgress is one direction's capture progress. held, first and last are
// the forwarder's; delivered and deliveredBytes are guarded by tee.mu.
type teeProgress struct {
	// held is the stamp (UnixNano) of the chunk the forwarder has read and not
	// yet queued, refused or stashed: 0 when it holds none, heldUnstamped
	// while it is stamping one.
	held atomic.Int64
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

// noteReleased says the forwarder holds no chunk: it queued, refused or
// stashed the one it read.
func (t *tee) noteReleased() { t.progress.held.Store(0) }

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
// parser has not consumed whole: delivered, queued, or in the forwarder's hand.
// ok is false when there is none; unstamped is true when the only one is in
// the forwarder's hand, not yet stamped (it can be from any time from now on).
//
// held is read before the lock: the forwarder sets it before it queues the
// chunk and clears it after, so a chunk it queues after this look read held
// clear is one it stamped no earlier than the look (heldUnstamped), and one it
// queued before is on the queue under the lock.
func (t *tee) oldestUnconsumed() (at int64, unstamped, ok bool) {
	h := t.progress.held.Load()
	t.mu.Lock()
	t.trimDeliveredLocked()
	switch {
	case len(t.progress.delivered) > 0:
		at, ok = t.progress.delivered[0].at, true
	case len(t.q) > 0:
		at, ok = t.q[0].ReadAt.UnixNano(), true
	}
	t.mu.Unlock()
	if ok {
		return at, false, true
	}
	switch {
	case h == heldUnstamped:
		return 0, true, true
	case h > 0:
		return h, false, true
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
		where = "a chunk in the forwarder's hand, being stamped"
	case ok:
		where = "oldest unconsumed chunk read at " + time.Unix(0, at).Format(time.RFC3339Nano)
	}
	consumed := int64(-1)
	if t.consumed != nil {
		consumed = t.consumed()
	}
	return fmt.Sprintf("%s: %s, parser took %d of %d bytes", t.dir, where, consumed, t.acceptedBytes())
}

// carriedNothingIn reports whether this connection carried no byte stamped
// within [start, end]: its first chunk came after end, or its newest before
// start with none in the forwarders' hands.
func (r *Relay) carriedNothingIn(start, end int64) bool {
	first := minNonZero(r.teeC2D.progress.first.Load(), r.teeD2C.progress.first.Load())
	if first == 0 {
		// Nothing read yet: whatever it reads is stamped from now on.
		return r.teeC2D.progress.held.Load() == 0 && r.teeD2C.progress.held.Load() == 0
	}
	if first > end {
		return true
	}
	if r.teeC2D.progress.held.Load() != 0 || r.teeD2C.progress.held.Load() != 0 {
		return false
	}
	return max(r.teeC2D.progress.last.Load(), r.teeD2C.progress.last.Load()) < start
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
// When it is not done it says why (for what holds the watermark).
func (r *Relay) passedThrough(t int64) (bool, string) {
	for _, tt := range [2]*tee{r.teeC2D, r.teeD2C} {
		if at, unstamped, ok := tt.oldestUnconsumed(); ok && (unstamped || at <= t) {
			return false, tt.directionState()
		}
	}
	last := max(r.clientStream.LastReadTime().UnixNano(), r.destStream.LastReadTime().UnixNano())
	if last > t {
		return true, ""
	}
	if r.caughtUp() {
		return true, ""
	}
	return false, fmt.Sprintf("parser is working on what it took (last read %s); %s; %s",
		time.Unix(0, last).Format(time.RFC3339Nano), r.teeC2D.directionState(), r.teeD2C.directionState())
}

// caughtUp reports whether the parser has consumed every byte the connection
// carried and is blocked waiting for more on at least one direction, and
// stayed so across the look.
func (r *Relay) caughtUp() bool {
	wc, nc := r.clientStream.Waiting()
	wd, nd := r.destStream.Waiting()
	if !wc && !wd {
		return false
	}
	for _, tt := range [2]*tee{r.teeC2D, r.teeD2C} {
		if tt.progress.held.Load() != 0 || tt.consumed == nil || tt.consumed() < tt.acceptedBytes() {
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
