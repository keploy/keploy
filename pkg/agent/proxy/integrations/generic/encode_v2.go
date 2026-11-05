package generic

import (
	"context"
	"errors"
	"io"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// chunkEvent is the unified item produced by the two reader goroutines.
// A nil-bytes event with err != nil signals end-of-stream for that side.
type chunkEvent struct {
	dir   fakeconn.Direction
	bytes []byte
	// connSeq is the chunk's capture number (fakeconn.Chunk.ConnSeq): what
	// says which of two chunks, one from each direction, was captured
	// first, and so whether a response can answer a request (order). The
	// times below are the mock's window, and order's only when the numbers
	// do not tell the two chunks apart.
	connSeq uint32
	// readAt is the timestamp at which the relay read this chunk off the
	// source socket. Used as ReqTimestampMock for the first client chunk
	// of each exchange.
	readAt time.Time
	// writtenAt is the timestamp at which the relay's write of this chunk
	// to the opposite socket returned (the client may have acted on the
	// bytes before it: exchangeSplit). The last answer chunk's is the
	// exchange's ResTimestampMock, the "last dest chunk WrittenAt" rule
	// from the migration spec.
	writtenAt time.Time
	// through is how far into its stream the reader was done once it had
	// read this chunk (fakeconn.FakeConn.ReadThrough): what mergeByCapture
	// compares with the session's capture order
	// (supervisor.Session.StreamedBefore).
	through int64
	err     error
}

// The generic V2 recorder's queues: each direction's reader hands its chunks
// to the merge (mergeByCapture) through a queue of streamQueueLen events, and
// the merge hands both directions on to the main loop through one of
// mergedQueueLen.
const (
	streamQueueLen = 8
	mergedQueueLen = 16
)

// testHookBeforeMerge runs on the parser's goroutine once the stream readers
// have started and before the merge does. It does nothing outside a test: a
// row sets it to start the merge only once a reader has queued an event, so
// the merge's first look finds events of both directions ready.
var testHookBeforeMerge = func() {}

// encodeGenericV2 implements the V2 record path for the generic parser.
// It drains the two FakeConn streams concurrently and pairs chunks into
// mocks with the same shape the legacy encodeGeneric path produces so
// that replay works against mocks recorded by either path.
func encodeGenericV2(sess *supervisor.Session, logger *zap.Logger) error {
	if sess.ClientStream == nil || sess.DestStream == nil {
		// Defensive: a session without streams can happen only if the
		// supervisor is misconfigured, but returning nil is safer than
		// panicking on a nil receiver deep inside a goroutine.
		if logger != nil {
			logger.Debug("generic v2: session missing streams, skipping")
		}
		return nil
	}

	// The generic parser consumes exchanges as "one or more client
	// chunks followed by one or more dest chunks." A dest chunk is
	// always CAUSED by a client chunk that was forwarded first, so the
	// first thing to happen on this connection is a client read: that
	// much we can rely on, and reading the initial client chunk on the
	// calling goroutine before starting the reader pair is what holds
	// it (otherwise a race between the two readers could surface a
	// dest event first on synthetic inputs where both streams are
	// pre-primed).
	//
	// Beyond that first chunk, the order the two readers below hand their
	// chunks on in says nothing of the order they were captured in. The relay
	// writes bytes to the opposite socket BEFORE it tees them here (relay.go),
	// and the two directions are drained by separate goroutines, so a reply
	// the client has already acted on can reach this parser behind the
	// request it unblocked, and a parser behind its capture, with both streams
	// full, gets each direction's chunks in whatever runs the readers'
	// scheduler picks. So the two directions are merged in capture order
	// (mergeByCapture) before they are paired: the pairing sees the chunks in
	// the order the relay captured them, whatever order they were read in.
	// Before, it took them in the order read, and a run of answers read ahead
	// of their requests (all three answers, then the requests) recorded the
	// first exchange only, with nothing left out or said.
	clientEvents := make(chan chunkEvent, streamQueueLen)
	destEvents := make(chan chunkEvent, streamQueueLen)
	events := make(chan chunkEvent, mergedQueueLen)

	initial, err := sess.ClientStream.ReadChunk()
	if err != nil {
		// No client bytes ever arrived: nothing to mock.
		if logger != nil && !isBenignReadErr(err) {
			logger.Debug("generic v2: initial client read failed",
				zap.Error(err))
		}
		return nil
	}

	// The initial client chunk is the merge's client head from its start,
	// so it goes on first whatever the dest's reader hands on: handed to the
	// merge through clientEvents instead, it would be one of two events
	// ready at once, and a select picks either, so an unnumbered reply (one
	// the merge cannot place, and so takes as received) could go on ahead of
	// it and be paired with no request.
	var first *chunkEvent
	if len(initial.Bytes) > 0 {
		first = &chunkEvent{
			dir:       fakeconn.FromClient,
			bytes:     initial.Bytes,
			connSeq:   initial.ConnSeq,
			readAt:    initial.ReadAt,
			writtenAt: initial.WrittenAt,
			through:   sess.ClientStream.ReadThrough(),
		}
	}

	// Reader goroutines: one per direction. Each loop reads chunks from
	// its FakeConn and forwards them onto its direction's events until
	// EOF / ErrClosed, then closes them. The merge hands both on, in
	// capture order, to events, and closes it once both directions have
	// ended and it has handed on everything they read, so the main loop
	// never observes a "close before drain" race.
	//
	// stop is closed when this function returns, on a panic of the main
	// loop too: nothing drains events after that, so the merge, with an
	// event to send, would block on the send for good, and with it the
	// readers, each with an event to send (a chunk, or the error that ends
	// its stream): three goroutines kept for the agent's life, with both
	// FakeConns, every chunk still queued on them and the events buffered.
	// Every send and every wait of the readers and of the merge selects on
	// it. On a normal return the loop has drained events to its close, so
	// the merge and both readers have returned already.
	stop := make(chan struct{})
	defer close(stop)
	go readStream(sess.ClientStream, fakeconn.FromClient, clientEvents, stop)
	go readStream(sess.DestStream, fakeconn.FromDest, destEvents, stop)
	testHookBeforeMerge()
	go mergeByCapture(first, [2]<-chan chunkEvent{clientEvents, destEvents}, events, sess.StreamedBefore, stop)

	return pairChunkEvents(events, sess, logger)
}

// pairChunkEvents is the generic parser's exchange state machine: it
// consumes the merged chunk stream and emits one mock per request/response
// exchange. It is separated from encodeGenericV2's plumbing so the pairing
// rules can be driven directly, in a chosen order, by tests — arrival order
// is the thing that goes wrong in production (see the split rule below) and
// a test that cannot choose it cannot cover it.
func pairChunkEvents(events <-chan chunkEvent, sess *supervisor.Session, logger *zap.Logger) error {
	var (
		genericRequests  []models.Payload
		genericResponses []models.Payload
		// reqSeq carries the capture number of each buffer in
		// genericRequests, same index. It is what lets a late-pushed
		// response be attributed to the request it actually answered
		// rather than to whatever happened to arrive first.
		reqSeq []uint32
		// reqReadAt carries the relay's ReadAt for each buffer in
		// genericRequests, same index: the times of the exchanges a split
		// makes of them.
		reqReadAt []time.Time
		// resHeadSeq and resHeadWrittenAt are the capture number and the
		// WrittenAt of the first response chunk currently held.
		resHeadSeq       uint32
		resHeadWrittenAt time.Time
		// resHeadAt is when the relay read that chunk (its write's return
		// when it has no read time): where the span of answer chunks left
		// out from it starts (leaveOutTail).
		resHeadAt        time.Time
		reqTimestampMock time.Time
		resTimestampMock time.Time
		// prevChunkWasReq tracks request→response transitions so we know
		// when to flush. Mirrors the legacy encoder's state machine.
		prevChunkWasReq = false
		// carriedBySplit marks an in-flight exchange that a split
		// carried forward rather than one a client chunk started.
		carriedBySplit = false
	)

	// flushMock reports whether the mock was LEFT OUT for the incomplete
	// flag rather than emitted, and the reason the flag was set for. The
	// split path needs that answer: a capture the relay holed must lose
	// its carried half too, which is reported for the same reason, and the
	// incomplete flag can be set by the relay's goroutine after any check
	// this function's caller made.
	// exchanges counts the connection's exchanges flushed so far, those
	// left out included (exchangeLifetime).
	exchanges := 0
	ctx := context.Background()
	if sess != nil && sess.Ctx != nil {
		ctx = sess.Ctx
	}
	// emit hands on the exchanges a preambleHold held (handOn).
	emit := func(m *models.Mock) { emitV2(sess, logger, m) }
	// leaveOutTail reports answer chunks no exchange it records holds as left
	// out: chunks the server sent after the exchange they would end was
	// handed on (the recording's stop decided it: each as the parser takes
	// it), or with no request before them, up to the next request. n is how
	// many there are (none: a no-op); they span from from, when the relay
	// read the first of them or earlier, to when its write of the last
	// returned (to): the times a test case that waited on them spans, so it
	// is left out, never saved over an answer cut short.
	//
	// Chunks after an exchange left out are the rest of it, counted and
	// spanned with it already: reported again, they would count a mock
	// twice, and span time its test cases need not span. That holds for an
	// exchange flushMock leaves out (the capture's incomplete flag) and for
	// one the session leaves out as flushMock hands it on (emitV2: refused,
	// or the channel full, at the stop). A held exchange (preambleHold) the
	// session leaves out as the hold hands it on is counted apart from its
	// tail: counted once more, its span only wider (fail-closed).
	var flushedLeftOut bool
	leaveOutTail := func(n int, from, to time.Time) {
		if n == 0 || flushedLeftOut {
			return
		}
		tail := &models.Mock{Version: models.GetVersion(), Name: "mocks", Kind: models.GENERIC}
		tail.Spec.ReqTimestampMock, tail.Spec.ResTimestampMock = from, to
		sess.ReportLeftOut(tail, leftOutAnswerTail)
		if logger != nil {
			logger.Debug("generic v2: left out answer chunks sent after the chunk their exchange records",
				zap.Int("chunks", n), zap.Time("from", from), zap.Time("to", to))
		}
	}
	// hold is the connection's first exchange, and those after it, while the
	// test that owns its request may own the whole connection (preambleHold).
	// lastAt is the connection's latest chunk time.
	var hold *preambleHold
	var lastAt time.Time
	// A parser that leaves holding (a panic the supervisor recovers) reports
	// what it held as left out, as any exchange it could not record, and
	// takes its hold off the stop's count: the stop is not to wait on it
	// forever.
	defer func() {
		if hold != nil {
			// Off the count whatever the report does.
			defer hold.handedOn()
			for _, m := range hold.release(false) {
				sess.ReportLeftOut(m, "the Generic recorder stopped while it held this exchange")
			}
		}
	}()
	release := func(wholly bool) {
		if hold == nil {
			return
		}
		hold.handOn(wholly, emit)
		hold = nil
	}
	// An exchange is held from its answer's first chunk until its answer has
	// ended, so that every chunk of an answer the server sent in more than one
	// (an answer over the relay's 32 KiB read, a header and a body written
	// apart) is recorded with it. What ends an answer is the stream's own
	// event, never a timer: the connection's next request (the back-stop of
	// the client case), a split, or the streams' end. Before, the exchange was
	// handed on at its answer's first chunk and the rest left out, so the test
	// case over it was left out too.
	//
	// The parser has read what it holds, so the capture's watermark and the
	// recording's stop, which go by how far the parser has read, cannot see
	// it: the last exchange of a pooled connection, held until a request that
	// may never come, would miss the stop. So the hold is counted with the
	// recording's manager (SyncMockManager.HoldExchanges, as the preamble
	// hold's is), which the stop finds pending
	// until it is handed on, and the stop decides it (answerStopping): once the
	// parser has taken every chunk the merge has ready, and every answer chunk
	// its dest reader has taken off its stream (destRead), the exchange is
	// handed on with the answer read so far. Its answer has then ended as far
	// as the capture shows. A chunk of it that the parser takes after that
	// (stopDecided) is left out with its span as the parser takes it
	// (leaveOutTail), not held to the next request or the streams' end, which
	// may come after the verdict of the test case over it: that test case is
	// left out, never saved over the answer cut short.
	var (
		answerStopping <-chan struct{}
		answerHandedOn func()
		// deciding is set once the stop has begun while an answer is held:
		// the loop hands it on at its next look that finds no event ready
		// and no answer chunk on its way from the dest's reader.
		deciding bool
		// destRead is how far into the dest's stream the answer chunks the
		// loop has taken go (chunkEvent.through): short of the stream's
		// ReadThrough, its reader has taken a chunk off the stream that is
		// still on its way to the loop (the reader's queue, the merge).
		destRead int64
		// stopDecided is set once the stop has decided a held answer: an
		// answer chunk the loop takes after that with no request before it,
		// in the recording whose stop it was (decidedBy), is the rest of an
		// answer the stop cut short. It is cleared at the connection's next
		// request: the merge orders numbered chunks by capture, so the rest
		// of the answer comes before that request. A chunk it cannot place
		// (no capture numbers) is taken as it comes, as anywhere else. A
		// pooled connection can outlive the recording whose stop decided its
		// answer: in a re-record on the same manager
		// (SyncMockManager.newHoldsSession) an answer chunk with no request
		// is held as any is, whether or not a request came first.
		stopDecided bool
		// decidedBy is the stop signal of the recording whose stop decided
		// the answer (answerStopping): the manager's HoldsSession while
		// that recording lasts.
		decidedBy <-chan struct{}
		// cutTailCounted is set once the first chunk of the rest of an
		// answer the stop cut short is counted (leaveOutCutTail), and
		// cleared with stopDecided.
		cutTailCounted bool
	)
	// leaveOutCutTail leaves out ev, a chunk of the rest of an answer the
	// stop cut short (stopDecided), with its span, as the parser takes it.
	// That rest is one exchange lost, however many chunks it comes in: its
	// first chunk is counted (leaveOutTail), and each after it, up to the
	// next request, is spanned and not counted again
	// (Session.ReportLeftOutPart). Counted per chunk, a rest in three chunks
	// read as three exchanges lost. The rest of an exchange left out as it
	// was handed on is counted and spanned with it (leaveOutTail).
	leaveOutCutTail := func(ev chunkEvent) {
		from, to := readOrWritten(ev), ev.writtenAt
		switch {
		case !cutTailCounted:
			cutTailCounted = true
			leaveOutTail(1, from, to)
		case !flushedLeftOut:
			sess.ReportLeftOutPart(from, to)
		}
	}
	// answerOnItsWay reports whether the dest's reader has taken a chunk off
	// its stream that the loop has not taken yet. A session with no dest
	// stream (a row driving the loop) has none.
	answerOnItsWay := func() bool {
		return sess.DestStream != nil && sess.DestStream.ReadThrough() > destRead
	}
	holdAnswer := func() {
		if answerHandedOn == nil {
			answerStopping, answerHandedOn = syncMock.FromContextOrGlobal(ctx).HoldExchanges(reqTimestampMock)
		}
		// Read and held, its chunks are no longer pending work: an idle
		// spell while it is held is a pooled connection's, not a hung parser.
		sess.MockHeld()
	}
	answerHandOn := func() {
		if answerHandedOn != nil {
			answerHandedOn()
		}
		answerStopping, answerHandedOn, deciding = nil, nil, false
	}
	// A parser that leaves holding an answer (a panic the supervisor
	// recovers) reports the exchange left out, and takes it off the stop's
	// count.
	defer func() {
		if answerHandedOn == nil {
			return
		}
		defer answerHandOn()
		m := &models.Mock{Version: models.GetVersion(), Name: "mocks", Kind: models.GENERIC}
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = reqTimestampMock, resTimestampMock
		sess.ReportLeftOut(m, leftOutHeldAnswer)
	}()
	flushMock := func() (leftOutFor string, leftOut bool) {
		if len(genericRequests) == 0 || len(genericResponses) == 0 {
			return "", false
		}
		// Handed on, left out, or held by the preamble hold (counted by its
		// own), the exchange is off the answer's hold once this returns.
		defer answerHandOn()
		n := exchanges
		lifetime := exchangeLifetime(n)
		exchanges++
		reset := func() {
			genericRequests = nil
			genericResponses = nil
			reqSeq = nil
			reqReadAt = nil
			resHeadSeq = 0
			resHeadWrittenAt = time.Time{}
			resHeadAt = time.Time{}
			reqTimestampMock = time.Time{}
			resTimestampMock = time.Time{}
		}

		metadata := map[string]string{
			"type":                     "config",
			models.MetadataLifetimeKey: lifetime,
		}
		if sess.ClientConnID != "" {
			metadata["connID"] = sess.ClientConnID
		}

		mock := &models.Mock{
			Version: models.GetVersion(),
			Name:    "mocks",
			Kind:    models.GENERIC,
			Spec: models.MockSpec{
				GenericRequests:  genericRequests,
				GenericResponses: genericResponses,
				ReqTimestampMock: reqTimestampMock,
				ResTimestampMock: resTimestampMock,
				Metadata:         metadata,
			},
		}
		// Leave the mock out if the relay has flagged the capture
		// incomplete (dropped chunk upstream, memory pressure, short
		// write, etc.). EmitMock honours the flag too, but the split path
		// needs to know, and with what reason, so the flag is taken here.
		// ReportLeftOut reports the exchange as one the connection could
		// not record, counts it and clears the session's pending work, as
		// EmitMock would have.
		if reason, ok := sess.TakeMockIncomplete(); ok {
			sess.ReportLeftOut(mock, reason)
			reset()
			flushedLeftOut = true
			return reason, true
		}
		flushedLeftOut = false
		// A held exchange is consumed input, as an emitted one is: the
		// session's pending work is cleared, or an idle spell while it is
		// held reads as a hung parser.
		switch {
		case hold != nil:
			hold.add(mock)
			sess.MockHeld()
			if hold.outlived() {
				release(false)
			}
			reset()
			return "", false
		case n == 0:
			if hold = holdPreamble(ctx, mock, mock.Spec.ReqTimestampMock, lastAt); hold != nil {
				sess.MockHeld()
				if hold.outlived() {
					release(false)
				}
				reset()
				return "", false
			}
		}
		// The loop goes on after the recording's stop, to the streams' end,
		// and so does what it records: emitV2 hands every exchange read
		// whole to the session, or leaves it out with its reason, and then
		// its tail is the rest of it.
		flushedLeftOut = emitV2(sess, logger, mock)
		reset()
		return "", false
	}

	// Drain every event the two reader goroutines produce. The closer
	// goroutine above closes `events` once both readers return, which
	// is the only termination signal this loop observes.
drain:
	for {
		// While the recorder holds the connection's first exchange, the
		// window that may own it leaving the open ones may decide it, and the
		// recording's stop, or the parser's end, decides it.
		var changes, stopping, done <-chan struct{}
		if hold != nil {
			changes, stopping, done = hold.changes(), hold.stopping, ctx.Done()
		}
		var (
			ev   chunkEvent
			open bool
		)
		if deciding {
			// The stop has begun while an answer is held: the chunks the
			// merge has ready go first, and at the first look that finds
			// none the exchange is handed on with its answer read so far.
			select {
			case ev, open = <-events:
			default:
				if answerOnItsWay() {
					// Taken off the stream before this look, so before
					// the stop decides: part of the answer, not cut
					// from it. The reader hands it on, or its stream's
					// end.
					ev, open = <-events
					break
				}
				// An answer is held only with its exchange's request
				// (holdAnswer), so the flush takes it, and ends deciding.
				decidedBy = answerStopping
				flushMock()
				stopDecided = true
				continue
			}
		} else {
			select {
			case <-changes:
				hold.refresh()
				if hold.outlived() {
					release(false)
				}
				continue
			case <-stopping:
				// The recording's stop has begun: decided with what is known.
				release(hold.wholly())
				continue
			case <-done:
				release(hold.wholly())
				continue
			case <-answerStopping:
				// While an answer is held (else nil), the recording's
				// stop decides it.
				deciding = true
				continue
			case ev, open = <-events:
			}
		}
		if !open {
			break drain
		}
		if ev.err != nil {
			// End-of-stream for this side. Classify benign terminations
			// (EOF / ErrClosed / deadline) distinctly from unexpected
			// errors so operator-facing logs stay quiet on clean exits
			// but still surface real problems.
			if logger != nil && !isBenignReadErr(ev.err) {
				logger.Debug("generic v2: stream read ended",
					zap.String("dir", ev.dir.String()),
					zap.Error(ev.err))
			}
			continue
		}

		// Ignore spurious empty chunks — they carry no payload, and
		// a response with none would pair with a request.
		if len(ev.bytes) == 0 {
			continue
		}
		if ev.dir == fakeconn.FromDest {
			destRead = ev.through
		}
		// The connection's life, by its chunks' recorded times: a held
		// first exchange is the preamble once a chunk comes after the
		// answer of the test that owns it. A chunk's time here is when the
		// relay read it, in either direction, never when its write
		// returned: the relay stamps a read before it writes the bytes on,
		// so the other side cannot have had them earlier, while a write's
		// return is stamped as late as the relay's goroutine runs. Under
		// load a destination's answer was stamped written after the client
		// had read it, ended its test and the next test had begun, so a
		// connection wholly inside its test read as outliving it, its only
		// exchange went to the session pool, and the test passed at replay
		// with its call skipped. A chunk with no read time (none the relay
		// stamped) does not count: no time it has shows the connection
		// outlived its test.
		at := ev.readAt
		if at.After(lastAt) {
			lastAt = at
		}
		if hold != nil {
			hold.saw(lastAt)
			if hold.outlived() {
				release(false)
			}
		}

		switch ev.dir {
		case fakeconn.FromClient:
			// The rest of an answer the stop cut short, if any, came
			// before this request (stopDecided).
			stopDecided, cutTailCounted = false, false
			// The next request ends the answer held (holdAnswer): the
			// exchange is handed on whole before a new one starts, as
			// the legacy encoder does.
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				flushMock()
			}
			// Starting a brand-new exchange: leave out any orphaned
			// response chunks, held with no exchange (reported, with
			// their span: leaveOutTail), and anchor the request
			// timestamp to THIS chunk's ReadAt so we record when
			// bytes actually hit the relay rather than when the
			// parser got around to pairing them.
			//
			// Orphaned is decided by capture order, not on arrival. A
			// single held response captured AFTER this request is not
			// the leftover of an earlier reply — on the wire it comes
			// second — so the two are one exchange, teed out of order
			// (the relay forwards bytes before it tees them, so either
			// direction can overtake the other). Dropping it here would
			// leave this request to pair with the NEXT response instead.
			reqFirst, _ := order(stamp{ev.connSeq, ev.readAt}, stamp{resHeadSeq, resHeadWrittenAt})
			answersThis := len(genericRequests) == 0 &&
				len(genericResponses) > 0 &&
				reqFirst
			if len(genericRequests) == 0 {
				if !answersThis {
					leaveOutTail(len(genericResponses), resHeadAt, resTimestampMock)
					genericResponses = nil
					resHeadSeq = 0
					resHeadWrittenAt = time.Time{}
					resHeadAt = time.Time{}
				}
				reqTimestampMock = ev.readAt
				carriedBySplit = false
			}
			genericRequests = append(genericRequests, encodePayload(ev.bytes, models.FromClient))
			reqSeq = append(reqSeq, ev.connSeq)
			reqReadAt = append(reqReadAt, ev.readAt)
			prevChunkWasReq = true
			if answersThis {
				// The answer is this request's, every chunk of it held:
				// each was captured after the head, which was captured
				// after the request. So the exchange is the one wire
				// order makes, the request and its answer so far, held
				// as any answered exchange is until its answer ends
				// (holdAnswer): the bytes a test replays do not depend
				// on which way the relay's two goroutines were
				// scheduled.
				if logger != nil {
					logger.Debug("generic v2: paired a request with the response that arrived ahead of it",
						zap.Uint32("requestConnSeq", ev.connSeq),
						zap.Uint32("responseConnSeq", resHeadSeq),
						zap.Time("requestReadAt", ev.readAt),
						zap.Time("responseWrittenAt", resHeadWrittenAt),
						zap.Int("responseChunksHeld", len(genericResponses)))
				}
				holdAnswer()
				prevChunkWasReq = false
			}

		case fakeconn.FromDest:
			// The half a split carried forward is an exchange this
			// parser created, so it owes the same precondition the
			// split itself is held to: a response captured before the
			// request cannot be its answer. In wire order that chunk
			// is the tail of the PREVIOUS reply and is left out (with
			// its span: leaveOutTail); pairing it here instead
			// records an answer this request never got — and generic
			// mocks are session-scoped (Metadata type "config"), so
			// replay's window filter short-circuits them into the
			// unfiltered pool BEFORE its response-precedes-request
			// check. Nothing downstream catches this; it replays.
			if carriedBySplit && len(reqSeq) > 0 {
				if resFirst, _ := order(stamp{ev.connSeq, ev.writtenAt}, stamp{reqSeq[0], reqReadAt[0]}); resFirst {
					if logger != nil {
						logger.Debug("generic v2: left out a response older than the request a split carried forward",
							zap.Uint32("responseConnSeq", ev.connSeq),
							zap.Uint32("requestConnSeq", reqSeq[0]),
							zap.Time("responseWrittenAt", ev.writtenAt),
							zap.Time("requestReadAt", reqReadAt[0]))
					}
					leaveOutTail(1, readOrWritten(ev), ev.writtenAt)
					continue
				}
			}
			// A request captured after this response is not what it
			// answers: the destination sent the response before it
			// could have had the request (connseq.Upstream numbers a
			// client chunk after every byte the destination had sent
			// when it was read, before the relay writes it on). So on
			// the wire the two are in different exchanges however they
			// reached us. Cut there: keep the requests that precede the
			// response, carry the rest forward. Without this, a reply
			// teed after the request it unblocked merges two commands
			// into one mock and drops the second reply — and replay,
			// which matches on the NUMBER of request buffers, then
			// finds nothing for either command, which is how a
			// recorded app dies at startup.
			//
			// This restores wire order; it does not make an opaque
			// stream understood. The parser still pairs by direction
			// transitions, so bytes the server sent unbidden — a push,
			// the tail of a multi-chunk reply — are attributed to
			// whatever request precedes them, wrongly, exactly as they
			// were before any of this (TestV2_ServerPushIsPairedThe
			// SameEitherWay walks one such stream both ways). Fixing
			// THAT needs a parser that frames the protocol, not a
			// capture order.
			//
			// A connection that lost a chunk drops its whole in-flight
			// exchange (flushMock below). Splitting first would keep
			// the tail half of a capture already known to be holed.
			if len(genericRequests) > 1 {
				if i := exchangeSplit(reqSeq, reqReadAt, stamp{ev.connSeq, ev.writtenAt}); i > 0 {
					heldReqs := append([]models.Payload(nil), genericRequests[i:]...)
					heldSeq := append([]uint32(nil), reqSeq[i:]...)
					heldAt := append([]time.Time(nil), reqReadAt[i:]...)
					genericRequests = genericRequests[:i]
					reqSeq = reqSeq[:i]
					reqReadAt = reqReadAt[:i]
					genericResponses = append(genericResponses, encodePayload(ev.bytes, models.FromServer))
					resTimestampMock = ev.writtenAt
					if reason, leftOut := flushMock(); leftOut {
						// The relay holed this capture — possibly
						// between our check and this flush, so the
						// answer has to come from the flush itself.
						// Let the carried half go with it rather
						// than record the surviving half of a
						// recording known to be missing bytes, and
						// report it for the same reason, as the
						// flush reported the first half: an
						// exchange the connection could not record,
						// counted. Its answer is yet to come, so it
						// spans its requests' reads, first to last,
						// which every test case that made one spans.
						carried := &models.Mock{Name: "mocks", Kind: models.GENERIC}
						carried.Spec.ReqTimestampMock = heldAt[0]
						carried.Spec.ResTimestampMock = heldAt[len(heldAt)-1]
						sess.ReportLeftOut(carried, reason)
						if logger != nil {
							logger.Debug("generic v2: dropped the half a split carried out of a holed exchange",
								zap.Int("requestsDropped", len(heldReqs)),
								zap.Time("firstRequestReadAt", heldAt[0]),
								zap.Time("lastRequestReadAt", heldAt[len(heldAt)-1]))
						}
						prevChunkWasReq = false
						continue
					}
					genericRequests = heldReqs
					reqSeq = heldSeq
					reqReadAt = heldAt
					reqTimestampMock = heldAt[0]
					carriedBySplit = true
					if logger != nil {
						logger.Debug("generic v2: split an exchange the response arrived behind",
							zap.Int("splitAfterRequests", i),
							zap.Int("requestsCarriedForward", len(heldReqs)),
							zap.Uint32("responseConnSeq", ev.connSeq),
							zap.Uint32("nextRequestConnSeq", heldSeq[0]),
							zap.Time("responseWrittenAt", ev.writtenAt),
							zap.Time("nextRequestReadAt", heldAt[0]))
					}
					continue
				}
			}
			if stopDecided && syncMock.FromContextOrGlobal(ctx).HoldsSession() == decidedBy {
				// The rest of an answer the stop cut short (or an
				// answer with no request, after the stop): left out
				// with its span as the parser takes it, not held to
				// the next request or the streams' end, which may come
				// after the verdict of the test case over it. The
				// decision took every request the parser had, and the
				// next request clears the flag, so none is held here.
				leaveOutCutTail(ev)
				continue
			}
			if len(genericResponses) == 0 {
				resHeadSeq = ev.connSeq
				resHeadWrittenAt = ev.writtenAt
				resHeadAt = readOrWritten(ev)
			}
			genericResponses = append(genericResponses, encodePayload(ev.bytes, models.FromServer))
			// Per the migration spec, ResTimestampMock is WrittenAt of
			// the last dest chunk for the matching response — always
			// overwrite so that if the response were to accumulate
			// multiple chunks before flush, the last wins.
			resTimestampMock = ev.writtenAt

			// An answered exchange is held until its answer ends
			// (holdAnswer), each chunk of it appended here: at the
			// next request, the streams' end, or as the recording's
			// stop decides it. Chunks with no request before them
			// are held with no exchange, and left out at the next
			// request or the streams' end (leaveOutTail).
			if len(genericRequests) > 0 {
				holdAnswer()
			}
			prevChunkWasReq = false
		}
	}

	// The streams' end ends the answer held: the exchange is handed on
	// whole. Answer chunks still held after it have no request to answer
	// (the flush takes an exchange with both): left out as at a next
	// request.
	flushMock()
	leaveOutTail(len(genericResponses), resHeadAt, resTimestampMock)
	// The connection has ended: the first exchange held until now is the
	// test's when the connection's whole lay inside it (preambleHold), known
	// once the test's answer is.
	if hold != nil {
		hold.settle(ctx)
		release(hold.wholly())
	}
	return nil
}

