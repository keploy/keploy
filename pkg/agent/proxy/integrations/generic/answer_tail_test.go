package generic

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// tailRun is what a recording of an answer sent in more than one chunk left:
// the mocks recorded, the reason of each mock left out, the spans left out,
// and the manager's count of mocks left out.
type tailRun struct {
	recorded []*models.Mock
	leftOut  []string
	spans    *syncMock.Spans
	counted  int64
}

// recordChunks records chunks, captured in their order (as the relay numbers
// them), with the generic parser from RecordOutgoing, its readers taking them
// from both streams and its merge pairing them in capture order, the mocks
// handed to the sync-mock manager when viaSyncMock is set (as in production)
// or to the session's channel. want is how many mocks it is to record.
func recordChunks(t *testing.T, chunks []fakeconn.Chunk, viaSyncMock bool, want int) tailRun {
	t.Helper()
	clientCh, destCh := make(chan fakeconn.Chunk, len(chunks)), make(chan fakeconn.Chunk, len(chunks))
	order := newCaptureOrder(nil)
	for _, c := range chunks {
		if c.Dir == fakeconn.FromClient {
			clientCh <- c
		} else {
			destCh <- c
		}
		order.put[dirIndex(c.Dir)] += int64(len(c.Bytes))
	}
	close(clientCh)
	close(destCh)
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Order = order.StreamedBefore
	core, logs := observer.New(zapcore.DebugLevel)
	sess.Logger = zap.New(core)
	mgr := syncMock.New(nil)
	sess.Mgr = mgr
	r := tailRun{spans: &syncMock.Spans{}}
	sess.Orphans = r.spans
	if viaSyncMock {
		mgr.SetOutputChannel(mocks)
		sess.RouteMocksViaSyncMock = true
	}
	// Its streams have ended, so the parser returns, bounded only to turn a
	// parser that never does into a failure.
	done := make(chan error, 1)
	go func() { done <- dispatch(t, sess) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the parser returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return at its streams' end")
	}
	// The parser has returned, so it has handed over every mock it records.
	// The bound only turns a mock that never comes into a failure; one more
	// than want, already handed on, is taken too.
	deadline := time.After(10 * time.Second)
collect:
	for len(r.recorded) < want {
		select {
		case m := <-mocks:
			r.recorded = append(r.recorded, m)
		case <-deadline:
			break collect
		}
	}
	select {
	case m := <-mocks:
		r.recorded = append(r.recorded, m)
	default:
	}
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			reason, _ := e.ContextMap()["reason"].(string)
			r.leftOut = append(r.leftOut, reason)
		}
	}
	r.counted = mgr.MocksLeftOut()
	return r
}

// pairCounted runs evs through the exchange state machine with the spans it
// could not record kept in spans, marking the capture holed first when holed
// is set, and returns the mocks it emitted and the manager's count of mocks
// left out.
func pairCounted(t *testing.T, spans *syncMock.Spans, holed bool, evs []chunkEvent) ([]*models.Mock, int64) {
	t.Helper()
	events := make(chan chunkEvent, len(evs))
	for _, ev := range evs {
		events <- ev
	}
	close(events)
	mocks := make(chan *models.Mock, 4*len(evs)+8)
	logger := zaptest.NewLogger(t)
	mgr := syncMock.New(nil)
	sess := &supervisor.Session{Mocks: mocks, Logger: logger, Ctx: context.Background(),
		ClientConnID: "test-client-conn", Mgr: mgr, Orphans: spans}
	if holed {
		sess.MarkMockIncomplete("memory_pressure")
	}
	if err := pairChunkEvents(events, sess, logger); err != nil {
		t.Fatalf("pairChunkEvents returned error: %v", err)
	}
	close(mocks)
	return drainMocks(mocks), mgr.MocksLeftOut()
}

// An answer teed ahead of its request in one chunk has no tail: nothing is
// left out for it.
func TestV2_AnAnswerTeedAheadOfItsRequestInOneChunkLeavesNothingOut(t *testing.T) {
	t.Parallel()

	spans := &syncMock.Spans{}
	got, counted := pairCounted(t, spans, false, []chunkEvent{
		destChunk("+PONG\r\n", 2, time.Unix(1001, 0)),
		clientChunk("PING\r\n", 1, time.Unix(1000, 0)),
		clientChunk("GET k\r\n", 3, time.Unix(1002, 0)),
		destChunk("$1\r\nv\r\n", 4, time.Unix(1003, 0)),
	})

	if len(got) != 2 || counted != 0 {
		t.Fatalf("recorded %d mock(s) and left out %d, want both exchanges and nothing left out", len(got), counted)
	}
	if c, o := spans.Counts(); c != 0 || o != 0 {
		t.Fatalf("the spans hold (closed=%d, open=%d), want none", c, o)
	}
}

