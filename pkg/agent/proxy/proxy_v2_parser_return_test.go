package proxy

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// returningParser is a V2 parser that reads the client's stream until it has
// read upTo chunks or the stream ends, and returns with no error, as a parser
// that stops recording a connection on purpose does (the MySQL recorder skips
// a connection it cannot decode that way).
type returningParser struct {
	stubParser
	upTo     int
	returned chan struct{}
}

func (returningParser) IsV2() bool { return true }

func (p returningParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	defer close(p.returned)
	for i := 0; i < p.upTo; i++ {
		if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
			return nil
		}
	}
	return nil
}

// A parser that returns while its connection goes on leaves the relay
// forwarding the application's bytes, and records none of them from then on:
// the test cases recorded while the connection carries them are left out
// rather than saved without their mocks. Nothing it carried before the return
// was lost, so nothing is left out until it carries more.
func TestRecordViaSupervisorLeavesOutWhatAConnectionCarriesAfterItsParserReturns(t *testing.T) {
	t.Parallel()
	mgr := syncMock.New(nil)
	parser := returningParser{upTo: 1, returned: make(chan struct{})}
	h := newDesyncFeedHarnessCtx(t, syncMock.NewContext(context.Background(), mgr), parser, newFeedProbe(), 1<<20)

	h.writeApp(t, []byte("first"))
	select {
	case <-parser.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the parser never returned")
	}
	time.Sleep(50 * time.Millisecond)
	if _, closed, open := mgr.OrphanRangeCount(); closed != 0 || open != 0 {
		t.Fatalf("spans (closed=%d, open=%d) before the connection carried anything after the parser returned", closed, open)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.writeApp(t, []byte("after"))
		now := time.Now()
		if ok, _ := mgr.WasMockOrphanedInWindow(now.Add(-time.Millisecond), now); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no span covers the traffic after the parser returned: its test cases are saved without their mocks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitForDest(t, h, "first")
}

// A parser that returns at its connection's end leaves nothing out: the
// connection carries nothing after it. The recording runs on, as it does past
// every connection that ends while it runs.
func TestRecordViaSupervisorLeavesNothingOutWhenItsParserReturnsAtTheConnectionsEnd(t *testing.T) {
	parser := returningParser{upTo: 1 << 30, returned: make(chan struct{})}
	h := newStopHarness(t, parser, zap.NewNop())
	h.send(t, "GET /only HTTP/1.1\r\n\r\n")
	h.end(t)
	waitFor(t, parser.returned, "the parser never returned")
	if recorded, closed, open := h.mgr.OrphanRangeCount(); recorded != 0 || closed != 0 || open != 0 {
		t.Fatalf("spans (recorded=%d, closed=%d, open=%d) for a connection that carried nothing after its parser returned at its end", recorded, closed, open)
	}
}

// drainingParser reads the client's first request, drains the destination's
// answer as the relay hands it on, and returns with no error once it is
// released (a nil release: never) or the recording stops: it has recorded
// everything it read.
type drainingParser struct {
	stubParser
	read    chan struct{}
	dest    chan *fakeconn.FakeConn
	release chan struct{}
}

func (drainingParser) IsV2() bool { return true }

func (p drainingParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	p.dest <- s.V2.DestStream
	close(p.read)
	go func() {
		for {
			if _, err := s.V2.DestStream.ReadChunk(); err != nil {
				return
			}
		}
	}()
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return nil
}

// A parser that returns as the recording stops leaves nothing out, as a stop
// stamped then does (TestRecordViaSupervisor_ARecordingsStopOpensNoSpanAndLogsNoRetirement):
// the connection is torn down with the recording. It can still carry bytes
// after the parser's return: the relay finishes a write of the destination's
// answer to an app that was slow to read it when the recording stopped. Left
// out from the next bytes it carried, the connection opened a span for them as
// the recording stopped, over the test cases in flight then.
func TestRecordViaSupervisorLeavesNothingOutWhenItsParserReturnsAsTheRecordingStops(t *testing.T) {
	parser := drainingParser{read: make(chan struct{}), dest: make(chan *fakeconn.FakeConn, 1)}
	h := newStopHarness(t, parser, zap.NewNop())
	h.slowApp(t)

	h.send(t, "GET /large HTTP/1.1\r\n\r\n")
	waitFor(t, parser.read, "the parser never read the request")
	dest := <-parser.dest
	h.answerUntilTheRelayIsBlocked(t)

	h.cancel()
	// The parser returns, and the dispatcher closes its streams once it has
	// decided what the connection carries from here leaves out.
	waitFor(t, dest.Done(), "the dispatcher never closed the parser's streams")
	// The app reads: the relay's write completes, and the connection carries
	// those bytes.
	h.appReads()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after the recording stopped")
	}
	if recorded, _, _ := h.mgr.OrphanRangeCount(); recorded != 0 {
		t.Errorf("%d spans after a parser returned as the recording stopped, want none: it leaves out the test cases in flight as the recording stops", recorded)
	}
}

