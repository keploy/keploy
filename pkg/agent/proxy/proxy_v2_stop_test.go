package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// stopThenHoldParser reads the client's first chunk, reports the exchange it
// stops on (Session.ReportStoppedOn), and returns with the error only once it
// is released: a parser that has stopped reading, and is slow to return, as on
// a starved agent.
type stopThenHoldParser struct {
	stubParser
	reported chan struct{}
	release  chan struct{}
}

func (stopThenHoldParser) IsV2() bool { return true }

func (p stopThenHoldParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	c, err := s.V2.ClientStream.ReadChunk()
	if err != nil {
		return err
	}
	s.V2.ReportStoppedOn(models.HTTP, c.ReadAt, "http decode error: x")
	close(p.reported)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return errors.New("http decode error: x")
}

// A capture hole can come after the parser stopped on an exchange and before
// it returns: the parser no longer reads, so its capture queue fills behind
// it. The span the hole opens must start where the exchange's span ends, at
// the session's stop instant (Session.StoppedAt): the bytes captured between
// the two are lost with the parser, and a test case recorded over them was
// saved without its mocks. The hole read the clock itself, so the two spans
// stayed apart once the connection ended.
func TestRecordViaSupervisor_AHoleAfterTheParsersStopOpensItsSpanAtTheStop(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	mgr := syncMock.New(nil)
	parser := stopThenHoldParser{reported: make(chan struct{}), release: make(chan struct{})}
	// A 4-byte cap: the first write fits, a longer one is a hole.
	h := newDesyncFeedHarnessCtx(t, syncMock.NewContext(context.Background(), mgr), parser, nil, 4)

	h.writeApp(t, []byte("a"))
	select {
	case <-parser.reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the parser never reported the exchange it stopped on")
	}
	// So the clock read at the hole cannot be the stop's.
	time.Sleep(time.Millisecond)
	h.writeApp(t, []byte("0123456789"))
	// The exchange's span, and the hole's: the parser is still held, so the
	// dispatcher has opened none.
	waitUntilT(t, 5*time.Second, "the hole opens its span while the parser is still held", func() bool {
		recorded, _, _ := mgr.OrphanRangeCount()
		return recorded == 2
	})
	close(parser.release)
	h.stop()
	waitUntilT(t, 5*time.Second, "the hole's span closes once the connection ends", func() bool {
		_, _, open := mgr.OrphanRangeCount()
		return open == 0
	})
	if _, closed, _ := mgr.OrphanRangeCount(); closed != 1 {
		t.Fatalf("%d spans once the connection ended, want one: the exchange the parser stopped on and the hole after it are apart, and a test case recorded between them is saved without its mocks", closed)
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the exchange the parser stopped on", n)
	}
}

// stopsReadingParser reads the client's first chunk and reads nothing more
// until the recording stops: its capture queue fills behind it, up to a hole.
type stopsReadingParser struct {
	stubParser
	read chan struct{}
}

func (stopsReadingParser) IsV2() bool { return true }

func (p stopsReadingParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	close(p.read)
	<-ctx.Done()
	return nil
}

