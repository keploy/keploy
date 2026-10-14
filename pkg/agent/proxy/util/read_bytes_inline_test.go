package util

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// stackReader records whether each Read ran on its caller's goroutine: the
// frame of the function that called ReadBytes is on that goroutine's stack and
// on no other.
type stackReader struct {
	data   []byte
	caller string
	onCall []bool
}

func (s *stackReader) Read(p []byte) (int, error) {
	buf := make([]byte, 64<<10)
	s.onCall = append(s.onCall, strings.Contains(string(buf[:runtime.Stack(buf, false)]), s.caller))
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil
}

// ReadBytes frames the HTTP and generic parsers' messages, a read at a time on
// the capture path. A goroutine, a channel and an errgroup per Read bought
// nothing: the deferred errgroup Wait still waited for the Read, so a
// cancelled context could not return any earlier.
func TestReadBytes_ReadsOnTheCallersGoroutine(t *testing.T) {
	r := &stackReader{data: []byte(strings.Repeat("x", 3000)), caller: "TestReadBytes_ReadsOnTheCallersGoroutine"}
	got, err := ReadBytes(context.Background(), zap.NewNop(), r)
	if len(got) != 3000 || (err != nil && err != io.EOF) {
		t.Fatalf("read %d bytes (%v), want 3000", len(got), err)
	}
	for i, on := range r.onCall {
		if !on {
			t.Fatalf("Read %d ran on another goroutine than ReadBytes' caller", i+1)
		}
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("decoder bug") }

// A Read that panics used to be recovered on the helper goroutine, which then
// never answered: ReadBytes waited for it forever, and so did its parser.
func TestReadBytes_PanickingReaderEndsTheRead(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := ReadBytes(context.Background(), zap.NewNop(), panicReader{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a panicking Read returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBytes hung on a Read that panicked")
	}
}

// A context already cancelled ends the read before it blocks on the reader.
func TestReadBytes_CancelledContextDoesNotRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	block := make(chan struct{})
	defer close(block)
	go func() {
		_, err := ReadBytes(ctx, zap.NewNop(), blockingReader{block})
		done <- err
	}()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBytes blocked in Read on a cancelled context")
	}
}

type blockingReader struct{ c chan struct{} }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.c
	return 0, io.EOF
}