// exchangeSplit reports where a response's arrival proves that the
// requests held in flight are really two exchanges: the index of the
// first request captured after the response (res), which the response
// therefore does not answer. reqSeq and reqReadAt are the requests'
// capture numbers and times, same index. It returns 0 for "no evidence",
// which is also the answer whenever nothing orders a request and the
// response (order) — such a chunk cannot be placed on either side of a
// response, and guessing is how a recorder starts emitting mocks nobody
// can explain.
//
// The capture numbers decide it where they order the two (the relay's
// always do). The times decide only for a producer that does not number its
// two directions apart, and they are right only where a response's stamp is
// no later than the application could act on it, as the relay's WrittenAt
// is (taken before its write). A stamp taken once the write returns can be
// later than that: the write wakes the application, which can send its
// next request before the forwarder gets its CPU back. A comparison of the
// two stamps then puts that request before the response that unblocked it,
// the split does not happen, and the two commands land in one mock that
// replay cannot match (go-redis's CLIENT SETINFO and PING).
//
// It also splits off a pipelined request the relay read only after the
// response had reached it, though the client sent the request before it
// saw the response: the response cannot answer what the destination did
// not have yet. Replay, where the client sends the two together, then
// finds no mock with both; before, a reply had also to be written back to
// the client before such a request was read, and that pipeline's second
// reply was lost instead. A parser that frames the protocol is what records
// a pipeline whole.
//
// Index 0 is never a split point. A response captured before even the
// FIRST held request is not this exchange's answer, and cutting there
// would emit a pair that never happened on the wire. Replay will not
// catch it: generic mocks are recorded session-scoped, and the window
// filter routes those to the unfiltered pool before it ever compares
// their two timestamps.
func exchangeSplit(reqSeq []uint32, reqReadAt []time.Time, res stamp) int {
	if len(reqSeq) == 0 {
		return 0
	}
	if resFirst, known := order(res, stamp{reqSeq[0], reqReadAt[0]}); !known || resFirst {
		return 0
	}
	for i := 1; i < len(reqSeq); i++ {
		resFirst, known := order(res, stamp{reqSeq[i], reqReadAt[i]})
		if !known {
			return 0
		}
		if resFirst {
			return i
		}
	}
	return 0
}