// A capture hole while the recording runs costs it what the connection carries
// from then on: the dispatcher opens its span, and says so, as the tee warns of
// the hole once its owner says it costs something (OnCaptureDesync). As the
// recording stops, a hole costs nothing, and neither the span nor the WARN goes
// out (Session.RecordingStopping); a hole then cannot be made to come through
// recordViaSupervisor, as the relay stops reading the sockets as the recording
// stops (relay.TestTee_WarnsOfAHoleOnlyWhenItCostsTheRecordingSomething).
func TestRecordViaSupervisor_AHoleWhileTheRecordingRunsIsWarnedOf(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	parser := stopsReadingParser{read: make(chan struct{})}
	h := newStopHarness(t, parser, zap.New(core))

	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitFor(t, parser.read, "the parser never read the request")
	// Far more than the parser's capture holds (the harness's 1 MiB cap on
	// its queue, and the chunks handed on ahead of it): a hole.
	go func() { _, _ = h.clientApp.Write(bytes.Repeat([]byte("x"), 16<<20)) }()
	waitUntilT(t, 20*time.Second, "the tee warns of the hole", func() bool {
		return logs.FilterMessageSnippet("relay: capture dropped a chunk").Len() > 0
	})
	// The span is open from the hole, before the tee warns of it. (More than
	// one, if the connection idled past the span's grace on a starved agent
	// and carried traffic again.)
	if recorded, _, _ := h.mgr.OrphanRangeCount(); recorded == 0 {
		t.Error("no span after a hole while the recording runs: the test cases recorded over what the connection carries from then on are saved without their mocks")
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after the recording stopped")
	}
	if n := logs.FilterMessageSnippet("relay: capture dropped a chunk").Len(); n != 1 {
		t.Errorf("%d WARNs for the hole, want one", n)
	}
}

// failingParser reads the client's first chunk and returns with an error,
// reporting nothing: a parser the dispatcher retires without knowing which
// exchange it was in.
type failingParser struct{ stubParser }

func (failingParser) IsV2() bool { return true }

func (failingParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	return errors.New("a parser error")
}

// panickingParser reads the client's first chunk and panics.
type panickingParser struct{ stubParser }

func (panickingParser) IsV2() bool { return true }

func (panickingParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	panic("boom")
}

// heldLog holds the first log entry at level whose message starts with
// prefix until it is released, as a log write can take a while on a starved
// agent: the dispatcher's "parser retired" WARN, say, or the supervisor's
// "parser panicked" ERROR, whose stack makes it the longest line it writes.
type heldLog struct {
	zapcore.Core
	level    zapcore.Level
	prefix   string
	entered  chan struct{}
	release  chan struct{}
	once     *sync.Once
	released *sync.Once
}

func newHeldLog(level zapcore.Level, prefix string) heldLog {
	return heldLog{Core: zapcore.NewNopCore(), level: level, prefix: prefix,
		entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}, released: &sync.Once{}}
}

// Release lets the held entry go. Idempotent.
func (c heldLog) Release() { c.released.Do(func() { close(c.release) }) }

func (c heldLog) Enabled(zapcore.Level) bool        { return true }
func (c heldLog) With([]zapcore.Field) zapcore.Core { return c }
func (c heldLog) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Level == c.level && strings.HasPrefix(e.Message, c.prefix) {
		held := false
		c.once.Do(func() { held = true; close(c.entered) })
		if held {
			select {
			case <-c.release:
			case <-time.After(10 * time.Second):
			}
		}
	}
	return ce
}

// stopHarness runs one connection through recordViaSupervisor over real
// sockets, with the manager the test checks the spans on.
type stopHarness struct {
	mgr       *syncMock.SyncMockManager
	clientApp net.Conn
	destSvc   net.Conn
	done      chan error
	// cancel stops the recording.
	cancel context.CancelFunc
	// appSide is the proxy's end of the app's connection, which the relay
	// writes the destination's answers to.
	appSide net.Conn

	mu       sync.Mutex
	upstream []byte
}

func newStopHarness(t *testing.T, parser integrations.Integrations, logger *zap.Logger) *stopHarness {
	t.Helper()
	return newStopHarnessWith(t, parser, logger, nil)
}

