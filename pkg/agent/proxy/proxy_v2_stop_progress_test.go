package proxy

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// scaledClock is a supervisor clock (stall.Clock) that runs scale times as
// fast as the wall clock: below 1, a starved agent, whose parser is run for a
// small part of the wall time that passes; above 1, a test that has the hang
// budget pass in a fraction of its wall time.
type scaledClock struct {
	start time.Time
	scale float64
}

func newScaledClock(scale float64) *scaledClock {
	return &scaledClock{start: time.Now(), scale: scale}
}

func (c *scaledClock) Runnable() time.Duration {
	return time.Duration(float64(time.Since(c.start)) * c.scale)
}

func (c *scaledClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(time.Duration(float64(d) / c.scale))
	return t.C, t.Stop
}

// foldsAtTheStopParser reads both its streams to their end, which they reach
// at the recording's stop, then hands over a record of what it read every
// 10ms (Session.Folded), records of them in all, and returns, having sent the
// count to out. As a parser does, it goes by its streams, not its context,
// which the supervisor cancels as the recording stops.
type foldsAtTheStopParser struct {
	stubParser
	records int
	readAll chan struct{}
	out     chan int
}

func newFoldsAtTheStopParser(records int) foldsAtTheStopParser {
	return foldsAtTheStopParser{records: records, readAll: make(chan struct{}), out: make(chan int, 1)}
}

func (foldsAtTheStopParser) IsV2() bool { return true }

func (p foldsAtTheStopParser) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	for _, fc := range []*fakeconn.FakeConn{s.V2.ClientStream, s.V2.DestStream} {
		for {
			if _, err := fc.ReadChunk(); err != nil {
				break
			}
		}
	}
	close(p.readAll)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	handed := 0
	for handed < p.records {
		<-tick.C
		s.V2.Folded()
		handed++
	}
	p.out <- handed
	return nil
}

// noWarns fails for every line at WARN or above in logs.
func noWarns(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	for _, e := range logs.All() {
		if e.Level >= zapcore.WarnLevel {
			t.Errorf("unexpected %v: %s %v", e.Level, e.Message, e.ContextMap())
		}
	}
}

// stuckUntilTheEnd is closed as the test ends: a stuck parser's way out.
func stuckUntilTheEnd(t *testing.T) <-chan struct{} {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	return stuck
}

// A parser that has read everything its connection carried and is still
// handing over records of it at the recording's stop is waited for while it
// does: it hands over every record and returns on its own before the record
// path does, and nothing says a parser was cut.
func TestRecordViaSupervisor_AParserHandingOverAtTheStopFinishesAndLosesNothing(t *testing.T) {
	const records = 50 // about half a second of hand-overs
	core, logs := observer.New(zapcore.DebugLevel)
	parser := newFoldsAtTheStopParser(records)
	h := newStopHarness(t, parser, zap.New(core))

	h.send(t, "GET /one HTTP/1.1\r\n\r\n")
	h.cancel()
	waitFor(t, parser.readAll, "the parser never read both its streams to their end")
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordViaSupervisor never returned after the recording stopped")
	}

	select {
	case handed := <-parser.out:
		if handed != records {
			t.Errorf("the parser handed over %d of its %d records", handed, records)
		}
	default:
		t.Error("the record path returned while the parser was still handing over records: it was cut")
	}
	noWarns(t, logs)
}

// behindAtTheStopParser is behind its capture at the recording's stop: it
// reads nothing until release is closed, then reads its client stream a chunk
// every pace, handing over a mock of each chunk as it reads it, then its
// server stream, and returns, having sent the chunks it read to out. With a
// nil release it never reads, and returns only once stuck is closed, whatever
// its context says: a parser stuck with captured bytes unread.
type behindAtTheStopParser struct {
	stubParser
	release <-chan struct{}
	stuck   <-chan struct{}
	pace    time.Duration
	at      time.Time // the requests' time, for the mocks
	lost    *atomic.Int64
	out     chan int
}