// A chunk of the answer before a split, reaching the parser after the request
// the split carried forward, is the tail of that answer: left out over its
// own time, never paired with the carried request, and no longer dropped
// with nothing said.
func TestV2_TheTailOfTheAnswerBeforeASplitIsLeftOut(t *testing.T) {
	t.Parallel()

	tail := time.Unix(1003, 0)
	spans := &syncMock.Spans{}
	got, counted := pairCounted(t, spans, false, []chunkEvent{
		clientChunk("HELLO", 1, time.Unix(1000, 0)),
		clientChunk("PING", 4, time.Unix(1004, 0)),
		destChunk("hello-reply", 2, time.Unix(1002, 0)),
		destChunk("hello-tail", 3, tail),
		destChunk("pong", 5, time.Unix(1005, 0)),
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"HELLO"}, []string{"hello-reply"})
	assertExchange(t, got[1], []string{"PING"}, []string{"pong"})
	if counted != 1 {
		t.Fatalf("the manager counts %d mock(s) left out, want the answer's tail", counted)
	}
	if over, _ := spans.Overlaps(tail, tail); !over {
		t.Fatal("a test case over the tail is not left out")
	}
	if over, _ := spans.Overlaps(tail.Add(time.Millisecond), tail.Add(time.Hour)); over {
		t.Fatal("the span is wider than the tail")
	}
}

// The tail of the answer before a split, reaching the parser after the
// request the split carried forward, is left out from when the relay read it,
// not from when its write returned: the relay stamps a write's return as late
// as its goroutine runs, so under load it can fall after the test that waited
// on the chunk has ended, and that test would be saved as whole.
func TestV2_TheTailOfTheAnswerBeforeASplitSpansFromItsRead(t *testing.T) {
	t.Parallel()

	read := time.Unix(1003, 0)
	written := read.Add(500 * time.Millisecond)
	tail := destChunk("hello-tail", 3, written)
	tail.readAt = read
	spans := &syncMock.Spans{}
	got, counted := pairCounted(t, spans, false, []chunkEvent{
		clientChunk("HELLO", 1, time.Unix(1000, 0)),
		clientChunk("PING", 4, time.Unix(1004, 0)),
		destChunk("hello-reply", 2, time.Unix(1002, 0)),
		tail,
		destChunk("pong", 5, time.Unix(1005, 0)),
	})
	if len(got) != 2 || counted != 1 {
		t.Fatalf("recorded %d mock(s) and left out %d, want both exchanges and the tail", len(got), counted)
	}
	if over, _ := spans.Overlaps(read, read); !over {
		t.Fatal("a test case over the tail's read is not left out: the span starts at its write")
	}
	if over, _ := spans.Overlaps(written, written); !over {
		t.Fatal("a test case over the tail's write is not left out")
	}
}

// endsAsHandedOver is an ended context that reads as live (Err nil) until
// ended is set: set as the session is handed a mock (Session.OnMockRecorded),
// the context ends between emitV2's look at it and EmitMock's own, and the
// session refuses the mock.
type endsAsHandedOver struct {
	context.Context
	ended atomic.Bool
}

func (c *endsAsHandedOver) Err() error {
	if !c.ended.Load() {
		return nil
	}
	return c.Context.Err()
}

