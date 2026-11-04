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
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	if err := dispatch(t, sess); err != nil {
		t.Fatalf("the parser returned %v", err)
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

// The chunks of the rows below: a request, and an answer the server sent in
// two chunks (head, then tail), read off the destination's socket and written
// on, then the next request and its one-chunk answer.
var (
	tailReq0At   = time.Unix(7000, 0)
	tailHeadRead = time.Unix(7001, 0)
	tailHeadSent = tailHeadRead.Add(100 * time.Millisecond)
	tailRead     = time.Unix(7002, 0)
	tailSent     = tailRead.Add(100 * time.Millisecond)
	tailReq1At   = time.Unix(7003, 0)
	tailResp1At  = time.Unix(7004, 0)
)

func tailChunks(withNext bool) []fakeconn.Chunk {
	chunks := []fakeconn.Chunk{
		{Dir: fakeconn.FromClient, Bytes: []byte("GET big\r\n"), ReadAt: tailReq0At, ConnSeq: 1},
		{Dir: fakeconn.FromDest, Bytes: []byte("head"), ReadAt: tailHeadRead, WrittenAt: tailHeadSent, ConnSeq: 2},
		{Dir: fakeconn.FromDest, Bytes: []byte("tail"), ReadAt: tailRead, WrittenAt: tailSent, ConnSeq: 3},
	}
	if withNext {
		chunks = append(chunks,
			fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("GET small\r\n"), ReadAt: tailReq1At, ConnSeq: 4},
			fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("+OK\r\n"), ReadAt: tailResp1At, WrittenAt: tailResp1At, ConnSeq: 5})
	}
	return chunks
}

// checkTailLeftOut fails t unless r left out exactly one mock, the answer's
// tail, for leftOutAnswerTail, counted, over the tail's own times: from when
// the relay read it to when its write returned.
func checkTailLeftOut(t *testing.T, r tailRun) {
	t.Helper()
	if len(r.leftOut) != 1 || r.leftOut[0] != leftOutAnswerTail || r.counted != 1 {
		t.Fatalf("left out %q (the manager counts %d), want the answer's tail alone, for %q",
			r.leftOut, r.counted, leftOutAnswerTail)
	}
	if c, o := r.spans.Counts(); c != 1 || o != 0 {
		t.Fatalf("the spans hold (closed=%d, open=%d), want the tail's", c, o)
	}
	if over, _ := r.spans.Overlaps(tailRead, tailRead); !over {
		t.Fatal("a test case over the tail's read is not left out")
	}
	if over, _ := r.spans.Overlaps(tailSent, tailSent); !over {
		t.Fatal("a test case over the tail's write is not left out")
	}
	if over, _ := r.spans.Overlaps(tailReq0At, tailRead.Add(-time.Millisecond)); over {
		t.Fatal("the span starts before the relay read the tail: it is wider than the chunks left out")
	}
	if over, _ := r.spans.Overlaps(tailSent.Add(time.Millisecond), tailResp1At.Add(time.Second)); over {
		t.Fatal("the span ends after the tail's write returned: it is wider than the chunks left out")
	}
}

// A server's answer in more than one chunk (over the relay's 32 KiB read, or
// written in parts) is recorded as its first chunk, as before, and the part
// after it is left out with its span, so the test case that waited on the
// whole answer is left out instead of saved with an answer cut short. Before,
// the later chunks were dropped with nothing left out, counted or said, on
// both deliveries and under both switches. The tail here comes before the next
// request.
func TestV2_AnAnswersLaterChunksAreLeftOutBeforeTheNextRequest(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		for _, viaSyncMock := range []bool{false, true} {
			t.Run("proven="+proven+"/viaSyncMock="+map[bool]string{false: "no", true: "yes"}[viaSyncMock], func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				r := recordChunks(t, tailChunks(true), viaSyncMock, 2)
				if len(r.recorded) != 2 {
					t.Fatalf("recorded %d mocks, want both exchanges", len(r.recorded))
				}
				assertExchange(t, r.recorded[0], []string{"GET big\r\n"}, []string{"head"})
				assertExchange(t, r.recorded[1], []string{"GET small\r\n"}, []string{"+OK\r\n"})
				checkTailLeftOut(t, r)
			})
		}
	}
}

