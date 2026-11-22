package relay

import (
	"net"
	"runtime"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
)

// The peer a forwarder writes to can take the bytes and act on them before
// the Write returns to the forwarder: the write wakes the peer, and on a
// starved host the forwarder gets its CPU back only later. An application
// that reads an answer and then its clock has read a time before the stamp
// of an answer stamped once the Write returned. The sockets below hold the
// relay's Write until the peer has taken the bytes and read its clock, and
// until the clock has moved past that read; each chunk's WrittenAt must be
// no later than the peer's read of the clock, and no earlier than its
// ReadAt.

// actsBeforeReturn is a socket whose Write returns only once the peer has
// taken the bytes (net.Pipe's Write waits for the peer's Read), said on acted
// when it then read its clock, and the clock has moved past that read.
type actsBeforeReturn struct {
	net.Conn
	acted <-chan time.Time
}

func (c *actsBeforeReturn) Write(p []byte) (int, error) {
	return writeThenWaitForTheAct(c.Conn, p, c.acted)
}

// upstreamActsBeforeReturn is actsBeforeReturn over the destination's
// Upstream, which the relay finds beneath it (connseq.Of).
type upstreamActsBeforeReturn struct {
	*connseq.Upstream
	acted <-chan time.Time
}

func (c *upstreamActsBeforeReturn) Write(p []byte) (int, error) {
	return writeThenWaitForTheAct(c.Upstream, p, c.acted)
}

// NetConn exposes the Upstream beneath, as a TLS conn does (connseq.Of).
func (c *upstreamActsBeforeReturn) NetConn() net.Conn { return c.Upstream }

func writeThenWaitForTheAct(c net.Conn, p []byte, acted <-chan time.Time) (int, error) {
	n, err := c.Write(p)
	if err != nil {
		return n, err
	}
	at := <-acted
	for !time.Now().After(at) {
		runtime.Gosched()
	}
	return n, nil
}

func TestWrittenAtIsNoLaterThanWhatThePeerDidWithTheBytes(t *testing.T) {
	t.Parallel()
	appActed := make(chan time.Time, 1)
	svcActed := make(chan time.Time, 1)
	h := newHarnessOverBoth(t, Config{}, func(c net.Conn) net.Conn {
		return &actsBeforeReturn{Conn: c, acted: appActed}
	}, func(u *connseq.Upstream) net.Conn {
		return &upstreamActsBeforeReturn{Upstream: u, acted: svcActed}
	})
	defer h.shutdown()

	// The request: the service takes it and reads its clock while the
	// relay's Write to it has not returned.
	wrote := make(chan error, 2)
	write := func(c net.Conn, p string) {
		_, err := c.Write([]byte(p))
		wrote <- err
	}
	go write(h.clientApp, "PutItem")
	h.readDest(len("PutItem"))
	svcAt := time.Now()
	svcActed <- svcAt
	req := readChunk(t, h.r.ClientStream())

	// The answer: the application takes it and reads its clock (as a
	// consumer stamps the event a pushed message is about) while the
	// relay's Write to it has not returned.
	go write(h.destSvc, "lead.created")
	h.readClient(len("lead.created"))
	appAt := time.Now()
	appActed <- appAt
	res := readChunk(t, h.r.DestStream())
	for i := 0; i < 2; i++ {
		if err := <-wrote; err != nil {
			t.Fatalf("a peer's write: %v", err)
		}
	}

	for _, c := range []struct {
		name          string
		readAt, wrote time.Time
		peerAt        time.Time
	}{
		{"the request, to the service", req.ReadAt, req.WrittenAt, svcAt},
		{"the answer, to the application", res.ReadAt, res.WrittenAt, appAt},
	} {
		if c.wrote.IsZero() {
			t.Fatalf("%s: no WrittenAt", c.name)
		}
		if c.wrote.After(c.peerAt) {
			t.Errorf("%s: WrittenAt %s is %s after the peer took the bytes and read its clock (%s): the stamp is later than what the peer did with them",
				c.name, c.wrote.Format(time.RFC3339Nano), c.wrote.Sub(c.peerAt), c.peerAt.Format(time.RFC3339Nano))
		}
		if c.wrote.Before(c.readAt) {
			t.Errorf("%s: WrittenAt %s is before ReadAt %s", c.name, c.wrote.Format(time.RFC3339Nano), c.readAt.Format(time.RFC3339Nano))
		}
	}
}