// newStopHarnessWith is newStopHarness with the connection's parser
// supervised by what newSupervisor builds (Proxy.recordSupervisor; nil is
// supervisor.New).
func newStopHarnessWith(t *testing.T, parser integrations.Integrations, logger *zap.Logger, newSupervisor func(supervisor.Config) *supervisor.Supervisor) *stopHarness {
	t.Helper()
	supervisor.ResetLeftOutWarningsForTest()
	h := &stopHarness{mgr: syncMock.New(nil), done: make(chan error, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), h.mgr), 30*time.Second)
	t.Cleanup(cancel)
	h.cancel = cancel

	clientApp, rawSrc, cleanupSrc := tcpConnPair(t)
	t.Cleanup(cleanupSrc)
	dstConn, destSvc, cleanupDst := tcpConnPair(t)
	t.Cleanup(cleanupDst)
	h.clientApp, h.destSvc, h.appSide = clientApp, destSvc, rawSrc
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}

	// The upstream keeps what it receives, so a test can tell when a request
	// has reached it: the relay forwarded it.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := destSvc.Read(buf)
			h.mu.Lock()
			h.upstream = append(h.upstream, buf[:n]...)
			h.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	p := &Proxy{
		logger:                     logger,
		recordBufferCap:            1 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     2 * time.Second,
		recordBufferHalfCloseGrace: 200 * time.Millisecond,
		recordSupervisor:           newSupervisor,
	}
	go func() {
		h.done <- p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser, integrations.HTTP,
			make(chan *models.Mock, 8), &errgroup.Group{}, logger, 1, 2, models.OutgoingOptions{})
	}()
	return h
}

// sentRequest is when the app wrote a request and when the upstream had it:
// the connection carried it in between.
type sentRequest struct{ sent, arrived time.Time }

// send writes req from the app and returns once the upstream has it.
func (h *stopHarness) send(t *testing.T, req string) sentRequest {
	t.Helper()
	sent := time.Now()
	if _, err := h.clientApp.Write([]byte(req)); err != nil {
		t.Fatalf("write %q: %v", req, err)
	}
	waitUntilT(t, 10*time.Second, "the relay forwards "+req, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return bytes.Contains(h.upstream, []byte(req))
	})
	return sentRequest{sent: sent, arrived: time.Now()}
}

// slowApp shrinks the socket buffers between the relay and the app, which
// reads nothing until appReads: the relay is soon blocked writing the
// destination's answer to it. Call it before the connection carries anything.
func (h *stopHarness) slowApp(t *testing.T) {
	t.Helper()
	if err := h.clientApp.(*net.TCPConn).SetReadBuffer(4096); err != nil {
		t.Fatalf("app's receive buffer: %v", err)
	}
	if err := h.appSide.(*net.TCPConn).SetWriteBuffer(4096); err != nil {
		t.Fatalf("relay's send buffer to the app: %v", err)
	}
}

// answerUntilTheRelayIsBlocked has the destination write an answer with no
// end, and returns once it is blocked writing it: nothing reads it on the way,
// as the relay is blocked writing it to an app that does not read (slowApp).
func (h *stopHarness) answerUntilTheRelayIsBlocked(t *testing.T) {
	t.Helper()
	var answered atomic.Int64
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			n, err := h.destSvc.Write(chunk)
			answered.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for stable, last := 0, int64(-1); stable < 10; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the destination never blocked writing its answer")
		}
		if n := answered.Load(); n > 0 && n == last {
			stable++
		} else {
			stable, last = 0, n
		}
	}
}

// appReads has the app read everything from now on: a write the relay is
// blocked in completes, and the connection carries those bytes.
func (h *stopHarness) appReads() {
	go func() { _, _ = io.Copy(io.Discard, h.clientApp) }()
}

// requireInASpanNow fails unless a test case over r, checked now, as the
// record route checks one in proxy mode (as it streams it), overlaps a span:
// it is left out, not saved without its mocks.
func (h *stopHarness) requireInASpanNow(t *testing.T, r sentRequest, while string) {
	t.Helper()
	if over, _ := h.mgr.WasMockOrphanedInWindow(r.sent, time.Now()); !over {
		t.Errorf("checked %s, a test case over the request sent at %v overlaps no span: proxy mode streams it without its mocks", while, r.sent)
	}
}

// requireInASpan fails unless the time the connection carried r overlaps a
// span: a test case recorded over r, checked from now on, is left out.
func (h *stopHarness) requireInASpan(t *testing.T, r sentRequest, after string) {
	t.Helper()
	if over, _ := h.mgr.WasMockOrphanedInWindow(r.sent, r.arrived); !over {
		t.Errorf("the request the connection carried %s, between %v and %v, is in no span: a test case recorded over it is saved without its mocks", after, r.sent, r.arrived)
	}
}

