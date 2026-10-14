package fakeconn

import (
	"errors"
	"io"
	"math"
	"sync"
	"testing"
	"time"
)

// numbered is a chunk captured as the seq-th chunk of its connection, at
// second seq of the test's clock.
func numbered(dir Direction, seq uint32, b string) Chunk {
	at := time.Unix(1_700_000_000+int64(seq), 0)
	return Chunk{Dir: dir, ConnSeq: seq, Bytes: []byte(b), ReadAt: at, WrittenAt: at.Add(time.Millisecond)}
}

// runs records what a floor reports.
type runs struct {
	mu  sync.Mutex
	got []Dropped
}

func (r *runs) report(d Dropped) {
	r.mu.Lock()
	r.got = append(r.got, d)
	r.mu.Unlock()
}

func (r *runs) all() []Dropped {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Dropped(nil), r.got...)
}

// The serial comparison is right across the 32-bit wrap, and ConnSeqOf never
// numbers a chunk 0, which is no number: past 2^32-1 it goes on at 1.
func TestConnSeqOfWrapsPastZeroAndCapturedBeforeOrdersAcrossTheWrap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n    uint64
		want uint32
	}{{1, 1}, {2, 2}, {math.MaxUint32, math.MaxUint32}, {math.MaxUint32 + 1, 1}, {2*math.MaxUint32 + 1, 1}} {
		if got := ConnSeqOf(c.n); got != c.want {
			t.Fatalf("ConnSeqOf(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	for n := uint64(math.MaxUint32 - 3); n < math.MaxUint32+3; n++ {
		a, b := Chunk{ConnSeq: ConnSeqOf(n)}, ConnSeqOf(n+1)
		if !a.CapturedBefore(b) || (Chunk{ConnSeq: b}).CapturedBefore(a.ConnSeq) || a.CapturedBefore(a.ConnSeq) {
			t.Fatalf("chunk %d (ConnSeq %d) and %d (ConnSeq %d) are not ordered as captured", n, a.ConnSeq, n+1, b)
		}
	}
}

// The floor drops every chunk captured before it, whenever it reaches the
// reader: one already queued, and one that arrives while the reader waits.
// The run is reported once, as it ends (the reader is handed the next chunk),
// with its span; the dropped chunks move the stream on (Consumed) but not its
// last delivered times.
func TestDropBeforeDropsWhatWasCapturedBeforeTheFloor(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 8)
	f := New(ch, nil, nil)
	ch <- numbered(FromDest, 1, "stale-1")
	r := &runs{}
	f.DropBefore(3, r.report)

	got := make(chan Chunk, 1)
	go func() {
		c, err := f.ReadChunk()
		if err != nil {
			t.Errorf("ReadChunk: %v", err)
		}
		got <- c
	}()
	// The reader drops the queued stale chunk and waits; a second one,
	// captured before the floor too, reaches it only now.
	waitWaiting(t, f)
	ch <- numbered(FromDest, 2, "stale-22")
	ch <- numbered(FromDest, 4, "answer")
	c := <-got
	if string(c.Bytes) != "answer" || c.ConnSeq != 4 {
		t.Fatalf("read %q (ConnSeq %d), want the answer captured after the floor", c.Bytes, c.ConnSeq)
	}
	runs := r.all()
	if len(runs) != 1 || runs[0].First != 1 || !runs[0].AtStart || !runs[0].From.Equal(numbered(0, 1, "").ReadAt) || !runs[0].To.Equal(numbered(0, 2, "").ReadAt) {
		t.Fatalf("reported %+v, want one run of the 2 stale chunks, from the first, where the stream starts, over their capture times", runs)
	}
	if got, want := f.Consumed(), int64(len("stale-1")+len("stale-22")+len("answer")); got != want {
		t.Fatalf("Consumed = %d, want %d: dropped bytes left the stream", got, want)
	}
	if !f.LastReadTime().Equal(c.ReadAt) {
		t.Fatalf("LastReadTime = %v, want the answer's %v: a dropped chunk moved it", f.LastReadTime(), c.ReadAt)
	}
}

