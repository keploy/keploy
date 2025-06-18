//go:build linux

package grpc

import (
	"net"
	"sync"
)

// singleConnListener adapts an existing net.Conn so it can be passed to
// grpc.Serve, which expects something that looks like a net.Listener but
// only ever needs to serve one connection.
type singleConnListener struct {
	conn      net.Conn  // the single connection we expose
	once      sync.Once // hands out conn exactly once
	closeOnce sync.Once // closes the "done" channel exactly once
	done      chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{
		conn: conn,
		done: make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var first bool
	l.once.Do(func() { first = true })

	if first {
		// Wrap the conn so that closing it notifies the listener.
		return &trackedConn{
			Conn: l.conn,
			onClose: func() {
				l.closeOnce.Do(func() { close(l.done) })
			},
		}, nil
	}

	// After the first connection, Serve() may call Accept() again.
	<-l.done // block until the trackedConn is closed
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.conn.Close()
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// trackedConn executes onClose exactly once when Close is called.
type trackedConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.onClose)
	return err
}
