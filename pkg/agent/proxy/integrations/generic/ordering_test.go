package generic

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// The relay pushes a chunk to the parser AFTER it has written those bytes
// to the opposite socket (relay.go: dst.Write, then t.push). For a
// response that means the application already has the reply — and can
// already have sent its next request — by the time the response chunk is
// queued for the parser. The two directions are then drained by separate
// goroutines, so the parser really does see the next REQUEST before the
// RESPONSE that preceded it on the wire.
//
// These tests drive that order deliberately. A chunk's capture number
// (fakeconn.Chunk.ConnSeq, given to each chunk here as the relay would
// number it) is the wire truth: the relay numbers a request after every
// byte the destination had sent when it read it, so a response numbered
// below a request was sent before the destination could have had it. The
// pairing rules are stated against those numbers, never against the order
// the events happen to arrive in. The chunks' times are the mocks' windows,
// and order only chunks the numbers do not tell apart (a producer that
// gives both directions one number).

// runPairing feeds evs through the exchange state machine in exactly the
// given order and returns the mocks it emitted.
func runPairing(t *testing.T, evs []chunkEvent) []*models.Mock {
	t.Helper()
	return runPairingOn(t, evs, false)
}

// runPairingHoled is runPairing on a capture the relay has already
// flagged as missing bytes.
func runPairingHoled(t *testing.T, evs []chunkEvent) []*models.Mock {
	t.Helper()
	return runPairingOn(t, evs, true)
}

func runPairingOn(t *testing.T, evs []chunkEvent, incomplete bool) []*models.Mock {
	t.Helper()
	events := make(chan chunkEvent, len(evs))
	for _, ev := range evs {
		events <- ev
	}
	close(events)

	// Sized well past any plausible emission count: EmitMock's direct
	// channel path selects on sess.Ctx, so an implementation that emits
	// more mocks than fit would BLOCK here and hang the test instead of
	// failing it with a count mismatch.
	mocks := make(chan *models.Mock, 4*len(evs)+8)
	logger := zaptest.NewLogger(t)
	sess := &supervisor.Session{
		Mocks:        mocks,
		Logger:       logger,
		Ctx:          context.Background(),
		ClientConnID: "test-client-conn",
	}
	if incomplete {
		sess.MarkMockIncomplete("test: relay dropped a chunk")
	}
	if err := pairChunkEvents(events, sess, logger); err != nil {
		t.Fatalf("pairChunkEvents returned error: %v", err)
	}
	close(mocks)
	return drainMocks(mocks)
}

// clientChunk / destChunk name the two directions the way the relay does.
// seq is the chunk's capture number (0: unnumbered), the order the rules
// read. readAt is when the relay read a request off the client socket;
// writtenAt is when its write of a response TO that socket returned: the
// mocks' times.
func clientChunk(b string, seq uint32, readAt time.Time) chunkEvent {
	return chunkEvent{dir: fakeconn.FromClient, bytes: []byte(b), connSeq: seq, readAt: readAt}
}

func destChunk(b string, seq uint32, writtenAt time.Time) chunkEvent {
	return chunkEvent{dir: fakeconn.FromDest, bytes: []byte(b), connSeq: seq, writtenAt: writtenAt}
}

// payloads flattens a mock's request (or response) buffers to strings so
// assertions read like the wire.
func payloads(ps []models.Payload) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if len(p.Message) == 0 {
			out = append(out, "<no message>")
			continue
		}
		out = append(out, p.Message[0].Data)
	}
	return out
}

