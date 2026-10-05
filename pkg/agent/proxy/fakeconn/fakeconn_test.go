package fakeconn

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestWriteRejected(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	f := New(ch, nil, nil)
	n, err := f.Write([]byte("hello"))
	if n != 0 {
		t.Errorf("Write returned n=%d, want 0", n)
	}
	if !errors.Is(err, ErrFakeConnNoWrite) {
		t.Errorf("Write err = %v, want ErrFakeConnNoWrite", err)
	}
}

func TestReadFromChunks(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromClient, Bytes: []byte("foo"), ReadAt: time.Unix(1, 0), WrittenAt: time.Unix(1, 500)}
	ch <- Chunk{Dir: FromClient, Bytes: []byte("bar"), ReadAt: time.Unix(2, 0), WrittenAt: time.Unix(2, 500)}
	close(ch)

	f := New(ch, nil, nil)
	buf := make([]byte, 6)
	total := 0
	for total < 6 {
		n, err := f.Read(buf[total:])
		if err != nil && err != io.EOF {
			t.Fatalf("Read: %v", err)
		}
		total += n
		if err == io.EOF {
			break
		}
	}
	if got := string(buf[:total]); got != "foobar" {
		t.Errorf("Read result = %q, want %q", got, "foobar")
	}
	if got, want := f.LastReadTime(), time.Unix(2, 0); !got.Equal(want) {
		t.Errorf("LastReadTime = %v, want %v", got, want)
	}
	if got, want := f.LastWrittenTime(), time.Unix(2, 500); !got.Equal(want) {
		t.Errorf("LastWrittenTime = %v, want %v", got, want)
	}
}

func TestLastWrittenTimeZeroBeforeAnyChunk(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	f := New(ch, nil, nil)
	if got := f.LastWrittenTime(); !got.IsZero() {
		t.Errorf("LastWrittenTime before any chunk = %v, want zero", got)
	}
}

// TestReadStashExhaustionNotEOF pins the invariant that draining
// the internal byte-stash does NOT surface as io.EOF to the caller.
// bytes.Buffer.Read returns io.EOF whenever it empties the buffer,
// which — if passed through unchanged — would make bufio.Reader /
// io.Copy / encoding/pkg readers think the stream is finished even
// though more chunks may still arrive on f.ch. Only the
// channel-close path (readChunkLocked) is a genuine EOF.
func TestReadStashExhaustionNotEOF(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	// Two chunks. The first one is larger than the Read buffer so
	// it lands in the stash; the second one arrives later.
	ch <- Chunk{Dir: FromClient, Bytes: []byte("hello world")}
	ch <- Chunk{Dir: FromClient, Bytes: []byte("second")}

	f := New(ch, nil, nil)

	// First Read: pulls the chunk, returns 5 bytes, stashes "o world"
	// (6 bytes) internally.
	p1 := make([]byte, 5)
	n1, err := f.Read(p1)
	if err != nil {
		t.Fatalf("first Read returned err = %v, want nil", err)
	}
	if string(p1[:n1]) != "hello" {
		t.Fatalf("first Read data = %q, want %q", p1[:n1], "hello")
	}

	// Second Read: asks for 6 bytes; the stash has exactly 6 bytes
	// (" world" — the bytes after "hello" in "hello world"), so
	// bytes.Buffer.Read empties the stash and returns io.EOF. The
	// FakeConn MUST mask that EOF because more chunks are still
	// arriving on f.ch.
	p2 := make([]byte, 6)
	n2, err := f.Read(p2)
	if err != nil {
		t.Fatalf("stash-exhaustion Read returned err = %v, want nil (EOF must be masked)", err)
	}
	if string(p2[:n2]) != " world" {
		t.Fatalf("stash-exhaustion Read data = %q, want %q", p2[:n2], " world")
	}

	// Third Read must still see the second chunk, proving the
	// premature EOF did not terminate the caller's stream.
	p3 := make([]byte, 16)
	var got string
	for len(got) < len("second") {
		n3, err := f.Read(p3)
		if err != nil {
			t.Fatalf("third Read (second chunk) err = %v", err)
		}
		got += string(p3[:n3])
	}
	if got != "second"[:len(got)] {
		t.Fatalf("second-chunk data = %q, want prefix of %q", got, "second")
	}
}

func TestReadChunkEOFOnChannelClose(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	close(ch)
	f := New(ch, nil, nil)
	_, err := f.ReadChunk()
	if err != io.EOF {
		t.Errorf("ReadChunk on closed empty channel = %v, want io.EOF", err)
	}
}