// end closes both peers and waits for recordViaSupervisor to return.
func (h *stopHarness) end(t *testing.T) {
	t.Helper()
	_ = h.clientApp.Close()
	_ = h.destSvc.Close()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after both peers closed")
	}
}

func waitFor(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatal(what)
	}
}

// A retired parser's capture ends where its stop is stamped (Session.StoppedAt),
// and the span of what the connection carries from then on must be open from
// that moment. The dispatcher opened it only once Run had returned, after its
// retirement WARN, from the stamped instant: a test case checked before then,
// as proxy mode checks each one as it streams it, overlapped no span and was
// saved without its mocks. The span now opens as the stop is stamped
// (Session.OnStop), before the WARN. Before that, it started at a clock read
// after the WARN, and the request sent during it was in no span at all.
func TestRecordViaSupervisor_TheSpanAfterARetirementStartsWhereTheCaptureEnds(t *testing.T) {
	warn := newHeldLog(zapcore.WarnLevel, "parser retired")
	defer warn.Release()
	h := newStopHarness(t, failingParser{}, zap.New(warn))

	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitFor(t, warn.entered, "the parser was never retired")
	second := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	h.requireInASpanNow(t, second, "while the retirement WARN is written")

	warn.Release()
	h.requireInASpan(t, second, "after its parser's retirement")
	h.end(t)
	if n := h.mgr.MocksLeftOut(); n != 0 {
		t.Errorf("the manager counts %d mocks left out, want none: the parser reported no exchange", n)
	}
}

// A parser that reports the exchange it stops on (ReportStoppedOn) stamps the
// stop then, and the span of what the connection carries after it must be open
// from then on: while the parser logs its error and returns, and while the
// dispatcher logs its retirement. The dispatcher opened it only after both.
func TestRecordViaSupervisor_TheSpanAfterAReportedStopOpensAsItIsReported(t *testing.T) {
	warn := newHeldLog(zapcore.WarnLevel, "parser retired")
	defer warn.Release()
	parser := stopThenHoldParser{reported: make(chan struct{}), release: make(chan struct{})}
	h := newStopHarness(t, parser, zap.New(warn))

	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitFor(t, parser.reported, "the parser never reported the exchange it stopped on")
	second := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	h.requireInASpanNow(t, second, "after the parser reported its stop, before it returned")

	close(parser.release)
	waitFor(t, warn.entered, "the parser was never retired")
	third := h.send(t, "GET /three HTTP/1.1\r\n\r\n")
	h.requireInASpanNow(t, third, "while the retirement WARN is written")

	warn.Release()
	h.requireInASpan(t, second, "after the parser reported its stop")
	h.requireInASpan(t, third, "after the parser reported its stop")
	h.end(t)
	if n := h.mgr.MocksLeftOut(); n != 1 {
		t.Errorf("the manager counts %d mocks left out, want the exchange the parser stopped on", n)
	}
}

// A parser that panics is gone as it panics, and its capture with it. The
// supervisor stamped the stop and paused the capture only after it had logged
// the panic, with its stack, and reported it: what the connection carried
// while it did was teed to a parser no one would run again, and was in no
// span. The supervisor now stamps the stop where it recovers the panic, and
// aborts before it logs.
func TestRecordViaSupervisor_TheSpanAfterAPanicStartsWhereTheParserDied(t *testing.T) {
	panicked := newHeldLog(zapcore.ErrorLevel, "parser panicked")
	defer panicked.Release()
	h := newStopHarness(t, panickingParser{}, zap.New(panicked))

	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitFor(t, panicked.entered, "the supervisor never logged the panic")
	second := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	h.requireInASpanNow(t, second, "while the supervisor logs the panic")

	panicked.Release()
	h.requireInASpan(t, second, "after its parser panicked")
	h.end(t)
	if n := h.mgr.MocksLeftOut(); n != 0 {
		t.Errorf("the manager counts %d mocks left out, want none: the parser reported no exchange", n)
	}
}

