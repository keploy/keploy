//go:build linux

package proxy

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
)

// realSink is a real destination that reads what it is sent and never
// answers, nor closes, until the test ends: a sink. got is told of each read,
// and eof of the client's half-close.
func realSink(t *testing.T) (addr string, got chan string, eof chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	got, eof = make(chan string, 16), make(chan struct{}, 1)
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conns = append(conns, c)
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				got <- string(buf[:n])
			}
			if err != nil {
				eof <- struct{}{}
				return // and holds the connection open, answering nothing
			}
		}
	}()
	return ln.Addr().String(), got, eof
}

// A mock.real connection whose application has ended its side (an app keploy
// stopped at the run's last set, or one that gave up on its call and closed)
// while the real destination never answers holds no work of keploy's: what is
// left is the destination's answer, which a test's end, and a set's end, do
// not wait on, as a record does not. It was pending until the destination
// ended, so the last set's read of a --keep-app-alive run whose app wrote to
// a sink waited out the settle watchdog's 600 wake-ups (the real-deps lane's
// step 5b: ten minutes for a step of thirty seconds). While the app's side is
// open, the relay reads it and the connection is settled, as before. On the
// K-B16 tip (596f559b9) the second wait never ends.
func TestARealConnectionItsAppEndedWaitsOnNoAnswer(t *testing.T) {
	sink, got, eof := realSink(t)
	p := realProxy(t, []string{sink}, nil)
	tr := &inflight{}
	app, accepted := acceptedPair(t, tr)
	ctx := integrations.WithRequestHolder(context.Background(), accepted)
	done := make(chan served, 1)
	go func() {
		h, c, err := p.serveReal(ctx, accepted, sink, "", 1, nil)
		done <- served{h, c, err}
	}()

	_, err := io.WriteString(app, "GET /f HTTP/1.1\r\nHost: localhost\r\n\r\n")
	require.NoError(t, err)
	require.Contains(t, <-got, "GET /f")
	// Waited for on the tracker's own change events, with no budget: a
	// connection that stays pending hangs the test to go test's -timeout,
	// with every goroutine's stack.
	settled := func(what string) {
		t.Helper()
		t.Log(what)
		waitSettled(t, tr, func() bool { return tr.Pending(true) == 0 })
	}
	settled("the app's side open, the relay reading it")

	require.NoError(t, app.Close())
	<-eof // the relay passed the app's end on
	settled("the app's side ended, the destination silent")
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a record: pending %d, want 0", n)
	}
}