// An exchange the session leaves out as the parser hands it on at the stop
// (the channel full, or the mock refused as the context ends) is counted once,
// over its own span: the answer chunks after it, which come after it was
// handed on (a split: the request after it flushes it), are the rest of it.
// Before, its tail was counted as a second mock left out. The exchange after
// it is handed on at the stop and left out for the full channel. Through the
// sync-mock manager neither is refused at the stop
// (TestV2_AnExchangeHandedOnAsTheStopComesIsRecordedWithItsTail).
func TestV2_AnExchangeLeftOutAsItIsHandedOnIsCountedOnceWithItsTail(t *testing.T) {
	for _, c := range []struct {
		name    string
		refused bool
		leftOut int64
	}{
		{name: "channel full at the stop", leftOut: 2},
		{name: "refused at the stop", refused: true, leftOut: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			ended, cancel := context.WithCancel(context.Background())
			cancel()
			mgr := syncMock.New(nil)
			spans := &syncMock.Spans{}
			logger := zaptest.NewLogger(t)
			// No room and no reader: a mock the session hands on would be
			// counted as left out at the stop, and wait on nothing before it.
			sess := &supervisor.Session{Mocks: make(chan *models.Mock), Logger: logger, Ctx: ended,
				ClientConnID: "test-client-conn", Mgr: mgr, Orphans: spans}
			if c.refused {
				live := &endsAsHandedOver{Context: ended}
				sess.Ctx = live
				sess.OnMockRecorded = func(*models.Mock) { live.ended.Store(true) }
			}
			asked := time.Unix(1000, 0)
			evs := []chunkEvent{
				clientChunk("HELLO", 1, asked),
				clientChunk("PING", 4, asked.Add(4*time.Second)),
				destChunk("hello-reply", 2, asked.Add(2*time.Second)),
				destChunk("hello-tail", 3, asked.Add(3*time.Second)),
				destChunk("pong", 5, asked.Add(5*time.Second)),
			}
			events := make(chan chunkEvent, len(evs))
			for _, ev := range evs {
				events <- ev
			}
			close(events)
			if err := pairChunkEvents(events, sess, logger); err != nil {
				t.Fatalf("pairChunkEvents returned error: %v", err)
			}
			if n := mgr.MocksLeftOut(); n != c.leftOut {
				t.Fatalf("the manager counts %d mock(s) left out, want %d: the exchange once, its tail with it", n, c.leftOut)
			}
			if over, _ := spans.Overlaps(asked, asked); !over {
				t.Fatal("a test case over the exchange is not left out")
			}
		})
	}
}

// An exchange the session's manager drops as the parser hands it on (the
// output closed) is counted once, by the manager: the answer chunks after it
// are the rest of it, not a mock left out. Before, the parser could not tell
// the drop, and counted its tail as a second mock lost. The same chunks of an
// exchange the manager takes report that tail, so the count tells the two
// apart. Memory pressure keeps every Generic mock (each derives as the
// session's), so its exchange is one taken.
func TestV2_AnExchangeTheManagerDropsIsCountedOnceWithItsTail(t *testing.T) {
	for _, c := range []struct {
		name               string
		set                func(*syncMock.SyncMockManager)
		leftOut            int64
		pressure, capacity int
		spanAt             time.Duration
	}{
		{"output closed", func(m *syncMock.SyncMockManager) { m.CloseOutChan() }, 0, 0, 2, 0},
		{"taken", func(*syncMock.SyncMockManager) {}, 1, 0, 0, 3 * time.Second},
		{"memory pressure keeps a Generic mock", func(m *syncMock.SyncMockManager) { m.SetMemoryPressure(true) }, 1, 0, 0, 3 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			ended, cancel := context.WithCancel(context.Background())
			cancel()
			mgr := syncMock.New(nil)
			mgr.SetOutputChannel(make(chan *models.Mock, 4))
			c.set(mgr)
			logger := zaptest.NewLogger(t)
			spans := &syncMock.Spans{}
			sess := &supervisor.Session{Logger: logger, Ctx: ended, ClientConnID: "test-client-conn",
				Mgr: mgr, Orphans: spans, RouteMocksViaSyncMock: true}
			asked := time.Now()
			// A split: the request after it flushes the first exchange, so
			// the tail of its answer comes after it was handed on.
			evs := []chunkEvent{
				clientChunk("HELLO", 1, asked),
				clientChunk("PING", 4, asked.Add(4*time.Second)),
				destChunk("hello-reply", 2, asked.Add(2*time.Second)),
				destChunk("hello-tail", 3, asked.Add(3*time.Second)),
				destChunk("pong", 5, asked.Add(5*time.Second)),
			}
			events := make(chan chunkEvent, len(evs))
			for _, ev := range evs {
				events <- ev
			}
			close(events)
			if err := pairChunkEvents(events, sess, logger); err != nil {
				t.Fatalf("pairChunkEvents returned error: %v", err)
			}
			if n := mgr.MocksLeftOut(); n != c.leftOut {
				t.Fatalf("%d mock(s) left out, want %d", n, c.leftOut)
			}
			// Not counted is not forgotten. An exchange dropped is spanned
			// over its own times, as one left out is, its tail with it; of one
			// taken, only the tail is missing, so only the tail is spanned.
			at := asked.Add(c.spanAt)
			if over, _ := spans.Overlaps(at, at); !over {
				t.Fatalf("a test case at %v into the exchange is not left out", c.spanAt)
			}
			_, pressure, _, _ := mgr.GetDropStats()
			if pressure != int64(c.pressure) || mgr.DropCount() != uint64(c.capacity) {
				t.Fatalf("pressure drops %d, capacity drops %d; want %d, %d", pressure, mgr.DropCount(), c.pressure, c.capacity)
			}
		})
	}
}