// endsAsTheRecordingStopsParser reads the client's first request, then ends as
// the recording stops, in one of the ways a parser can: parked in a read, which
// observes no ctx, as every live connection's parser is when a recording stops
// ("parked"); by panicking ("panics"); or by returning an error that is not a
// cancel ("fails").
type endsAsTheRecordingStopsParser struct {
	stubParser
	read chan struct{}
	ends string
}

func (endsAsTheRecordingStopsParser) IsV2() bool { return true }

func (p endsAsTheRecordingStopsParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	close(p.read)
	switch p.ends {
	case "panics":
		<-ctx.Done()
		panic("a parser bug, hit as the recording stops")
	case "fails":
		<-ctx.Done()
		return errors.New("a parser error, as the recording stops")
	}
	for {
		if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
			return err
		}
	}
}

// A recording's stop ends every live V2 parser, and that is the end of its
// connection, not a loss: the connection is torn down with the recording. So
// the stop it stamps opens no span (openSpan), which would leave out the test
// cases in flight as the recording stops, and the dispatcher logs no "parser
// retired" WARN, which would say that every test case recorded from then on is
// left out. One rule decides both, the recording's context being done, however
// the parser ends and whatever status the supervisor gives it: a parked parser
// returns once the relay ends its stream, or is aborted once the supervisor's
// grace is over (canceled); one that panics is retired (panicked); and one
// that returns an error returns within the grace, after the supervisor has
// seen the stop, and is not retired. The WARN was decided apart, by a status
// of canceled: a parser that panicked as the recording stopped had its WARN
// logged, with no span opened. A parser's return wins the supervisor's select
// over the stop (error) only if it comes before the stop: its stop is then
// stamped while the recording runs, a loss, not one of these.
func TestRecordViaSupervisor_ARecordingsStopOpensNoSpanAndLogsNoRetirement(t *testing.T) {
	for _, tc := range []struct {
		ends string
		// statuses are those the dispatcher's fallthrough may log at Debug
		// for it, "" for a parser the supervisor does not retire
		// (Result.FallthroughToPassthrough unset).
		statuses []string
	}{
		// Canceled, if the parser does not return within the supervisor's
		// grace: in each case, if it is not run in time on a starved agent.
		{ends: "parked", statuses: []string{"", supervisor.StatusCanceled.String()}},
		{ends: "panics", statuses: []string{supervisor.StatusPanicked.String(), supervisor.StatusCanceled.String()}},
		// Error, if the supervisor reaches its select only after both the
		// stop and the return, on a starved agent: the return still comes
		// after the stop, and is judged as the recording stops.
		{ends: "fails", statuses: []string{"", supervisor.StatusError.String(), supervisor.StatusCanceled.String()}},
	} {
		t.Run(tc.ends, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			parser := endsAsTheRecordingStopsParser{read: make(chan struct{}), ends: tc.ends}
			h := newStopHarness(t, parser, zap.New(core))

			h.send(t, "GET /one HTTP/1.1\r\n\r\n")
			waitFor(t, parser.read, "the parser never read the request")
			h.cancel()
			select {
			case <-h.done:
			case <-time.After(20 * time.Second):
				t.Fatal("recordViaSupervisor did not return after the recording stopped")
			}

			status := ""
			for _, e := range logs.FilterMessageSnippet("parser supervisor triggered passthrough fallback").All() {
				status, _ = e.ContextMap()["status"].(string)
			}
			if !slices.Contains(tc.statuses, status) {
				t.Fatalf("the supervisor ended the parser with status %q, want one of %q: the test does not drive the path it is for", status, tc.statuses)
			}
			t.Logf("status %q", status)
			if n := logs.FilterMessageSnippet("parser retired").Len(); n != 0 {
				t.Errorf("%d \"parser retired\" WARNs after a recording's stop, want none: it says every test case recorded from then on is left out, and none is", n)
			}
			if recorded, _, _ := h.mgr.OrphanRangeCount(); recorded != 0 {
				t.Errorf("%d spans after a recording's stop, want none: it leaves out the test cases in flight as the recording stops", recorded)
			}
			if n := h.mgr.MocksLeftOut(); n != 0 {
				t.Errorf("the manager counts %d mocks left out, want none", n)
			}
		})
	}
}

