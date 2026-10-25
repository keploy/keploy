package fakeconn

import (
	"io"
	"testing"
	"time"
	"unsafe"
)

// Consumed and Waiting tell a producer when the reader has taken all it was
// given and is done with it: blocked for more.
func TestFakeConn_ConsumedAndWaiting(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("hello"), ReadAt: time.Now()}
	buf := make([]byte, 3)
	if n, err := f.Read(buf); err != nil || n != 3 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if got := f.Consumed(); got != 3 {
		t.Fatalf("Consumed = %d after 3 bytes read, want 3", got)
	}
	if w, _ := f.Waiting(); w {
		t.Fatal("waiting while bytes were buffered for the reader")
	}
	if n, _ := f.Read(buf); n != 2 || f.Consumed() != 5 {
		t.Fatalf("read %d, consumed %d, want 2 and 5", n, f.Consumed())
	}
	_, before := f.Waiting()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.Read(buf)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if w, n := f.Waiting(); w && n == before+1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a reader blocked with nothing to take is not reported waiting")
		}
		time.Sleep(time.Millisecond)
	}
	ch <- Chunk{Bytes: []byte("x"), ReadAt: time.Now()}
	<-done
	if w, _ := f.Waiting(); w {
		t.Fatal("still reported waiting after it took a chunk")
	}
	if f.Consumed() != 6 {
		t.Fatalf("Consumed = %d, want 6", f.Consumed())
	}
}

// ProcessedThrough counts the bytes the discard watermark covers as done with
// before the reader pulls them: it is never handed them, however it stops.
func TestFakeConn_ProcessedThroughCountsWhatTheWatermarkCovers(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("hello"), ReadAt: time.Now()}
	ch <- Chunk{Bytes: []byte("world"), ReadAt: time.Now()}
	buf := make([]byte, 5)
	if n, err := f.Read(buf); err != nil || n != 5 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if got, exact := f.ProcessedThrough(); got != 5 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v after 5 bytes read, want 5, exact", got, exact)
	}
	f.DiscardBefore(8)
	if got, exact := f.ProcessedThrough(); got != 8 || !exact || f.Consumed() != 5 {
		t.Fatalf("ProcessedThrough = %d, %v, Consumed = %d with the watermark at 8 and nothing pulled, want 8, exact and 5", got, exact, f.Consumed())
	}
	if n, err := f.Read(buf); err != nil || n != 2 {
		t.Fatalf("Read = %d, %v, want the 2 bytes past the watermark", n, err)
	}
	if got, exact := f.ProcessedThrough(); got != 10 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v, want 10, exact", got, exact)
	}
}

// What ReadAhead hands out is done with only as the parser marks it processed:
// ProcessedThrough is the first byte read ahead and not yet processed, however
// far the reader has read ahead of it. A parser that stops there recorded
// nothing past it.
func TestFakeConn_ProcessedThroughIsTheFirstByteReadAheadAndNotProcessed(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("hello"), ReadAt: time.Now()}
	ch <- Chunk{Bytes: []byte("wor"), ReadAt: time.Now()}
	var ahead []Chunk
	for range 2 {
		c, err := f.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		ahead = append(ahead, c)
	}
	if got, exact := f.ProcessedThrough(); got != 0 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v with 8 bytes read ahead and none processed, want 0, exact", got, exact)
	}
	f.MarkProcessed(ahead[0])
	if got, exact := f.ProcessedThrough(); got != 5 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v with the first chunk processed, want 5, exact", got, exact)
	}
	f.MarkProcessed(ahead[1])
	if got, exact := f.ProcessedThrough(); got != 8 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v with all processed, want 8, exact", got, exact)
	}
}