func assertExchange(t *testing.T, m *models.Mock, wantReqs, wantResps []string) {
	t.Helper()
	if got := payloads(m.Spec.GenericRequests); !equalStrings(got, wantReqs) {
		t.Errorf("requests = %q, want %q", got, wantReqs)
	}
	if got := payloads(m.Spec.GenericResponses); !equalStrings(got, wantResps) {
		t.Errorf("responses = %q, want %q", got, wantResps)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestV2_LateResponsePushKeepsExchangesApart is the recorded failure from
// CI's mock-mismatch-streaming lane, reduced to its chunks.
//
// go-redis opens its pool connection with HELLO 3, waits for the reply,
// then sends PING. The reply's chunk reached the parser after PING's, so
// both commands landed in ONE mock and the PONG was dropped: the test set
// came back with 10 generic mocks instead of 11, none of them a lone
// 22-byte HELLO. On replay the app's own HELLO then matched nothing
// (findExactMatch requires equal buffer counts), the proxy closed the
// connection, and the sample died at startup with "cannot reach Redis:
// EOF" — every test case in the set lost, reported as a keploy failure.
func TestV2_LateResponsePushKeepsExchangesApart(t *testing.T) {
	t.Parallel()

	var (
		helloRead   = time.Unix(1000, 0)
		helloReply  = time.Unix(1001, 0)
		pingRead    = time.Unix(1002, 0)
		pongWritten = time.Unix(1003, 0)
	)

	got := runPairing(t, []chunkEvent{
		clientChunk("*1\r\n$5\r\nhello\r\n", 1, helloRead),
		clientChunk("*1\r\n$4\r\nping\r\n", 3, pingRead),
		destChunk("%7\r\n$6\r\nserver\r\n", 2, helloReply),
		destChunk("+PONG\r\n", 4, pongWritten),
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks (one per command), got %d", len(got))
	}
	assertExchange(t, got[0], []string{"*1\r\n$5\r\nhello\r\n"}, []string{"%7\r\n$6\r\nserver\r\n"})
	assertExchange(t, got[1], []string{"*1\r\n$4\r\nping\r\n"}, []string{"+PONG\r\n"})

	if !got[0].Spec.ReqTimestampMock.Equal(helloRead) {
		t.Errorf("first ReqTimestampMock = %v, want %v", got[0].Spec.ReqTimestampMock, helloRead)
	}
	if !got[0].Spec.ResTimestampMock.Equal(helloReply) {
		t.Errorf("first ResTimestampMock = %v, want %v", got[0].Spec.ResTimestampMock, helloReply)
	}
	if !got[1].Spec.ReqTimestampMock.Equal(pingRead) {
		t.Errorf("second ReqTimestampMock = %v, want %v", got[1].Spec.ReqTimestampMock, pingRead)
	}
	if !got[1].Spec.ResTimestampMock.Equal(pongWritten) {
		t.Errorf("second ResTimestampMock = %v, want %v", got[1].Spec.ResTimestampMock, pongWritten)
	}
}

// A client that really does pipeline — both commands written before the
// first reply came back — must still be recorded as ONE exchange with two
// request buffers, because that is what replay will see it send. The only
// difference from the test above is which side of the reply's arrival at
// the proxy the second request was read on.
func TestV2_PipelinedRequestsStayInOneExchange(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("MULTI\r\n", 1, time.Unix(1000, 0)),
		clientChunk("EXEC\r\n", 2, time.Unix(1001, 0)),
		destChunk("+OK\r\n", 3, time.Unix(1002, 0)),
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"MULTI\r\n", "EXEC\r\n"}, []string{"+OK\r\n"})
}

// A response split across chunks keeps its legacy shape: the head chunk
// is the mock's response and the tail chunks are dropped. The next
// request was read after every one of them, so nothing here looks like a
// late push and the split rule must stay out of it.
func TestV2_MultiChunkResponseStillDropsItsTail(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("GET big\r\n", 1, time.Unix(1000, 0)),
		destChunk("head", 2, time.Unix(1001, 0)),
		destChunk("tail", 3, time.Unix(1002, 0)),
		clientChunk("GET small\r\n", 4, time.Unix(1003, 0)),
		destChunk("+OK\r\n", 5, time.Unix(1004, 0)),
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"GET big\r\n"}, []string{"head"})
	assertExchange(t, got[1], []string{"GET small\r\n"}, []string{"+OK\r\n"})
}

// The mirror image: the relay can be descheduled between forwarding a
// request and queueing its chunk, so a response can reach the parser
// first. The request was captured BEFORE that response, which is what
// says they belong together — without that test
// the response is treated as an orphan and thrown away, and the request
// then pairs with the NEXT response, i.e. a mock that replays one
// dependency's answer for another's question.
func TestV2_LateRequestPushStillPairsWithItsResponse(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		destChunk("+PONG\r\n", 2, time.Unix(1001, 0)),
		clientChunk("PING\r\n", 1, time.Unix(1000, 0)),
		clientChunk("GET k\r\n", 3, time.Unix(1002, 0)),
		destChunk("$1\r\nv\r\n", 4, time.Unix(1003, 0)),
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"PING\r\n"}, []string{"+PONG\r\n"})
	assertExchange(t, got[1], []string{"GET k\r\n"}, []string{"$1\r\nv\r\n"})
}

