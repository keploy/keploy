package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// jdkRequest is a request of a JDK HttpURLConnection client for order id: 182
// bytes for a one-digit id, 183 for a two-digit one.
func jdkRequest(id int) string {
	return fmt.Sprintf("GET /orders/%d HTTP/1.1\r\nAccept: application/json\r\nUser-Agent: Java/17.0.12\r\n"+
		"Host: orders.internal:8443\r\nConnection: keep-alive\r\nX-Request-Source: checkout-service-replica-pool-01\r\n\r\n", id)
}

// jdkResponse is the answer to jdkRequest(id), which names id in its body:
// 203 bytes for a one-digit id, 204 for a two-digit one.
func jdkResponse(id int) string {
	body := fmt.Sprintf(`{"order":%d,"status":"SHIPPED","items":3}`, id)
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nDate: Mon, 05 Oct 2026 10:00:00 GMT\r\n"+
		"Server: orders-service-edge01\r\nConnection: keep-alive\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

// idleTimeout is a server's 408 as it times an idle keep-alive connection out.
const idleTimeout = "HTTP/1.1 408 Request Timeout\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"

// spanLog is a session's OrphanSpans that keeps every span it is given.
type spanLog struct {
	mu    sync.Mutex
	spans [][2]time.Time
}

func (l *spanLog) Record(start, end time.Time) {
	l.mu.Lock()
	l.spans = append(l.spans, [2]time.Time{start, end})
	l.mu.Unlock()
}

func (l *spanLog) Open(start time.Time) func() { return func() { l.Record(start, time.Now()) } }

func (l *spanLog) all() [][2]time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][2]time.Time(nil), l.spans...)
}

// capture is a connection as a producer hands it to recordV2: each chunk
// numbered with the connection-wide capture sequence the test gives it
// (fakeconn.Chunk.ConnSeq), whatever order the test hands it over in.
type capture struct {
	sess         *supervisor.Session
	client, dest chan fakeconn.Chunk
	mocks        chan *models.Mock
	spans        *spanLog
	mgr          *syncMock.SyncMockManager
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{client: make(chan fakeconn.Chunk, 64), dest: make(chan fakeconn.Chunk, 64),
		mocks: make(chan *models.Mock, 64), spans: &spanLog{}, mgr: syncMock.New(nil)}
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443}
	c.sess = &supervisor.Session{
		ClientStream: fakeconn.New(c.client, addr, addr),
		DestStream:   fakeconn.New(c.dest, addr, addr),
		Mocks:        c.mocks,
		Mgr:          c.mgr,
		Orphans:      c.spans,
		Logger:       zaptest.NewLogger(t),
		ClientConnID: "pooled-1",
		Ctx:          context.Background(),
	}
	return c
}

// newJoinedCapture is a capture of a connection that began after the
// connection was established (an agent restart, a TLS hook attached to a
// pooled connection), as its producer says (Session.JoinedMidConnection).
func newJoinedCapture(t *testing.T) *capture {
	t.Helper()
	c := newCapture(t)
	c.sess.JoinedMidConnection = true
	return c
}

// capturedAt is the capture time of the chunk numbered seq.
func capturedAt(seq uint32) time.Time {
	return time.Unix(1_759_658_400, 0).Add(time.Duration(seq) * 10 * time.Millisecond)
}

func (c *capture) req(seq uint32, b string) {
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: seq, Bytes: []byte(b), ReadAt: capturedAt(seq), WrittenAt: capturedAt(seq)}
}

func (c *capture) resp(seq uint32, b string) {
	c.dest <- fakeconn.Chunk{Dir: fakeconn.FromDest, ConnSeq: seq, Bytes: []byte(b), ReadAt: capturedAt(seq), WrittenAt: capturedAt(seq)}
}

// run ends both streams and runs recordV2 over the connection.
func (c *capture) run(t *testing.T) ([]*models.Mock, error) {
	t.Helper()
	close(c.client)
	close(c.dest)
	return c.record(t)
}

