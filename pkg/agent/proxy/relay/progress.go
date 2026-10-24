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
// places until its parser has processed it whole:
//
//   - in the forwarder's hand, between the read and the push (held);
//   - on the tee's queue (tee.q);
//   - delivered: taken off the queue by the drain, in the drain's hand, on the
//     FakeConn-facing channel, in the FakeConn partly read, or read ahead and
//     not yet processed by its parser's loop (delivered);
//   - processed: the FakeConn has handed every byte of it on, or swallowed it
//     for the parser, or will, as it lies below the discard watermark
//     (DiscardBefore), and a parser that reads ahead has marked it processed
//     (fakeconn.FakeConn.ProcessedThrough). Where ProcessedThrough cannot say
//     exactly, handed on is all the tee knows, and passedThrough judges the
//     stream by the last chunk its loop processed instead.
//
// The oldest chunk of a direction not yet processed is how far its parser has
// got through the traffic. Each place is looked at in an order that cannot
// miss a chunk moving between two of them (oldestUnprocessed).

// heldUnstamped is held while the forwarder has a chunk in hand and has not yet
// stamped it. A look that saw held clear takes a chunk the forwarder reads
// after it for one stamped after it: a stamp is taken as the Read returns,
// just before held is set. A stamp a TLS layer gives from the read under it
// (observedReadAt) can be older, and a look does not see that.
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
	// has not processed whole, oldest first, by where each ends in the stream:
	// at most TeeChanBuf+2 of them that the FakeConn has not handed on (the
	// channel's, the one in the drain's hand and the one it is in the middle
	// of), and those that cover the bytes a parser that reads ahead took and
	// has not marked processed, which its own queue bounds. Marks count bytes,
	// so a parser that never marks one chunk keeps only as many bytes' worth
	// of the newest chunks it took here, not every chunk after that one.
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
// parser processes it whole. Caller holds t.mu.
func (t *tee) noteDeliveringLocked(c fakeconn.Chunk) {
	t.trimDeliveredLocked()
	t.progress.deliveredBytes += int64(len(c.Bytes))
	t.progress.delivered = append(t.progress.delivered,
		deliveredChunk{end: t.progress.deliveredBytes, at: c.ReadAt.UnixNano()})
}