// Chunks with neither a capture number nor a timestamp carry no ordering
// evidence, so the parser must fall back to arrival order rather than
// invent a split. The relay numbers and stamps every chunk.
func TestV2_UnorderedChunksKeepArrivalOrder(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("A", 0, time.Time{}),
		clientChunk("B", 0, time.Time{}),
		destChunk("R", 0, time.Time{}),
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"A", "B"}, []string{"R"})
}

// What the rules do NOT claim.
//
// The parser pairs by direction transitions, so bytes the server sent on
// its own — an unsolicited push, the tail of a reply whose head was
// already emitted — are attributed to whatever request precedes them.
// That is wrong, and it is wrong BEFORE any of this: the in-order stream
// produces exactly the same attribution. These tests pin that equality,
// because it is the honest scope of the fix. Arrival order stops
// mattering; understanding an opaque protocol still needs a parser that
// frames it.
func TestV2_ServerPushIsPairedTheSameEitherWay(t *testing.T) {
	t.Parallel()

	var (
		req0 = time.Unix(1000, 0)
		push = time.Unix(1001, 0)
		req1 = time.Unix(1002, 0)
		d0   = time.Unix(1003, 0)
		d1   = time.Unix(1004, 0)
	)

	inOrder := runPairing(t, []chunkEvent{
		clientChunk("REQ0", 1, req0),
		destChunk("PUSH", 2, push),
		clientChunk("REQ1", 3, req1),
		destChunk("D0", 4, d0),
		destChunk("D1", 5, d1),
	})
	teedLate := runPairing(t, []chunkEvent{
		clientChunk("REQ0", 1, req0),
		clientChunk("REQ1", 3, req1),
		destChunk("PUSH", 2, push),
		destChunk("D0", 4, d0),
		destChunk("D1", 5, d1),
	})

	if len(inOrder) != 2 || len(teedLate) != 2 {
		t.Fatalf("mock counts differ: in-order %d, teed-late %d", len(inOrder), len(teedLate))
	}
	for i := range inOrder {
		assertExchange(t, teedLate[i], payloads(inOrder[i].Spec.GenericRequests), payloads(inOrder[i].Spec.GenericResponses))
	}
	// And state it outright, so a future reader sees what is being
	// accepted rather than inferring it from an equality.
	assertExchange(t, teedLate[0], []string{"REQ0"}, []string{"PUSH"})
	assertExchange(t, teedLate[1], []string{"REQ1"}, []string{"D0"})
}

// A response captured before even the FIRST request held cannot be that
// exchange's answer, so it is not evidence of where the exchange ends
// and the rule stays out of it. The bytes are still mis-paired — they
// were mis-paired before the rule existed, because an opaque parser has
// nowhere else to put a chunk the server sent unbidden — but the
// recording is left exactly as the un-split parser wrote it instead of
// being cut on a contradiction.
func TestV2_ResponseOlderThanEveryRequestDoesNotSplit(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("REQ0", 2, time.Unix(1000, 0)),
		clientChunk("REQ1", 3, time.Unix(1002, 0)),
		destChunk("STALE", 1, time.Unix(999, 0)),
		destChunk("D1", 4, time.Unix(1003, 0)),
	})

	if len(got) != 1 {
		t.Fatalf("expected the exchange to be left whole (1 mock), got %d", len(got))
	}
	assertExchange(t, got[0], []string{"REQ0", "REQ1"}, []string{"STALE"})
}

