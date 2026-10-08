// Package connseq numbers the chunks a relayed connection carries, both
// directions from one counter (fakeconn.Chunk.ConnSeq), at the proxy's end of
// its connection to the destination.
package connseq

import (
	"errors"
	"net"
	"sync"
	"syscall"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// Upstream is the proxy's end of a connection to a destination, made by
// NewUpstream where the connection is dialled, under any TLS. Everything the
// proxy reads from the destination is read through it, whatever reads it (a
// TLS handshake, a CONNECT tunnel's buffer, a probe, the relay), so what it has
// returned and what the socket has received are counted in the same bytes,
// from the connection's first.
//
// It numbers the chunks of both directions of the connection from one counter,
// by one rule: a chunk read from the client is numbered after every byte the
// destination had sent the proxy when the chunk was read (FromClient), and
// before every byte the destination sent after that (FromDest). What the
// destination had sent is the socket's count of the bytes it received
// (TCP_INFO tcpi_bytes_received), so bytes waiting in the socket, unread when
// the client's request is read, are numbered before the request: the
// destination sent them before it could have had the request, so they are not
// its answer. The order the two directions' reads return in cannot say that:
// they are read by two goroutines, and the one reading the destination may get
// to bytes that were waiting long after the other has read the request.
//
// It numbers by what had reached the proxy, not by when the destination sent
// it. Bytes the destination sent before it had the request, still on their way
// to the proxy when the request was read, are numbered after the request, and
// a parser takes them for its answer: at the proxy they cannot be told from
// one. That window is the round trip between the proxy and the destination:
// microseconds on loopback, and to a remote destination as wide as the
// scheduling window the socket's count closes, or wider. Off Linux, where no
// socket is asked (received_others.go), bytes waiting at the proxy are
// numbered after the request too.
//
// Nothing waits for anything. A client chunk read while bytes the destination
// sent wait unread places a cut at the socket's count and reserves, for those
// bytes, a number below its own, which they take when they are read. A Read
// never returns bytes from both sides of a cut, so a chunk read from the
// destination is wholly before or wholly after each client chunk; with TLS
// above, a chunk is a record's plaintext, and a record is after a client chunk
// if any of its bytes reached the proxy after it.
type Upstream struct {
	net.Conn
	// socket asks the TCP socket under Conn for its count of the bytes it has
	// received, on Linux; nil for any other conn, and for every conn off
	// Linux. Decided once, by NewUpstream (sentLocked).
	socket socketCounter

	mu sync.Mutex
	// n is the count of numbers given: the last is fakeconn.ConnSeqOf(n).
	n uint64
	// read is the count of bytes Read has returned.
	read uint64
	// cuts are the cuts no chunk read from the destination has passed yet,
	// ascending; last is the highest placed.
	cuts []cut
	last uint64
	// rest is what the conn returned past a cut, which the next Reads return
	// first, and restErr the error that came with it.
	rest    []byte
	restErr error
}

// cut is a client chunk's: at is the count of the bytes the destination had
// sent when the chunk was numbered, and n the number of the chunks read from
// the destination that end at or before it, one below the client chunk's.
type cut struct {
	at uint64
	n  uint64
}

// socketCounter is a socket's count of the bytes it has received.
type socketCounter interface{ received() uint64 }

// NewUpstream makes the proxy's end of the connection conn, just dialled to a
// destination: nothing has been read from it yet. It decides, once, what says
// what the destination has sent (sentLocked): the socket's count, for a TCP
// socket on Linux (socketCounterOf), and otherwise what Read has returned.
func NewUpstream(conn net.Conn) *Upstream {
	return &Upstream{Conn: conn, socket: socketCounterOf(conn)}
}

// Of returns the Upstream the conn c reads its destination through: c itself,
// or the conn beneath the wrappers that expose the conn under them (NetConn,
// the convention *tls.Conn set). nil if there is none.
func Of(c net.Conn) *Upstream {
	for i := 0; i < 8 && c != nil; i++ { // bounded: a cycle must not hang
		if u, ok := c.(*Upstream); ok {
			return u
		}
		w, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = w.NetConn()
	}
	return nil
}

// FromClient numbers a chunk read from the client, as its Read returns: after
// every byte the destination had sent the proxy by then, read or not.
func (u *Upstream) FromClient() uint32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if sent := u.sentLocked(); sent > u.last {
		u.n++
		u.cuts = append(u.cuts, cut{at: sent, n: u.n})
		u.last = sent
	}
	u.n++
	return fakeconn.ConnSeqOf(u.n)
}