// A run that the stream's end ends is reported then, once; and what is left of
// a chunk read in part is the floor's too, when that chunk was captured
// before it. The run is not the stream's start: the reader read the chunk's
// first bytes before its first floor.
func TestARunTheStreamEndsIsReportedOnceAndTheRestOfAChunkIsDropped(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 4)
	f := New(ch, nil, nil)
	ch <- numbered(FromDest, 1, "abcdef")
	buf := make([]byte, 2)
	if n, err := f.Read(buf); err != nil || string(buf[:n]) != "ab" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
	r := &runs{}
	f.DropBefore(2, r.report)
	ch <- numbered(FromDest, 1, "more-stale")
	close(ch)
	if _, err := f.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %v, want io.EOF: the rest of the chunk and the next were captured before the floor", err)
	}
	if _, err := f.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second Read = %v, want io.EOF", err)
	}
	if runs := r.all(); len(runs) != 1 || runs[0].First != 1 || runs[0].AtStart || !runs[0].To.Equal(numbered(0, 1, "").ReadAt) {
		t.Fatalf("reported %+v, want one run (the rest of the first chunk and the second), once, not at the stream's start", runs)
	}
	if got := f.Consumed(); got != int64(len("abcdef")+len("more-stale")) {
		t.Fatalf("Consumed = %d", got)
	}
}

// Only the run of the stream's start is AtStart: a run after a chunk captured
// after the first floor came off the stream is not, however many runs there
// are.
func TestOnlyTheRunOfTheStreamsStartIsAtTheStart(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 8)
	f := New(ch, nil, nil)
	r := &runs{}
	f.DropBefore(3, r.report)
	ch <- numbered(FromDest, 1, "stale")
	ch <- numbered(FromDest, 4, "answer")
	if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != "answer" {
		t.Fatalf("ReadChunk = %q, %v", c.Bytes, err)
	}
	f.DropBefore(7, r.report)
	ch <- numbered(FromDest, 5, "between")
	ch <- numbered(FromDest, 8, "next answer")
	if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != "next answer" {
		t.Fatalf("ReadChunk = %q, %v", c.Bytes, err)
	}
	got := r.all()
	if len(got) != 2 || got[0].First != 1 || !got[0].AtStart || got[1].First != 5 || got[1].AtStart {
		t.Fatalf("reported %+v, want the stale chunk's run at the start and the later one not", got)
	}
}

// Peek shows the next chunk and takes nothing: the next read hands out the
// same bytes, and the peek moves neither Consumed nor LastReadTime.
func TestPeekTakesNothing(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- numbered(FromClient, 7, "GET /")
	c, err := f.Peek()
	if err != nil || string(c.Bytes) != "GET /" || c.ConnSeq != 7 {
		t.Fatalf("Peek = %q (ConnSeq %d), %v", c.Bytes, c.ConnSeq, err)
	}
	if f.Consumed() != 0 || !f.LastReadTime().IsZero() {
		t.Fatalf("Peek delivered: Consumed %d, LastReadTime %v", f.Consumed(), f.LastReadTime())
	}
	if w, _ := f.Waiting(); w {
		t.Fatal("waiting with a peeked chunk in hand")
	}
	got, err := f.ReadChunk()
	if err != nil || string(got.Bytes) != "GET /" || got.ConnSeq != 7 {
		t.Fatalf("ReadChunk after Peek = %q (ConnSeq %d), %v", got.Bytes, got.ConnSeq, err)
	}
	if f.Consumed() != 5 || !f.LastReadTime().Equal(c.ReadAt) {
		t.Fatalf("ReadChunk did not deliver: Consumed %d, LastReadTime %v", f.Consumed(), f.LastReadTime())
	}
}

