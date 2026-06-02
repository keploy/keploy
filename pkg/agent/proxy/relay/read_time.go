package relay

import (
	"net"
	"sync/atomic"
	"time"
)

type readTimeProvider interface {
	LastReadTime() time.Time
}

type stashedPayload struct {
	bytes  []byte
	readAt time.Time
}

func (s stashedPayload) len() int {
	return len(s.bytes)
}

func joinStashed(parts []stashedPayload) stashedPayload {
	if len(parts) == 0 {
		return stashedPayload{}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	total := 0
	var firstReadAt time.Time
	for _, p := range parts {
		total += len(p.bytes)
		if firstReadAt.IsZero() && !p.readAt.IsZero() {
			firstReadAt = p.readAt
		}
	}
	if total == 0 {
		return stashedPayload{}
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p.bytes...)
	}
	return stashedPayload{bytes: out, readAt: firstReadAt}
}

func observedReadAt(conn net.Conn, fallback time.Time) time.Time {
	if p, ok := conn.(readTimeProvider); ok {
		if ts := p.LastReadTime(); !ts.IsZero() {
			return ts
		}
	}
	return fallback
}

type readTrackingConn struct {
	net.Conn
	lastReadNano atomic.Int64
}

func newReadTrackingConn(conn net.Conn) *readTrackingConn {
	return &readTrackingConn{Conn: conn}
}

func (c *readTrackingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.lastReadNano.Store(observedReadAt(c.Conn, time.Now()).UnixNano())
	}
	return n, err
}

func (c *readTrackingConn) LastReadTime() time.Time {
	n := c.lastReadNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

type readTimeReportingConn struct {
	net.Conn
	source readTimeProvider
}

func newReadTimeReportingConn(conn net.Conn, source readTimeProvider) net.Conn {
	if conn == nil || source == nil {
		return conn
	}
	return &readTimeReportingConn{Conn: conn, source: source}
}

func (c *readTimeReportingConn) LastReadTime() time.Time {
	return c.source.LastReadTime()
}