// One late response per exchange is the common case; three in a row is
// the same rule applied three times. The last request is never answered,
// so it is carried and never emitted — one exchange lost, not four
// mis-paired.
func TestV2_SplitsCascadeAcrossAWholeBurst(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("A", 1, time.Unix(1000, 0)),
		clientChunk("B", 3, time.Unix(1002, 0)),
		clientChunk("C", 5, time.Unix(1004, 0)),
		clientChunk("D", 7, time.Unix(1006, 0)),
		destChunk("a", 2, time.Unix(1001, 0)),
		destChunk("b", 4, time.Unix(1003, 0)),
		destChunk("c", 6, time.Unix(1005, 0)),
	})

	if len(got) != 3 {
		t.Fatalf("expected 3 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"A"}, []string{"a"})
	assertExchange(t, got[1], []string{"B"}, []string{"b"})
	assertExchange(t, got[2], []string{"C"}, []string{"c"})
}

// A request buffer with neither a capture number nor a timestamp cannot be
// placed on either side of the response, so the scan stops there rather
// than guessing. The exchange stays whole — the same mock the parser
// produced before any of this existed.
func TestV2_SplitStopsAtAnUnorderedRequest(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("A", 1, time.Unix(1000, 0)),
		clientChunk("B", 0, time.Time{}),
		clientChunk("C", 4, time.Unix(1004, 0)),
		destChunk("a", 2, time.Unix(1001, 0)),
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"A", "B", "C"}, []string{"a"})
}

// A capture that has already lost a chunk drops its in-flight exchange
// whole. Splitting first would keep the tail half of a recording known
// to be holed — a mock that looks well-formed and replays a hole.
func TestV2_IncompleteCaptureIsNotSplitIntoASurvivor(t *testing.T) {
	t.Parallel()

	got := runPairingHoled(t, []chunkEvent{
		clientChunk("HELLO", 1, time.Unix(1000, 0)),
		clientChunk("PING", 3, time.Unix(1002, 0)),
		destChunk("hello-reply", 2, time.Unix(1001, 0)),
		// PING's own reply: without the guard the split hands the
		// second half of the holed exchange to this chunk and emits a
		// mock that looks perfectly well-formed.
		destChunk("pong", 4, time.Unix(1003, 0)),
	})

	if len(got) != 0 {
		t.Fatalf("expected the holed exchange to be dropped, got %d mock(s): %q",
			len(got), payloads(got[0].Spec.GenericRequests))
	}
}

// A reply the server splits across chunks lands as its head alone —
// that is the shape every generic mock has, and replay writes every
// response buffer in a mock back to the client. So when a request is
// teed in behind a reply that has already delivered two chunks, the
// pairing takes the head and drops the tail, which is exactly what the
// same bytes in wire order produce. Carrying both in would make the
// replayed bytes depend on how the relay's goroutines were scheduled
// while recording.
func TestV2_AdoptTakesTheHeadChunkOnlyLikeWireOrder(t *testing.T) {
	t.Parallel()

	var (
		reqRead  = time.Unix(1000, 0)
		headSeen = time.Unix(1001, 0)
		tailSeen = time.Unix(1002, 0)
	)

	inOrder := runPairing(t, []chunkEvent{
		clientChunk("GET big", 1, reqRead),
		destChunk("head", 2, headSeen),
		destChunk("tail", 3, tailSeen),
	})
	teedLate := runPairing(t, []chunkEvent{
		destChunk("head", 2, headSeen),
		destChunk("tail", 3, tailSeen),
		clientChunk("GET big", 1, reqRead),
	})

	if len(inOrder) != 1 {
		t.Fatalf("wire order: expected 1 mock, got %d", len(inOrder))
	}
	if len(teedLate) != 1 {
		t.Fatalf("request teed late: expected 1 mock, got %d", len(teedLate))
	}
	assertExchange(t, inOrder[0], []string{"GET big"}, []string{"head"})
	assertExchange(t, teedLate[0], []string{"GET big"}, []string{"head"})
	if !teedLate[0].Spec.ResTimestampMock.Equal(headSeen) {
		t.Errorf("ResTimestampMock = %v, want the head chunk's %v",
			teedLate[0].Spec.ResTimestampMock, headSeen)
	}
}

// A request with neither a capture number nor a timestamp says nothing
// about what it answers: 0 compared as a serial number is before half of
// all numbers, and the zero time before every real instant, so without an
// explicit guard it would adopt a response held, on no evidence at all.
func TestV2_UnorderedRequestDoesNotAdoptAHeldResponse(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		destChunk("orphan", 2, time.Unix(1001, 0)),
		clientChunk("REQ", 0, time.Time{}),
		destChunk("answer", 3, time.Unix(1002, 0)),
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 mock, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"REQ"}, []string{"answer"})
}