// The rest of a chunk read in part keeps the chunk's numbers and times, so a
// floor compares it as the chunk it came from.
func TestTheRestOfAChunkKeepsItsNumbers(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	f := New(ch, nil, nil)
	in := numbered(FromDest, 9, "headbody")
	in.SeqNo = 4
	ch <- in
	buf := make([]byte, 4)
	if n, _ := f.Read(buf); string(buf[:n]) != "head" {
		t.Fatalf("Read = %q", buf[:n])
	}
	rest, err := f.ReadChunk()
	if err != nil || string(rest.Bytes) != "body" {
		t.Fatalf("ReadChunk = %q, %v", rest.Bytes, err)
	}
	if rest.ConnSeq != 9 || rest.SeqNo != 4 || rest.Dir != FromDest || !rest.ReadAt.Equal(in.ReadAt) || !rest.WrittenAt.Equal(in.WrittenAt) {
		t.Fatalf("the rest of the chunk is %+v, want the chunk's numbers and times", rest)
	}
}

// Under a floor a chunk its producer did not number cannot be ordered: the
// read refuses it (ErrUnnumbered) rather than pairing a request with whatever
// came first.
func TestAnUnnumberedChunkUnderAFloorIsRefused(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	f := New(ch, nil, nil)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("HTTP/1.1 200 OK\r\n\r\n"), ReadAt: time.Now()}
	close(ch)
	f.DropBefore(5, func(Dropped) { t.Error("an unnumbered chunk was dropped as one captured before the floor") })
	if _, err := f.ReadChunk(); !errors.Is(err, ErrUnnumbered) {
		t.Fatalf("ReadChunk = %v, want ErrUnnumbered", err)
	}
	if f.Consumed() != 0 {
		t.Fatalf("Consumed = %d: the refused chunk left the stream", f.Consumed())
	}
}