func TestReadChunkBlocksThenReturns(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	f := New(ch, nil, nil)
	go func() {
		time.Sleep(10 * time.Millisecond)
		ch <- Chunk{Dir: FromDest, Bytes: []byte("hi"), ReadAt: time.Unix(5, 0)}
	}()
	c, err := f.ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if string(c.Bytes) != "hi" {
		t.Errorf("got %q, want %q", c.Bytes, "hi")
	}
}

func TestReadAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	f := New(ch, nil, nil)
	_ = f.Close()
	_, err := f.Read(make([]byte, 8))
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Read after Close = %v, want ErrClosed", err)
	}
}

// TestReadChunkAfterCloseDrainsBuffered is the FakeConn-side regression
// guard for the startup-mock "server closed before response" drop. The
// relay tee delivers the final response chunk into f.ch and the relay
// then calls Close() during teardown. A chunk already sitting in f.ch is
// a fully recorded wire event: Close means "no more blocking", not
// "discard bytes already delivered to me". ReadChunk after Close must
// therefore return the buffered chunk(s) first and only report ErrClosed
// once nothing buffered remains. Before the fix, the f.closed short
// circuit returned ErrClosed immediately and the buffered response chunk
// (carrying the startup mock body) was lost.
func TestReadChunkAfterCloseDrainsBuffered(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("startup-secret"), WrittenAt: time.Unix(7, 0)}
	f := New(ch, nil, nil)
	_ = f.Close()

	c, err := f.ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk after Close with buffered chunk = %v, want the chunk", err)
	}
	if string(c.Bytes) != "startup-secret" {
		t.Fatalf("got %q, want %q", c.Bytes, "startup-secret")
	}

	// Nothing buffered now → ErrClosed.
	if _, err := f.ReadChunk(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second ReadChunk after Close = %v, want ErrClosed", err)
	}
}

// TestReadAfterCloseDrainsBuffered is the byte-oriented counterpart:
// Read (not ReadChunk) after Close must likewise hand back bytes the
// relay already delivered before reporting ErrClosed.
func TestReadAfterCloseDrainsBuffered(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("body"), WrittenAt: time.Unix(7, 0)}
	f := New(ch, nil, nil)
	_ = f.Close()

	p := make([]byte, 8)
	n, err := f.Read(p)
	if err != nil {
		t.Fatalf("Read after Close with buffered chunk = %v, want bytes", err)
	}
	if string(p[:n]) != "body" {
		t.Fatalf("got %q, want %q", p[:n], "body")
	}
	if _, err := f.Read(p); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Read after Close = %v, want ErrClosed", err)
	}
}

// A reader that found nothing and is about to wait, when a chunk is delivered
// and Close follows, still gets the chunk: the relay's teardown delivers what
// its tee holds and then closes the stream, and a parser a moment behind lost
// the connection's last request or response to it. Here the chunk and the
// Close arrive between Waiting and the wait (beforeWait) in every round, so the
// reader waits with both ready every time; it used to take the Close about
// half the time, and lose the chunk.
func TestReadChunkAboutToWaitWhenAChunkAndCloseArriveGetsTheChunk(t *testing.T) {
	// Not parallel: it sets the package's beforeWait. The hook acts only for
	// this test's FakeConn, and runs on the goroutine that reads it.
	t.Cleanup(func() { beforeWait.Store(nil) })
	const rounds = 2000
	lost := 0
	for i := 0; i < rounds; i++ {
		ch := make(chan Chunk, 1)
		f := New(ch, nil, nil)
		arrived := false
		hook := func(waiting *FakeConn) {
			if waiting != f || arrived {
				return
			}
			arrived = true
			ch <- Chunk{Dir: FromDest, Bytes: []byte("last response")}
			close(ch)
			_ = f.Close()
		}
		beforeWait.Store(&hook)
		if _, err := f.ReadChunk(); err != nil {
			lost++
		}
		if !arrived {
			t.Fatal("the reader did not wait: the chunk and the Close did not arrive as it was about to")
		}
	}
	if lost != 0 {
		t.Fatalf("a chunk delivered before Close was lost in %d of %d rounds", lost, rounds)
	}
}

func TestReadDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	f := New(ch, nil, nil)
	_ = f.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	_, err := f.Read(make([]byte, 8))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("Read with expired deadline: err=%v, want net.Error with Timeout()=true", err)
	}
}

