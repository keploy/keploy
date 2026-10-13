package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
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
// it. The span the hole opens starts at the session's stop instant
// (Session.StoppedAt), which the parser's report stamped, not where the hole
// read the clock: the bytes captured between the two are lost with the
// parser, and a test case recorded over them was saved without its mocks. The
// hole read the clock itself, so its span started after them. Here the parser
// had read all the connection carried before its stop, so the exchange's span
// (to the last byte it read) and the hole's meet.
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

// A capture hole stops its connection at the chunk it loses
// (Session.StoppedAtHole), not where the parser had got to: the parser goes
// on with what it had queued before the hole, intact, and records it, so a
// test case over that backlog keeps its mocks and is not left out. Dated
// where the parser had got to, the hole's span would leave out a full
// capture queue of test cases on every hole.
func TestRecordViaSupervisor_AHoleLeavesTheBacklogBeforeItToTheParser(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	mgr := syncMock.New(nil)
	parser := &heldParser{release: make(chan struct{}), took: make(chan struct{})}
	defer close(parser.release)
	// A 4-byte cap: the first write fits, a longer one is a hole.
	h := newDesyncFeedHarnessCtx(t, syncMock.NewContext(context.Background(), mgr), parser, nil, 4)

	sent := time.Now()
	h.writeApp(t, []byte("a")) // queued, behind a parser that is not reading
	waitUntilT(t, 5*time.Second, "the relay forwards the backlog", func() bool { return h.destSeen() == "a" })
	arrived := time.Now()
	time.Sleep(time.Millisecond) // so the hole's chunk is stamped after the backlog's window
	h.writeApp(t, []byte("0123456789"))
	waitUntilT(t, 5*time.Second, "the hole opens its span", func() bool {
		recorded, _, _ := mgr.OrphanRangeCount()
		return recorded >= 1
	})
	if over, _ := mgr.WasMockOrphanedInWindow(sent, arrived); over {
		t.Fatal("the backlog queued before the hole, which the parser goes on to record, is left out: the hole's stop was dated where the parser had got to")
	}
	if over, _ := mgr.WasMockOrphanedInWindow(arrived, time.Now()); !over {
		t.Fatal("what the connection carried from the hole on is in no span")
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

// newStopHarnessWith is newStopHarness with tune, when non-nil, setting the
// proxy up before the connection is recorded.
func newStopHarnessWith(t *testing.T, parser integrations.Integrations, logger *zap.Logger, tune func(*Proxy)) *stopHarness {
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
	}
	if tune != nil {
		tune(p)
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

// diesBehindParser reads the client's first chunk, then, once released, ends
// without reporting the exchange it was in, as a parser behind its traffic
// does that panics or returns an error: the chunks queued behind it are lost
// with it.
type diesBehindParser struct {
	stubParser
	read    chan struct{}
	release chan struct{}
	panics  bool
}

func (diesBehindParser) IsV2() bool { return true }

func (p diesBehindParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	close(p.read)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.panics {
		panic("a parser bug, hit behind the traffic")
	}
	return errors.New("a parser error, behind the traffic")
}

// A parser behind its traffic that panics or returns an error, without
// reporting the exchange it was in, loses every request queued behind it. Its
// stop is dated at the newest chunk the connection carried, so the span it
// opens starts after that backlog; the requests it never read are left out
// as its Run returns (parserReads), before the watermark lets go of the test
// cases held for them.
func TestRecordViaSupervisor_AParserThatDiesBehindItsTrafficLeavesItsBacklogOut(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name := "returns an error"
		if panics {
			name = "panics"
		}
		t.Run(name, func(t *testing.T) {
			parser := diesBehindParser{read: make(chan struct{}), release: make(chan struct{}), panics: panics}
			h := newStopHarness(t, parser, zap.NewNop())

			h.send(t, "GET /one HTTP/1.1\r\n\r\n")
			waitFor(t, parser.read, "the parser never read the first request")
			// The parser's backlog: a request inside a test case, and one
			// after it.
			backlog := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
			after := h.send(t, "GET /three HTTP/1.1\r\n\r\n")

			close(parser.release)
			waitUntilT(t, 10*time.Second, "the parser's stop to open its span", func() bool {
				over, _ := h.mgr.WasMockOrphanedInWindow(after.sent, after.arrived)
				return over
			})
			waitUntilT(t, 10*time.Second, "the requests the parser never read to be left out as it returns", func() bool {
				over, _ := h.mgr.WasMockOrphanedInWindow(backlog.sent, backlog.arrived)
				return over
			})
			h.end(t)
			h.requireInASpan(t, backlog, "queued behind a parser that "+name+", once the connection ended")
		})
	}
}

// diesInAnExchangeParser begins an exchange as HTTP/1 and MySQL do
// (Session.NextRequest), reads it whole, its request and its response, and,
// once released, dies before it records it: as a parser that panics building
// the mock, or returns an error on it, does. The next request is queued
// behind it.
type diesInAnExchangeParser struct {
	stubParser
	read    chan struct{}
	release chan struct{}
	panics  bool
}

func (diesInAnExchangeParser) IsV2() bool { return true }

func (p diesInAnExchangeParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.NextRequest(models.HTTP, false); err != nil {
		return err
	}
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	if _, err := s.V2.DestStream.ReadChunk(); err != nil {
		return err
	}
	close(p.read)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.panics {
		panic("a parser bug building the mock of the exchange it read")
	}
	return errors.New("a parser error on the exchange it read")
}

// The exchange a parser was in when it died is lost with it, though it had
// read it whole: it had not recorded it. Neither the stop's span (from the
// newest chunk) nor the requests it never read (the next one) covered it, and
// a test case over it, ended once the app had the answer, was saved without
// its mock. The supervisor now reports it as the parser would have, from its
// request to the last byte it read (Session.ReportUnfinishedExchange).
func TestRecordViaSupervisor_AParserThatDiesInAnExchangeItReadLeavesItOut(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name := "returns an error"
		if panics {
			name = "panics"
		}
		t.Run(name, func(t *testing.T) {
			parser := diesInAnExchangeParser{read: make(chan struct{}), release: make(chan struct{}), panics: panics}
			h := newStopHarness(t, parser, zap.NewNop())

			tcStart := time.Now()
			h.send(t, "GET /one HTTP/1.1\r\n\r\n")
			resp := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
			if _, err := h.destSvc.Write([]byte(resp)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(resp))
			if _, err := io.ReadFull(h.clientApp, buf); err != nil {
				t.Fatal(err)
			}
			tcEnd := time.Now() // the app has its answer: the test case ends
			waitFor(t, parser.read, "the parser never read the exchange")
			time.Sleep(time.Millisecond) // so the next request is stamped after the test case
			next := h.send(t, "GET /two HTTP/1.1\r\n\r\n")

			close(parser.release)
			// The stop's span opens as the stop is stamped; the exchange is
			// reported just after, on the supervisor's goroutine: wait for both.
			waitUntilT(t, 10*time.Second, "the parser's stop to open its span and its exchange to be reported", func() bool {
				over, _ := h.mgr.WasMockOrphanedInWindow(next.sent, next.arrived)
				return over && h.mgr.MocksLeftOut() == 1
			})
			if over, _ := h.mgr.WasMockOrphanedInWindow(tcStart, tcEnd); !over {
				t.Errorf("a test case over the exchange the parser died in, read whole and not recorded, is in no span: saved without its mock")
			}
			h.end(t)
		})
	}
}

// recordsThenDiesParser records its connection's first exchange, then begins
// the next, reads its request and, once released, dies on it without
// reporting it.
type recordsThenDiesParser struct {
	stubParser
	recorded chan struct{}
	release  chan struct{}
}

func (recordsThenDiesParser) IsV2() bool { return true }

func (p recordsThenDiesParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.NextRequest(models.HTTP, false); err != nil {
		return err
	}
	req, err := s.V2.ClientStream.ReadChunk()
	if err != nil {
		return err
	}
	resp, err := s.V2.DestStream.ReadChunk()
	if err != nil {
		return err
	}
	if err := s.V2.EmitMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: req.ReadAt, ResTimestampMock: resp.ReadAt}}); err != nil {
		return err
	}
	if _, err := s.V2.NextRequest(models.HTTP, false); err != nil {
		return err
	}
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	close(p.recorded)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return errors.New("a parser error on the second exchange")
}