// A chunk read whole is let go: the FakeConn keeps no slice of it, so an idle
// connection does not pin the backing array of the largest read it made.
// Before, the stash was a bytes.Buffer that kept the array it grew to.
func TestAChunkReadWholeIsLetGo(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	f := New(ch, nil, nil)
	ch <- numbered(FromDest, 1, string(make([]byte, 1<<20)))
	buf := make([]byte, 1<<19)
	for i := 0; i < 2; i++ {
		if n, err := f.Read(buf); err != nil || n != 1<<19 {
			t.Fatalf("Read %d = %d, %v", i, n, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasPending || f.pending.Bytes != nil {
		t.Fatalf("the chunk read whole is still held: %d bytes, cap %d", len(f.pending.Bytes), cap(f.pending.Bytes))
	}
}

// capturedAt is the capture time numbered gives chunk seq.
func capturedAt(seq uint32) time.Time { return numbered(FromDest, seq, "").ReadAt }

// stamped is server chunk seq, captured (by its time) when chunk asOf was.
func stamped(seq, asOf uint32, b string) Chunk {
	c := numbered(FromDest, seq, b)
	c.ReadAt = capturedAt(asOf)
	return c
}

// readChunk reads the next chunk, which must be want.
func readChunk(t *testing.T, f *FakeConn, want string) {
	t.Helper()
	c, err := f.ReadChunk()
	if err != nil || string(c.Bytes) != want {
		t.Fatalf("ReadChunk = %q, %v; want %q", c.Bytes, err, want)
	}
}

// SkipThrough cuts the stream by capture time as it was captured, before the
// floor: what it discards, and whether it can cut at all, are what they are
// with no floor set. The floor then applies to what the skip left, at the
// reader's next read. A parser that re-aligns after an answer it could not
// frame has read the head of that answer, and set the floor at its next
// request before it skips through to that request's capture time, so both
// rules meet on the rest of that answer: each byte of it leaves the stream
// once, and what the skip discards is in no run (the parser reports the answer
// it left out itself).
func TestSkipThroughCutsByCaptureTimeBeforeTheFloor(t *testing.T) {
	t.Parallel()
	// The request is chunk 5. Numbers and times agree: the skip discards what
	// was captured before it, and the floor finds nothing left to drop.
	t.Run("numbers and times agree", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 1, "head ")
		ch <- numbered(FromDest, 2, "rest of ")
		ch <- numbered(FromDest, 3, "the answer")
		ch <- numbered(FromDest, 6, "next answer")
		f := New(ch, nil, nil)
		readChunk(t, f, "head ")
		var dropped runs
		f.DropBefore(5, dropped.report)
		n, last, err := f.SkipThrough(capturedAt(5))
		if err != nil || n != int64(len("rest of the answer")) || !last.Equal(capturedAt(3)) {
			t.Fatalf("SkipThrough = (%d, %v, %v), want the two chunks captured before the request, as with no floor", n, last, err)
		}
		readChunk(t, f, "next answer")
		if got := dropped.all(); len(got) != 0 {
			t.Fatalf("the floor dropped %d run(s) after the skip, want none: the skip had discarded them, and a byte leaves the stream once", len(got))
		}
		if want := int64(len("head rest of the answernext answer")); f.Consumed() != want {
			t.Fatalf("Consumed = %d, want %d: each byte counted once", f.Consumed(), want)
		}
	})

	// A chunk numbered before the request and stamped after it (it waited at
	// the capture while the request was read): the skip leaves it, as it does
	// with no floor, and the floor drops it at the next read, once.
	t.Run("a chunk numbered before the request, stamped after it", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 1, "head ")
		ch <- numbered(FromDest, 2, "rest")
		ch <- stamped(3, 7, "late")
		ch <- numbered(FromDest, 8, "next answer")
		f := New(ch, nil, nil)
		readChunk(t, f, "head ")
		var dropped runs
		f.DropBefore(5, dropped.report)
		if n, _, err := f.SkipThrough(capturedAt(5)); err != nil || n != int64(len("rest")) {
			t.Fatalf("SkipThrough = (%d, %v), want (4, nil): the chunk stamped after the request is not the skip's", n, err)
		}
		if got := dropped.all(); len(got) != 0 {
			t.Fatalf("the floor dropped %d run(s) during the skip, want none: the skip is before the floor", len(got))
		}
		readChunk(t, f, "next answer")
		got := dropped.all()
		if len(got) != 1 || got[0].First != 3 || got[0].AtStart {
			t.Fatalf("the floor dropped %+v, want the one chunk numbered before the request, once, not at the stream's start", got)
		}
	})

	// What the skip refuses with no floor it refuses under one: a chunk it
	// cannot place, though the floor would drop it, and a chunk read in part
	// that was stamped after the request.
	t.Run("a chunk the skip cannot place is not dropped for it", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 2)
		ch <- stamped(3, 5, "unplaced")
		close(ch)
		f := New(ch, nil, nil)
		var dropped runs
		f.DropBefore(5, dropped.report)
		if n, _, err := f.SkipThrough(capturedAt(5)); !errors.Is(err, ErrUnplaced) || n != 0 {
			t.Fatalf("SkipThrough = (%d, %v), want (0, ErrUnplaced), as with no floor", n, err)
		}
		if got := dropped.all(); len(got) != 0 {
			t.Fatalf("the floor dropped %d run(s) during the skip, want none", len(got))
		}
	})
	t.Run("a chunk read in part and stamped after the request is not cut", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 2)
		ch <- stamped(3, 7, "ABCDEF")
		close(ch)
		f := New(ch, nil, nil)
		if _, err := io.ReadFull(f, make([]byte, 2)); err != nil {
			t.Fatal(err)
		}
		var dropped runs
		f.DropBefore(5, dropped.report)
		if n, _, err := f.SkipThrough(capturedAt(5)); !errors.Is(err, ErrSkipMidChunk) || n != 0 {
			t.Fatalf("SkipThrough = (%d, %v), want (0, ErrSkipMidChunk), as with no floor", n, err)
		}
		if got := dropped.all(); len(got) != 0 || f.Consumed() != 2 {
			t.Fatalf("the floor dropped %d run(s) and %d bytes left the stream during the skip, want none and the 2 read", len(got), f.Consumed())
		}
	})
}