func TestReadDeadlineCleared(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	f := New(ch, nil, nil)
	_ = f.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
	time.Sleep(10 * time.Millisecond)
	_ = f.SetReadDeadline(time.Time{}) // clear
	ch <- Chunk{Bytes: []byte("x"), ReadAt: time.Unix(9, 0)}
	buf := make([]byte, 4)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("Read after clearing deadline: %v", err)
	}
	if n != 1 || buf[0] != 'x' {
		t.Errorf("got %q n=%d, want %q n=1", buf[:n], n, "x")
	}
}

func TestWakeOnClose(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk)
	f := New(ch, nil, nil)
	done := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 4))
		done <- err
	}()
	time.Sleep(5 * time.Millisecond)
	_ = f.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("Read woken by Close returned %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock on Close")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	f := New(make(chan Chunk), nil, nil)
	if err := f.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestPlaceholderAddrWhenNil(t *testing.T) {
	t.Parallel()
	f := New(make(chan Chunk), nil, nil)
	if f.LocalAddr().Network() != "fakeconn" {
		t.Errorf("LocalAddr.Network = %q, want fakeconn", f.LocalAddr().Network())
	}
	if f.RemoteAddr().Network() != "fakeconn" {
		t.Errorf("RemoteAddr.Network = %q, want fakeconn", f.RemoteAddr().Network())
	}
}

type testAddr struct{ s string }

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return a.s }

func TestLocalRemoteAddrPassedThrough(t *testing.T) {
	t.Parallel()
	local := testAddr{s: "127.0.0.1:1"}
	remote := testAddr{s: "127.0.0.1:2"}
	f := New(make(chan Chunk), local, remote)
	if f.LocalAddr().String() != "127.0.0.1:1" {
		t.Errorf("LocalAddr = %v, want 127.0.0.1:1", f.LocalAddr())
	}
	if f.RemoteAddr().String() != "127.0.0.1:2" {
		t.Errorf("RemoteAddr = %v, want 127.0.0.1:2", f.RemoteAddr())
	}
}

func TestSetWriteDeadlineIsNoop(t *testing.T) {
	t.Parallel()
	f := New(make(chan Chunk), nil, nil)
	if err := f.SetWriteDeadline(time.Now()); err != nil {
		t.Errorf("SetWriteDeadline = %v, want nil", err)
	}
}

func TestPartialReadStashesRemainder(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	ch <- Chunk{Bytes: []byte("abcdef"), ReadAt: time.Unix(1, 0)}
	close(ch)
	f := New(ch, nil, nil)

	small := make([]byte, 3)
	n, err := f.Read(small)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if n != 3 || string(small) != "abc" {
		t.Errorf("first Read = %q, want abc", small[:n])
	}

	rest := make([]byte, 8)
	n2, err := f.Read(rest)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if n2 != 3 || string(rest[:n2]) != "def" {
		t.Errorf("second Read = %q, want def", rest[:n2])
	}

	_, err = f.Read(rest)
	if err != io.EOF {
		t.Errorf("third Read after channel close = %v, want io.EOF", err)
	}
}

type captureLogger struct{ warns int }

func (c *captureLogger) Debug(string, ...any) { c.warns++ }

func TestWriteLoggerInvoked(t *testing.T) {
	t.Parallel()
	log := &captureLogger{}
	f := NewWithLogger(make(chan Chunk), nil, nil, log)
	_, _ = f.Write([]byte("x"))
	_, _ = f.Write([]byte("y"))
	if log.warns != 2 {
		t.Errorf("logger.Debug called %d times, want 2", log.warns)
	}
}

// The DiscardBefore tests below pin the three properties the relay
// depends on when it consumes bytes it has already teed (MySQL's
// CLIENT_SSL ClientHello): the watermark reaches bytes wherever they
// currently sit, it can split a chunk mid-way, and it never touches
// anything above it.

