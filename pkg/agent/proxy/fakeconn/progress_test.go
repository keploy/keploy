package fakeconn

import (
	"testing"
	"time"
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

// ReadThrough counts the bytes the discard watermark covers as done with
// before the reader pulls them: it is never handed them, however it stops.
func TestFakeConn_ReadThroughCountsWhatTheWatermarkCovers(t *testing.T) {
	ch := make(chan Chunk, 2)
	f := New(ch, nil, nil)
	ch <- Chunk{Bytes: []byte("hello"), ReadAt: time.Now()}
	ch <- Chunk{Bytes: []byte("world"), ReadAt: time.Now()}
	buf := make([]byte, 5)
	if n, err := f.Read(buf); err != nil || n != 5 {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if got := f.ReadThrough(); got != 5 {
		t.Fatalf("ReadThrough = %d after 5 bytes read, want 5", got)
	}
	f.DiscardBefore(8)
	if got, consumed := f.ReadThrough(), f.Consumed(); got != 8 || consumed != 5 {
		t.Fatalf("ReadThrough = %d, Consumed = %d with the watermark at 8 and nothing pulled, want 8 and 5", got, consumed)
	}
	if n, err := f.Read(buf); err != nil || n != 2 {
		t.Fatalf("Read = %d, %v, want the 2 bytes past the watermark", n, err)
	}
	if got := f.ReadThrough(); got != 10 {
		t.Fatalf("ReadThrough = %d, want 10", got)
	}
}