// The spans of what a connection carries after a stop open only while the
// recording runs, wherever one opens (openSpan). One opens at the stop and
// closes once the connection has been idle for its grace; the traffic after
// that opens the next. That traffic can come as the recording stops: the
// relay, blocked writing an answer to an app slow to read it, finishes the
// write once the app reads. The rule was read at the stop alone, and that
// write opened a span after the recording had stopped, over the test cases in
// flight then.
func TestRecordViaSupervisor_TrafficAfterAnIdleCloseOpensNoSpanAsTheRecordingStops(t *testing.T) {
	h := newStopHarness(t, failingParser{}, zap.NewNop())
	h.slowApp(t)

	h.send(t, "GET /large HTTP/1.1\r\n\r\n")
	// The parser fails on the request while the recording runs: a stop, and a
	// span of what the connection carries from then on.
	waitUntilT(t, 5*time.Second, "the stop opens its span", func() bool {
		recorded, _, _ := h.mgr.OrphanRangeCount()
		return recorded > 0
	})
	h.answerUntilTheRelayIsBlocked(t)
	// The connection carries nothing while the relay is blocked: its span
	// closes for idleness.
	waitUntilT(t, 10*time.Second, "the span closes for idleness", func() bool {
		_, _, open := h.mgr.OrphanRangeCount()
		return open == 0
	})
	before, _, _ := h.mgr.OrphanRangeCount()

	stopped := time.Now()
	h.cancel()
	h.appReads()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		t.Fatal("recordViaSupervisor did not return after the recording stopped")
	}
	after, _, _ := h.mgr.OrphanRangeCount()
	if over, _ := h.mgr.WasMockOrphanedInWindow(stopped, time.Now()); after != before || over {
		t.Errorf("%d spans, %d before the recording stopped; a span after the stop: %v. It leaves out the test cases in flight as the recording stops", after, before, over)
	}
}

// aheadThenPanicParser reads its client stream ahead of its loop (ReadAhead),
// as most V2 parsers' readers do: it takes n chunks, telling took of each,
// marks only the first processed, and panics once die is closed, with the rest
// taken and never parsed.
type aheadThenPanicParser struct {
	stubParser
	n    int
	took chan struct{}
	die  chan struct{}
}

func (aheadThenPanicParser) IsV2() bool { return true }

func (p aheadThenPanicParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	for i := 0; i < p.n; i++ {
		c, err := s.V2.ClientStream.ReadAhead()
		if err != nil {
			return err
		}
		if i == 0 {
			s.V2.MarkProcessed(fakeconn.FromClient, c)
		}
		p.took <- struct{}{}
	}
	<-p.die
	panic("a main-loop bug, with chunks taken and not parsed")
}