// TestDiscardBeforeSplitsAChunk: the parser consumed the head of a
// chunk and the relay consumed its tail. This is the coalesced
// SSLRequest+ClientHello case, and the reason the watermark counts
// bytes rather than chunks.
func TestDiscardBeforeSplitsAChunk(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromClient, Bytes: []byte("HEADTAILTAIL"), ReadAt: time.Unix(1, 0)}
	f := New(ch, nil, nil)

	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if string(head) != "HEAD" {
		t.Fatalf("head = %q, want HEAD", head)
	}

	// Everything the producer had accepted at this point (12 bytes) is
	// off limits from here on.
	if pending := f.DiscardBefore(12); pending != 8 {
		t.Fatalf("DiscardBefore reported %d bytes pending, want the 8 unread tail bytes", pending)
	}

	ch <- Chunk{Dir: FromClient, Bytes: []byte("NEXT"), ReadAt: time.Unix(2, 0)}
	got := make([]byte, 4)
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatalf("read after discard: %v", err)
	}
	if string(got) != "NEXT" {
		t.Fatalf("after the discard the reader got %q, want NEXT — the watermark must split "+
			"the chunk it lands inside, not round to a chunk boundary", got)
	}
}

// TestDiscardBeforeReachesUndeliveredChunks: the bytes to drop had not
// reached this FakeConn yet when the watermark was set. Naming an
// absolute offset is what makes that work without quiescing the
// producer.
func TestDiscardBeforeReachesUndeliveredChunks(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 3)
	ch <- Chunk{Dir: FromClient, Bytes: []byte("SSLREQ"), ReadAt: time.Unix(1, 0)}
	f := New(ch, nil, nil)

	first := make([]byte, 6)
	if _, err := io.ReadFull(f, first); err != nil {
		t.Fatalf("read first: %v", err)
	}

	// The producer has accepted 6+5 bytes; only the first 6 were ever
	// the parser's. The 5 are still in the channel, unseen here.
	f.DiscardBefore(11)
	ch <- Chunk{Dir: FromClient, Bytes: []byte("HELLO"), ReadAt: time.Unix(2, 0)}
	ch <- Chunk{Dir: FromClient, Bytes: []byte("AFTER"), ReadAt: time.Unix(3, 0)}

	got := make([]byte, 5)
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatalf("read after discard: %v", err)
	}
	if string(got) != "AFTER" {
		t.Fatalf("after the discard the reader got %q, want AFTER — a watermark set before the "+
			"bytes arrived must still swallow them", got)
	}
}

// TestDiscardBeforeIsMonotonicAndNeverOverruns: a lower offset is
// ignored, and an offset already behind the reader is a no-op rather
// than a rewind that would eat live traffic.
func TestDiscardBeforeIsMonotonicAndNeverOverruns(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromClient, Bytes: []byte("ABCDEF"), ReadAt: time.Unix(1, 0)}
	f := New(ch, nil, nil)

	f.DiscardBefore(3)
	if pending := f.DiscardBefore(1); pending != 3 {
		t.Fatalf("a lower offset moved the watermark: pending = %d, want 3", pending)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "DEF" {
		t.Fatalf("got %q, want DEF", got)
	}
	if pending := f.DiscardBefore(2); pending != 0 {
		t.Fatalf("an offset behind the reader reported %d bytes to discard, want 0", pending)
	}
	ch <- Chunk{Dir: FromClient, Bytes: []byte("GHI"), ReadAt: time.Unix(2, 0)}
	got = make([]byte, 3)
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatalf("read after stale watermark: %v", err)
	}
	if string(got) != "GHI" {
		t.Fatalf("got %q, want GHI — a stale watermark must not consume live bytes", got)
	}
}

// TestDiscardBeforeAfterCloseDoesNotBlock: a watermark armed against
// bytes that will now never arrive must not turn every later read into
// a block. Post-Close the discard is best effort.
func TestDiscardBeforeAfterCloseDoesNotBlock(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	ch <- Chunk{Dir: FromClient, Bytes: []byte("XY"), ReadAt: time.Unix(1, 0)}
	f := New(ch, nil, nil)
	f.DiscardBefore(1000)
	_ = f.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4)
		_, _ = f.Read(buf)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read blocked forever on a discard that can never complete after Close")
	}
}