// The half a split carries forward is an exchange this parser invented,
// and it owes the same precondition the split is held to. Here the tail
// of the first reply is teed after the second request: pairing them
// records an answer that request never got, and nothing downstream
// catches it — generic mocks are session-scoped, so replay's window
// filter takes them before it would compare their timestamps. Wire
// order drops the tail as an orphan; so does this.
func TestV2_CarriedHalfDoesNotPairWithAnOlderTail(t *testing.T) {
	t.Parallel()

	var (
		first    = time.Unix(1000, 0)
		headSeen = time.Unix(1001, 0)
		tailSeen = time.Unix(1002, 0)
		second   = time.Unix(1003, 0)
	)

	inOrder := runPairing(t, []chunkEvent{
		clientChunk("A", 1, first),
		destChunk("a-head", 2, headSeen),
		destChunk("a-tail", 3, tailSeen),
		clientChunk("B", 4, second),
	})
	teedLate := runPairing(t, []chunkEvent{
		clientChunk("A", 1, first),
		clientChunk("B", 4, second),
		destChunk("a-head", 2, headSeen),
		destChunk("a-tail", 3, tailSeen),
	})

	if len(inOrder) != 1 || len(teedLate) != 1 {
		t.Fatalf("mock counts differ: wire order %d, teed late %d", len(inOrder), len(teedLate))
	}
	assertExchange(t, teedLate[0], []string{"A"}, []string{"a-head"})
	for _, m := range teedLate {
		if m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
			t.Errorf("emitted a mock whose response (%v) precedes its request (%v)",
				m.Spec.ResTimestampMock, m.Spec.ReqTimestampMock)
		}
	}
}

// Where the capture numbers tell two chunks apart, their times order
// nothing, whatever they say: two chunks stamped in the same instant, or a
// response stamped after the request it unblocked, are ordered by their
// numbers. The relay stamps a chunk on the goroutine that forwards it,
// after its read or its write returns, and that goroutine can lose its CPU
// before it stamps.
func TestV2_TimesDoNotOrderTheDirections(t *testing.T) {
	t.Parallel()

	same := time.Unix(1000, 0)

	split := runPairing(t, []chunkEvent{
		clientChunk("A", 1, same),
		clientChunk("B", 3, same),
		destChunk("r", 2, same),
	})
	if len(split) != 1 {
		t.Fatalf("split: expected 1 mock, got %d", len(split))
	}
	assertExchange(t, split[0], []string{"A"}, []string{"r"})

	adopt := runPairing(t, []chunkEvent{
		destChunk("r", 2, same),
		clientChunk("A", 1, same),
		destChunk("r2", 3, same),
	})
	if len(adopt) != 1 {
		t.Fatalf("adopt: expected 1 mock, got %d", len(adopt))
	}
	assertExchange(t, adopt[0], []string{"A"}, []string{"r"})
}