// A skip where the stream starts can discard bytes captured before the
// reader's first floor: a parser that joined a connection mid-way, whose first
// request's answer it found it cannot frame before it read a byte of the
// server's stream, skips through to its next request, and takes with it the
// answer in flight when the capture began. Those bytes are in the floor's run
// there (AtStart), as they are when the floor drops them, and nothing else is:
// the run ends at the first chunk the skip takes that was captured after the
// first floor, though a later floor covers it (the first request's own
// response, discarded with the answer in flight when the floor has moved on to
// the next request), so it is reported once, over the bytes captured before
// the first request alone, however the skip ends: with a chunk left for the
// reader, at the first chunk it discards past the start, at the stream's end,
// or at a refusal. What the skip discards, refuses and returns is what it is
// with no floor.
func TestSkipThroughWhereTheStreamStartsCountsWhatTheFloorCoversInItsRun(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		chunks []Chunk
		close  bool
		// floors are the floors set before the skip, in order; the skip
		// is to the last one's capture time.
		floors []uint32
		// n and err are the skip's, with a floor or none.
		n   int64
		err error
		// to is the capture time of the run's last chunk.
		to uint32
	}{
		{"it leaves a chunk for the reader", []Chunk{numbered(FromDest, 2, "in flight"), numbered(FromDest, 3, "unread"), numbered(FromDest, 6, "next answer")}, false, []uint32{5}, int64(len("in flightunread")), nil, 3},
		{"it discards a chunk past the start", []Chunk{numbered(FromDest, 2, "in flight"), stamped(6, 4, "early"), numbered(FromDest, 7, "next answer")}, false, []uint32{5}, int64(len("in flightearly")), nil, 2},
		{"it discards a chunk past the start that a later floor covers", []Chunk{numbered(FromDest, 2, "in flight"), numbered(FromDest, 4, "the first request's response"), numbered(FromDest, 6, "next answer")}, false, []uint32{3, 5}, int64(len("in flightthe first request's response")), nil, 2},
		{"the stream ends", []Chunk{numbered(FromDest, 2, "in flight"), numbered(FromDest, 3, "unread")}, true, []uint32{5}, int64(len("in flightunread")), io.EOF, 3},
		{"it cannot place a chunk", []Chunk{numbered(FromDest, 2, "in flight"), stamped(6, 5, "unplaced")}, false, []uint32{5}, int64(len("in flight")), ErrUnplaced, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			skip := func(floor bool) (*FakeConn, *runs, int64, time.Time, error) {
				ch := make(chan Chunk, len(c.chunks))
				for _, k := range c.chunks {
					ch <- k
				}
				if c.close {
					close(ch)
				}
				f := New(ch, nil, nil)
				r := &runs{}
				if floor {
					for _, seq := range c.floors {
						f.DropBefore(seq, r.report)
					}
				}
				n, last, err := f.SkipThrough(capturedAt(c.floors[len(c.floors)-1]))
				return f, r, n, last, err
			}
			bare, _, n0, last0, err0 := skip(false)
			f, r, n, last, err := skip(true)
			if n != c.n || !errors.Is(err, c.err) || n != n0 || !last.Equal(last0) || !errors.Is(err, err0) {
				t.Fatalf("SkipThrough = (%d, %v, %v) under the floor and (%d, %v, %v) with none, want (%d, _, %v) for both", n, last, err, n0, last0, err0, c.n, c.err)
			}
			if f.Consumed() != bare.Consumed() || !f.LastReadTime().Equal(bare.LastReadTime()) {
				t.Fatalf("Consumed %d and LastReadTime %v under the floor, %d and %v with none: the skip moved them differently", f.Consumed(), f.LastReadTime(), bare.Consumed(), bare.LastReadTime())
			}
			got := r.all()
			if len(got) != 1 || got[0].First != 2 || !got[0].AtStart || !got[0].From.Equal(capturedAt(2)) || !got[0].To.Equal(capturedAt(c.to)) {
				t.Fatalf("reported %+v as the skip returned, want one run from chunk 2, where the stream starts, over the chunks captured before the first floor that the skip discarded", got)
			}
		})
	}
}

