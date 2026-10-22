package generic

import (
	"context"
	"errors"
	"io"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
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
	// bytes before it: exchangeSplit). Used as ResTimestampMock for the
	// response head chunk of each exchange, which matches the "last dest
	// chunk WrittenAt" rule from the migration spec (there is only one
	// response chunk per mock by construction in the generic parser, so
	// "last" == "this").
	writtenAt time.Time
	err       error
}

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
	// Beyond that first chunk, arrival order guarantees nothing. The
	// relay writes bytes to the opposite socket BEFORE it tees them
	// here (relay.go), and the two directions are drained by separate
	// goroutines, so a reply the client has already acted on can reach
	// this loop behind the request it unblocked. The pairing rules
	// below read the chunks' capture numbers (fakeconn.Chunk.ConnSeq,
	// order) instead of trusting the order these events turn up in.
	events := make(chan chunkEvent, 16)

	initial, err := sess.ClientStream.ReadChunk()
	if err != nil {
		// No client bytes ever arrived: nothing to mock.
		if logger != nil && !isBenignReadErr(err) {
			logger.Debug("generic v2: initial client read failed",
				zap.Error(err))
		}
		return nil
	}

	// Seed the events channel with the initial client chunk so the
	// main loop's state machine treats it like any other.
	if len(initial.Bytes) > 0 {
		events <- chunkEvent{
			dir:       fakeconn.FromClient,
			bytes:     initial.Bytes,
			connSeq:   initial.ConnSeq,
			readAt:    initial.ReadAt,
			writtenAt: initial.WrittenAt,
		}
	}

	// Reader goroutines: one per direction. Each loop reads chunks from
	// its FakeConn and forwards them onto the shared events channel
	// until EOF / ErrClosed. We count their exits via a WaitGroup-style
	// pattern (two reads off readerDone) and close the events channel
	// only after both have exited so the main loop never observes a
	// "close before drain" race.
	readerDone := make(chan struct{}, 2)
	go readStream(sess.ClientStream, fakeconn.FromClient, events, readerDone)
	go readStream(sess.DestStream, fakeconn.FromDest, events, readerDone)

	// closer goroutine: waits for both readers to return, then closes
	// events so the main-loop range exits exactly once. Without this,
	// the main loop would need its own termination condition based on
	// per-side done flags, which races against in-flight data events
	// that landed in the buffer before the EOF event.
	go func() {
		<-readerDone
		<-readerDone
		close(events)
	}()

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
	emit := func(m *models.Mock) {
		// An ended session's context would discard an ordinary emit of what
		// the recorder held until it ended.
		send := sess.EmitMock
		if ctx.Err() != nil {
			send = sess.EmitMockOnShutdown
		}
		if err := send(m); err != nil && logger != nil {
			logger.Debug("generic v2: EmitMock returned error", zap.Error(err))
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
	flushMock := func() (leftOutFor string, leftOut bool) {
		if len(genericRequests) == 0 || len(genericResponses) == 0 {
			return "", false
		}
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
			return reason, true
		}
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
		// EmitMock runs sess.OnMockRecorded before sending on sess.Mocks
		// and short-circuits if the incomplete flag is set. Any error
		// from EmitMock (ctx cancellation mid-send) is logged but does
		// not stop the loop — the ctx will close the streams shortly
		// after and the top-level for-loop will exit on EOF / ErrClosed.
		if err := sess.EmitMock(mock); err != nil && logger != nil {
			logger.Debug("generic v2: EmitMock returned error", zap.Error(err))
		}
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
		var ev chunkEvent
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
		case e, ok := <-events:
			if !ok {
				break drain
			}
			ev = e
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
		// The connection's life, by its chunks' recorded times: a held
		// first exchange is the preamble once a chunk comes after the
		// answer of the test that owns it.
		at := ev.writtenAt
		if ev.dir == fakeconn.FromClient {
			at = ev.readAt
		}
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
			// Back-stop: if the previous completed req/resp exchange
			// was not already flushed (the common case flushes on the
			// first response chunk below), flush it now before we
			// start a new exchange. Mirrors the legacy encoder.
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				flushMock()
			}
			// Starting a brand-new exchange: drop any orphaned
			// response chunks from a previous multi-chunk server
			// reply whose head chunk was already flushed, and anchor
			// the request timestamp to THIS chunk's ReadAt so we
			// record when bytes actually hit the relay rather than
			// when the parser got around to pairing them.
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
					genericResponses = nil
					resHeadSeq = 0
					resHeadWrittenAt = time.Time{}
				}
				reqTimestampMock = ev.readAt
				carriedBySplit = false
			}
			genericRequests = append(genericRequests, encodePayload(ev.bytes, models.FromClient))
			reqSeq = append(reqSeq, ev.connSeq)
			reqReadAt = append(reqReadAt, ev.readAt)
			prevChunkWasReq = true
			if answersThis {
				// The exchange is already complete — flush it now so
				// the next request starts clean, exactly as it would
				// have had the two chunks arrived in wire order. That
				// means the HEAD chunk only, with the rest of a reply
				// the server split across chunks dropped: in wire
				// order the head flushes the exchange and the tail is
				// orphaned. Carrying the tail in as well would emit a
				// multi-response mock this parser has never produced,
				// and replay writes every response buffer back — so
				// the bytes a test replays would depend on which way
				// the relay's two goroutines happened to be scheduled.
				heldChunks := len(genericResponses)
				genericResponses = genericResponses[:1]
				resTimestampMock = resHeadWrittenAt
				if logger != nil {
					logger.Debug("generic v2: paired a request with the response that arrived ahead of it",
						zap.Uint32("requestConnSeq", ev.connSeq),
						zap.Uint32("responseConnSeq", resHeadSeq),
						zap.Time("requestReadAt", ev.readAt),
						zap.Time("responseWrittenAt", resHeadWrittenAt),
						zap.Int("responseChunksHeld", heldChunks),
						zap.Int("responseChunksKept", len(genericResponses)))
				}
				flushMock()
				prevChunkWasReq = false
			}

		case fakeconn.FromDest:
			// The half a split carried forward is an exchange this
			// parser created, so it owes the same precondition the
			// split itself is held to: a response captured before the
			// request cannot be its answer. In wire order that chunk
			// is the tail of the PREVIOUS reply and is dropped;
			// pairing it here instead records an answer this
			// request never got — and generic mocks are session-scoped
			// (Metadata type "config"), so replay's window filter short
			// -circuits them into the unfiltered pool BEFORE its
			// response-precedes-request check. Nothing downstream
			// catches this; it replays.
			if carriedBySplit && len(reqSeq) > 0 {
				if resFirst, _ := order(stamp{ev.connSeq, ev.writtenAt}, stamp{reqSeq[0], reqReadAt[0]}); resFirst {
					if logger != nil {
						logger.Debug("generic v2: dropped a response older than the request a split carried forward",
							zap.Uint32("responseConnSeq", ev.connSeq),
							zap.Uint32("requestConnSeq", reqSeq[0]),
							zap.Time("responseWrittenAt", ev.writtenAt),
							zap.Time("requestReadAt", reqReadAt[0]))
					}
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
			if len(genericResponses) == 0 {
				resHeadSeq = ev.connSeq
				resHeadWrittenAt = ev.writtenAt
			}
			genericResponses = append(genericResponses, encodePayload(ev.bytes, models.FromServer))
			// Per the migration spec, ResTimestampMock is WrittenAt of
			// the last dest chunk for the matching response — always
			// overwrite so that if the response were to accumulate
			// multiple chunks before flush, the last wins.
			resTimestampMock = ev.writtenAt

			// Flush the moment the first response chunk for an
			// outstanding request arrives. This makes the mock
			// visible BEFORE the next request (which may be seconds
			// away on a pooled connection) so the syncMock buffer
			// can associate it with the currently-active test.
			if prevChunkWasReq && len(genericRequests) > 0 {
				flushMock()
			}
			prevChunkWasReq = false
		}
	}

	// Final flush for any in-flight exchange that had at least one
	// request and one response chunk by the time streams closed.
	flushMock()
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
// always do). A response's WrittenAt is stamped once the relay's write of
// it to the application returns, and the application can act on the bytes
// before that: the write wakes it, and the forwarder may get its CPU back
// only after the application has sent its next request and the other
// forwarder has read it. A comparison of the two stamps then puts that
// request before the response that unblocked it, the split does not
// happen, and the two commands land in one mock that replay cannot match
// (go-redis's CLIENT SETINFO and PING, CI's mock-mismatch-streaming lane).
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

// readStream pumps Chunks from one FakeConn onto the shared events
// channel. It returns on EOF / ErrClosed / deadline-exceeded; the
// supervisor.Session's ctx is observed indirectly: when the ctx is
// cancelled the relay closes the FakeConn streams, which surfaces
// here as ErrClosed.
func readStream(fc *fakeconn.FakeConn, dir fakeconn.Direction, out chan<- chunkEvent, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	if fc == nil {
		out <- chunkEvent{dir: dir, err: io.EOF}
		return
	}
	for {
		c, err := fc.ReadChunk()
		if err != nil {
			out <- chunkEvent{dir: dir, err: err}
			return
		}
		// Empty chunk with no error shouldn't happen on a real stream
		// but guard against it so the consumer never stalls.
		if len(c.Bytes) == 0 {
			continue
		}
		out <- chunkEvent{
			dir:       dir,
			bytes:     c.Bytes,
			connSeq:   c.ConnSeq,
			readAt:    c.ReadAt,
			writtenAt: c.WrittenAt,
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