// Bytes swallowed for the reader between chunks it read ahead count processed
// at once, past the first byte it has not processed: Processed still counts
// exactly what it took and did not process, but is not that byte's offset, and
// ProcessedThrough says it is not exact (the caller keeps the last exact one)
// until the parser has processed all it read ahead. It never claims a byte
// read ahead and not processed as done with.
func TestFakeConn_ProcessedThroughIsNotExactPastADiscardBetweenChunksReadAhead(t *testing.T) {
	ch := make(chan Chunk, 4)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("aaaaa"), ReadAt: time.Now()}
	ch <- Chunk{Bytes: []byte("xxx"), ReadAt: time.Now()}
	ch <- Chunk{Bytes: []byte("bb"), ReadAt: time.Now()}
	ch <- Chunk{Bytes: []byte("cc"), ReadAt: time.Now()}
	a, err := f.ReadAhead()
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	f.DiscardBefore(8)
	b, err := f.ReadAhead()
	if err != nil || string(b.Bytes) != "bb" {
		t.Fatalf("ReadAhead = %q, %v, want the chunk past the watermark", b.Bytes, err)
	}
	if took := f.Consumed() - f.Processed(); took != 7 {
		t.Fatalf("Consumed less Processed = %d, want the 7 bytes read ahead", took)
	}
	if got, exact := f.ProcessedThrough(); exact || got != 0 {
		t.Fatalf("ProcessedThrough = %d, %v with the first chunk read ahead and not processed, want 0, not exact", got, exact)
	}
	f.MarkProcessed(a)
	if got, exact := f.ProcessedThrough(); exact || got != 0 {
		t.Fatalf("ProcessedThrough = %d, %v with the chunk past the watermark not processed, want 0, not exact", got, exact)
	}
	if waiting, _ := f.Waiting(); waiting {
		t.Fatal("Waiting with no read blocked")
	}
	f.MarkProcessed(b)
	if got, exact := f.ProcessedThrough(); got != 10 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v with all processed, want 10, exact", got, exact)
	}
	// Exact again: what is read ahead next is past no swallowed byte.
	if _, err := f.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	if got, exact := f.ProcessedThrough(); got != 10 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v with the next chunk read ahead, want 10, exact", got, exact)
	}
}

// Bytes swallowed while nothing read ahead is unprocessed lie past no
// unprocessed byte: the chunk read ahead next leaves ProcessedThrough exact, at
// its first byte.
func TestFakeConn_ADiscardWithNothingReadAheadUnprocessedLeavesProcessedThroughExact(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("aaaa")}
	ch <- Chunk{Bytes: []byte("bbbb")}
	f.DiscardBefore(4)
	c, err := f.ReadAhead()
	if err != nil || string(c.Bytes) != "bbbb" {
		t.Fatalf("ReadAhead = %q, %v, want the chunk past the watermark", c.Bytes, err)
	}
	if got, exact := f.ProcessedThrough(); got != 4 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v, want 4, exact: the swallowed bytes followed nothing unprocessed", got, exact)
	}
	f.MarkProcessed(c)
	if got, exact := f.ProcessedThrough(); got != 8 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v, want 8, exact", got, exact)
	}
}

// Past a discard behind bytes read ahead, ProcessedThrough stays not exact
// until the parser has processed every byte it read ahead, the last one too:
// with one byte read ahead left, Processed is one short of Consumed, yet the
// first unprocessed byte lies before the swallowed ones.
func TestFakeConn_ProcessedThroughIsNotExactWhileOneByteReadAheadIsUnprocessed(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("aa")}
	ch <- Chunk{Bytes: []byte("b")}
	close(ch)
	a, err := f.ReadAhead()
	if err != nil || string(a.Bytes) != "aa" {
		t.Fatalf("ReadAhead = %q, %v", a.Bytes, err)
	}
	f.DiscardBefore(3)
	if _, err := f.ReadAhead(); err != io.EOF {
		t.Fatalf("ReadAhead past the swallowed chunk at the stream's end: %v, want io.EOF", err)
	}
	f.MarkProcessed(Chunk{Bytes: a.Bytes[:1]})
	if p, c := f.Processed(), f.Consumed(); p != c-1 {
		t.Fatalf("test setup: Processed = %d, Consumed = %d, want one byte apart", p, c)
	}
	if got, exact := f.ProcessedThrough(); exact {
		t.Fatalf("ProcessedThrough = %d, exact with byte 1 read ahead and not processed, past which byte 2 was swallowed", got)
	}
	f.MarkProcessed(Chunk{Bytes: a.Bytes[1:]})
	if got, exact := f.ProcessedThrough(); got != 3 || !exact {
		t.Fatalf("ProcessedThrough = %d, %v with all processed, want 3, exact", got, exact)
	}
}

