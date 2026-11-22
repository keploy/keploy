package relay

import (
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// lastBytesWithEOF is a conn whose stream ends after left bytes, and whose
// Read that returns the last of them returns io.EOF with them, as io.Reader
// allows.
type lastBytesWithEOF struct {
	net.Conn
	left int
}

func (c *lastBytesWithEOF) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.EOF
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.Conn.Read(p)
	c.left -= n
	if c.left == 0 && err == nil {
		err = io.EOF
	}
	return n, err
}

// NetConn is the conn beneath, so the relay finds the destination's
// connseq.Upstream under it (connseq.Of).
func (c *lastBytesWithEOF) NetConn() net.Conn { return c.Conn }

// A Read may return a stream's last bytes together with io.EOF. The relay
// forwards them to the peer and hands them to the parser before it ends that
// direction, from either side: the parser's stream reads them, then io.EOF.
func TestTheLastBytesOfAReadThatEndsTheStreamAreForwardedAndCaptured(t *testing.T) {
	t.Parallel()
	const last = "the last bytes"
	for _, c := range []struct {
		name   string
		client bool // the client's stream ends with them; else the destination's
	}{
		{"the destination's", false},
		{"the client's", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var h *relayHarness
			if c.client {
				h = newHarnessOver(t, Config{}, func(s net.Conn) net.Conn { return &lastBytesWithEOF{Conn: s, left: len(last)} })
			} else {
				h = newHarnessOverBoth(t, Config{}, nil, func(u *connseq.Upstream) net.Conn { return &lastBytesWithEOF{Conn: u, left: len(last)} })
			}
			from, to, stream, other, dir := h.destSvc, h.clientApp, h.r.DestStream(), h.r.ClientStream(), fakeconn.FromDest
			if c.client {
				from, to, stream, other, dir = h.clientApp, h.destSvc, h.r.ClientStream(), h.r.DestStream(), fakeconn.FromClient
			}
			wrote := make(chan error, 1)
			go func() { _, err := from.Write([]byte(last)); wrote <- err }()
			if got := readExact(t, to, len(last)); string(got) != last {
				t.Fatalf("the peer got %q, want %q", got, last)
			}
			if err := <-wrote; err != nil {
				t.Fatal(err)
			}
			_ = h.clientApp.Close()
			_ = h.destSvc.Close()
			select {
			case <-h.runDone:
			case <-time.After(10 * time.Second):
				t.Fatal("the relay did not end once both peers had closed")
			}
			ch, err := stream.ReadChunk()
			if err != nil || string(ch.Bytes) != last {
				t.Fatalf("the parser's stream read %q, %v, want %q: the bytes that came with io.EOF were not captured", ch.Bytes, err, last)
			}
			if ch.Dir != dir {
				t.Fatalf("the chunk's direction is %v, want %v", ch.Dir, dir)
			}
			if _, err := stream.ReadChunk(); err != io.EOF {
				t.Fatalf("after the last bytes, the parser's stream read %v, want io.EOF", err)
			}
			if _, err := other.ReadChunk(); err != io.EOF {
				t.Fatalf("the other stream, which carried nothing, read %v, want io.EOF", err)
			}
		})
	}
}
