package proxy

import (
	"context"
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