// A chunk ReadAhead hands out has left the FakeConn but is not processed until
// the parser marks it so: Consumed less Processed is what a parser that reads
// ahead took and never processed, and LastReadTime is where it got to, not
// where its reader got to.
func TestFakeConn_ReadAheadIsTakenNotProcessed(t *testing.T) {
	ch := make(chan Chunk, 3)
	f := New(ch, nil, nil)
	t0 := time.Unix(1000, 0)
	a := Chunk{Bytes: []byte("aaa"), ReadAt: t0, WrittenAt: t0.Add(time.Millisecond)}
	b := Chunk{Bytes: []byte("bb"), ReadAt: t0.Add(time.Second), WrittenAt: t0.Add(time.Second + time.Millisecond)}
	c := Chunk{Bytes: []byte("cccc"), ReadAt: t0.Add(2 * time.Second)}
	ch <- a
	ch <- b
	ch <- c

	if got, err := f.ReadChunk(); err != nil || string(got.Bytes) != "aaa" {
		t.Fatalf("ReadChunk = %q, %v", got.Bytes, err)
	}
	for _, want := range []string{"bb", "cccc"} {
		if got, err := f.ReadAhead(); err != nil || string(got.Bytes) != want {
			t.Fatalf("ReadAhead = %q, %v, want %q", got.Bytes, err, want)
		}
	}
	if consumed, processed := f.Consumed(), f.Processed(); consumed != 9 || processed != 3 {
		t.Fatalf("Consumed = %d, Processed = %d with 3 bytes read and 6 read ahead, want 9 and 3", consumed, processed)
	}
	if got, gotW := f.LastReadTime(), f.LastWrittenTime(); !got.Equal(a.ReadAt) || !gotW.Equal(a.WrittenAt) {
		t.Fatalf("LastReadTime, LastWrittenTime = %v, %v with only the first chunk processed, want its %v, %v", got, gotW, a.ReadAt, a.WrittenAt)
	}

	f.MarkProcessed(b)
	if processed := f.Processed(); processed != 5 {
		t.Fatalf("Processed = %d after the parser processed the 2 bytes of b, want 5", processed)
	}
	if got, gotW := f.LastReadTime(), f.LastWrittenTime(); !got.Equal(b.ReadAt) || !gotW.Equal(b.WrittenAt) {
		t.Fatalf("LastReadTime, LastWrittenTime = %v, %v, want b's %v, %v", got, gotW, b.ReadAt, b.WrittenAt)
	}

	// More than was handed out is never processed: Processed stays within
	// Consumed.
	f.MarkProcessed(Chunk{Bytes: make([]byte, 10)})
	if consumed, processed := f.Consumed(), f.Processed(); consumed != 9 || processed != 9 {
		t.Fatalf("Consumed = %d, Processed = %d after a mark past what was handed out, want 9 and 9", consumed, processed)
	}
	if got := f.LastReadTime(); !got.Equal(b.ReadAt) {
		t.Fatalf("LastReadTime = %v after a mark with no times, want b's %v kept", got, b.ReadAt)
	}
}

// Bytes swallowed for the reader (DiscardBefore) are processed, as bytes it
// reads itself are, while what it read ahead stays unprocessed.
func TestFakeConn_ReadAheadKeepsWhatItHoldsAcrossADiscard(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("held")}
	ch <- Chunk{Bytes: []byte("swallowed!")}
	if got, err := f.ReadAhead(); err != nil || string(got.Bytes) != "held" {
		t.Fatalf("ReadAhead = %q, %v", got.Bytes, err)
	}
	f.DiscardBefore(4 + 9)
	if got, err := f.ReadAhead(); err != nil || string(got.Bytes) != "!" {
		t.Fatalf("ReadAhead = %q, %v, want the byte past the watermark", got.Bytes, err)
	}
	if consumed, processed := f.Consumed(), f.Processed(); consumed != 14 || processed != 9 {
		t.Fatalf("Consumed = %d, Processed = %d with 9 bytes swallowed and 5 read ahead, want 14 and 9", consumed, processed)
	}
}

// A FakeConn is one per direction of every connection a parser reads, so it
// stays in the 288 byte size class (processed took the place of a field).
func TestFakeConn_StaysInItsSizeClass(t *testing.T) {
	var f FakeConn
	if size := unsafe.Sizeof(f); size > 288 {
		t.Fatalf("a FakeConn is %d bytes, past the 288 byte size class", size)
	}
}
