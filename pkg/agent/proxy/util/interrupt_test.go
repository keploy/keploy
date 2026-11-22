package util

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// enteredConn tells on entered as each read and write on it starts.
type enteredConn struct {
	net.Conn
	entered chan struct{}
}

func (c *enteredConn) Read(p []byte) (int, error) {
	c.entered <- struct{}{}
	return c.Conn.Read(p)
}

func (c *enteredConn) Write(p []byte) (int, error) {
	c.entered <- struct{}{}
	return c.Conn.Write(p)
}

// Once the context ends, the read and the write in progress on the
// connection end with a timeout, and undoing it leaves the connection usable,
// with no deadline.
func TestInterruptOnDoneEndsTheReadAndWriteInProgress(t *testing.T) {
	app, pipe := net.Pipe()
	defer app.Close()
	defer pipe.Close()
	conn := &enteredConn{Conn: pipe, entered: make(chan struct{}, 3)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	undo := InterruptOnDone(ctx, conn)

	// Nothing is written to conn, and nothing reads app: on a pipe both
	// wait for the other end.
	read, wrote := make(chan error, 1), make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); read <- err }()
	go func() { _, err := conn.Write([]byte("x")); wrote <- err }()
	<-conn.entered
	<-conn.entered
	cancel()
	for _, op := range []struct {
		name  string
		ended chan error
	}{{"read", read}, {"write", wrote}} {
		select {
		case err := <-op.ended:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("the %s ended with %v, want the deadline's timeout", op.name, err)
			}
		case <-time.After(time.Minute): // the bound only turns a hang into a failure
			t.Fatalf("the %s in progress did not end with the context", op.name)
		}
	}

	undo()
	undo() // a second undo is a no-op
	go func() { _, _ = app.Write([]byte("y")) }()
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil || buf[0] != 'y' {
		t.Fatalf("after the undo the connection read %q, %v; want \"y\": the deadline is still set", buf, err)
	}
}

// Undone before the context ends, it sets no deadline when the context ends.
func TestInterruptOnDoneUndoneBeforeTheEndSetsNoDeadline(t *testing.T) {
	app, conn := net.Pipe()
	defer app.Close()
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	InterruptOnDone(ctx, conn)()
	cancel()
	go func() { _, _ = app.Write([]byte("y")) }()
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil || buf[0] != 'y' {
		t.Fatalf("the connection read %q, %v; want \"y\": a deadline was set after the undo", buf, err)
	}
}

// JoinParser returns once the goroutine has closed its channel: the first
// result it sent, nil for the client's end (io.EOF) or for none, and the
// context's error once the context is done, but for a panic, which the proxy
// counts whenever it came.
func TestJoinParserReturnsWhatTheGoroutineReported(t *testing.T) {
	failed := errors.New("a parser failure")
	for _, tc := range []struct {
		name      string
		sent      []error
		cancelled bool
		want      error
	}{
		{"a failure", []error{failed}, false, failed},
		{"the first of two (a panic after a result)", []error{failed, ErrParserPanicked}, false, failed},
		{"the client's end", []error{io.EOF}, false, nil},
		{"no result", nil, false, nil},
		{"the context's end", []error{failed}, true, context.Canceled},
		{"the context's end, no result", nil, true, context.Canceled},
		{"a panic, the context ended", []error{ErrParserPanicked}, true, ErrParserPanicked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			errCh := make(chan error, len(tc.sent))
			for _, err := range tc.sent {
				errCh <- err
			}
			close(errCh)
			if got := JoinParser(ctx, errCh); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
				t.Fatalf("JoinParser = %v, want %v", got, tc.want)
			}
		})
	}
}
