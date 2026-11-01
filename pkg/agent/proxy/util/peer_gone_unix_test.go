//go:build linux || darwin

package util

import (
	"net"
	"testing"
	"time"
)

// loopbackPair is a loopback TCP connection: the dialer's end and the
// accepted one.
func loopbackPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, s := tcpPair(t, ln)
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

// eventuallyGone waits, bounded, for PeerGone to say conn's peer has gone:
// the peer's FIN or reset reaches the socket a moment after the peer sends it.
func eventuallyGone(t *testing.T, conn net.Conn) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if gone, known := PeerGone(conn); !known {
			t.Fatal("PeerGone could not ask the socket")
		} else if gone {
			return
		}
	}
	t.Fatal("PeerGone never said the peer had gone")
}

// A peer that is there is not gone; one that closed is, with bytes it sent
// still unread before its FIN, as is one that reset; a wrapper that gives
// its conn is seen through; and a closed socket cannot be asked.
func TestPeerGone(t *testing.T) {
	c, s := loopbackPair(t)
	if gone, known := PeerGone(s); gone || !known {
		t.Fatalf("a peer that is there: gone %v, known %v", gone, known)
	}
	if _, err := c.Write([]byte("a request the server has not read")); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	eventuallyGone(t, s)
	eventuallyGone(t, &Conn{Conn: s})

	c, s = loopbackPair(t)
	_ = c.SetLinger(0)
	_ = c.Close() // a reset
	eventuallyGone(t, s)

	_ = s.Close()
	if _, known := PeerGone(s); known {
		t.Fatal("PeerGone asked a closed socket")
	}
}
