package pkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
)

// peerEnd is how the app ended a connection keploy dialled for one test
// request, if it did: the error the first failed read of one of the request's
// connections returned for the app's end of it (io.EOF, a reset).
//
// net/http closes a connection itself once a read of it fails. When the app
// ends the connection while the request is still being written (a body sent
// in a write of its own after the headers, as net/http sends a body it cannot
// size up, or one too big for one write), the write that close cuts short can
// be what client.Do returns, "write tcp ...: use of closed network
// connection", which says only that net/http closed the connection. Which of
// the two net/http returns, its read's end or its write's, is a race inside
// it (a select between the two). The read is what happened: the app ended the
// connection, a drop like any other (isConnDropped).
//
// A peerEnd serves one request: its transport is the request's own
// (prepareHTTPRequest), and a request is sent again only after a refusal,
// which dials no connection.
type peerEnd struct {
	mu  sync.Mutex
	err error
}

// dial is the request's Transport.DialContext: it dials as net/http's own
// dialer does (a zero net.Dialer), and notes the app's end of each connection.
func (p *peerEnd) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &peerEndConn{Conn: c, end: p}, nil
}

func (p *peerEnd) note(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *peerEnd) ended() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// explain is err, an error client.Do returned for the request, with the app's
// end of its connection added when err is net/http's own close of that
// connection (net.ErrClosed) and the app had ended it first, as a
// *PeerEndedError that keeps err's *url.Error outermost. Any other error is
// returned as it is: the app's end is not what one says.
func (p *peerEnd) explain(err error) error {
	end := p.ended()
	if err == nil || end == nil || !errors.Is(err, net.ErrClosed) {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue == err {
		out := *ue
		out.Err = &PeerEndedError{Err: ue.Err, End: end}
		return &out
	}
	return &PeerEndedError{Err: err, End: end}
}

// PeerEndedError is a test request's error that net/http reported as its own
// close of the connection (Err), after the app had ended that connection
// (End: io.EOF, a reset). It is both: errors.Is finds either, so the drop
// classifies as the drop it is (isConnDropped).
type PeerEndedError struct {
	Err error
	End error
}

func (e *PeerEndedError) Error() string {
	return fmt.Sprintf("%v (the app had ended the connection: %v)", e.Err, e.End)
}

func (e *PeerEndedError) Unwrap() []error { return []error{e.Err, e.End} }

// peerEndConn is a connection of a test request that notes the app's end of
// it (peerEnd).
type peerEndConn struct {
	net.Conn
	end *peerEnd
}

// ReadFrom is the connection's own, so net/http writes a request body as it
// does on a bare connection (a *net.TCPConn's ReadFrom, which can splice).
func (c *peerEndConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

// Read notes a failed read that is the app's end of the connection
// (isConnDropped: io.EOF, a reset), and no other: not net/http's own close of
// it (net.ErrClosed), nor a deadline.
func (c *peerEndConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && isConnDropped(err) {
		c.end.note(err)
	}
	return n, err
}