// The stream's start is what was captured before the reader's first floor, by
// every read: a run the floor drops past it, though a later floor covers it, is
// not AtStart. A reader that skips the first request's response (it has no
// answer) and reads the next request's has its floor at that request when it
// first reads: the start's run ends at the first chunk captured after the first
// request, and the bytes between the two requests are a run of their own.
func TestTheStartIsWhatWasCapturedBeforeTheFirstFloor(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 4)
	ch <- numbered(FromDest, 2, "in flight")
	ch <- numbered(FromDest, 4, "between the requests")
	ch <- numbered(FromDest, 6, "next answer")
	f := New(ch, nil, nil)
	r := &runs{}
	f.DropBefore(3, r.report)
	f.DropBefore(5, r.report)
	readChunk(t, f, "next answer")
	got := r.all()
	if len(got) != 2 || got[0].First != 2 || !got[0].AtStart || !got[0].To.Equal(capturedAt(2)) ||
		got[1].First != 4 || got[1].AtStart || !got[1].From.Equal(capturedAt(4)) {
		t.Fatalf("reported %+v, want the start's run of chunk 2 alone, and chunk 4's run, not at the start", got)
	}
}

// SettleStart reads on for a reader that will not read the stream again, so
// the run of its start is reported though the reader never read past it: it
// drops the start's chunks, which the floor covers, and waits for the next as a
// read does, until a chunk captured after the first floor comes off the stream
// (left in hand, not read), or the stream ends. It returns at once with no
// floor (nothing orders the stream then) and once the start is behind it.
func TestSettleStartReportsTheStartOfAStreamTheReaderDoesNotRead(t *testing.T) {
	t.Parallel()
	settled := func(t *testing.T, f *FakeConn) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.SettleStart()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("SettleStart did not return")
		}
	}
	t.Run("a chunk past the start comes after it", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(3, r.report)
		ch <- numbered(FromDest, 1, "in flight 1")
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.SettleStart()
		}()
		waitWaiting(t, f)
		ch <- numbered(FromDest, 2, "in flight 2")
		ch <- numbered(FromDest, 4, "after the request")
		<-done
		if got := r.all(); len(got) != 1 || got[0].First != 1 || !got[0].AtStart || !got[0].From.Equal(capturedAt(1)) || !got[0].To.Equal(capturedAt(2)) {
			t.Fatalf("reported %+v, want one run of chunks 1 and 2, at the start", got)
		}
		if want := int64(len("in flight 1in flight 2")); f.Consumed() != want {
			t.Fatalf("Consumed = %d, want %d: the chunk after the start is left in hand, not read", f.Consumed(), want)
		}
		readChunk(t, f, "after the request")
	})
	t.Run("the stream ends", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 2)
		ch <- numbered(FromDest, 1, "in flight")
		close(ch)
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(3, r.report)
		settled(t, f)
		if got := r.all(); len(got) != 1 || got[0].First != 1 || !got[0].AtStart {
			t.Fatalf("reported %+v, want chunk 1's run, at the start, as the stream ended", got)
		}
	})
	t.Run("the floor has moved on", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 2, "in flight")
		ch <- numbered(FromDest, 4, "between the requests")
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(3, r.report)
		f.DropBefore(5, r.report)
		settled(t, f)
		if got := r.all(); len(got) != 1 || got[0].First != 2 || !got[0].AtStart || !got[0].To.Equal(capturedAt(2)) {
			t.Fatalf("reported %+v, want chunk 2's run alone, at the start", got)
		}
		if f.Consumed() != int64(len("in flight")) {
			t.Fatalf("Consumed = %d: SettleStart went past the start", f.Consumed())
		}
	})
	t.Run("a chunk of the start the skip left in hand", func(t *testing.T) {
		t.Parallel()
		// Chunk 3 was captured before the request (chunk 5) by its number, and
		// stamped as the request was: the skip cannot place it, and leaves it.
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 2, "in flight")
		ch <- stamped(3, 5, "unplaced")
		close(ch)
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(5, r.report)
		if _, _, err := f.SkipThrough(capturedAt(5)); !errors.Is(err, ErrUnplaced) {
			t.Fatalf("SkipThrough = %v, want ErrUnplaced", err)
		}
		if got := r.all(); len(got) != 0 {
			t.Fatalf("reported %+v as the skip returned, want nothing yet: the start goes on in the chunk it left", got)
		}
		settled(t, f)
		if got := r.all(); len(got) != 1 || got[0].First != 2 || !got[0].AtStart || !got[0].To.Equal(capturedAt(5)) {
			t.Fatalf("reported %+v, want one run of chunks 2 and 3, at the start", got)
		}
	})
	t.Run("no floor", func(t *testing.T) {
		t.Parallel()
		f := New(make(chan Chunk), nil, nil)
		settled(t, f)
	})
	t.Run("the start is behind it", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 1, "in flight")
		ch <- numbered(FromDest, 4, "answer")
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(3, r.report)
		readChunk(t, f, "answer")
		settled(t, f)
		if got := r.all(); len(got) != 1 {
			t.Fatalf("reported %+v, want chunk 1's run, once", got)
		}
	})
}

