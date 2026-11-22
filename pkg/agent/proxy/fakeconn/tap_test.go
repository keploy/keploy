package fakeconn

import (
	"bytes"
	"io"
	"testing"
	"time"
	"unsafe"
)

// Every connection has two FakeConns. The tap took the place of a field a
// FakeConn already had (a copy of consumed), so it is no larger for it: 304
// bytes on a 64-bit platform, as before.
func TestFakeConnSize(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sized for a 64-bit platform")
	}
	if got := unsafe.Sizeof(FakeConn{}); got > 304 {
		t.Fatalf("a FakeConn is %d bytes, more than the 304 it was", got)
	}
}

// The tap is told of every byte as it leaves the FakeConn, in the stream's
// order, however it leaves: read, swallowed below the discard watermark,
// dropped by the floor, or discarded by SkipThrough's cut. A peeked chunk has
// not left it, and Consumed counts what the tap was told.
func TestTheTapIsToldOfEveryByteAsItLeaves(t *testing.T) {
	ch := make(chan Chunk, 8)
	at := func(s int64) time.Time { return time.Unix(s, 0) }
	ch <- Chunk{Bytes: []byte("ab"), ConnSeq: 1, ReadAt: at(1)}   // swallowed: below the watermark
	ch <- Chunk{Bytes: []byte("cdef"), ConnSeq: 2, ReadAt: at(2)} // read in part, the rest cut
	ch <- Chunk{Bytes: []byte("gh"), ConnSeq: 3, ReadAt: at(2)}   // cut
	ch <- Chunk{Bytes: []byte("ij"), ConnSeq: 4, ReadAt: at(5)}   // dropped by the floor: numbered before 6
	ch <- Chunk{Bytes: []byte("kl"), ConnSeq: 6, ReadAt: at(6)}   // read
	close(ch)
	f := New(ch, nil, nil)
	var told []byte
	f.Tap(func(b []byte) { told = append(told, b...) })

	f.DiscardBefore(2)
	p := make([]byte, 1)
	if n, err := f.Read(p); n != 1 || err != nil || p[0] != 'c' {
		t.Fatalf("Read = %d %v %q, want the c after the swallowed ab", n, err, p[:n])
	}
	if n, _, err := f.SkipThrough(at(3)); err != nil || n != 5 {
		t.Fatalf("SkipThrough = %d, %v, want the 5 bytes captured before 3s cut", n, err)
	}
	f.DropBefore(6, func(Dropped) {})
	if c, err := f.Peek(); err != nil || string(c.Bytes) != "kl" {
		t.Fatalf("Peek = %q, %v, want kl past the floor", c.Bytes, err)
	}
	if want := "abcdefghij"; string(told) != want {
		t.Fatalf("the tap was told %q before the peeked chunk was read, want %q", told, want)
	}
	if _, err := io.ReadAll(f); err != nil {
		t.Fatal(err)
	}
	if want := "abcdefghijkl"; string(told) != want {
		t.Fatalf("the tap was told %q, want every byte of the stream once, in order: %q", told, want)
	}
	if got := f.Consumed(); got != int64(len(told)) {
		t.Fatalf("Consumed = %d, the tap was told %d bytes", got, len(told))
	}

	f.Tap(nil)
	if _, err := f.Read(p); err != io.EOF {
		t.Fatalf("Read after the end = %v, want io.EOF", err)
	}
	if !bytes.Equal(told, []byte("abcdefghijkl")) {
		t.Fatalf("the tap removed was told %q", told)
	}
}