// trimDeliveredLocked forgets the delivered chunks the parser has processed
// whole: those that end at or below ProcessedThrough. When ProcessedThrough
// cannot say exactly (bytes were swallowed below the discard watermark,
// dropped by the floor or settled for the parser behind bytes it read ahead
// and has not processed), it forgets those the FakeConn had handed on
// (Consumed) before that answer, and passedThrough judges the stream by what
// its loop processed last: forgetting none until ProcessedThrough is exact
// again would hold a stream whose parser is always a chunk ahead of its loop
// for as long as it carries traffic, and keep every chunk it carried.
//
// Consumed is read first. The reader and the loop run on while the tee holds
// its lock. Read after the answer, Consumed could include a chunk the reader
// took once the loop had processed everything before it: that stream is exact
// again, so passedThrough no longer judges it by what the loop processed last,
// and the tee would have forgotten the chunk. Read first, Consumed is only
// what had left when the answer was not exact: passedThrough judges those by
// what the loop processed last, or finds the stream exact, which it is only
// once all that left is processed. Caller holds t.mu.
func (t *tee) trimDeliveredLocked() {
	if t.processedThrough == nil || len(t.progress.delivered) == 0 {
		return
	}
	consumed := t.consumed()
	done, exact := t.processedThrough()
	if !exact {
		done = consumed
	}
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

// oldestUnprocessed is the stamp of the oldest chunk of this direction that its
// parser has not processed whole: delivered, queued, or in the forwarder's hand.
// ok is false when there is none; unstamped is true when the only one is in
// the forwarder's hand, not yet stamped (it can be from any time from now on).
//
// held is read before the lock: the forwarder sets it before it queues the
// chunk and clears it after, so a chunk it queues after this look read held
// clear is one it stamped no earlier than the look (heldUnstamped), and one it
// queued before is on the queue under the lock.
func (t *tee) oldestUnprocessed() (at int64, unstamped, ok bool) {
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
	at, unstamped, ok := t.oldestUnprocessed()
	where := "nothing unprocessed"
	switch {
	case unstamped:
		where = "a chunk in the forwarder's hand, being stamped"
	case ok:
		where = "oldest unprocessed chunk read at " + time.Unix(0, at).Format(time.RFC3339Nano)
	}
	consumed, through := int64(-1), "unknown"
	if t.consumed != nil {
		consumed = t.consumed()
	}
	if t.processedThrough != nil {
		if n, exact := t.processedThrough(); exact {
			through = fmt.Sprint(n)
		}
	}
	return fmt.Sprintf("%s: %s, parser took %d of %d bytes, processed through %s",
		t.dir, where, consumed, t.acceptedBytes(), through)
}

// carriedNothingIn reports whether this connection carried no byte stamped
// within [start, end]: its first chunk came after end, or its newest before
// start. A chunk in a forwarder's hand counts by its stamp, wherever that lies:
// one stamped before start is no reason to hold the window, however long the
// forwarder keeps it (a Write blocked on a full peer). Only a chunk in hand
// not yet stamped holds every window, as its stamp is not known yet. Nothing
// read and nothing in hand is nothing carried: a chunk read after the look is
// taken for one from after it (heldUnstamped).
//
// held is read before first and last. The forwarder stores a chunk's stamp in
// held before it moves first and last, and clears held only after, so a chunk
// this look found in hand counts by held even before first and last show it,
// and one it did not is in first and last already or was read after the look.
func (r *Relay) carriedNothingIn(start, end int64) bool {
	hc, hd := r.teeC2D.progress.held.Load(), r.teeD2C.progress.held.Load()
	if hc == heldUnstamped || hd == heldUnstamped {
		return false
	}
	first := minNonZero(
		minNonZero(r.teeC2D.progress.first.Load(), r.teeD2C.progress.first.Load()),
		minNonZero(hc, hd))
	last := max(r.teeC2D.progress.last.Load(), r.teeD2C.progress.last.Load(), hc, hd)
	return first == 0 || first > end || last < start
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
// (delivered, read ahead and not processed, queued or in a forwarder's hand),
// and it has either taken a chunk from after t, or taken everything it was
// given and waits for more. A parser that reads one exchange at a time and has
// moved past t has handed over (or reported) what it had from before it. One
// that reads each direction in a goroutine of its own and waits on one while
// the other is in the middle of a chunk is taken as done with it: the same
// reading the enterprise capture watermark makes of such a parser. One that
// reads ahead of itself has processed every chunk from up to t once none
// waits for it, whatever its loop has queued from after t: the rest only adds
// a wait. Where ProcessedThrough cannot say exactly on a stream, the tee
// follows its chunks only until they are handed on, and the stream is done
// with t only once its loop has processed a chunk from after t: the loop
// processes a stream in order, so it has processed every chunk before that.
// That needs LastReadTime to move on such a stream only as the loop marks a
// chunk processed: it does for a parser that takes the stream only by
// ReadAhead, as ReadAhead requires (SkipThrough and the synchronous reads
// move it as they hand a chunk on, and are for a parser that reads so).
//
// When it is not done it says why (for what holds the watermark).
func (r *Relay) passedThrough(t int64) (bool, string) {
	for _, tt := range [2]*tee{r.teeC2D, r.teeD2C} {
		if at, unstamped, ok := tt.oldestUnprocessed(); ok && (unstamped || at <= t) {
			return false, tt.directionState()
		}
	}
	for _, d := range [2]struct {
		s  *fakeconn.FakeConn
		tt *tee
	}{{r.clientStream, r.teeC2D}, {r.destStream, r.teeD2C}} {
		if _, exact := d.s.ProcessedThrough(); !exact && processedNothingAfter(d.s, t) {
			return false, fmt.Sprintf("%s: parser cannot say exactly how far it processed, and has processed nothing from after this time; %s; %s",
				d.tt.dir, r.teeC2D.directionState(), r.teeD2C.directionState())
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

// processedNothingAfter reports whether the last chunk s's parser processed
// (LastReadTime) is stamped no later than t, or it has processed none.
func processedNothingAfter(s *fakeconn.FakeConn, t int64) bool {
	last := s.LastReadTime()
	return last.IsZero() || last.UnixNano() <= t
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

// consumedBytes is how many bytes the parser has taken of both directions,
// and how many of them it has processed: it only grows while it parses, a
// parser whose loop works through what its reader read ahead included.
func (r *Relay) consumedBytes() uint64 {
	var n int64
	for _, s := range [2]*fakeconn.FakeConn{r.clientStream, r.destStream} {
		n += max(s.Consumed(), 0) + max(s.Processed(), 0)
	}
	return uint64(n)
}