// What a parser recorded before it died is not lost with it: it ended that
// exchange as it emitted its mock, and the exchange reported for it is the one
// it died in (Session.ReportUnfinishedExchange), from that one's request. A
// test case over the recorded exchange keeps its mock and is not left out.
func TestRecordViaSupervisor_WhatAParserRecordedBeforeItDiedIsNotLeftOut(t *testing.T) {
	parser := recordsThenDiesParser{recorded: make(chan struct{}), release: make(chan struct{})}
	h := newStopHarness(t, parser, zap.NewNop())

	tcStart := time.Now()
	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	if _, err := h.destSvc.Write([]byte(resp)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(resp))
	if _, err := io.ReadFull(h.clientApp, buf); err != nil {
		t.Fatal(err)
	}
	tcEnd := time.Now()
	time.Sleep(time.Millisecond) // so the next request is stamped after the test case
	next := h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	waitFor(t, parser.recorded, "the parser never recorded the first exchange")

	close(parser.release)
	waitUntilT(t, 10*time.Second, "the exchange the parser died in to be left out", func() bool {
		over, _ := h.mgr.WasMockOrphanedInWindow(next.sent, next.arrived)
		return over && h.mgr.MocksLeftOut() == 1
	})
	if over, _ := h.mgr.WasMockOrphanedInWindow(tcStart, tcEnd); over {
		t.Error("a test case over the exchange the parser recorded before it died is left out")
	}
	h.end(t)
}

