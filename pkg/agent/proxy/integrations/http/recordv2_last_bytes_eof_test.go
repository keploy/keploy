//go:build linux

package http

import (
	"io"
	"net"
	"testing"
)

// endsWithItsLastBytes reads the destination through the conn under it, and
// its Read that returns the last of n bytes returns io.EOF with them, as
// io.Reader allows: the stream ends there.
type endsWithItsLastBytes struct {
	net.Conn
	n int
}

func (c *endsWithItsLastBytes) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, io.EOF
	}
	n, err := c.Conn.Read(p[:min(len(p), c.n)])
	c.n -= n
	if c.n == 0 && err == nil {
		err = io.EOF
	}
	return n, err
}

// NetConn is the conn under it, the connseq.Upstream the relay numbers by.
func (c *endsWithItsLastBytes) NetConn() net.Conn { return c.Conn }

// Through the relay, as the proxy records: the destination answers and ends
// the connection, and the Read that returns the answer's last bytes returns
// io.EOF with them. The relay writes them to the client and hands them to the
// parser before it ends the destination's stream, so the exchange is recorded
// whole and nothing is left out.
func TestRecordV2_ThroughTheRelayAnAnswerWhoseLastBytesComeWithEOFIsRecorded(t *testing.T) {
	t.Parallel()
	answer := jdkResponse(7)
	for _, row := range []struct {
		name  string
		first int // bytes the destination sends before the rest
	}{
		{"in one read", len(answer)},
		{"its last byte in a read of its own", len(answer) - 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			c := newRelayedConn(t)
			c.wrapDst = func(u net.Conn) net.Conn { return &endsWithItsLastBytes{Conn: u, n: len(answer)} }
			c.start(t)
			write(t, c.app, c.src, jdkRequest(7))
			relayed(t, c.dest, jdkRequest(7))
			write(t, c.dest, c.dst, answer[:row.first])
			relayed(t, c.app, answer[:row.first])
			if rest := answer[row.first:]; rest != "" {
				write(t, c.dest, c.dst, rest)
				relayed(t, c.app, rest)
			}
			mocks := c.end(t)
			assertPairs(t, mocks, 7)
			if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
				t.Fatalf("left out %v (%d counted), want nothing: the answer is whole", s, c.mgr.MocksLeftOut())
			}
		})
	}
}
