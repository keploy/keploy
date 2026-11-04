//go:build linux

package http

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
)

// Through the relay, as the proxy records: a dependency answers a call with a
// chunked response and flushes its body before it ends the message (a Go
// handler that calls Flush, any server that streams). The client reads what it
// wants of it and closes the connection: a Go http.Client with keep-alives off
// closes on Body.Close once the app decoded the JSON it asked for, before the
// last chunk came; an app that abandons a stream, or whose own timeout fires,
// closes the same way. What the destination sends then, the client refuses
// with a reset, whether it reached the proxy before the client's FIN (the
// relay's forwarder was not run in between, as on a busy host) or after it (a
// handler that returns only as its client goes, which the relay's forwarding
// of the FIN tells it). The exchange is recorded as far as the client read it,
// with a stopped_reading fault (models.FaultStoppedReading),
// with no length, chunked as it was sent: replay serves it so, and holds the
// connection open without the end until the app closes it, as here. Before,
// the exchange was left out as one the client closed partway (unless what it
// refused, before its FIN, was the framing's end alone), so a test lost its
// dependency's mock (and, when the parser ran ahead of the case's save, the
// test case over it) on how the client's close raced the last chunk. A chunk
// the capture lost still leaves the exchange out, under its mark: what the
// stream holds is then not what the client read.
func TestRecordV2_ThroughTheRelayAChunkedResponseWhoseLastChunkTheClientDidNotTake(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n"
	const data = `{"id":"ch_1","status":"pending"}`
	chunk := "20\r\n" + data + "\r\n"
	// A last chunk whose extension is longer than the rest of its framing.
	longExt := "0;note=" + strings.Repeat("x", 200) + "\r\n\r\n"
	for _, row := range []struct {
		name string
		// read is what the client reads before it closes; after is what the
		// destination sends then, and later what it sends once the relay
		// said the client stopped, after the client's FIN.
		read, after, later string
		// mark: the capture marked the exchange's mock incomplete (a chunk
		// it lost) before the stream ended: left out.
		mark string
		// body is the body recorded: the content the client read.
		body string
	}{
		{name: "the last chunk and the empty line after it", read: head + chunk, after: "0\r\n\r\n", body: data},
		{name: "a last chunk with an extension", read: head + chunk, after: "0;done=1\r\n\r\n", body: data},
		{name: "a last chunk with a long extension", read: head + chunk, after: longExt, body: data},
		{name: "the empty line alone, after the last chunk the client took", read: head + chunk + "0\r\n", after: "\r\n", body: data},
		{name: "the CRLF after the data the client took, and the last chunk", read: head + "20\r\n" + data, after: "\r\n0\r\n\r\n", body: data},
		{name: "the last chunk, and the empty line after the client stopped", read: head + chunk, after: "0\r\n", later: "\r\n", body: data},
		{name: "the last chunk, and a trailer field after the client stopped", read: head + chunk, after: "0\r\n", later: "X-Checksum: 1\r\n\r\n", body: data},
		{name: "the last chunk, after a chunk the capture lost", read: head + chunk, after: "0\r\n\r\n", mark: "per_conn_cap"},
		{name: "the last chunk of an empty body", read: head, after: "0\r\n\r\n"},
		{name: "a data chunk before the last chunk", read: head + chunk, after: "5\r\nmore!\r\n0\r\n\r\n", body: data},
		{name: "the rest of a chunk the client took the start of", read: head + "20\r\n" + data[:10], after: data[10:] + "\r\n0\r\n\r\n", body: data[:10]},
		{name: "a trailer field", read: head + chunk, after: "0\r\nX-Checksum: 1\r\n\r\n", body: data},
		{name: "the last chunk without the empty line", read: head + chunk, after: "0\r\n", body: data},
		{name: "a stream the client abandoned", read: head + chunk + chunk, after: chunk + "0\r\n\r\n", body: data + data},
	} {
		for _, beforeFIN := range []bool{true, false} {
			for _, proven := range []string{"", "1"} {
				when := "after the client's FIN"
				if beforeFIN {
					when = "before the client's FIN"
				}
				t.Run(when+"/"+row.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
					t.Setenv("KEPLOY_PROVEN", proven)
					c := newRelayedConn(t)
					var held *readsHeldAfter
					if beforeFIN {
						held = &readsHeldAfter{n: len(row.read), release: make(chan struct{})}
						c.wrapDst = func(u net.Conn) net.Conn { held.Conn = u; return held }
					}
					c.start(t)
					if held != nil {
						// Before the relay's own cleanup waits for it to end.
						t.Cleanup(held.let)
					}
					write(t, c.app, c.src, jdkRequest(7))
					relayed(t, c.dest, jdkRequest(7))
					if _, err := c.dest.Write([]byte(row.read)); err != nil {
						t.Fatal(err)
					}
					relayed(t, c.app, row.read)
					if beforeFIN {
						// At the proxy's socket, unread: the relay's forwarder
						// reads it only once the client's FIN is in.
						write(t, c.dest, c.dst, row.after)
					}
					if err := c.app.Close(); err != nil {
						t.Fatal(err)
					}
					awaitFIN(t, c.dest)
					if beforeFIN {
						held.let()
					} else if _, err := c.dest.Write([]byte(row.after)); err != nil {
						t.Fatal(err)
					}
					if row.later != "" {
						for limit := time.Now().Add(10 * time.Second); !c.sess.EndedWhereClientStopped(); {
							if time.Now().After(limit) {
								t.Fatal("the relay did not say the client stopped")
							}
							time.Sleep(time.Millisecond)
						}
						if _, err := c.dest.Write([]byte(row.later)); err != nil {
							t.Fatal(err)
						}
					}
					if row.mark != "" {
						c.sess.MarkMockIncomplete(row.mark)
					}
					mocks := c.end(t)
					if !c.sess.EndedWhereClientStopped() {
						t.Fatal("the relay did not say the destination's stream ended where the client stopped")
					}
					if row.mark != "" {
						if len(mocks) != 0 {
							t.Fatalf("recorded %d mock(s), the first with the body %q; want none: a chunk the capture lost",
								len(mocks), mocks[0].Spec.HTTPResp.Body)
						}
						reason := row.mark + ": " + supervisor.LeftOutClientClosedPartway
						if l := c.leftOut(reason); len(l) != 1 || c.mgr.MocksLeftOut() != 1 {
							t.Fatalf("%d left-out lines for a chunk the capture lost, %d left out; want the one exchange", len(l), c.mgr.MocksLeftOut())
						}
						return
					}
					if len(mocks) != 1 {
						t.Fatalf("recorded %d mocks, want the one exchange, as far as the client read it", len(mocks))
					}
					wantStoppedReading(t, mocks[0], row.body, sentChunked)
					if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
						t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
					}
					if w := c.warnings(); len(w) != 0 {
						t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
					}
				})
			}
		}
	}
}

// readsHeldAfter is the destination's connection as the relay reads it, whose
// Reads return its first n bytes and then wait until let is called: what the
// destination sends meanwhile waits at the proxy's socket, unread, as it does
// on a busy host when the relay's forwarder is not run.
type readsHeldAfter struct {
	net.Conn
	n       int
	release chan struct{}
	once    sync.Once
}

func (c *readsHeldAfter) Read(p []byte) (int, error) {
	// Only the relay's forwarder reads it.
	if c.n == 0 {
		<-c.release
		return c.Conn.Read(p)
	}
	n, err := c.Conn.Read(p[:min(len(p), c.n)])
	c.n -= n
	return n, err
}

// NetConn is the conn under it, the connseq.Upstream the relay numbers by.
func (c *readsHeldAfter) NetConn() net.Conn { return c.Conn }

func (c *readsHeldAfter) let() { c.once.Do(func() { close(c.release) }) }
