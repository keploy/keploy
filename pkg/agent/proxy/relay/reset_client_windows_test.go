//go:build windows

package relay

import (
	"context"
	"errors"
	"io"
	"net"
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