// stamp is what places a chunk against a chunk of the other direction:
// its capture number (fakeconn.Chunk.ConnSeq) and its time (a request's
// ReadAt, a response's WrittenAt).
type stamp struct {
	seq uint32
	at  time.Time
}

// order reports whether x was captured before y (first), and whether
// anything says (known). The capture numbers say, when both chunks are
// numbered and the numbers differ (fakeconn.Chunk.CapturedBefore): the relay
// numbers a client chunk after every byte the destination had sent when it
// read it, before it writes it on, so a response numbered below a request
// was sent before the destination could have had the request. Otherwise
// the times say, compared strictly, when both are stamped: a producer that
// does not number its two directions apart (a capture that gives both one
// number) is ordered as it was before the numbers existed, with the times'
// fault (exchangeSplit). Equal times say nothing is first.
func order(x, y stamp) (first, known bool) {
	if x.seq != 0 && y.seq != 0 && x.seq != y.seq {
		return fakeconn.Chunk{ConnSeq: x.seq}.CapturedBefore(y.seq), true
	}
	if x.at.IsZero() || y.at.IsZero() {
		return false, false
	}
	return x.at.Before(y.at), true
}

// leftOutAnswerTail is the reason the parser leaves out answer chunks no
// exchange it records holds (pairChunkEvents' leaveOutTail): chunks the server
// sent after the exchange they would end was handed on (decided at the
// recording's stop, or cut by a split), or with no request before them.
const leftOutAnswerTail = "the server sent this part of an answer after the Generic recorder had handed on the exchange it would end (decided at the recording's stop, or at a request it answered first), or with no request before it, and it was left out"