// Through the sync-mock manager, as the agent records, an exchange handed on
// as the stop comes (the context ending in its post-record hook, after
// emitV2's look at it and before EmitMock's own) is recorded whole, its
// answer's tail with it, and nothing is left out: it was captured before the
// stop. Before, EmitMock refused it, and it was left out with its tail.
func TestV2_AnExchangeHandedOnAsTheStopComesIsRecordedWithItsTail(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	live := &endsAsHandedOver{Context: ended}
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)
	spans := &syncMock.Spans{}
	logger := zaptest.NewLogger(t)
	// No room and no reader: a mock handed on here instead of through the
	// manager would be left out.
	sess := &supervisor.Session{Mocks: make(chan *models.Mock), Logger: logger, Ctx: live,
		ClientConnID: "test-client-conn", Mgr: mgr, Orphans: spans, RouteMocksViaSyncMock: true,
		OnMockRecorded: func(*models.Mock) { live.ended.Store(true) }}
	asked := time.Unix(1000, 0)
	evs := []chunkEvent{
		clientChunk("q0", 1, asked),
		destChunk("r0", 2, time.Unix(1001, 0)),
		destChunk("r0-tail", 3, time.Unix(1002, 0)),
	}
	events := make(chan chunkEvent, len(evs))
	for _, ev := range evs {
		events <- ev
	}
	close(events)
	if err := pairChunkEvents(events, sess, logger); err != nil {
		t.Fatalf("pairChunkEvents returned error: %v", err)
	}
	if n := mgr.MocksLeftOut(); n != 0 {
		t.Fatalf("the manager counts %d mock(s) left out, want none", n)
	}
	if over, _ := spans.Overlaps(asked, asked); over {
		t.Fatal("a test case over the exchange is left out")
	}
	select {
	case m := <-out:
		assertExchange(t, m, []string{"q0"}, []string{"r0", "r0-tail"})
	case <-time.After(10 * time.Second):
		t.Fatal("the exchange handed on as the stop came was not recorded")
	}
	select {
	case m := <-out:
		t.Fatalf("recorded a second mock %q, want the one exchange", m.Name)
	default:
	}
}

// pairLeavingOutTheFirst runs evs through the exchange state machine with the
// first exchange it records left out: for the capture's incomplete flag as it
// is flushed (holed), or else at the stop, as it is handed on to a channel that
// is full, the channel then emptied for the exchanges after it. It returns the
// mocks recorded, the reason of each mock left out, and the manager's count of
// mocks left out.
func pairLeavingOutTheFirst(t *testing.T, spans *syncMock.Spans, holed bool, evs []chunkEvent) ([]*models.Mock, []string, int64) {
	t.Helper()
	events := make(chan chunkEvent, len(evs))
	for _, ev := range evs {
		events <- ev
	}
	close(events)
	mocks := make(chan *models.Mock, len(evs))
	core, logs := observer.New(zapcore.DebugLevel)
	mgr := syncMock.New(nil)
	sess := &supervisor.Session{Mocks: mocks, Logger: zap.New(core), Ctx: context.Background(),
		ClientConnID: "test-client-conn", Mgr: mgr, Orphans: spans}
	if holed {
		sess.MarkMockIncomplete("memory_pressure")
	} else {
		ended, cancel := context.WithCancel(context.Background())
		cancel()
		sess.Ctx = ended
		for len(mocks) < cap(mocks) {
			mocks <- &models.Mock{Name: "placeholder"}
		}
		// The session calls the hook as it hands a mock on, before it sends
		// it: the first finds the channel full, the second an empty one.
		handed := 0
		sess.OnMockRecorded = func(*models.Mock) {
			if handed++; handed == 2 {
				for len(mocks) > 0 {
					<-mocks
				}
			}
		}
	}
	if err := pairChunkEvents(events, sess, zap.New(core)); err != nil {
		t.Fatalf("pairChunkEvents returned error: %v", err)
	}
	close(mocks)
	var reasons []string
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			reason, _ := e.ContextMap()["reason"].(string)
			reasons = append(reasons, reason)
		}
	}
	return drainMocks(mocks), reasons, mgr.MocksLeftOut()
}

