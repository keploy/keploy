//go:build windows

package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A Windows socket with no count from the accept under it (CountClientWrites)
// cannot say whether the client acknowledged every byte written to it: no
// count of what its send buffer holds. It is not reset, though the client
// read every byte: the proxy's close stays a FIN, and the client reads the
// end of the stream.
func TestResetClientOnCloseKeepsAFINWithNoCountFromTheAccept(t *testing.T) {
	app, counted := tcpPair(t)
	proxy := AcceptedConn(counted) // the socket alone, as the listener gave it
	if _, ok := proxy.(*net.TCPConn); !ok {
		t.Fatalf("the accepted conn is %T, want the socket", proxy)
	}
	if _, err := proxy.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(app, buf); err != nil {
		t.Fatal(err)
	}
	if resetClientOnClose(context.Background(), proxy, time.Second) {
		t.Fatal("resetClientOnClose = true for a Windows socket with no count from the accept; want the close kept a FIN")
	}
	_ = proxy.Close()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := app.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("the client read %d byte(s), %v after the close; want the end of the stream", n, err)
	}
}

// The count from the accept with a write that went around it (a write to the
// socket itself): the count cannot say what was written, so the socket is not
// reset, and the client reads every byte, then the end of the stream.
func TestResetClientOnCloseKeepsAFINWhenAWriteWentAroundTheCount(t *testing.T) {
	app, counted := tcpPair(t)
	if _, err := AcceptedConn(counted).Write([]byte("around")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(app, buf); err != nil {
		t.Fatal(err)
	}
	if resetClientOnClose(context.Background(), counted, time.Second) {
		t.Fatal("resetClientOnClose = true with a write around the count; want the close kept a FIN")
	}
	_ = counted.Close()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := app.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("the client read %d byte(s), %v after the close; want the end of the stream", n, err)
	}
}

// The wait on the client's socket (resetOnceAcked), on its answers alone: an
// answer shorter than keploy reads says nothing of the client, so nothing is
// reset, at once, though grace is an hour; a connection the socket says is
// closed is reset at once, though 2 of the 6 bytes written are in flight: a
// closed connection holds nothing the client could still take.
func TestResetOnceAckedTakesTheSocketsWordAtOnce(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		ans    []byte
		resets int32
	}{
		{"an answer too short", winInfo(wtcpInfoNeeded-1, wtcpEstablished, 0, 6, 0, 0), 0},
		{"closed, bytes in flight", winInfo(wtcpInfoSize, wtcpClosed, 2, 6, 0, 0), 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := winAnsweringSocket(func() []byte { return row.ans })
			s.beginFrom(&countedClient{count: &writeCount{said: 6}})
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel) // ends a wait that did not end at once
			var resets atomic.Int32
			got := make(chan bool, 1)
			go func() {
				got <- resetOnceAcked(ctx, s, time.Hour, func() bool { resets.Add(1); return true })
			}()
			select {
			case reset := <-got:
				if want := row.resets == 1; reset != want || resets.Load() != row.resets {
					t.Fatalf("resetOnceAcked = %v after %d reset(s); want %v after %d", reset, resets.Load(), want, row.resets)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resetOnceAcked still waits after 5s; want its answer at once")
			}
		})
	}
}