// TestDiscardBeforeZeroLengthReadDoesNotBlock: a zero-length Read
// consumes nothing, so it must not be made to wait on a discard that is
// still waiting on the producer. The watermark check sits after the
// len(p)==0 early return for exactly this reason.
func TestDiscardBeforeZeroLengthReadDoesNotBlock(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk) // nothing queued, nothing coming
	f := New(ch, nil, nil)
	f.DiscardBefore(64)

	done := make(chan struct{})
	go func() {
		defer close(done)
		n, err := f.Read(nil)
		if n != 0 || err != nil {
			t.Errorf("Read(nil) = (%d, %v), want (0, nil)", n, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read with a zero-length buffer blocked on a pending discard")
	}
}

// TestSkipThroughStopsAtTheFirstChunkAfterThePoint: the rest of the chunk
// being read and every chunk captured no later than the point are discarded;
// the first chunk captured after it is left whole for the next Read, with its
// own capture time.
func TestSkipThroughStopsAtTheFirstChunkAfterThePoint(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 4)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("HEADtail"), ReadAt: time.Unix(1, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("more"), ReadAt: time.Unix(2, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("NEXT"), ReadAt: time.Unix(4, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("LAST"), ReadAt: time.Unix(5, 0)}
	f := New(ch, nil, nil)

	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	n, last, err := f.SkipThrough(time.Unix(3, 0))
	if err != nil {
		t.Fatalf("SkipThrough: %v", err)
	}
	if n != 8 || !last.Equal(time.Unix(2, 0)) {
		t.Fatalf("SkipThrough = (%d, %v), want (8, the second chunk's capture time): the rest of the first chunk and the whole second", n, last)
	}
	got := make([]byte, 8)
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatalf("read after the skip: %v", err)
	}
	if string(got) != "NEXTLAST" {
		t.Fatalf("after the skip the reader got %q, want NEXTLAST: the first chunk captured after the point starts the next read", got)
	}
	if want := int64(len("HEADtailmoreNEXTLAST")); f.Consumed() != want {
		t.Fatalf("Consumed = %d, want %d: discarded bytes have left the FakeConn", f.Consumed(), want)
	}
}

// TestSkipThroughKeepsTheChunksCaptureTime: the chunk left unread is read with
// its own capture time, as any chunk is.
func TestSkipThroughKeepsTheChunksCaptureTime(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("new"), ReadAt: time.Unix(7, 0)}
	f := New(ch, nil, nil)
	if _, _, err := f.SkipThrough(time.Unix(3, 0)); err != nil {
		t.Fatalf("SkipThrough: %v", err)
	}
	c, err := f.ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if string(c.Bytes) != "new" || !c.ReadAt.Equal(time.Unix(7, 0)) {
		t.Fatalf("ReadChunk = %q at %v, want \"new\" at its own capture time", c.Bytes, c.ReadAt)
	}
	if !f.LastReadTime().Equal(time.Unix(7, 0)) {
		t.Fatalf("LastReadTime = %v, want the chunk being read's", f.LastReadTime())
	}
}

// TestSkipThroughRefusesToCutAChunkCapturedAfterThePoint: a reader partway
// through a chunk captured after the point cannot be put at a boundary there;
// nothing is discarded.
func TestSkipThroughRefusesToCutAChunkCapturedAfterThePoint(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("ABCDEF"), ReadAt: time.Unix(5, 0)}
	f := New(ch, nil, nil)
	if _, err := io.ReadFull(f, make([]byte, 2)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n, _, err := f.SkipThrough(time.Unix(3, 0)); !errors.Is(err, ErrSkipMidChunk) || n != 0 {
		t.Fatalf("SkipThrough = (%d, %v), want (0, ErrSkipMidChunk)", n, err)
	}
	rest := make([]byte, 4)
	if _, err := io.ReadFull(f, rest); err != nil || string(rest) != "CDEF" {
		t.Fatalf("read after the refusal = %q, %v: want the chunk's rest, untouched", rest, err)
	}
}

// TestSkipThroughRefusesAChunkItCannotPlace: a chunk with no capture time, or
// captured at the point itself, cannot be told to be before or after it; it
// is left unread.
func TestSkipThroughRefusesAChunkItCannotPlace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		at   time.Time
	}{{"no capture time", time.Time{}}, {"captured at the point", time.Unix(3, 0)}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan Chunk, 2)
			ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0)}
			ch <- Chunk{Dir: FromDest, Bytes: []byte("unplaced"), ReadAt: c.at}
			f := New(ch, nil, nil)
			n, _, err := f.SkipThrough(time.Unix(3, 0))
			if !errors.Is(err, ErrUnplaced) || n != 3 {
				t.Fatalf("SkipThrough = (%d, %v), want (3, ErrUnplaced)", n, err)
			}
			got := make([]byte, 8)
			if _, err := io.ReadFull(f, got); err != nil || string(got) != "unplaced" {
				t.Fatalf("read after the refusal = %q, %v: want the chunk, unread", got, err)
			}
		})
	}
}