// A parser whose loop panics dies with what its readers took ahead of it not
// parsed. Those requests came before the panic, so the stop's span, which
// starts at the panic, does not cover them, and the return left them out at
// Debug alone: the test cases over them were set aside with no reason, and
// the recording's summary counted nothing. Each span the return leaves out
// after a panic is now reported left out (Session.ReportLeftOut), with a
// reason naming the panic and the parser, and the request the parser did
// parse is in none.
func TestRecordViaSupervisor_APanicReportsWhatItsParserTookAndNeverParsed(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p := aheadThenPanicParser{n: 3, took: make(chan struct{}, 3), die: make(chan struct{})}
	h := newStopHarness(t, p, zap.New(core))
	var once sync.Once
	die := func() { once.Do(func() { close(p.die) }) }
	defer die()

	one := h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitFor(t, p.took, "the parser never took the first request")
	two := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	waitFor(t, p.took, "the parser never took the second request")
	three := h.send(t, "GET /three HTTP/1.1\r\n\r\n")
	waitFor(t, p.took, "the parser never took the third request")
	die()
	h.end(t)

	h.requireInASpan(t, two, "taken and not parsed before its parser panicked")
	h.requireInASpan(t, three, "taken and not parsed before its parser panicked")
	if over, _ := h.mgr.WasMockOrphanedInWindow(one.sent, one.arrived); over {
		t.Errorf("the request the parser parsed before it panicked is in a span")
	}
	var reasons []string
	for _, e := range logs.All() {
		if e.Message != supervisor.LeftOutWarnMsg && e.Message != supervisor.LeftOutDebugMsg {
			continue
		}
		reason, _ := e.ContextMap()["reason"].(string)
		reasons = append(reasons, reason)
		if !strings.HasPrefix(reason, "parser panicked: the http parser died with ") || strings.Contains(reason, "panic:") {
			t.Errorf("left out for %q, want the panic named, with its parser, and no \"panic:\" token", reason)
		}
	}
	if n := h.mgr.MocksLeftOut(); n == 0 || n != int64(len(reasons)) {
		t.Errorf("the manager counts %d mocks left out, and %d were logged (%q): want each span the panic left out reported", n, len(reasons), reasons)
	}
}

// aheadThenStopParser reads its client stream ahead of its loop, as
// aheadThenPanicParser does: it takes n chunks, telling took of each, and
// marks only the first processed. Then its Run ends in a stop with the rest
// taken and never parsed: it returns fail, or, when fail is nil, waits on its
// context until the supervisor retires it (a hang, or its memory cap).
type aheadThenStopParser struct {
	stubParser
	n    int
	took chan struct{}
	fail error
}

func (aheadThenStopParser) IsV2() bool { return true }

func (p aheadThenStopParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	for i := 0; i < p.n; i++ {
		c, err := s.V2.ClientStream.ReadAhead()
		if err != nil {
			return err
		}
		if i == 0 {
			s.V2.MarkProcessed(fakeconn.FromClient, c)
		}
		p.took <- struct{}{}
	}
	if p.fail != nil {
		return p.fail
	}
	<-ctx.Done()
	return ctx.Err()
}

// armedClock is a stall.ManualClock that says, on armed, each time the
// supervisor's watchdog arms its next look, so a test moves the clock past
// the hang budget only once the watchdog waits on it.
type armedClock struct {
	*stall.ManualClock
	armed chan struct{}
}

func (c armedClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	ch, stop := c.ManualClock.After(d)
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return ch, stop
}

// sendTaken writes req from the app and returns once the parser took it
// (took): the relay read it, and teed it, in between.
func (h *stopHarness) sendTaken(t *testing.T, req string, took <-chan struct{}) sentRequest {
	t.Helper()
	sent := time.Now()
	if _, err := h.clientApp.Write([]byte(req)); err != nil {
		t.Fatalf("write %q: %v", req, err)
	}
	waitFor(t, took, "the parser never took "+req)
	return sentRequest{sent: sent, arrived: time.Now()}
}

// parserStopCase is a stop a row ends a parser's Run in: fail is the error it
// returns (nil: it waits on its context); retire, when set, retires it once
// it took every request; supervise builds its connection's supervisor (nil:
// supervisor.New); want is how the reason of a span it leaves out begins.
type parserStopCase struct {
	name, want string
	fail       error
	retire     func(t *testing.T)
	supervise  func(supervisor.Config) *supervisor.Supervisor
}