// TestV2_AReplyStampedAfterTheNextRequestStillEndsItsExchange is CI's
// mock-mismatch-streaming lane again (run 39725971345), with the stamps the
// relay took. go-redis opens its pool connection with HELLO 3, then CLIENT
// SETINFO twice in one write, then PING, each after the reply before it. The
// relay wrote the two OKs on to the app, and the app sent PING, which the
// other forwarder read, before the first one stamped its write: the OKs'
// WrittenAt is after PING's ReadAt, and their chunk was teed after PING's.
// Read by those times, SETINFO and PING were one exchange: one mock with two
// request buffers, the PONG dropped, 10 mocks where the app made 11 calls.
// On replay the app's lone SETINFO matched nothing (findExactMatch requires
// equal buffer counts), the proxy closed the connection, and the sample died
// at startup ("cannot reach Redis: EOF"). The OKs were captured before PING
// (the relay numbers PING after every byte Redis had sent when it read it),
// and that says they are SETINFO's answer and PING is an exchange of its own.
// The numbers are the ones the relay gives this exchange (connseq.Upstream:
// a client chunk read after the destination's bytes reserves a number below
// itself for them, so the clients' are 1, 4 and 7).
func TestV2_AReplyStampedAfterTheNextRequestStillEndsItsExchange(t *testing.T) {
	t.Parallel()

	base := time.Unix(1000, 0)
	at := func(us int) time.Time { return base.Add(time.Duration(us) * time.Microsecond) }
	const (
		hello      = "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n"
		helloReply = "%7\r\n$6\r\nserver\r\n$5\r\nredis\r\n"
		setinfo    = "*4\r\n$6\r\nclient\r\n$7\r\nsetinfo\r\n$8\r\nLIB-NAME\r\n$19\r\ngo-redis(,go1.27.0)\r\n" +
			"*4\r\n$6\r\nclient\r\n$7\r\nsetinfo\r\n$7\r\nLIB-VER\r\n$5\r\n9.7.1\r\n"
		oks  = "+OK\r\n+OK\r\n"
		ping = "*1\r\n$4\r\nping\r\n"
		pong = "+PONG\r\n"
	)

	got := runPairing(t, []chunkEvent{
		clientChunk(hello, 1, at(0)),
		destChunk(helloReply, 2, at(350)),
		clientChunk(setinfo, 4, at(480)),
		clientChunk(ping, 7, at(750)),
		destChunk(oks, 5, at(900)),
		destChunk(pong, 8, at(1000)),
	})

	if len(got) != 3 {
		for _, m := range got {
			t.Logf("mock: requests %q, responses %q", payloads(m.Spec.GenericRequests), payloads(m.Spec.GenericResponses))
		}
		t.Fatalf("expected 3 mocks (HELLO, SETINFO, PING), got %d", len(got))
	}
	assertExchange(t, got[0], []string{hello}, []string{helloReply})
	assertExchange(t, got[1], []string{setinfo}, []string{oks})
	assertExchange(t, got[2], []string{ping}, []string{pong})
	if !got[2].Spec.ReqTimestampMock.Equal(at(750)) || !got[2].Spec.ResTimestampMock.Equal(at(1000)) {
		t.Errorf("PING's mock spans %v..%v, want its own chunks' times %v..%v",
			got[2].Spec.ReqTimestampMock, got[2].Spec.ResTimestampMock, at(750), at(1000))
	}
}

// A producer that gives both directions of a connection one number (a
// capture that cannot number them apart) leaves the chunks' times to order
// them, as they did before the numbers existed: the rules still fire on
// them, and a reply teed behind the request it unblocked still ends its
// exchange when its stamp says so.
func TestV2_ChunksNumberedAlikeAreOrderedByTheirTimes(t *testing.T) {
	t.Parallel()

	got := runPairing(t, []chunkEvent{
		clientChunk("HELLO", 1, time.Unix(1000, 0)),
		clientChunk("PING", 1, time.Unix(1002, 0)),
		destChunk("hello-reply", 1, time.Unix(1001, 0)),
		destChunk("pong", 1, time.Unix(1003, 0)),
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 mocks, got %d", len(got))
	}
	assertExchange(t, got[0], []string{"HELLO"}, []string{"hello-reply"})
	assertExchange(t, got[1], []string{"PING"}, []string{"pong"})
}

// Chunks numbered alike and stamped in the same instant carry no order
// between them, and the times compare strictly, so neither rule fires: the
// recording is whatever arrival order produced. A producer that gives both
// directions one number and one clock stamps its chunks so.
func TestV2_ChunksNumberedAlikeAndStampedAlikeFireNeitherRule(t *testing.T) {
	t.Parallel()

	same := time.Unix(1000, 0)

	split := runPairing(t, []chunkEvent{
		clientChunk("A", 1, same),
		clientChunk("B", 1, same),
		destChunk("r", 1, same),
	})
	if len(split) != 1 {
		t.Fatalf("split: expected 1 mock, got %d", len(split))
	}
	assertExchange(t, split[0], []string{"A", "B"}, []string{"r"})

	adopt := runPairing(t, []chunkEvent{
		destChunk("r", 1, same),
		clientChunk("A", 1, same),
		destChunk("r2", 1, same),
	})
	if len(adopt) != 1 {
		t.Fatalf("adopt: expected 1 mock, got %d", len(adopt))
	}
	assertExchange(t, adopt[0], []string{"A"}, []string{"r2"})
}