// TestSkipThroughWaitsForTheNextChunk: with nothing captured after the point
// yet, it waits for it, as a Read would.
func TestSkipThroughWaitsForTheNextChunk(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0)}
	f := New(ch, nil, nil)
	done := make(chan error, 1)
	go func() {
		_, _, err := f.SkipThrough(time.Unix(3, 0))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("SkipThrough returned (%v) before a chunk captured after the point arrived", err)
	case <-time.After(50 * time.Millisecond):
	}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("new"), ReadAt: time.Unix(4, 0)}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SkipThrough: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SkipThrough did not return once a chunk captured after the point arrived")
	}
}

// TestSkipThroughEndsWithTheStream: a stream that ends before a chunk captured
// after the point arrives ends the skip with what a Read gets there.
func TestSkipThroughEndsWithTheStream(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 1)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0)}
	close(ch)
	f := New(ch, nil, nil)
	if n, last, err := f.SkipThrough(time.Unix(3, 0)); err != io.EOF || n != 3 || !last.Equal(time.Unix(1, 0)) {
		t.Fatalf("SkipThrough = (%d, %v, %v), want (3, the chunk's capture time, io.EOF)", n, last, err)
	}

	closed := New(make(chan Chunk), nil, nil)
	_ = closed.Close()
	if _, _, err := closed.SkipThrough(time.Unix(3, 0)); !errors.Is(err, ErrClosed) {
		t.Fatalf("SkipThrough on a closed FakeConn = %v, want ErrClosed", err)
	}
}

// TestSkipThroughRefusesAfterReadingAChunkCapturedAfterThePoint: a reader that
// has read the whole of a chunk captured after the point (a length that ran
// over into the next answer) is past it; skipping on would start at an answer
// after the one the point starts. A chunk read whole that was captured at the
// point itself cannot be placed.
func TestSkipThroughRefusesAfterReadingAChunkCapturedAfterThePoint(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		at   time.Time
		want error
	}{{"captured after the point", time.Unix(5, 0), ErrSkipMidChunk}, {"captured at the point", time.Unix(3, 0), ErrUnplaced}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan Chunk, 2)
			ch <- Chunk{Dir: FromDest, Bytes: []byte("read"), ReadAt: c.at}
			ch <- Chunk{Dir: FromDest, Bytes: []byte("next"), ReadAt: time.Unix(6, 0)}
			f := New(ch, nil, nil)
			if _, err := io.ReadFull(f, make([]byte, 4)); err != nil {
				t.Fatalf("read: %v", err)
			}
			if n, _, err := f.SkipThrough(time.Unix(3, 0)); !errors.Is(err, c.want) || n != 0 {
				t.Fatalf("SkipThrough = (%d, %v), want (0, %v)", n, err, c.want)
			}
		})
	}
}

// TestSkipThroughLeavesAChunkItLeftUnread: a chunk a skip left unread, captured
// after a later skip's point too, is where that skip puts the reader as well.
func TestSkipThroughLeavesAChunkItLeftUnread(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("new"), ReadAt: time.Unix(7, 0)}
	f := New(ch, nil, nil)
	if _, _, err := f.SkipThrough(time.Unix(4, 0)); err != nil {
		t.Fatalf("SkipThrough: %v", err)
	}
	if n, _, err := f.SkipThrough(time.Unix(3, 0)); err != nil || n != 0 {
		t.Fatalf("a second SkipThrough = (%d, %v), want (0, nil): the reader is at the chunk already", n, err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(f, got); err != nil || string(got) != "new" {
		t.Fatalf("read = %q, %v, want the chunk left unread", got, err)
	}
}

// TestSkipThroughLastReadTimeNamesWhatWasRead: the chunk left unread is not
// the one LastReadTime names until its bytes are read.
func TestSkipThroughLastReadTimeNamesWhatWasRead(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0), WrittenAt: time.Unix(1, 1)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("new"), ReadAt: time.Unix(7, 0), WrittenAt: time.Unix(7, 1)}
	f := New(ch, nil, nil)
	if _, _, err := f.SkipThrough(time.Unix(4, 0)); err != nil {
		t.Fatalf("SkipThrough: %v", err)
	}
	if got := f.LastReadTime(); !got.Equal(time.Unix(1, 0)) {
		t.Fatalf("LastReadTime after the skip = %v, want the last chunk it discarded (%v), not the one it left unread", got, time.Unix(1, 0))
	}
	if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, w := f.LastReadTime(), f.LastWrittenTime(); !got.Equal(time.Unix(7, 0)) || !w.Equal(time.Unix(7, 1)) {
		t.Fatalf("LastReadTime/LastWrittenTime once its bytes are read = %v/%v, want the chunk's", got, w)
	}
}