// The same when the connection ends after the tail: the streams' end is where
// the parser learns no request follows it.
func TestV2_AnAnswersLaterChunksAreLeftOutAtTheStreamsEnd(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		for _, viaSyncMock := range []bool{false, true} {
			t.Run("proven="+proven+"/viaSyncMock="+map[bool]string{false: "no", true: "yes"}[viaSyncMock], func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				r := recordChunks(t, tailChunks(false), viaSyncMock, 1)
				if len(r.recorded) != 1 {
					t.Fatalf("recorded %d mocks, want the exchange", len(r.recorded))
				}
				assertExchange(t, r.recorded[0], []string{"GET big\r\n"}, []string{"head"})
				checkTailLeftOut(t, r)
			})
		}
	}
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

// An answer teed ahead of its request in more than one chunk is paired with
// the request by its first chunk, and the rest of it is left out, from that
// chunk to the last one's write. Before, the rest was dropped with nothing
// said. A chunk with no read time spans from its write.
func TestV2_TheTailOfAnAnswerTeedAheadOfItsRequestIsLeftOut(t *testing.T) {
	t.Parallel()

	var (
		head = time.Unix(1001, 0)
		tail = time.Unix(1002, 0)
	)
	spans := &syncMock.Spans{}
	got, counted := pairCounted(t, spans, false, []chunkEvent{
		destChunk("head", 2, head),
		destChunk("tail", 3, tail),
		clientChunk("GET big\r\n", 1, time.Unix(1000, 0)),
		clientChunk("GET small\r\n", 4, time.Unix(1003, 0)),
		destChunk("+OK\r\n", 5, time.Unix(1004, 0)),
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"GET big\r\n"}, []string{"head"})
	assertExchange(t, got[1], []string{"GET small\r\n"}, []string{"+OK\r\n"})
	if counted != 1 {
		t.Fatalf("the manager counts %d mock(s) left out, want the answer's tail", counted)
	}
	if over, _ := spans.Overlaps(tail, tail); !over {
		t.Fatal("a test case over the tail is not left out")
	}
	if over, _ := spans.Overlaps(tail.Add(time.Millisecond), tail.Add(time.Hour)); over {
		t.Fatal("the span ends after the tail's write returned")
	}
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
// over its own span: the answer chunks after it are the rest of it. Before,
// its tail was counted as a second mock left out.
func TestV2_AnExchangeLeftOutAsItIsHandedOnIsCountedOnceWithItsTail(t *testing.T) {
	for _, c := range []struct {
		name                 string
		refused, viaSyncMock bool
	}{
		{name: "channel full at the stop"},
		{name: "refused at the stop", refused: true},
		{name: "refused at the stop/viaSyncMock", refused: true, viaSyncMock: true},
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
				ClientConnID: "test-client-conn", Mgr: mgr, Orphans: spans, RouteMocksViaSyncMock: c.viaSyncMock}
			if c.refused {
				live := &endsAsHandedOver{Context: ended}
				sess.Ctx = live
				sess.OnMockRecorded = func(*models.Mock) { live.ended.Store(true) }
			}
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
			if n := mgr.MocksLeftOut(); n != 1 {
				t.Fatalf("the manager counts %d mock(s) left out, want the exchange once, its tail with it", n)
			}
			if over, _ := spans.Overlaps(asked, asked); !over {
				t.Fatal("a test case over the exchange is not left out")
			}
		})
	}
}

// Chunks after an exchange left out are the rest of it, reported with it. The
// next exchange the parser records is its own: the tail of its answer is left
// out, as on any connection.
func TestV2_TheTailOfAnExchangeAfterOneLeftOutIsLeftOut(t *testing.T) {
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
	assertExchange(t, got[0], []string{"q1"}, []string{"r1"})
	if counted != 2 {
		t.Fatalf("the manager counts %d mock(s) left out, want the holed exchange (its tail with it) and the second one's tail", counted)
	}
	if over, _ := spans.Overlaps(time.Unix(1005, 0), time.Unix(1005, 0)); !over {
		t.Fatal("a test case over the second answer's tail is not left out")
	}
}