// leftOutHeldAnswer is the reason the parser leaves out the exchange it held
// for the rest of its answer when it stops holding it (a panic the supervisor
// recovers).
const leftOutHeldAnswer = "the Generic recorder stopped while it held this exchange for the rest of its answer"

// readOrWritten is when the relay read ev's chunk, or when its write of it
// returned when it has no read time.
func readOrWritten(ev chunkEvent) time.Time {
	if ev.readAt.IsZero() {
		return ev.writtenAt
	}
	return ev.readAt
}

// leftOutRefusedAtStop is the reason emitV2 leaves out a mock the session
// refused as its context ended.
const leftOutRefusedAtStop = "the recording stopped as the mock of an exchange read whole was handed over"

// emitV2 hands the session m, the mock of an exchange read whole, whether or
// not the session's context (Session.Ctx) has ended: EmitMock refuses every
// mock once it has, so a mock handed over after the recording's stop, by a
// parser behind its capture, goes through EmitMockOnShutdown, which records
// it. Before, every exchange recorded after the stop was refused, with a
// Debug line only. A mock refused all the same (the context ended between
// this check and EmitMock's own) is left out with its reason, counted and
// spanned, never dropped with nothing said. It returns whether m was left
// out, by the session (EmitMockOutcome) or here.
func emitV2(sess *supervisor.Session, logger *zap.Logger, m *models.Mock) (leftOut bool) {
	atStop := sess.Ctx != nil && sess.Ctx.Err() != nil
	leftOut, err := sess.EmitMockOutcome(m, atStop)
	if err != nil {
		if logger != nil {
			logger.Debug("generic v2: the session refused a mock as its context ended", zap.Error(err))
		}
		sess.ReportLeftOut(m, leftOutRefusedAtStop)
		return true
	}
	return leftOut
}

