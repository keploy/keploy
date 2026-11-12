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
// handler that calls Flush, any server that streams). The client reads the
// head and the body, has the whole JSON it asked for, and closes the
// connection (a Go http.Client with keep-alives off closes on Body.Close
// before EOF). The end of the message's framing, the last chunk and the empty
// line after it, had reached the proxy before the client's FIN, but the relay
// writes it to the client only after that (its forwarder was not run in
// between, as on a busy host), and the client refuses it with a reset. The
// destination ended the message before it could know the client had closed,
// and the client took every byte the response carries: the exchange is whole
// and recorded, its body the one the client read, and nothing is left out.
// Before, the parser was given the stream only up to the refused bytes, found
// a chunked body with no end, and left the exchange out as one the client
// closed partway, so a test lost its dependency's mock (and, when the parser
// ran ahead of the case's save, the test case over it) on how the client's
// close raced the last chunk. The rows end what the destination sends in ways
// that carry content, and those stay left out: the client never read it.
//
// The same ends reaching the proxy only after the client's FIN, which the
// relay forwards to the destination, stay left out, as before: a server whose
// streaming handler returns when its client goes (an SSE or long-poll answer
// the app stopped reading) writes them then, in answer to the close, and the
// client did not have all the destination would have sent.
func TestRecordV2_ThroughTheRelayAChunkedResponseWhoseLastChunkTheClientDidNotTake(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n"
	const data = `{"id":"ch_1","status":"pending"}`
	chunk := "20\r\n" + data + "\r\n"
	// A last chunk whose extension is longer than any end of framing is
	// short: what the relay keeps of the refusal holds it whole.
	longExt := "0;note=" + strings.Repeat("x", 200) + "\r\n\r\n"
	for _, row := range []struct {
		name string
		// read is what the client reads before it closes; after is what the
		// destination sends then, and later what it sends once the relay
		// said the client stopped, after the client's FIN.
		read, after, later string
		// mark: the capture marked the exchange's mock incomplete (a chunk
		// it lost) before the stream ended.
		mark string
		// whole: the exchange is recorded with body when after reached the
		// proxy before the client's FIN; else it is left out. It is left
		// out whenever after reached the proxy after the FIN.
		whole bool
		body  string
	}{
		{name: "the last chunk and the empty line after it", read: head + chunk, after: "0\r\n\r\n", whole: true, body: data},
		{name: "a last chunk with an extension", read: head + chunk, after: "0;done=1\r\n\r\n", whole: true, body: data},
		{name: "a last chunk with a long extension", read: head + chunk, after: longExt, whole: true, body: data},
		{name: "the empty line alone, after the last chunk the client took", read: head + chunk + "0\r\n", after: "\r\n", whole: true, body: data},
		{name: "the CRLF after the data the client took, and the last chunk", read: head + "20\r\n" + data, after: "\r\n0\r\n\r\n", whole: true, body: data},
		{name: "the last chunk, and the empty line after the client stopped", read: head + chunk, after: "0\r\n", later: "\r\n"},
		{name: "the last chunk, and a trailer field after the client stopped", read: head + chunk, after: "0\r\n", later: "X-Checksum: 1\r\n\r\n"},
		{name: "the last chunk, after a chunk the capture lost", read: head + chunk, after: "0\r\n\r\n", mark: "per_conn_cap"},
		{name: "the last chunk of an empty body", read: head, after: "0\r\n\r\n", whole: true},
		{name: "a data chunk before the last chunk", read: head + chunk, after: "5\r\nmore!\r\n0\r\n\r\n"},
		{name: "the rest of a chunk the client took the start of", read: head + "20\r\n" + data[:10], after: data[10:] + "\r\n0\r\n\r\n"},
		{name: "a trailer field", read: head + chunk, after: "0\r\nX-Checksum: 1\r\n\r\n"},
		{name: "the last chunk without the empty line", read: head + chunk, after: "0\r\n"},
	} {
		for _, beforeFIN := range []bool{true, false} {
			when := "after the client's FIN"
			if beforeFIN {
				when = "before the client's FIN"
			}
			t.Run(when+"/"+row.name, func(t *testing.T) {
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
				if !row.whole || !beforeFIN {
					if len(mocks) != 0 {
						t.Fatalf("recorded %d mock(s), the first with the body %q; want none: the client never read what came after its close",
							len(mocks), mocks[0].Spec.HTTPResp.Body)
					}
					reason := supervisor.LeftOutClientClosedPartway
					if row.mark != "" {
						reason = row.mark + ": " + reason
					}
					if l := c.leftOut(reason); len(l) != 1 || c.mgr.MocksLeftOut() != 1 {
						t.Fatalf("%d left-out lines for a close partway, %d left out; want the one exchange", len(l), c.mgr.MocksLeftOut())
					}
					return
				}
				if len(mocks) != 1 {
					t.Fatalf("recorded %d mocks, want the one exchange: the client took every byte the response carries", len(mocks))
				}
				if got := mocks[0].Spec.HTTPResp; got.StatusCode != 200 || got.Body != row.body {
					t.Fatalf("recorded a %d with the body %q, want a 200 with %q", got.StatusCode, got.Body, row.body)
				}
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