// hungStop retires the parser as hung: its watchdog reads a clock the row
// moves past the hang budget each time the watchdog waits on it, once the
// parser took every request. A look can find activity after the last move:
// the relay's own, as it finishes forwarding the last request after the
// parser took it (BumpActivity follows the write to the destination). That
// look arms the next, which finds the hang, as nothing moves after it.
func hungStop() parserStopCase {
	const budget = time.Minute
	clk := armedClock{ManualClock: &stall.ManualClock{}, armed: make(chan struct{}, 1)}
	return parserStopCase{
		name: "hung",
		want: "parser hung: the http parser was retired as hung (no progress for its hang budget) with ",
		supervise: func(cfg supervisor.Config) *supervisor.Supervisor {
			cfg.HangBudget, cfg.Clock = budget, clk
			return supervisor.New(cfg)
		},
		retire: func(t *testing.T) {
			stop := make(chan struct{})
			t.Cleanup(func() { close(stop) })
			go func() {
				for {
					select {
					case <-clk.armed:
						clk.Advance(budget + time.Millisecond)
					case <-stop:
						return
					}
				}
			}()
		},
	}
}

// memCapStop retires the parser over its connection's memory cap.
func memCapStop() parserStopCase {
	built := make(chan *supervisor.Supervisor, 1)
	return parserStopCase{
		name: "memory cap",
		want: "parser over its memory cap: the http parser was retired over its connection's memory cap with ",
		supervise: func(cfg supervisor.Config) *supervisor.Supervisor {
			sv := supervisor.New(cfg)
			built <- sv
			return sv
		},
		retire: func(*testing.T) { (<-built).MarkMemCapExceeded() },
	}
}

// Every stop a parser's Run ends in leaves what its readers took ahead of its
// loop, and what was still queued for it, not parsed, as a panic does: a
// parser that returned an error, one the watchdog found hung, and one over its
// connection's memory cap. Those requests came before the stop, so the stop's
// span, which starts at it, does not cover them, and the return left them out
// at Debug alone: the test cases over them were set aside with no reason, and
// the recording's summary counted nothing. Only the per-connection "parser
// retired" WARN spoke for them, and it does not say which test cases it cost.
// Each span the return leaves out after any stop is now reported left out
// (Session.ReportLeftOut), with a reason naming the stop and the parser, and
// the request the parser did parse is in none.
func TestRecordViaSupervisor_EveryStopReportsWhatItsParserTookAndNeverParsed(t *testing.T) {
	for _, tc := range []parserStopCase{
		{name: "error", fail: errors.New("a frame that does not decode"),
			want: "parser stopped on an error: the http parser returned an error with "},
		hungStop(),
		memCapStop(),
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			p := aheadThenStopParser{n: 3, took: make(chan struct{}, 3), fail: tc.fail}
			h := newStopHarnessWith(t, p, zap.New(core), tc.supervise)

			one := h.sendTaken(t, "GET /one HTTP/1.1\r\n\r\n", p.took)
			two := h.sendTaken(t, "GET /two HTTP/1.1\r\n\r\n", p.took)
			three := h.sendTaken(t, "GET /three HTTP/1.1\r\n\r\n", p.took)
			if tc.retire != nil {
				tc.retire(t)
			}
			h.end(t)

			h.requireInASpan(t, two, "taken and not parsed before its parser's stop")
			h.requireInASpan(t, three, "taken and not parsed before its parser's stop")
			if over, _ := h.mgr.WasMockOrphanedInWindow(one.sent, one.arrived); over {
				t.Errorf("the request the parser parsed before its stop is in a span")
			}
			var reasons []string
			for _, e := range logs.All() {
				if e.Message != supervisor.LeftOutWarnMsg && e.Message != supervisor.LeftOutDebugMsg {
					continue
				}
				reason, _ := e.ContextMap()["reason"].(string)
				reasons = append(reasons, reason)
				if !strings.HasPrefix(reason, tc.want) {
					t.Errorf("left out for %q, want the stop named, with its parser: %q...", reason, tc.want)
				}
			}
			if n := h.mgr.MocksLeftOut(); n == 0 || n != int64(len(reasons)) {
				t.Errorf("the manager counts %d mocks left out, and %d were logged (%q): want each span the stop left out reported", n, len(reasons), reasons)
			}
		})
	}
}