// FromDest numbers a chunk read from the destination, through u, as its Read
// returns: with the first cut its bytes do not pass (the number reserved below
// the client chunk numbered when they were already sent), or, past every cut,
// after every chunk numbered so far.
func (u *Upstream) FromDest() uint32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	passed := 0
	for passed < len(u.cuts) && u.cuts[passed].at < u.read {
		passed++
	}
	u.cuts = append(u.cuts[:0], u.cuts[passed:]...)
	if len(u.cuts) > 0 {
		return fakeconn.ConnSeqOf(u.cuts[0].n)
	}
	u.n++
	return fakeconn.ConnSeqOf(u.n)
}

// Read reads what the destination sent, up to the next cut: bytes past it are
// held back and returned by the next Read, so no Read returns bytes from both
// sides of a cut.
func (u *Upstream) Read(p []byte) (int, error) {
	u.mu.Lock()
	if len(u.rest) > 0 {
		n := copy(p[:min(len(p), u.roomLocked(len(u.rest)))], u.rest)
		u.rest = u.rest[n:]
		u.read += uint64(n)
		var err error
		if len(u.rest) == 0 {
			u.rest, err, u.restErr = nil, u.restErr, nil
		}
		u.mu.Unlock()
		return n, err
	}
	u.mu.Unlock()

	n, err := u.Conn.Read(p)

	u.mu.Lock()
	defer u.mu.Unlock()
	if keep := u.roomLocked(n); keep < n {
		u.rest = append([]byte(nil), p[keep:n]...)
		u.restErr, err = err, nil
		n = keep
	}
	u.read += uint64(n)
	return n, err
}

// roomLocked is how many of the next n bytes a Read may return: up to the
// first cut ahead of what Read has returned. Caller holds mu.
func (u *Upstream) roomLocked(n int) int {
	for _, c := range u.cuts {
		if c.at > u.read {
			if ahead := c.at - u.read; ahead < uint64(n) {
				return int(ahead)
			}
			return n
		}
	}
	return n
}

// sentLocked is the count of the bytes the destination has sent the proxy
// that a Read can still return, by the one source NewUpstream chose for the
// connection: the socket's count of the bytes it received (one more once the
// destination's FIN is in, which only places a cut no byte is past); or, for
// a conn with no socket counter (a net.Pipe, which holds nothing back from
// its reader, or any conn off Linux, received_others.go), what Read has
// returned and the rest it holds back.
// Caller holds mu.
func (u *Upstream) sentLocked() uint64 {
	if u.socket == nil {
		return u.read + uint64(len(u.rest))
	}
	return u.socket.received()
}

var errNotASocket = errors.New("connseq: the connection is not a socket")

// SyscallConn is the socket's, for what looks at it without reading from it
// (a peek, a socket option): bytes read through it would not be counted.
func (u *Upstream) SyscallConn() (syscall.RawConn, error) {
	sc, ok := u.Conn.(syscall.Conn)
	if !ok {
		return nil, errNotASocket
	}
	return sc.SyscallConn()
}

// CloseWrite half-closes the connection, as the conn beneath does. Upstream
// embeds net.Conn as an interface, so its CloseWrite is not promoted, and the
// relay's half-close of a plaintext destination goes through here.
func (u *Upstream) CloseWrite() error {
	cw, ok := u.Conn.(interface{ CloseWrite() error })
	if !ok {
		return errNoCloseWrite
	}
	return cw.CloseWrite()
}

var errNoCloseWrite = errors.New("connseq: the connection cannot half-close")