// readStream pumps Chunks from one FakeConn onto its direction's events, out,
// and closes out as it returns: on EOF / ErrClosed / deadline-exceeded, or at
// its first send once stop has closed. The supervisor.Session's ctx is
// observed indirectly: when the ctx is cancelled the relay ends the FakeConn
// streams and closes them, which surfaces here as EOF (ErrClosed when the
// session's abort closed a stream before its end, however late the reader
// gets to it: relay's tee.endStream). A panic of the main loop aborts the
// session too, and closes stop, and nothing drains events after it, so every
// send to out, the end-of-stream error's included, selects on stop: a reader
// waiting on a read with nothing queued reads the abort's ErrClosed, and its
// error send may find out full. A send looks at stop before it tries out, so
// a reader takes at most one more chunk off its stream once stop has closed:
// each chunk it takes can count as read (FakeConn.ReadThrough) when the proxy
// decides which requests the parser left unread, and nothing records it.
func readStream(fc *fakeconn.FakeConn, dir fakeconn.Direction, out chan<- chunkEvent, stop <-chan struct{}) {
	defer close(out)
	send := func(ev chunkEvent) bool {
		// A select with out's room and stop both ready picks either: on
		// its own it would let a reader go on reading after stop.
		select {
		case <-stop:
			return false
		default:
		}
		select {
		case out <- ev:
			return true
		case <-stop:
			return false
		}
	}
	if fc == nil {
		send(chunkEvent{dir: dir, err: io.EOF})
		return
	}
	for {
		c, err := fc.ReadChunk()
		if err != nil {
			send(chunkEvent{dir: dir, err: err})
			return
		}
		// Empty chunk with no error shouldn't happen on a real stream
		// but guard against it so the consumer never stalls.
		if len(c.Bytes) == 0 {
			continue
		}
		if !send(chunkEvent{
			dir:       dir,
			bytes:     c.Bytes,
			connSeq:   c.ConnSeq,
			readAt:    c.ReadAt,
			writtenAt: c.WrittenAt,
			through:   fc.ReadThrough(),
		}) {
			return
		}
	}
}