// An exchange left out before the tail of its answer reaches the parser is
// counted once, with its tail, for an answer teed ahead of its request and for
// the answer before a split, whose tail comes after the request the split
// carried forward: the tail is the rest of the exchange left out. Counted
// again, one exchange would count as two mocks left out. The exchange is left
// out as it is flushed (the capture's incomplete flag) or as it is handed on
// at the stop (the channel full). A split whose flush is holed leaves out the
// half it carries too (TestV2_TheHalfASplitCarriesOutOfAHoledExchangeIsReported),
// so only the full channel leaves out the exchange before a split and records
// the next.
func TestV2_TheTailOfAnExchangeLeftOutIsCountedWithIt(t *testing.T) {
	asked := time.Unix(1000, 0)
	answerAhead := []chunkEvent{
		destChunk("head", 2, time.Unix(1001, 0)),
		destChunk("tail", 3, time.Unix(1002, 0)),
		clientChunk("GET big\r\n", 1, asked),
		clientChunk("GET small\r\n", 4, time.Unix(1003, 0)),
		destChunk("+OK\r\n", 5, time.Unix(1004, 0)),
	}
	split := []chunkEvent{
		clientChunk("HELLO", 1, asked),
		clientChunk("PING", 4, time.Unix(1004, 0)),
		destChunk("hello-reply", 2, time.Unix(1002, 0)),
		destChunk("hello-tail", 3, time.Unix(1003, 0)),
		destChunk("pong", 5, time.Unix(1005, 0)),
	}
	for _, c := range []struct {
		name            string
		holed           bool
		evs             []chunkEvent
		reason          string
		wantReq, wantRe string
	}{
		{"answer teed ahead/holed", true, answerAhead, "memory_pressure", "GET small\r\n", "+OK\r\n"},
		{"answer teed ahead/channel full at the stop", false, answerAhead, supervisor.LeftOutChannelFullAtStop,
			"GET small\r\n", "+OK\r\n"},
		{"split/channel full at the stop", false, split, supervisor.LeftOutChannelFullAtStop, "PING", "pong"},
	} {
		t.Run(c.name, func(t *testing.T) {
			spans := &syncMock.Spans{}
			got, reasons, counted := pairLeavingOutTheFirst(t, spans, c.holed, c.evs)
			if len(got) != 1 {
				t.Fatalf("recorded %d mock(s), want the second exchange alone", len(got))
			}
			assertExchange(t, got[0], []string{c.wantReq}, []string{c.wantRe})
			if counted != 1 || len(reasons) != 1 || reasons[0] != c.reason {
				t.Fatalf("left out %q (the manager counts %d), want the first exchange once, for %q, its tail with it",
					reasons, counted, c.reason)
			}
			if closed, open := spans.Counts(); closed != 1 || open != 0 {
				t.Fatalf("the spans hold (closed=%d, open=%d), want the exchange's alone", closed, open)
			}
			if over, _ := spans.Overlaps(asked, asked); !over {
				t.Fatal("a test case over the exchange left out is not left out")
			}
		})
	}
}

// Chunks after an exchange left out are the rest of it, reported with it. The
// next exchange the parser records is its own, recorded whole: the tail of
// its answer is held with it, and no test case over it is left out.
func TestV2_TheExchangeAfterOneLeftOutIsRecordedWhole(t *testing.T) {
	t.Parallel()

	spans := &syncMock.Spans{}
	got, counted := pairCounted(t, spans, true, []chunkEvent{
		clientChunk("q0", 1, time.Unix(1000, 0)),
		destChunk("r0", 2, time.Unix(1001, 0)),
		destChunk("r0-tail", 3, time.Unix(1002, 0)),
		clientChunk("q1", 4, time.Unix(1003, 0)),
		destChunk("r1", 5, time.Unix(1004, 0)),
		destChunk("r1-tail", 6, time.Unix(1005, 0)),
	})

	if len(got) != 1 {
		t.Fatalf("expected the second exchange alone, got %d mock(s)", len(got))
	}
	assertExchange(t, got[0], []string{"q1"}, []string{"r1", "r1-tail"})
	if counted != 1 {
		t.Fatalf("the manager counts %d mock(s) left out, want the holed exchange once, its tail with it", counted)
	}
	if over, _ := spans.Overlaps(time.Unix(1002, 0), time.Unix(1002, 0)); !over {
		t.Fatal("a test case over the holed exchange's tail is not left out")
	}
	if over, _ := spans.Overlaps(time.Unix(1003, 0), time.Unix(1005, 0)); over {
		t.Fatal("a test case over the second exchange, recorded whole, is left out")
	}
}
