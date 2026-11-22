//go:build linux

package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The relay rows of a 408 an idle server sends as the client's next request
// is on its way, through recordv2_relay_order_test.go's relayedConn. They run
// on Linux, as they did before that file's rows ran on macOS and Windows.

// droppedLines counts the Debug lines for server bytes dropped with nothing
// left out for them: bytes that answer no request.
func (c *relayedConn) droppedLines() int {
	return c.logs.FilterMessageSnippet("dropped server bytes captured before the request after them").Len()
}

// refused408 checks the call a 408 raced in a round where the client closed
// without reading it (closeUnread), and returns the mocks without it. Either
// the relay wrote the 408 to the client before its close, and the 408 is in
// the capture, captured before the request and dropped with a Debug line: the
// end after it is the server's idle close, and nothing is recorded for the
// request (the client retries it on a new connection). Or it wrote it once the
// client had closed, which the client's stack refused: the 408 is no part of
// the capture (relay: client_took.go), the destination's stream ended where
// the client stopped, and the application closed with nothing read. The call
// is recorded as what it saw, a timeout before any answer, the last mock. One
// of the two, never both nor neither.
func refused408(t *testing.T, round int, c *relayedConn, mocks []*models.Mock, path string) []*models.Mock {
	t.Helper()
	dropped, stopped := c.droppedLines(), c.sess.EndedWhereClientStopped()
	if (dropped == 1) == stopped || dropped > 1 {
		t.Fatalf("round %d: %d Debug lines for the 408, the stream ended where the client stopped: %v; want either the "+
			"line or the stream ended there, not both", round, dropped, stopped)
	}
	if dropped == 1 {
		return mocks
	}
	if len(mocks) == 0 {
		t.Fatalf("round %d: nothing recorded for %s; want the call the application closed on with nothing read, as a "+
			"timeout", round, path)
	}
	m := mocks[len(mocks)-1]
	if m.Spec.HTTPReq == nil || !strings.HasSuffix(m.Spec.HTTPReq.URL, path) {
		t.Fatalf("round %d: the last mock is %v, want the call to %s", round, m.Spec.HTTPReq, path)
	}
	wantFault(t, m, models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true})
	return mocks[:len(mocks)-1]
}

// rounds is how many connections each test records: on the relay as it
// numbered chunks by the order its reads returned in, the 408 was taken for
// the request's answer in a few rounds of every hundred at the connection's
// start, and in tens mid-connection.
const rounds = 50

// Through the relay, as the proxy records: a server that times an idle
// keep-alive connection out sends a 408, and it reaches the proxy just before
// the client's next request does (the keep-alive idle-close race); the server
// closes without answering the request. The 408 reached the proxy before the
// request, so it is not its answer, and after the request before it, so it
// answers no request at all: no mock is recorded for the request (the client
// retries it on a new connection, where it is recorded), and nothing is left
// out, so no test case in flight is suppressed for it. Before, the request
// was recorded with the 408, a mock that replays a timeout for a request that
// never got one. The 408 marks the request (Session.ServerSpokeUnasked), so
// the end after it is not taken for a fault either.
func TestRecordV2_ThroughTheRelayA408SentBeforeTheNextRequestIsNotItsAnswer(t *testing.T) {
	t.Parallel()
	for i := 0; i < rounds; i++ {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, jdkRequest(7))
		relayed(t, c.dest, jdkRequest(7))
		write(t, c.dest, c.dst, jdkResponse(7))
		relayed(t, c.app, jdkResponse(7))
		write(t, c.dest, c.dst, idleTimeout) // at the proxy, read by the relay or not
		write(t, c.app, c.src, jdkRequest(8))
		relayed(t, c.dest, jdkRequest(8))
		// In half the rounds the client reads the 408 before it closes, so the
		// parser has it: nothing is recorded for the request. In the others
		// it closes without (refused408).
		read := i%2 == 0
		if read {
			relayed(t, c.app, idleTimeout)
		} else {
			closeUnread(t, c)
		}
		mocks := c.end(t)
		if !read {
			mocks = refused408(t, i, c, mocks, "/orders/8")
		}
		assertPairs(t, mocks, 7)
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
			t.Fatalf("round %d: left out %v (%d counted), want nothing: the 408 answers no request", i, s, c.mgr.MocksLeftOut())
		}
	}
}

// Through the relay, as the proxy records: the proxy dialled the destination
// while it held the application's connect, and the connection idled past the
// server's request timeout before the application's first request. The
// server's 408 and the request are both waiting when the relay starts. The
// relay carries the connection from its first byte, so nothing was sent on it
// before the capture began: the 408 is the server's own, and answers no
// request. It is not the request's answer, and nothing is left out for it, so
// the test case that sent the request (which the application retries on a new
// connection, where it is recorded) is not suppressed. Nor is the end after it
// taken for a fault on the connection's first request: the 408 marks the
// request (Session.ServerSpokeUnasked).
func TestRecordV2_ThroughTheRelayA408BeforeTheFirstRequestAnswersNoRequest(t *testing.T) {
	t.Parallel()
	for i := 0; i < rounds; i++ {
		c := newRelayedConn(t)
		write(t, c.dest, c.dst, idleTimeout)
		write(t, c.app, c.src, jdkRequest(7))
		c.start(t)
		relayed(t, c.dest, jdkRequest(7))
		// In half the rounds the client reads the 408 before it closes, so the
		// parser has it, and says it dropped it: nothing is recorded or left
		// out. In the others it closes without (refused408).
		read := i%2 == 0
		if read {
			relayed(t, c.app, idleTimeout)
		} else {
			closeUnread(t, c)
		}
		mocks := c.end(t)
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
			t.Fatalf("round %d: left out %v (%d counted); want nothing left out", i, s, c.mgr.MocksLeftOut())
		}
		if read {
			if dropped, stopped := c.droppedLines(), c.sess.EndedWhereClientStopped(); dropped != 1 || stopped {
				t.Fatalf("round %d: %d Debug lines for the 408, the stream ended where the client stopped: %v; want "+
					"the line: the client read the 408", i, dropped, stopped)
			}
		} else {
			mocks = refused408(t, i, c, mocks, "/orders/7")
		}
		if len(mocks) != 0 {
			t.Fatalf("round %d: recorded %d mock(s), the first %s -> %q (fault %v); want none: the 408 came before the "+
				"request, so the end after it is the server's idle close", i, len(mocks), mocks[0].Spec.HTTPReq.URL,
				mocks[0].Spec.HTTPResp.Body, mocks[0].Spec.Fault)
		}
	}
}