// hungInExchangeParser begins an exchange as HTTP/1 does (NextRequest), reads
// its request, and hangs there, deaf to its context, until released: a
// parser the dispatcher must retire without it ever returning.
type hungInExchangeParser struct {
	stubParser
	release chan struct{}
}

func (hungInExchangeParser) IsV2() bool { return true }

func (p hungInExchangeParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.NextRequest(models.HTTP, false); err != nil {
		return err
	}
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	<-p.release
	return nil
}

// A parser hung in an exchange, retired by the dispatcher (the hang
// watchdog's abort) without ever returning, cannot report the exchange; the
// abort reports it for it (Session.ReportUnfinishedExchange), as the
// supervisor does for one that returns an error or panics.
func TestRecordViaSupervisor_AParserRetiredInAnExchangeLeavesItOut(t *testing.T) {
	parser := hungInExchangeParser{release: make(chan struct{})}
	t.Cleanup(func() { close(parser.release) })
	h := newStopHarnessWith(t, parser, zap.NewNop(), func(p *Proxy) { p.parserHangBudget = 200 * time.Millisecond })
	one := h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitUntilT(t, 20*time.Second, "the exchange the retired parser was in to be left out", func() bool {
		over, _ := h.mgr.WasMockOrphanedInWindow(one.sent, one.arrived)
		return over && h.mgr.MocksLeftOut() == 1
	})
}