// streamedBefore is the session's capture order
// (supervisor.Session.StreamedBefore): whether every chunk of dir captured
// before the chunk numbered seq has gone on dir's stream, or never will, and
// then how far into the stream they all are (end); while one may still go on
// it, changed is closed once that may have changed. ok and changed are both
// unset when the producer cannot say.
type streamedBefore func(dir fakeconn.Direction, seq uint32) (end int64, ok bool, changed <-chan struct{})

// mergeByCapture hands the events of the connection's two directions (in, the
// client's first) on to out in the order they were captured, closing out once
// both directions have ended (closed) and it has handed on all they sent.
//
// Each direction's chunks come in the order captured, so a merge needs only
// the next of each, its head. With both heads, the one captured first goes on
// (fakeconn.Chunk.CapturedBefore: client and server chunks never share a
// number). With one, it goes on once no chunk of the other direction captured
// before it can come: the other has ended, or the session's capture order
// (order) says every such chunk has gone on its stream and the other's reader
// has handed on its stream through there (chunkEvent.through), so its head
// would be here; until then the merge waits for the other's next event, or
// for the order to change. The wait is on capture alone, never on the
// network: a chunk the order waits on has reached the relay, and goes on its
// stream, or is known never to, as the relay goes on; a stream the relay ends
// ends the wait.
//
// What cannot be placed goes on in the order the merge received it: a chunk
// its producer did not number (ConnSeq 0), two chunks with one number, and a
// head whose producer cannot say its capture order (order says neither ok nor
// changed: a session with no Session.Order, a test's; the relay always says),
// as every chunk went on before.
//
// It returns once stop has closed: nothing drains out after the main loop
// returns. A select of it that finds stop and another case ready at once
// picks either, so it may still put events in what room out has then, where
// nothing reads them. It takes nothing off a stream (only the readers do),
// so it moves nothing that counts as read (chunkEvent.through).
//
// first, when set, is the connection's first chunk, the client's, read before
// the readers started: the merge starts with it as the client's head, received
// before anything else.
func mergeByCapture(first *chunkEvent, in [2]<-chan chunkEvent, out chan<- chunkEvent, order streamedBefore, stop <-chan struct{}) {
	defer close(out)
	dirs := [2]fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest}
	var (
		head    [2]chunkEvent
		has     [2]bool
		ended   [2]bool
		through [2]int64
		arrived [2]uint64 // when each head was received, for what cannot be placed
		n       uint64
	)
	if first != nil {
		n++
		head[0], has[0], arrived[0], through[0] = *first, true, n, first.through
	}
	// placed reports whether i's head can go on before anything the other
	// direction has yet to hand on, and if not, what may tell when it can.
	placed := func(i int) (ok bool, wake <-chan struct{}) {
		o := 1 - i
		seq := head[i].connSeq
		if ended[o] || seq == 0 {
			return true, nil
		}
		end, streamed, changed := order(dirs[o], seq)
		switch {
		case streamed:
			return through[o] >= end, nil
		case changed == nil:
			return true, nil // the producer cannot say
		default:
			return false, changed
		}
	}
	for {
		pick := -1
		var wake <-chan struct{}
		switch {
		case has[0] && has[1]:
			pick = 0
			c, d := head[0].connSeq, head[1].connSeq
			switch {
			case c != 0 && d != 0 && c != d:
				if (fakeconn.Chunk{ConnSeq: d}).CapturedBefore(c) {
					pick = 1
				}
			case arrived[1] < arrived[0]:
				pick = 1
			}
		case has[0] || has[1]:
			i := 0
			if has[1] {
				i = 1
			}
			var ok bool
			if ok, wake = placed(i); ok {
				pick = i
			}
		case ended[0] && ended[1]:
			return
		}
		if pick >= 0 {
			select {
			case out <- head[pick]:
				head[pick], has[pick] = chunkEvent{}, false
			case <-stop:
				return
			}
			continue
		}
		// Take the next event of each direction with no head that has not
		// ended.
		var from [2]<-chan chunkEvent
		for i := range from {
			if !has[i] && !ended[i] {
				from[i] = in[i]
			}
		}
		take := func(i int, ev chunkEvent, open bool) {
			switch {
			case !open:
				ended[i] = true
			case ev.err != nil:
				// The direction's end: it comes after every chunk of its
				// direction, and goes on as a head that waits on nothing
				// (it has no number), for its log.
				ended[i] = true
				n++
				head[i], has[i], arrived[i] = ev, true, n
			default:
				through[i] = ev.through
				n++
				head[i], has[i], arrived[i] = ev, true, n
			}
		}
		select {
		case ev, open := <-from[0]:
			take(0, ev, open)
		case ev, open := <-from[1]:
			take(1, ev, open)
		case <-wake:
		case <-stop:
			return
		}
	}
}

// isBenignReadErr reports whether err is one of the expected end-of-stream
// signals from a FakeConn.
func isBenignReadErr(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
		return true
	}
	// net.Error with Timeout()=true — deadline expired, not an error.
	type timeoutErr interface{ Timeout() bool }
	if t, ok := err.(timeoutErr); ok && t.Timeout() {
		return true
	}
	return false
}