// TestSkipThroughOnAClosedConnSkipsWhatArrived: a FakeConn closed with chunks
// still queued skips through them as an open one does, and leaves the first
// captured after the point readable.
func TestSkipThroughOnAClosedConnSkipsWhatArrived(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 2)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(1, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("new"), ReadAt: time.Unix(7, 0)}
	f := New(ch, nil, nil)
	_ = f.Close()
	if n, _, err := f.SkipThrough(time.Unix(4, 0)); err != nil || n != 3 {
		t.Fatalf("SkipThrough = (%d, %v), want (3, nil)", n, err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(f, got); err != nil || string(got) != "new" {
		t.Fatalf("read after the skip = %q, %v, want the chunk captured after the point", got, err)
	}
}

// TestSkipThroughHonoursAPendingDiscard: bytes a DiscardBefore watermark
// swallows are gone before the skip looks at the stream.
func TestSkipThroughHonoursAPendingDiscard(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 3)
	ch <- Chunk{Dir: FromDest, Bytes: []byte("SWALLOW"), ReadAt: time.Unix(9, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("old"), ReadAt: time.Unix(10, 0)}
	ch <- Chunk{Dir: FromDest, Bytes: []byte("new"), ReadAt: time.Unix(12, 0)}
	f := New(ch, nil, nil)
	f.DiscardBefore(7)
	if n, _, err := f.SkipThrough(time.Unix(11, 0)); err != nil || n != 3 {
		t.Fatalf("SkipThrough = (%d, %v), want (3, nil): the 7 bytes below the watermark are not the skip's", n, err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(f, got); err != nil || string(got) != "new" {
		t.Fatalf("read after the skip = %q, %v", got, err)
	}
}

// TestAtChunkBoundaryIsNoChunkReadInPart: the reader is at a chunk's boundary
// before its first byte, once it has read a chunk whole, and at a chunk Peek
// showed it that it has not taken. It is not once some of a chunk's bytes have
// left the FakeConn, read or swallowed for the reader by a watermark, until
// the rest of that chunk is read. Peek is how a session finds a request's
// first chunk (supervisor.Session.NextRequest): a boundary that meant "nothing
// in hand" would say every request found so starts in the middle of a chunk.
func TestAtChunkBoundaryIsNoChunkReadInPart(t *testing.T) {
	t.Parallel()
	ch := make(chan Chunk, 4)
	for _, b := range []string{"abcd", "efgh", "ijkl", "mnop"} {
		ch <- Chunk{Dir: FromClient, Bytes: []byte(b)}
	}
	f := New(ch, nil, nil)
	at := func(want bool, when string) {
		t.Helper()
		if got := f.AtChunkBoundary(); got != want {
			t.Fatalf("AtChunkBoundary = %v %s, want %v", got, when, want)
		}
	}
	read := func(n int) {
		t.Helper()
		if _, err := io.ReadFull(f, make([]byte, n)); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	peek := func(want string) {
		t.Helper()
		c, err := f.Peek()
		if err != nil || string(c.Bytes) != want {
			t.Fatalf("Peek = %q, %v; want %q", c.Bytes, err, want)
		}
	}
	at(true, "before the stream's first byte")
	read(2)
	at(false, "with half of the first chunk read")
	read(2)
	at(true, "with the first chunk read whole")
	peek("efgh")
	at(true, "at a whole chunk Peek showed and the reader has not taken")
	read(1)
	at(false, "with one byte of the second chunk read")
	peek("fgh")
	at(false, "at the rest of a chunk read in part, which Peek showed")
	if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != "fgh" {
		t.Fatalf("ReadChunk = %q, %v; want the rest of the second chunk", c.Bytes, err)
	}
	at(true, "with the rest of the second chunk read")
	// A watermark two bytes into the third chunk: they are swallowed for the
	// reader, and what is left does not start a chunk.
	f.DiscardBefore(f.Consumed() + 2)
	peek("kl")
	at(false, "at what a watermark left of the third chunk")
	read(2)
	at(true, "with the third chunk gone")
	peek("mnop")
	at(true, "at the fourth chunk, whole")
}
