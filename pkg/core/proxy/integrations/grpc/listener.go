package grpc

import (
	"net"
	"sync"
)

// singleConnListener is a net.Listener that only ever returns a single connection.
// This allows us to wrap an existing net.Conn and pass it to grpc.Serve(),
// which expects a listener.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	ch   chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{
		conn: conn,
		ch:   make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() {
		conn = l.conn
	})
	if conn != nil {
		return conn, nil
	}
	// Block forever after the first connection is returned.
	<-l.ch
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	close(l.ch)
	return l.conn.Close()
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}