func (c *capture) record(t *testing.T) ([]*models.Mock, error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(c.sess.Ctx, c.sess) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s")
	}
	var mocks []*models.Mock
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return mocks, err
		}
	}
}

// assertPairs fails t unless mocks hold one exchange per id, in order, each
// request with its own response.
func assertPairs(t *testing.T, mocks []*models.Mock, ids ...int) {
	t.Helper()
	var got []string
	for _, m := range mocks {
		got = append(got, m.Spec.HTTPReq.URL+" -> "+m.Spec.HTTPResp.Body)
	}
	var want []string
	for _, id := range ids {
		want = append(want, fmt.Sprintf(`/orders/%d -> {"order":%d,"status":"SHIPPED","items":3}`, id, id))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recorded:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// assertLeftOut fails t unless exactly the spans want were reported left out,
// each counted once.
func (c *capture) assertLeftOut(t *testing.T, want ...[2]time.Time) {
	t.Helper()
	got := c.spans.all()
	if fmt.Sprint(got) != fmt.Sprint(want) || c.mgr.MocksLeftOut() != int64(len(want)) {
		t.Fatalf("left out %v (%d counted), want %v", got, c.mgr.MocksLeftOut(), want)
	}
}

// The release-candidate QA run restarted the node agent while a JDK client had
// a request in flight on a pooled TLS connection. The capture of that
// connection started with the response to the request in flight, whose own
// bytes were sent before the restart:
//
//	R203 Q182 R203 Q182 R203 Q182 R203 Q183 R204
//
// The recorder paired each request with the response before its own: four
// mocks, each with another request's answer, recorded silently, and the last
// response left on the server's stream.
//
// The first response was captured before any request, so it answers none of
// them: it is reported once, as one span, and every request is recorded with
// its own response.
func TestRecordV2_TheReleaseCandidateCaptureRecordsEachRequestWithItsOwnResponse(t *testing.T) {
	t.Parallel()
	if len(jdkRequest(7)) != 182 || len(jdkRequest(10)) != 183 || len(jdkResponse(6)) != 203 || len(jdkResponse(10)) != 204 {
		t.Fatalf("the frames are not the release candidate's: %d %d %d %d", len(jdkRequest(7)), len(jdkRequest(10)), len(jdkResponse(6)), len(jdkResponse(10)))
	}
	c := newJoinedCapture(t)
	c.resp(1, jdkResponse(6))
	seq := uint32(2)
	for id := 7; id <= 10; id++ {
		c.req(seq, jdkRequest(id))
		c.resp(seq+1, jdkResponse(id))
		seq += 2
	}
	mocks, err := c.run(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	assertPairs(t, mocks, 7, 8, 9, 10)
	c.assertLeftOut(t, [2]time.Time{capturedAt(1), capturedAt(1)})
	if mocks[0].Spec.ReqTimestampMock != capturedAt(2) || mocks[0].Spec.ResTimestampMock != capturedAt(3) {
		t.Fatalf("the first mock spans [%v, %v], want its own exchange's [%v, %v]", mocks[0].Spec.ReqTimestampMock,
			mocks[0].Spec.ResTimestampMock, capturedAt(2), capturedAt(3))
	}
}

// A capture that begins in the middle of a response hands recordV2 the rest of
// it first: bytes that do not frame as a response, here a body that happens to
// read as a status line with a Content-Length near 2^63. Captured before every
// request, they answer none: they are dropped unparsed, and reported once.
func TestRecordV2_ACaptureThatBeganMidResponseDropsItsRest(t *testing.T) {
	t.Parallel()
	c := newJoinedCapture(t)
	c.resp(1, `"items":[{"sku":"A1"}]}`)
	c.resp(2, "\r\nHTTP/1.1 200 OK\r\nContent-Length: 9223372036854775800\r\n\r\n")
	c.req(3, jdkRequest(7))
	c.resp(4, jdkResponse(7))
	mocks, err := c.run(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	assertPairs(t, mocks, 7)
	c.assertLeftOut(t, [2]time.Time{capturedAt(1), capturedAt(2)})
}

// The client closes its connection in the middle of a request (it gave up
// sending it), so recordV2 never reads the server's stream: the answer in
// flight when the capture began is reported all the same, once, as recordV2
// ends (Session.EndExchanges), as it is when recordV2 reads past it. Before,
// nothing was reported, and the test case it belonged to was saved without its
// mock. A client that closes without a request reports nothing: with no
// request, nothing orders the server's bytes against one, so they cannot be
// told to be an answer in flight.
func TestRecordV2_AnAnswerInFlightIsReportedWhenTheClientCloses(t *testing.T) {
	t.Parallel()
	t.Run("in the middle of a request", func(t *testing.T) {
		t.Parallel()
		c := newJoinedCapture(t)
		c.resp(1, jdkResponse(6))
		c.req(2, "GET /orders/7 HTTP/1.1\r\nHost: orders.internal\r\n")
		mocks, err := c.run(t)
		if err != nil || len(mocks) != 0 {
			t.Fatalf("recordV2 = %v, %d mock(s); want nil and none", err, len(mocks))
		}
		c.assertLeftOut(t, [2]time.Time{capturedAt(1), capturedAt(1)})
	})
	t.Run("before a request", func(t *testing.T) {
		t.Parallel()
		c := newJoinedCapture(t)
		c.resp(1, jdkResponse(6))
		mocks, err := c.run(t)
		if err != nil || len(mocks) != 0 {
			t.Fatalf("recordV2 = %v, %d mock(s); want nil and none", err, len(mocks))
		}
		c.assertLeftOut(t)
	})
}

// The agent restarted while a request was being sent, and the capture of its
// connection begins with the request's last bytes. An old client sends a CRLF
// after a POST body (RFC 9112 §2.2): that CRLF is all of the request the
// capture has, and the answer to the request follows it, before the next
// request. A chunk of nothing but empty lines is no part of a request, so the
// next request starts at the client's next chunk, and the answer captured
// before it is not its answer: the next request is recorded with its own
// response, and the answer to the cut one is reported once.
func TestRecordV2_ACaptureThatBeganInARequestsLastBytesPairsTheNextRequestWithItsOwnAnswer(t *testing.T) {
	t.Parallel()
	c := newJoinedCapture(t)
	c.req(1, "\r\n")
	c.resp(2, jdkResponse(6))
	c.req(3, jdkRequest(7))
	c.resp(4, jdkResponse(7))
	mocks, err := c.run(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	assertPairs(t, mocks, 7)
	c.assertLeftOut(t, [2]time.Time{capturedAt(2), capturedAt(2)})
}

// The agent restarted in the middle of a request's body: the capture begins
// with the rest of it, which is no request. Re-aligning past it is not in this
// recorder (a hole's re-align point is the next release's); what it must not do
// is pair the next request with the cut one's answer. It reads the rest of the
// body and the next request as one request, which does not parse, leaves that
// exchange out and stops, recording no mock.
func TestRecordV2_ACaptureThatBeganInARequestsBodyRecordsNoWrongMock(t *testing.T) {
	t.Parallel()
	c := newJoinedCapture(t)
	c.req(1, `{"order":6,"qty":2}`)
	c.resp(2, jdkResponse(6))
	c.req(3, jdkRequest(7))
	c.resp(4, jdkResponse(7))
	mocks, err := c.run(t)
	if err == nil {
		t.Fatal("recordV2 returned no error for a request that does not parse")
	}
	if len(mocks) != 0 {
		t.Fatalf("recorded %d mock(s), the first %s -> %q: a request paired with another's answer",
			len(mocks), mocks[0].Spec.HTTPReq.URL, mocks[0].Spec.HTTPResp.Body)
	}
	if n := c.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d left out, want the exchange it stopped on", n)
	}
}

// The relay tees each direction on a goroutine of its own, after it forwards
// the bytes, so a server chunk captured before a request can reach recordV2
// after the request, while it waits for the request's response. Captured
// before the request, it is not its answer, however late it arrives. The
// relay captures a connection from its first byte, so bytes the server sent
// before the first request are its own, and nothing is left out for them.
func TestRecordV2_AResponseTeedAfterItsRequestButCapturedBeforeItIsNotItsAnswer(t *testing.T) {
	t.Parallel()
	c := newCapture(t)
	c.req(2, jdkRequest(7))
	type result struct {
		mocks []*models.Mock
		err   error
	}
	done := make(chan result, 1)
	go func() {
		m, err := c.record(t)
		done <- result{m, err}
	}()
	// recordV2 has the request, and waits for its answer.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if w, _ := c.sess.DestStream.Waiting(); w {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recordV2 never waited for the response")
		}
		time.Sleep(time.Millisecond)
	}
	c.resp(1, jdkResponse(6))
	c.resp(3, jdkResponse(7))
	close(c.client)
	close(c.dest)
	r := <-done
	if r.err != nil {
		t.Fatalf("recordV2 = %v", r.err)
	}
	assertPairs(t, r.mocks, 7)
	c.assertLeftOut(t)
}

// A client may send empty lines after a request, and an old one sends a CRLF
// after a POST body (RFC 9112 §2.2): a chunk of them comes between two
// requests, captured before the first one's answer. It is no part of either
// request: the second starts at the client's next chunk, so server bytes
// captured after the CRLF but before that chunk are not its answer. Here the
// server times the idle keep-alive connection out as the client sends its next
// request, and closes without answering it. Its 408 answers no request: the
// first request is recorded with its own response, none is recorded for the
// second, and nothing is left out. Were the second request started at the
// CRLF, it would be recorded with the 408.
func TestRecordV2_ServerBytesAfterAnEmptyLineChunkAreNotTheNextRequestsAnswer(t *testing.T) {
	t.Parallel()
	c := newCapture(t)
	c.req(1, jdkRequest(7))
	c.req(2, "\r\n")
	c.resp(3, jdkResponse(7))
	c.resp(4, idleTimeout)
	c.req(5, jdkRequest(8))
	mocks, err := c.run(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	assertPairs(t, mocks, 7)
	c.assertLeftOut(t)
}

// Order is the chunks' capture sequence, never their times: a request whose
// first chunk carries no capture time is ordered by its number all the same.
// Before, a zero time read as "before everything", and the request's own
// answer was dropped.
func TestRecordV2_ARequestWithNoCaptureTimeIsOrderedByItsNumber(t *testing.T) {
	t.Parallel()
	c := newJoinedCapture(t)
	c.resp(1, jdkResponse(6))
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: 2, Bytes: []byte(jdkRequest(7))}
	c.resp(3, jdkResponse(7))
	mocks, err := c.run(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	assertPairs(t, mocks, 7)
	c.assertLeftOut(t, [2]time.Time{capturedAt(1), capturedAt(1)})
}

// A producer that does not number its chunks breaks the one contract recordV2
// orders the two directions by: it refuses the connection, loudly, rather than
// pairing requests with whatever answer came first.
func TestRecordV2_RefusesACaptureWithoutCaptureSequence(t *testing.T) {
	t.Parallel()
	c := newCapture(t)
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(jdkRequest(7)), ReadAt: time.Now()}
	c.dest <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte(jdkResponse(7)), ReadAt: time.Now()}
	mocks, err := c.run(t)
	if !errors.Is(err, fakeconn.ErrUnnumbered) {
		t.Fatalf("recordV2 = %v, want fakeconn.ErrUnnumbered", err)
	}
	if len(mocks) != 0 {
		t.Fatalf("recorded %d mock(s) from unnumbered chunks", len(mocks))
	}
}

// A stream ends when its channel closes (io.EOF) or its reader is closed
// (ErrClosed), and only then. No producer sends a chunk with no bytes (the
// relay tees only what a Read returned), so recordV2 gives an empty chunk no
// meaning of its own: it adds no bytes to what is being read, and the exchange
// around it is recorded as it is without it. Before, an empty chunk where a
// response began ended the connection's recording, and one inside a request
// or a response ended it there, as if the stream had.
func TestRecordV2_AnEmptyChunkEndsNoStream(t *testing.T) {
	t.Parallel()
	post := "POST /orders HTTP/1.1\r\nHost: orders.internal:8443\r\nContent-Length: 11\r\n\r\n{\"items\":3}"
	chunkedPost := "POST /orders HTTP/1.1\r\nHost: orders.internal:8443\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"b\r\n{\"items\":3}\r\n0\r\n\r\n"
	chunkedResp := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nb\r\n{\"order\":7}\r\n0\r\n\r\n"
	toEnd := "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n{\"order\":7}"
	get, ok := jdkRequest(7), jdkResponse(7)
	for _, tc := range []struct {
		name      string
		req, resp string
		// The empty chunk goes in front of this byte of the request or the
		// response; -1 for neither.
		inReq, inResp int
	}{
		{"where the request begins", get, ok, 0, -1},
		{"where the response begins", get, ok, -1, 0},
		{"in the request's head", get, ok, 20, -1},
		{"in the request's body", post, ok, len(post) - 4, -1},
		{"in the request's chunked body", chunkedPost, ok, len(chunkedPost) - 8, -1},
		{"in the response's head", get, ok, -1, 20},
		{"in the response's body", get, ok, -1, len(ok) - 4},
		{"in the response's chunked body", get, chunkedResp, -1, len(chunkedResp) - 8},
		{"in a response read to the stream's end", get, toEnd, -1, len(toEnd) - 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := recordWithAnEmptyChunk(t, tc.req, tc.resp, -1, -1)
			if len(want) != 1 {
				t.Fatalf("the exchange alone records %d mocks, want 1", len(want))
			}
			got := recordWithAnEmptyChunk(t, tc.req, tc.resp, tc.inReq, tc.inResp)
			if len(got) != 1 {
				t.Fatalf("with an empty chunk %d mocks are recorded, want the exchange's", len(got))
			}
			w, g := want[0].Spec, got[0].Spec
			if g.HTTPReq.URL != w.HTTPReq.URL || g.HTTPReq.Body != w.HTTPReq.Body ||
				g.HTTPResp.StatusCode != w.HTTPResp.StatusCode || g.HTTPResp.Body != w.HTTPResp.Body {
				t.Fatalf("recorded %s %q -> %d %q, want %s %q -> %d %q",
					g.HTTPReq.URL, g.HTTPReq.Body, g.HTTPResp.StatusCode, g.HTTPResp.Body,
					w.HTTPReq.URL, w.HTTPReq.Body, w.HTTPResp.StatusCode, w.HTTPResp.Body)
			}
		})
	}
}

// recordWithAnEmptyChunk records the exchange req then resp, numbered in that
// order, with an empty chunk in front of byte inReq of the request and byte
// inResp of the response (-1: none).
func recordWithAnEmptyChunk(t *testing.T, req, resp string, inReq, inResp int) []*models.Mock {
	t.Helper()
	c := newCapture(t)
	var seq uint32
	send := func(ch chan fakeconn.Chunk, dir fakeconn.Direction, b string, at int) {
		var pieces []string
		if at > 0 {
			pieces = append(pieces, b[:at])
		}
		if at >= 0 {
			pieces, b = append(pieces, ""), b[at:]
		}
		for _, p := range append(pieces, b) {
			seq++
			ch <- fakeconn.Chunk{Dir: dir, ConnSeq: seq, Bytes: []byte(p), ReadAt: capturedAt(seq), WrittenAt: capturedAt(seq)}
		}
	}
	send(c.client, fakeconn.FromClient, req, inReq)
	send(c.dest, fakeconn.FromDest, resp, inResp)
	mocks, err := c.run(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	return mocks
}
