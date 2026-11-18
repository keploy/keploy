//go:build darwin || windows

package relay

import (
	"net"
	"sync"
	"syscall"
)

// The count from the accept: macOS's and Windows' sockets do not say how many
// bytes were written to them (client_took_darwin.go, client_socket_windows.go),
// so the proxy counts every byte written to a client's socket from its accept
// (CountClientWrites), and the relay's looks at that socket read it
// (countFromAccept). Each OS keeps what its socket answered as the count
// began (acceptInfo, answerAtAccept) and begins its looks from it (beginFrom).

// writeCount counts the bytes written to a client's socket through
// countedClient: said, those the Writes said they wrote; running, every
// byte of the Writes still running; unsure, those failed Writes did not say
// they wrote, which they may have.
type writeCount struct {
	mu                    sync.Mutex
	said, running, unsure uint64
}

// tally is a writeCount at one instant: the bytes the Writes said they wrote,
// and maybe, those that may have been written as well.
type tally struct{ said, maybe uint64 }

func (c *writeCount) now() tally {
	c.mu.Lock()
	defer c.mu.Unlock()
	return tally{said: c.said, maybe: c.running + c.unsure}
}

// write writes b to conn, counting it.
func (c *writeCount) write(conn net.Conn, b []byte) (int, error) {
	c.mu.Lock()
	c.running += uint64(len(b))
	c.mu.Unlock()
	n, err := conn.Write(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	// A Write returns no more than it was given (io.Writer).
	c.running -= uint64(len(b))
	c.said += uint64(n)
	c.unsure += uint64(len(b) - n)
	return n, err
}

// countFromAccept has the looks at c, the TCP socket under src
// (clientSocketOf), read what the client acknowledged from the count the proxy
// put on that socket at its accept (CountClientWrites), when one is under src:
// every layer's writes, the relay's among them, reach the socket through it.
// On Windows the looks read from it what was written to the socket
// (client_socket_windows.go). With none, c is asked as its socket alone
// answers: macOS by the count of sequence numbers, Windows by the bytes sent.
// Run in New, before any look.
func countFromAccept(src net.Conn, c clientSocket) {
	s, ok := c.(*tcpClientSocket)
	if !ok {
		return
	}
	if a := countedClientUnder(src); a != nil {
		s.beginFrom(a)
	}
}

// countedClient is a client's socket as the proxy accepted it (tcp), counting
// every byte written to it (writeCount) from begin, its TCP_INFO as the count
// began (CountClientWrites). It promotes only net.Conn's methods, so a
// ReadFrom of the socket (sendfile) cannot write around the count, and gives
// the socket to what reads its state (SyscallConn: the proxy's tracking, the
// walks to the socket) or half-closes it (CloseWrite), and the socket itself
// (NetConn, AcceptedConn).
type countedClient struct {
	net.Conn
	tcp   *net.TCPConn
	count *writeCount
	begin acceptInfo
}

func (c *countedClient) Write(b []byte) (int, error) { return c.count.write(c.tcp, b) }

// NetConn is the socket under the count.
func (c *countedClient) NetConn() net.Conn { return c.tcp }

// SyscallConn is the socket's own: what reads the kernel's state of the
// socket reads it through the count. Nothing writes through it.
func (c *countedClient) SyscallConn() (syscall.RawConn, error) { return c.tcp.SyscallConn() }

// CloseWrite half-closes the socket.
func (c *countedClient) CloseWrite() error { return c.tcp.CloseWrite() }

// CountClientWrites is the client connection c, as the proxy accepted it, for
// every layer above it (the proxy's tracking, TLS, a parser, the relay) to
// write through: a count of every byte written to its socket, which the
// relay's look at the client reads what was written to the client from, and
// on macOS what the client acknowledged (countFromAccept). c itself when it is
// not a *net.TCPConn (a nil one among them, whose SyscallConn would
// dereference it), or its socket cannot answer as the count begins
// (answerAtAccept): the relay then asks the socket alone (countFromAccept).
// Call it once, as c is accepted, before anything is written to it. What
// keys or reads the accepted socket itself finds it under the count
// (AcceptedConn, NetConn).
func CountClientWrites(c net.Conn) net.Conn {
	tc, ok := c.(*net.TCPConn)
	if !ok || tc == nil {
		return c
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return c
	}
	a := &countedClient{Conn: tc, tcp: tc, count: &writeCount{}}
	answered := false
	// A socket Control cannot reach (closed) asks nothing, which reads as no
	// answer.
	_ = rc.Control(func(fd uintptr) { answered = answerAtAccept(fd, &a.begin) })
	if !answered {
		return c
	}
	return a
}

// AcceptedConn is the conn the listener gave, under CountClientWrites' count;
// c itself when c is not that count.
func AcceptedConn(c net.Conn) net.Conn {
	if a, ok := c.(*countedClient); ok {
		return a.tcp
	}
	return c
}

// countedClientUnder is the count the proxy put on the client's socket at its
// accept (countedClient) among c and the conns under it (NetConn), within the
// bound clientSocketOf walks; nil when there is none. The walk stops at the
// first *net.TCPConn, which has no conn under it, so the count it finds is
// over the socket clientSocketOf found.
func countedClientUnder(c net.Conn) *countedClient {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if a, ok := c.(*countedClient); ok {
			return a
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
}
