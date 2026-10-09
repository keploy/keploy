package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// The keep-alive idle-close race: the upstream closes a pooled connection as
// idle just as the app sends its next request on it. After the upstream's FIN
// the relay keeps feeding the client direction to the parser (HalfCloseGrace),
// so the HTTP recorder reads that request and then finds the server's stream
// ended with no response. Nothing was lost: there is no response, so there is
// no mock to record, and an app's HTTP client (Go's Transport, Apache
// HttpClient, OkHttp) retries the request on a new connection, where it is
// recorded. The recorder must not report a mock left out for it: its span
// would leave out every test case in flight at that moment, on any route.
//
// Real sockets through recordViaSupervisor and the HTTP parser, as a proxied
// connection runs. The app here reads the upstream's FIN before it writes, so
// the relay has seen the FIN first whatever the scheduling; the bytes the
// parser is fed are those of the race. The app keeps its end open, and the
// relay ends the connection once the client direction has been idle for its
// HalfCloseGrace: the parser has long been waiting for the response by then,
// and finds the server's stream run out (io.EOF).
func TestRecordViaSupervisor_AnUpstreamIdleCloseLeavesNothingOut(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	mgr := syncMock.New(nil)
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 30*time.Second)
	defer cancel()

	clientApp, rawSrc, cleanupSrc := tcpConnPair(t)
	defer cleanupSrc()
	dstConn, destSvc, cleanupDst := tcpConnPair(t)
	defer cleanupDst()
	// Exactly what handleConnection builds before dispatch.
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}

	p := &Proxy{
		logger:                     logger,
		recordBufferCap:            1 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     2 * time.Second,
		recordBufferHalfCloseGrace: 200 * time.Millisecond,
	}

	// The upstream answers the first request, then closes the connection.
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(destSvc))
		if err != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = destSvc.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		_ = destSvc.Close()
	}()

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), httpint.New(logger), integrations.HTTP,
			make(chan *models.Mock, 8), &errgroup.Group{}, logger, 1, 2, models.OutgoingOptions{})
	}()

	if _, err := clientApp.Write([]byte("GET /one HTTP/1.1\r\nHost: upstream\r\n\r\n")); err != nil {
		t.Fatalf("write the first request: %v", err)
	}
	_ = clientApp.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(clientApp)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the first response: %v", err)
	}
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ok" {
		t.Fatalf("the first response's body is (%q, %v), want ok", body, err)
	}
	// The upstream's FIN, relayed: the connection is closed as idle.
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("the app read %v after the response, want the upstream's FIN (io.EOF)", err)
	}

	// The app's next request on the pooled connection, which the upstream
	// will never answer. (The app finds the connection closed and retries on
	// a connection of its own.)
	if _, err := clientApp.Write([]byte("GET /two HTTP/1.1\r\nHost: upstream\r\n\r\n")); err != nil {
		t.Fatalf("write the second request: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recordViaSupervisor returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after the upstream closed")
	}

	// The parser did read the second request and find the upstream's stream
	// ended, or this test checks nothing. The relay's teardown closes the
	// stream after it ends it, so a parser that got there late finds it
	// closed (ErrClosed); either way it ended with no response.
	ended := logs.FilterMessage("V2 HTTP record: dest stream ended before response").All()
	if len(ended) != 1 {
		t.Fatalf("the parser did not stop on the second request at the end of the upstream's stream: %d such lines", len(ended))
	}
	if e := ended[0].ContextMap()["error"]; e != io.EOF.Error() && e != fakeconn.ErrClosed.Error() {
		t.Fatalf("the parser stopped on the second request with %v, want the end of the upstream's stream", e)
	}
	if _, _, added, _ := mgr.GetDropStats(); added != 1 {
		t.Fatalf("%d mocks recorded, want the first exchange's", added)
	}
	if n := mgr.MocksLeftOut(); n != 0 {
		t.Errorf("the manager counts %d mocks left out, want none: the request the upstream never answered has no response to record", n)
	}
	if over, n := mgr.WasMockOrphanedInWindow(started, time.Now()); over {
		t.Errorf("%d orphan spans over the connection, want none: every test case in flight there would be left out", n)
	}
	if w := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(supervisor.LeftOutWarnMsg).All(); len(w) != 0 {
		t.Errorf("%d left-out WARNs, want none: %v", len(w), w[0].ContextMap())
	}
}

// A parser that stops on an exchange it cannot record reports it up to the
// stop (Session.ReportStoppedOn), and the dispatcher then leaves out what the
// connection carries from the parser's retirement on. The two spans must meet:
// traffic captured between them is in neither, and a test case recorded
// there is saved without its mocks. Each end read the stop from the clock, the
// dispatcher after the parser's return and its retirement WARN, so the two
// spans were apart, and the connection's spans stayed two once it ended. Both
// now take the session's stop instant (Session.StoppedAt), and they join into
// one.
//
// Real sockets through recordViaSupervisor and the HTTP parser: the upstream
// answers with a Content-Length that does not parse, so the parser stops on
// the exchange and returns with the error.
func TestRecordViaSupervisor_TheExchangeAParserStopsOnMeetsTheSpanAfterIt(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	logger := zap.NewNop()
	mgr := syncMock.New(nil)
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 30*time.Second)
	defer cancel()

	clientApp, rawSrc, cleanupSrc := tcpConnPair(t)
	defer cleanupSrc()
	dstConn, destSvc, cleanupDst := tcpConnPair(t)
	defer cleanupDst()
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}

	p := &Proxy{
		logger:                     logger,
		recordBufferCap:            1 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     2 * time.Second,
		recordBufferHalfCloseGrace: 200 * time.Millisecond,
	}

	go func() {
		req, err := http.ReadRequest(bufio.NewReader(destSvc))
		if err != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = destSvc.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: zz\r\n\r\nok"))
	}()

	done := make(chan error, 1)
	go func() {
		done <- p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), httpint.New(logger), integrations.HTTP,
			make(chan *models.Mock, 8), &errgroup.Group{}, logger, 1, 2, models.OutgoingOptions{})
	}()

	if _, err := clientApp.Write([]byte("GET /one HTTP/1.1\r\nHost: upstream\r\n\r\n")); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	// The exchange's span, then the retirement's.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if recorded, _, _ := mgr.OrphanRangeCount(); recorded == 2 {
			break
		}
		if time.Now().After(deadline) {
			recorded, closed, open := mgr.OrphanRangeCount()
			t.Fatalf("spans recorded=%d (closed=%d, open=%d), want the exchange the parser stopped on and what the connection carries after its retirement", recorded, closed, open)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The connection ends: the span after the retirement closes with it.
	_ = clientApp.Close()
	_ = destSvc.Close()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after both peers closed")
	}
	for {
		_, closed, open := mgr.OrphanRangeCount()
		if open == 0 {
			if closed+open != 1 {
				t.Fatalf("%d spans once the connection ended, want one: the exchange the parser stopped on and what the connection carried after its retirement are apart, and a test case recorded between them is saved without its mocks", closed+open)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the span after the retirement is still open (closed=%d, open=%d) after the connection ended", closed, open)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the exchange the parser stopped on", n)
	}
}
