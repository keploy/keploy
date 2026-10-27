package fakeconn

import (
	"errors"
	"io"
	"testing"
)

// holeStream is a closed stream of chunks "a", "b", "c", "d" whose SeqNos
// skip 3 and 6: a hole before "c" and one before "d".
func holeStream() *FakeConn {
	ch := make(chan Chunk, 4)
	for i, c := range []struct {
		b   string
		seq uint64
	}{{"a", 1}, {"b", 2}, {"c", 4}, {"d", 7}} {
		ch <- Chunk{Dir: FromClient, ConnSeq: ConnSeqOf(uint64(i + 1)), Bytes: []byte(c.b), SeqNo: c.seq}
	}
	close(ch)
	return New(ch, nil, nil)
}

// A reader that reports holes is told of each one once, where it is, with
// nothing delivered, and reads on from the chunk after it.
func TestReportHoles_EachHoleOnceWhereItIs(t *testing.T) {
	f := holeStream()
	f.ReportHoles()
	var got []string
	buf := make([]byte, 8)
	for {
		n, err := f.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, ErrCaptureHole) {
			if n != 0 {
				t.Fatalf("ErrCaptureHole delivered %d bytes", n)
			}
			got = append(got, "|")
			continue
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, string(buf[:n]))
	}
	if want := []string{"a", "b", "|", "c", "|", "d"}; !equal(got, want) {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// Peek and ReadChunk report the hole too, and once: the next call returns the
// chunk after it.
func TestReportHoles_PeekAndReadChunk(t *testing.T) {
	f := holeStream()
	f.ReportHoles()
	for _, want := range []string{"a", "b"} {
		if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != want {
			t.Fatalf("ReadChunk = %q, %v; want %q", c.Bytes, err, want)
		}
	}
	if _, err := f.Peek(); !errors.Is(err, ErrCaptureHole) {
		t.Fatalf("Peek before the hole = %v, want ErrCaptureHole", err)
	}
	if c, err := f.Peek(); err != nil || string(c.Bytes) != "c" {
		t.Fatalf("Peek after the hole = %q, %v; want \"c\"", c.Bytes, err)
	}
	if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != "c" {
		t.Fatalf("ReadChunk = %q, %v; want \"c\"", c.Bytes, err)
	}
	if _, err := f.ReadChunk(); !errors.Is(err, ErrCaptureHole) {
		t.Fatalf("ReadChunk before the second hole = %v, want ErrCaptureHole", err)
	}
}

// A reader that does not ask reads across holes, as every reader always has.
func TestReportHoles_NotAskedReadsAcross(t *testing.T) {
	f := holeStream()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "abcd" {
		t.Fatalf("ReadAll = %q, %v; want \"abcd\"", b, err)
	}
}

// A chunk skipped before it is read takes its hole with it, and the chunk
// after it, which follows it with no hole, is read with none.
func TestReportHoles_SkippedChunkTakesItsHoleAlone(t *testing.T) {
	ch := make(chan Chunk, 3)
	for i, seq := range []uint64{1, 3, 4} {
		ch <- Chunk{Dir: FromClient, ConnSeq: ConnSeqOf(uint64(i + 1)), Bytes: []byte{'a' + byte(i)}, SeqNo: seq}
	}
	close(ch)
	f := New(ch, nil, nil)
	f.ReportHoles()
	if _, err := f.ReadChunk(); err != nil {
		t.Fatal(err)
	}
	f.DiscardBefore(2) // "b", after the hole, unread
	if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != "c" {
		t.Fatalf("ReadChunk = %q, %v; want \"c\" with no hole before it", c.Bytes, err)
	}
}

// A chunk skipped before it is read takes its hole with it: what the hole
// lost came before that chunk.
func TestReportHoles_SkippedChunkTakesItsHole(t *testing.T) {
	f := holeStream()
	f.ReportHoles()
	if _, err := f.ReadChunk(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadChunk(); err != nil {
		t.Fatal(err)
	}
	// Discard "c" (offset 2..3) without reading it: the hole before it goes
	// with it, and the one before "d" is still reported.
	f.DiscardBefore(3)
	if _, err := f.ReadChunk(); !errors.Is(err, ErrCaptureHole) {
		t.Fatalf("ReadChunk = %v, want the hole before \"d\"", err)
	}
	if c, err := f.ReadChunk(); err != nil || string(c.Bytes) != "d" {
		t.Fatalf("ReadChunk = %q, %v; want \"d\"", c.Bytes, err)
	}
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
