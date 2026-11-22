//go:build windows

package relay

import (
	"context"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

// headTaken writes "head" to the client through proxy and has the client
// read it.
func headTaken(t *testing.T, app, proxy net.Conn) {
	t.Helper()
	if _, err := proxy.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if _, err := io.ReadFull(app, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
}

// readsFIN fails the row unless the client's next read of app is the end of
// the stream, every byte before it read.
func readsFIN(t *testing.T, app net.Conn) {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if n, err := app.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Errorf("the client read %d bytes, %v after the close; want the FIN (EOF)", n, err)
	}
}

// With no count from the accept under the conn, Windows' socket cannot say
// what its send buffer holds: nothing is done, and the close stays a FIN, as
// before.
func TestWindowsResetClientOnCloseKeepsAFINWithNoCountFromTheAccept(t *testing.T) {
	t.Parallel()
	app, raw := loopbackPair(t)
	headTaken(t, app, raw)
	if resetClientOnClose(context.Background(), raw, time.Second) {
		t.Fatal("resetClientOnClose = true on a socket with no count of the writes to it; want the close kept a FIN")
	}
	_ = raw.Close()
	readsFIN(t, app)
}

// An answer of the socket that cannot say what the client took ends the wait
// at once, however long the grace, and the close stays a FIN: a failed
// SIO_TCP_INFO call, one shorter than keploy reads, counts outside the fence
// (a fast retransmit or a timeout ran), and a write around the count from the
// accept, straight to the socket, which the count did not see.
func TestWindowsResetClientOnCloseKeepsAFINWhenTheSocketCannotSay(t *testing.T) {
	t.Parallel()
	fenced := func(edit func(*winTCPInfo)) func(uintptr, *[wtcpInfoSize]byte) (int, error) {
		return func(fd uintptr, buf *[wtcpInfoSize]byte) (int, error) {
			n, err := getTCPInfo(fd, buf)
			info, ok := readWinTCPInfo(buf[:n])
			if err != nil || !ok {
				t.Errorf("the socket's own SIO_TCP_INFO answered %d bytes, %v", n, err)
				return n, err
			}
			edit(&info)
			return copy(buf[:], winAnswer(wtcpInfoSize, info)), nil
		}
	}
	for _, row := range []struct {
		name   string
		ask    func(uintptr, *[wtcpInfoSize]byte) (int, error)
		around bool // a byte written straight to the socket, around the count
	}{
		{name: "a failed call", ask: func(uintptr, *[wtcpInfoSize]byte) (int, error) { return 0, syscall.EINVAL }},
		{name: "a short answer", ask: func(fd uintptr, buf *[wtcpInfoSize]byte) (int, error) {
			n, err := getTCPInfo(fd, buf)
			return min(n, wtcpInfoNeeded-1), err
		}},
		{name: "a fast retransmit", ask: fenced(func(i *winTCPInfo) { i.fastRetrans = 1 })},
		{name: "a timeout", ask: fenced(func(i *winTCPInfo) { i.timeouts = 1 })},
		{name: "a write around the count", ask: getTCPInfo, around: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, raw := loopbackPair(t)
			proxy := CountClientWrites(raw)
			headTaken(t, app, proxy)
			if row.around {
				if _, err := AcceptedConn(proxy).Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(app, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
			}
			ctx, stop := context.WithCancel(context.Background())
			t.Cleanup(stop)
			done := make(chan bool, 1)
			go func() { done <- resetClientAsking(ctx, proxy, time.Hour, row.ask) }()
			select {
			case reset := <-done:
				if reset {
					t.Fatal("resetClientOnClose = true; want the close kept a FIN")
				}
			case <-time.After(tcpSettle):
				t.Fatalf("resetClientOnClose still waited after %v on an answer that cannot say; want it done at once", tcpSettle)
			}
			_ = proxy.Close()
			readsFIN(t, app)
		})
	}
}

// What was written is counted from the bytes the socket had sent as its count
// began: a socket that had sent bytes before it, every one of them and of the
// writes through it taken, is reset.
func TestWindowsResetClientOnCloseCountsFromWhatTheSocketHadSentAtTheAccept(t *testing.T) {
	t.Parallel()
	app, raw := loopbackPair(t)
	headTaken(t, app, raw)
	proxy := CountClientWrites(raw)
	if a, ok := proxy.(*countedClient); !ok {
		t.Fatal("the socket is not counted")
	} else if begun, _ := a.begin.sentOnce(); begun != 4 {
		t.Fatalf("the count began at %d bytes sent; want the 4 sent before it", begun)
	}
	headTaken(t, app, proxy)
	if !resetClientOnClose(context.Background(), proxy, tcpSettle) {
		t.Fatal("resetClientOnClose = false once the client took every byte, before the count and through it; want a reset")
	}
	_ = proxy.Close()
	if err := readsReset(t, app); err != nil {
		t.Errorf("the client read %v after the close; want a reset", err)
	}
}

// A Write through the count that failed may have written bytes it did not
// say it wrote (writeCount's unsure): the reset waits for them as for any
// byte written, and with none acknowledged within the grace, the close stays
// a FIN.
func TestWindowsResetClientOnCloseWaitsForWhatAFailedWriteMayHaveWritten(t *testing.T) {
	t.Parallel()
	app, raw := loopbackPair(t)
	proxy := CountClientWrites(raw)
	headTaken(t, app, proxy)
	a := proxy.(*countedClient)
	a.count.mu.Lock()
	a.count.unsure += 2
	a.count.mu.Unlock()
	if resetClientOnClose(context.Background(), proxy, 300*time.Millisecond) {
		t.Fatal("resetClientOnClose = true with 2 bytes a failed write may have written unacknowledged; want the close kept a FIN")
	}
	_ = proxy.Close()
	readsFIN(t, app)
}