// A chunk is the connection's traffic from when the relay read and stamped
// it: the span of a stopped connection reaches it then (relay.Config.OnCapture,
// noted at stamping), not only once the relay has written it on and teed it
// (OnCarried), which a peer slow to read holds back. Noted only then, a test
// case over a chunk the relay was blocked writing, checked meanwhile, was
// saved without its mocks.
func TestRecordViaSupervisor_AChunkIsNotedAsItIsStamped(t *testing.T) {
	var cfg relay.Config
	configured := make(chan struct{})
	h := newStopHarnessWith(t, failingParser{}, zap.NewNop(), func(p *Proxy) {
		p.relayConfigured = func(c *relay.Config) { cfg = *c; close(configured) }
	})
	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	waitFor(t, configured, "the relay was never configured")
	waitUntilT(t, 5*time.Second, "the stop opens its span", func() bool {
		recorded, _, _ := h.mgr.OrphanRangeCount()
		return recorded > 0
	})
	waitUntilT(t, 10*time.Second, "the span closes for idleness", func() bool {
		_, _, open := h.mgr.OrphanRangeCount()
		return open == 0
	})
	if cfg.OnCapture == nil {
		t.Fatal("the relay is not told to note each chunk as it stamps it")
	}
	stamped := time.Now()
	cfg.OnCapture(stamped) // a chunk read and stamped, not written on yet
	if over, _ := h.mgr.WasMockOrphanedInWindow(stamped, stamped); !over {
		t.Fatal("a chunk the relay stamped after the stop is in no span until it is written on")
	}
	h.end(t)
}

// hungParser reads nothing, and returns only when it is cancelled: a parser
// hung with the connection's requests queued for it.
type hungParser struct{ stubParser }

func (hungParser) IsV2() bool { return true }

func (hungParser) RecordOutgoing(ctx context.Context, _ *integrations.RecordSession) error {
	<-ctx.Done()
	return ctx.Err()
}

// A hung parser is retired once it has had work pending for the hang budget
// with no progress, and its capture is paused and its streams closed. The
// requests queued for it that it never read are left out as its Run returns
// (parserReads), the older one too, though the stop is dated at the newest.
func TestRecordViaSupervisor_AHungParsersStopCoversWhatWasQueuedForIt(t *testing.T) {
	h := newStopHarnessWith(t, hungParser{}, zap.NewNop(), func(p *Proxy) { p.parserHangBudget = 200 * time.Millisecond })
	queued := h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	time.Sleep(time.Millisecond) // so the second is stamped after the first
	h.send(t, "GET /two HTTP/1.1\r\n\r\n")
	waitUntilT(t, 20*time.Second, "the requests the hung parser never read to be left out", func() bool {
		over, _ := h.mgr.WasMockOrphanedInWindow(queued.sent, queued.arrived)
		return over
	})
	h.end(t)
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

// gatedReadParser reads one client chunk, says so, and reads on to the
// stream's end.
type gatedReadParser struct {
	stubParser
	got chan struct{}
}

func (gatedReadParser) IsV2() bool { return true }

func (p gatedReadParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	close(p.got)
	for {
		if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
			return nil
		}
	}
}

// A parser held behind its gate (KEPLOY_DEBUG_PARSER_GATE) gets nothing to
// consume for as long as the test that set the gate chooses: the hang watchdog
// is off for it, or it would take the parser for hung and retire it, and a
// lane that orders parsers by their gates would test a retirement instead.
func TestRecordViaSupervisor_AParserHeldBehindItsGateIsNotRetiredAsHung(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(debugParserGateEnv, dir)
	const budget = 20 * time.Millisecond
	parser := gatedReadParser{got: make(chan struct{})}
	h := newStopHarnessWith(t, parser, zap.NewNop(), func(p *Proxy) {
		p.parserHangBudget = budget
		p.parserGates = debugParserGate(zap.NewNop())
	})
	h.send(t, "GET /held HTTP/1.1\r\nHost: x\r\n\r\n")
	select {
	case err := <-h.done:
		t.Fatalf("the parser held behind its gate was retired (%v) within %v, 25 hang budgets", err, 25*budget)
	case <-time.After(25 * budget):
	}
	if err := os.WriteFile(filepath.Join(dir, "1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parser.got:
	case err := <-h.done:
		t.Fatalf("the parser returned (%v) before its gate let the request through", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the request never reached the parser once its gate opened")
	}
	h.end(t)
}
