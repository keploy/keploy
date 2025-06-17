package grpc

import (
	"bytes"
	"net"
)

// replayConn first serves the bytes in buf, then falls through to Conn.
type replayConn struct {
	net.Conn
	buf *bytes.Reader
}

func newReplayConn(initial []byte, c net.Conn) net.Conn {
	return &replayConn{
		Conn: c,
		buf:  bytes.NewReader(initial),
	}
}

func (r *replayConn) Read(p []byte) (int, error) {
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	return r.Conn.Read(p)
}