// A parser that returned while the recording ran leaves its connection out
// from the next bytes it carries (StopFromNextBytes). Here they come only once
// the recording has stopped: the relay was blocked writing an answer to an app
// slow to read it, and finishes the write when the app reads. The connection
// is torn down with the recording then, and they open no span, as the rule is
// read as the span opens (openSpan). Read as the stop was armed, while the
// recording ran, it let them open a span after the recording had stopped, over
// the test cases in flight then.
func TestRecordViaSupervisorLeavesNothingOutWhenTheBytesAfterItsParsersReturnComeAsTheRecordingStops(t *testing.T) {
	parser := drainingParser{read: make(chan struct{}), dest: make(chan *fakeconn.FakeConn, 1), release: make(chan struct{})}
	h := newStopHarness(t, parser, zap.NewNop())
	h.slowApp(t)

	h.send(t, "GET /large HTTP/1.1\r\n\r\n")
	waitFor(t, parser.read, "the parser never read the request")
	dest := <-parser.dest
	h.answerUntilTheRelayIsBlocked(t)
	// The parser returns while the recording runs, and the dispatcher closes
	// its streams once it has left the connection out from its next bytes.
	close(parser.release)
	waitFor(t, dest.Done(), "the dispatcher never closed the parser's streams")
	if recorded, _, _ := h.mgr.OrphanRangeCount(); recorded != 0 {
		t.Fatalf("%d spans before the connection carried anything after its parser returned: the test does not drive the path it is for", recorded)
	}

	stopped := time.Now()
	h.cancel()
	h.appReads()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after the recording stopped")
	}
	if recorded, _, _ := h.mgr.OrphanRangeCount(); recorded != 0 {
		after, _ := h.mgr.WasMockOrphanedInWindow(stopped, time.Now())
		t.Errorf("%d spans (one after the recording stopped: %v), want none: the connection carried bytes after its parser returned only once the recording had stopped, and a span leaves out the test cases in flight then", recorded, after)
	}
}

// peekingParser reads the client's first chunk, waits until the next one is in
// its stream, and returns without reading it: the connection carried a
// request, teed to the parser, that the parser never recorded.
type peekingParser struct {
	stubParser
	stream   chan *fakeconn.FakeConn
	returned chan struct{}
}

func (peekingParser) IsV2() bool { return true }

func (p peekingParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	defer close(p.returned)
	p.stream <- s.V2.ClientStream
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return nil
	}
	_, _ = s.V2.ClientStream.Peek()
	return nil
}

// A parser that returns with a request in its stream it never read recorded
// only what it read: the request is left out, though the relay told the
// connection of it as it forwarded it, before the parser returned. A rule that
// went by the order of the two left it out of every span: a test case recorded
// over it was saved without its mocks. And it is that request that is left
// out, not the one the parser read: a test case over that one has its mocks.
func TestRecordViaSupervisorLeavesOutTheRequestsItsParserReturnedWithoutReading(t *testing.T) {
	parser := peekingParser{stream: make(chan *fakeconn.FakeConn, 1), returned: make(chan struct{})}
	h := newStopHarness(t, parser, zap.NewNop())

	one := h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	stream := <-parser.stream
	two := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	waitFor(t, parser.returned, "the parser never returned")
	// The dispatcher closes the parser's streams once it has decided what the
	// connection carries from here leaves out.
	waitFor(t, stream.Done(), "the dispatcher never closed the parser's streams")

	// The span opens as the parser's return is seen, or, when the relay tells
	// of the request only after that, as it does: from when it was read
	// either way.
	waitUntilT(t, 10*time.Second, "a span covers the request the parser returned without reading: a test case recorded over it is saved without its mocks", func() bool {
		over, _ := h.mgr.WasMockOrphanedInWindow(two.sent, two.arrived)
		return over
	})
	if over, _ := h.mgr.WasMockOrphanedInWindow(one.sent, one.arrived); over {
		t.Errorf("the request the parser read, between %v and %v, is in a span: a test case over it, whose mocks were recorded, is left out", one.sent, one.arrived)
	}
}

