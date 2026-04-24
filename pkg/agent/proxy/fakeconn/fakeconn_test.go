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