func (behindAtTheStopParser) IsV2() bool { return true }

func (p behindAtTheStopParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if p.release == nil {
		<-p.stuck
		return nil
	}
	<-p.release
	chunks := 0
	for {
		if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
			break
		}
		chunks++
		m := &models.Mock{Kind: models.HTTP, Name: "mock-" + strconv.Itoa(chunks)}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = p.at, p.at
		if lost, err := s.V2.EmitMockOutcome(m, ctx.Err() != nil); lost || err != nil {
			p.lost.Add(1)
		}
		time.Sleep(p.pace)
	}
	for {
		if _, err := s.V2.DestStream.ReadChunk(); err != nil {
			break
		}
	}
	p.out <- chunks
	return nil
}

// A parser behind its capture at the recording's stop, on a starved agent,
// that goes on reading it slowly, is waited for however much wall time that
// takes: it is cut only once it makes no progress for its hang budget, on the
// time the agent could run, never on the wall clock. Here the agent's clock
// runs at a twentieth of the wall clock (a CPU-starved pod), and the parser
// is not run for six seconds of wall time after the stop, then reads a chunk
// every 100ms. It reads and hands over every request the connection carried
// before the stop, the record path returns only after it does, no request is
// in a span (a test case recorded over it would be left out), and nothing
// says a parser was cut.
func TestRecordViaSupervisor_AParserBehindAtTheStopOnAStarvedAgentKeepsEveryRequest(t *testing.T) {
	const requests = 5
	core, logs := observer.New(zapcore.DebugLevel)
	release := make(chan struct{})
	parser := behindAtTheStopParser{release: release, pace: 100 * time.Millisecond, at: time.Now(), lost: &atomic.Int64{}, out: make(chan int, 1)}
	h := newStopHarnessWithClock(t, parser, zap.New(core), newScaledClock(1.0/20))

	var sent []sentRequest
	for i := 0; i < requests; i++ {
		sent = append(sent, h.send(t, "GET /"+strconv.Itoa(i)+" HTTP/1.1\r\n\r\n"))
	}
	h.cancel()
	time.Sleep(6 * time.Second)
	close(release)
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		t.Fatal("recordViaSupervisor never returned after the recording stopped")
	}

	select {
	case chunks := <-parser.out:
		if chunks == 0 {
			t.Error("the parser read none of the requests the connection carried before the stop")
		}
	default:
		t.Error("the record path returned while the parser was still reading what the connection carried before the stop: it was cut")
	}
	if n := parser.lost.Load(); n != 0 {
		t.Errorf("%d of the parser's mocks were left out or refused", n)
	}
	for i, r := range sent {
		if over, _ := h.mgr.WasMockOrphanedInWindow(r.sent, r.arrived); over {
			t.Errorf("request %d, carried before the stop and recorded, is in a span: a test case over it is left out", i)
		}
	}
	noWarns(t, logs)
}