// A parser that returns at its connection's end with server bytes it never
// read leaves nothing out: they answer no request it read and did not answer
// (a server's goodbye before it closes, MySQL's ERR at wait_timeout), the
// server's own by the rule the floor applies to the server bytes it drops
// between requests (Session.dropped).
func TestRecordViaSupervisorLeavesNothingOutForServerBytesItsParserReturnedWithoutReading(t *testing.T) {
	parser := returningParser{upTo: 1 << 30, returned: make(chan struct{})}
	h := newStopHarness(t, parser, zap.NewNop())
	h.send(t, "GET /only HTTP/1.1\r\n\r\n")
	if _, err := h.destSvc.Write([]byte("goodbye")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	buf := make([]byte, len("goodbye"))
	if _, err := io.ReadFull(h.clientApp, buf); err != nil {
		t.Fatalf("app read: %v", err)
	}
	h.end(t)
	waitFor(t, parser.returned, "the parser never returned")
	if recorded, closed, open := h.mgr.OrphanRangeCount(); recorded != 0 {
		t.Errorf("spans (recorded=%d, closed=%d, open=%d) for server bytes the parser returned without reading at its connection's end, want none", recorded, closed, open)
	}
}

// The ledger sees every stop and every chunk of the connection, and the
// parser's return before the tees are paused. Its rules are tested on their
// own (parser_reads_test.go); the links to the dispatcher are pinned on the
// source here, as relay_config_test.go pins the relay's:
//   - OnCarried is reads.carried, or no chunk reaches it;
//   - the session's OnStop is reads.stop, or a stop before the return is
//     unknown to it, and the requests captured before that stop are in no span;
//   - the recording's stop is stamped into it (context.AfterFunc on the
//     recording's context), or what it leaves out as the recording stops has
//     no bound;
//   - reads.follow comes before the relay runs, or no chunk is kept;
//   - reads.parserReturned() comes after sv.Run and before the teardown's
//     r.PauseTees(), or the chunks the paused tees refuse are told of before
//     the return.
func TestRecordViaSupervisorWiresParserReads(t *testing.T) {
	t.Parallel()
	fields, _ := relayConfigLiteral(t)
	if got := fields["OnCarried"]; got != "reads.carried" {
		t.Errorf("relay.Config's OnCarried is %q, want reads.carried", got)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "proxy_v2.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy_v2.go: %v", err)
	}
	render := func(n ast.Node) string {
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, fset, n)
		return buf.String()
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "recordViaSupervisor" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("recordViaSupervisor not found in proxy_v2.go")
	}
	onStop, stamped := false, false
	returnedAt, pausedAt, followedAt, relayAt, runAt := -1, -1, -1, -1, -1
	for i, st := range fn.Body.List {
		switch src := render(st); {
		case src == "svSess.OnStop = reads.stop":
			onStop = true
		case strings.Contains(src, "context.AfterFunc(ctx,") && strings.Contains(src, "reads.recordingStopped("):
			stamped = true
		case src == "reads.follow(svSess.ClientStream, r.Teed)":
			followedAt = i
		case strings.HasPrefix(src, "go func() { relayDone <- r.Run("):
			relayAt = i
		case strings.HasPrefix(src, "result := sv.Run("):
			runAt = i
		case src == "reads.parserReturned()":
			returnedAt = i
		case src == "r.PauseTees()":
			pausedAt = i
		}
	}
	if followedAt < 0 || relayAt < 0 || followedAt > relayAt {
		t.Errorf("reads.follow(svSess.ClientStream, r.Teed) at statement %d, the relay started at %d: the ledger must follow the parser's streams before the relay runs, or it goes back to deciding by order", followedAt, relayAt)
	}
	if runAt < 0 || returnedAt < runAt {
		t.Errorf("sv.Run at statement %d, reads.parserReturned() at %d: the return is seen once Run has returned", runAt, returnedAt)
	}
	if !onStop {
		t.Error("the session's OnStop is not reads.stop: a stop before the parser's return is unknown to the ledger")
	}
	if !stamped {
		t.Error("the recording's stop is not stamped into the ledger with context.AfterFunc on the recording's context")
	}
	if returnedAt < 0 || pausedAt < 0 || returnedAt > pausedAt {
		t.Errorf("reads.parserReturned() at statement %d, the teardown's r.PauseTees() at %d: the return must be seen before the tees are paused", returnedAt, pausedAt)
	}
}