// Every read that returns ends the floor's run, but one at the read deadline,
// after which the reader can read on: so a run is reported once, and never
// left unreported by a read that returned for good. A read refused for an
// unnumbered chunk reports the run before it; a run a deadline interrupts goes
// on at the next read and is reported once, whole; the end of the start's run
// puts the start behind the stream, so a chunk of it a later read takes is in
// a run of its own, not the start's; and a skip ends a run a deadline left
// open, whether it cuts (what it discards is in no run) or cannot.
func TestEveryReadButADeadlineEndsTheRun(t *testing.T) {
	t.Parallel()
	t.Run("a read refused for an unnumbered chunk", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 2)
		ch <- numbered(FromDest, 1, "stale")
		ch <- Chunk{Dir: FromDest, Bytes: []byte("unnumbered"), ReadAt: time.Now()}
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(3, r.report)
		if _, err := f.ReadChunk(); !errors.Is(err, ErrUnnumbered) {
			t.Fatalf("ReadChunk = %v, want ErrUnnumbered", err)
		}
		if got := r.all(); len(got) != 1 || got[0].First != 1 || !got[0].AtStart {
			t.Fatalf("reported %+v, want the stale chunk's run, once, as the read was refused", got)
		}
	})
	t.Run("a read deadline", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 1, "stale-1")
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(3, r.report)
		_ = f.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := f.ReadChunk(); !errors.Is(err, ErrDeadlineExceeded) {
			t.Fatalf("ReadChunk = %v, want the deadline's error", err)
		}
		if got := r.all(); len(got) != 0 {
			t.Fatalf("reported %+v at the deadline, want nothing yet: the reader reads on", got)
		}
		_ = f.SetReadDeadline(time.Time{})
		ch <- numbered(FromDest, 2, "stale-2")
		ch <- numbered(FromDest, 4, "answer")
		readChunk(t, f, "answer")
		if got := r.all(); len(got) != 1 || got[0].First != 1 || !got[0].AtStart || !got[0].To.Equal(capturedAt(2)) {
			t.Fatalf("reported %+v, want one run over both stale chunks, where the stream starts", got)
		}
	})
	t.Run("a read refused for an unnumbered chunk, past the start", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 4, "answer")
		f := New(ch, nil, nil)
		readChunk(t, f, "answer")
		r := &runs{}
		f.DropBefore(7, r.report)
		ch <- numbered(FromDest, 5, "stale")
		ch <- Chunk{Dir: FromDest, Bytes: []byte("unnumbered"), ReadAt: time.Now()}
		if _, err := f.ReadChunk(); !errors.Is(err, ErrUnnumbered) {
			t.Fatalf("ReadChunk = %v, want ErrUnnumbered", err)
		}
		if got := r.all(); len(got) != 1 || got[0].First != 5 || got[0].AtStart {
			t.Fatalf("reported %+v, want the stale chunk's run, once, as the read was refused", got)
		}
	})
	t.Run("a run of the start that a read's return ended is its only one", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 1, "in flight 1")
		f := New(ch, nil, nil)
		r := &runs{}
		f.DropBefore(5, r.report)
		_ = f.Close()
		if _, err := f.ReadChunk(); !errors.Is(err, ErrClosed) {
			t.Fatalf("ReadChunk = %v, want ErrClosed", err)
		}
		// A chunk captured before the floor lands after the read returned,
		// and the next read takes it.
		ch <- numbered(FromDest, 2, "in flight 2")
		if _, err := f.ReadChunk(); !errors.Is(err, ErrClosed) {
			t.Fatalf("second ReadChunk = %v, want ErrClosed", err)
		}
		if got := r.all(); len(got) != 2 || got[0].First != 1 || !got[0].AtStart || got[1].First != 2 || got[1].AtStart {
			t.Fatalf("reported %+v, want chunk 1's run at the start, and chunk 2's not: the start is reported once", got)
		}
	})
	t.Run("a skip that cuts, after a deadline", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 4, "answer")
		f := New(ch, nil, nil)
		readChunk(t, f, "answer")
		r := &runs{}
		f.DropBefore(7, r.report)
		ch <- numbered(FromDest, 5, "stale")
		_ = f.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := f.ReadChunk(); !errors.Is(err, ErrDeadlineExceeded) {
			t.Fatalf("ReadChunk = %v, want the deadline's error", err)
		}
		_ = f.SetReadDeadline(time.Time{})
		ch <- numbered(FromDest, 6, "rest")
		ch <- numbered(FromDest, 9, "next answer")
		if n, _, err := f.SkipThrough(capturedAt(8)); err != nil || n != int64(len("rest")) {
			t.Fatalf("SkipThrough = (%d, %v), want (4, nil)", n, err)
		}
		if got := r.all(); len(got) != 1 || got[0].First != 5 || got[0].AtStart || !got[0].To.Equal(capturedAt(5)) {
			t.Fatalf("reported %+v as the skip returned, want the stale chunk's run alone, once", got)
		}
	})
	t.Run("a skip that cannot cut, after a deadline", func(t *testing.T) {
		t.Parallel()
		ch := make(chan Chunk, 4)
		ch <- numbered(FromDest, 4, "answer")
		f := New(ch, nil, nil)
		readChunk(t, f, "answer")
		r := &runs{}
		f.DropBefore(7, r.report)
		ch <- numbered(FromDest, 5, "stale")
		_ = f.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := f.ReadChunk(); !errors.Is(err, ErrDeadlineExceeded) {
			t.Fatalf("ReadChunk = %v, want the deadline's error", err)
		}
		// The last chunk read was captured after the point to skip through.
		if _, _, err := f.SkipThrough(capturedAt(3)); !errors.Is(err, ErrSkipMidChunk) {
			t.Fatalf("SkipThrough = %v, want ErrSkipMidChunk", err)
		}
		if got := r.all(); len(got) != 1 || got[0].First != 5 || got[0].AtStart {
			t.Fatalf("reported %+v, want the stale chunk's run, once, as the skip refused", got)
		}
	})
}

func waitWaiting(t *testing.T, f *FakeConn) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if w, _ := f.Waiting(); w {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the reader never blocked for the next chunk")
		}
		time.Sleep(time.Millisecond)
	}
}