// A parser stuck at the recording's stop, with bytes its connection carried
// before the stop unread, is cut once it makes no progress for its hang
// budget, on the time the agent could run (here a clock a hundred times as
// fast as the wall clock: its minute in under a second). The request it never
// read is left out (a test case over it is left out, not saved without its
// mock) as the stop cut's, by which the recording names that test case as it
// ends, and one WARN names the connection and the test cases that cost: those
// over the window of the requests it left out, which holds the request.
func TestRecordViaSupervisor_AParserStuckAtTheStopIsCutAndTheTestCasesItCostAreNamed(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	parser := behindAtTheStopParser{stuck: stuckUntilTheEnd(t), lost: &atomic.Int64{}, out: make(chan int, 1)}
	h := newStopHarnessWithClock(t, parser, zap.New(core), newScaledClock(100))

	r := h.send(t, "GET /stuck HTTP/1.1\r\n\r\n")
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the hang watchdog never cut the parser stuck at the stop")
	}

	h.requireInASpan(t, r, "before the stop, which the stuck parser never read")
	if !h.mgr.WasStopCutInWindow(r.sent, r.arrived) {
		t.Error("the request the stuck parser never read is left out, but not as the stop cut's: the recording does not name the test case over it")
	}
	cut := logs.FilterMessage(stopCutWarnMsg).All()
	if len(cut) != 1 {
		t.Fatalf("%d WARNs say the stop cut the parser, want 1", len(cut))
	}
	e := cut[0]
	f := e.ContextMap()
	if e.Level != zapcore.WarnLevel || f["clientConnID"] != "1" || f["parser"] != string(integrations.HTTP) {
		t.Errorf("the cut is logged at %v for connection %v, parser %v; want a WARN for connection 1, parser %s", e.Level, f["clientConnID"], f["parser"], integrations.HTTP)
	}
	if n, _ := f["unread_request_chunks"].(int64); n < 1 {
		t.Errorf("the WARN counts %v unread request chunks, want at least the one sent", f["unread_request_chunks"])
	}
	from, okFrom := f["test_cases_from"].(time.Time)
	to, okTo := f["test_cases_to"].(time.Time)
	if !okFrom || !okTo || from.After(r.arrived) || to.Before(r.sent) {
		t.Errorf("the WARN names the test cases from %v to %v, which misses the request carried from %v to %v", f["test_cases_from"], f["test_cases_to"], r.sent, r.arrived)
	}
	if n := logs.FilterMessageSnippet("parser retired").Len(); n != 0 {
		t.Errorf("%d WARNs say the connection can no longer be recorded, at the recording's stop", n)
	}
}

// A parser the hang watchdog cuts while the recording runs is said to leave
// its connection unrecorded ("parser retired"), not to have been cut by the
// stop: the request it never read is left out, and not as the stop cut's.
func TestRecordViaSupervisor_AParserStuckBeforeTheStopIsNotSaidCutByTheStop(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	parser := behindAtTheStopParser{stuck: stuckUntilTheEnd(t), lost: &atomic.Int64{}, out: make(chan int, 1)}
	h := newStopHarnessWithClock(t, parser, zap.New(core), newScaledClock(100))

	r := h.send(t, "GET /stuck HTTP/1.1\r\n\r\n")
	waitUntilT(t, 30*time.Second, "the hang watchdog retires the stuck parser", func() bool {
		return logs.FilterMessageSnippet("parser retired").Len() > 0
	})
	h.end(t)

	if n := logs.FilterMessage(stopCutWarnMsg).Len(); n != 0 {
		t.Errorf("%d WARNs say the recording's stop cut a parser retired while it ran", n)
	}
	h.requireInASpan(t, r, "while the recording ran, which the retired parser never read")
	if h.mgr.WasStopCutInWindow(r.sent, r.arrived) {
		t.Error("the request a parser retired while the recording ran never read is left out as the stop cut's: the recording names its test case as cut by the stop")
	}
}

// With nothing left out of the requests (the parser had read them all), the
// WARN names no window of test cases, and says only the exchange in hand may
// lack its mock.
func TestWarnStopCut_NamesNoWindowWhenNoRequestWasLeftOut(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	warnStopCut(zap.New(core), integrations.HTTP, "7", leftOutRequests{})
	all := logs.All()
	if len(all) != 1 {
		t.Fatalf("%d lines, want the one WARN", len(all))
	}
	f := all[0].ContextMap()
	if _, ok := f["test_cases_from"]; ok {
		t.Errorf("the WARN names a window of test cases with no request left out: %v", f)
	}
	if _, ok := f["test_cases_to"]; ok {
		t.Errorf("the WARN names a window of test cases with no request left out: %v", f)
	}
	if next, _ := f["next_step"].(string); !containsAll(next, "had read every request", "only the exchange it was in") {
		t.Errorf("next_step %q does not say only the exchange in hand may lack its mock", next)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
