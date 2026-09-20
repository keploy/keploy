package generic

import (
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
	// readAt is the timestamp at which the relay read this chunk off the
	// source socket. Used as ReqTimestampMock for the first client chunk
	// of each exchange.
	readAt time.Time
	// writtenAt is the timestamp at which the relay wrote this chunk to
	// the opposite socket. Used as ResTimestampMock for the response
	// head chunk of each exchange — it reflects when the client actually
	// observed the bytes, which matches the "last dest chunk WrittenAt"
	// rule from the migration spec (there is only one response chunk per
	// mock by construction in the generic parser, so "last" == "this").
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
	// below read the relay's timestamps instead of trusting the order
	// these events turn up in.
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
		// reqReadAt carries the relay's ReadAt for each buffer in
		// genericRequests, same index. It is what lets a late-pushed
		// response be attributed to the request it actually answered
		// rather than to whatever happened to arrive first.
		reqReadAt []time.Time
		// resHeadWrittenAt is the WrittenAt of the first response chunk
		// currently held — the moment the client could first see a reply.
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

	// flushMock reports whether the mock was DROPPED rather than emitted.
	// The split path needs that answer: a capture the relay holed must
	// lose its carried half too, and the incomplete flag can be set by
	// the relay's goroutine after any check this function's caller made.
	flushMock := func() (dropped bool) {
		if len(genericRequests) == 0 || len(genericResponses) == 0 {
			return false
		}
		// Drop the in-flight mock if the relay has flagged it incomplete
		// (dropped chunk upstream, memory pressure, short write, etc.).
		// EmitMock also honours this flag, but checking here avoids
		// building and allocating the mock for nothing.
		if sess.IsMockIncomplete() {
			genericRequests = nil
			genericResponses = nil
			reqReadAt = nil
			resHeadWrittenAt = time.Time{}
			reqTimestampMock = time.Time{}
			resTimestampMock = time.Time{}
			// Clear the incomplete flag so the next cycle has a fresh
			// chance, matching EmitMock's own reset semantics.
			sess.MarkMockComplete()
			// Clear pending work — the parser has consumed the input
			// even though the mock is being abandoned. Without this
			// the hang watchdog stays armed on the supervisor side
			// and can fire spurious aborts after the connection goes
			// idle. EmitMock's drop path does the same; this early
			// return would skip it if we didn't replicate it here.
			if sess.OnPendingCleared != nil {
				sess.OnPendingCleared()
			}
			return true
		}

		metadata := map[string]string{
			"type": "config",
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
		// EmitMock runs sess.OnMockRecorded before sending on sess.Mocks
		// and short-circuits if the incomplete flag is set. Any error
		// from EmitMock (ctx cancellation mid-send) is logged but does
		// not stop the loop — the ctx will close the streams shortly
		// after and the top-level for-loop will exit on EOF / ErrClosed.
		if err := sess.EmitMock(mock); err != nil && logger != nil {
			logger.Debug("generic v2: EmitMock returned error", zap.Error(err))
		}
		genericRequests = nil
		genericResponses = nil
		reqReadAt = nil
		resHeadWrittenAt = time.Time{}
		reqTimestampMock = time.Time{}
		resTimestampMock = time.Time{}
		return false
	}

	// Drain every event the two reader goroutines produce. The closer
	// goroutine above closes `events` once both readers return, which
	// is the only termination signal this loop observes.
	for ev := range events {
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

		// Ignore spurious empty chunks — they carry no payload and
		// their timestamps would bias the pairing logic.
		if len(ev.bytes) == 0 {
			continue
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
			// Orphaned is decided on the wire clock, not on arrival.
			// A single held response the client could not have seen
			// until AFTER this request was read is not the leftover of
			// an earlier reply — on the wire it comes second — so the
			// two are one exchange, teed out of order (the relay
			// forwards bytes before it tees them, so either direction
			// can overtake the other). Dropping it here would leave
			// this request to pair with the NEXT response instead.
			answersThis := len(genericRequests) == 0 &&
				len(genericResponses) > 0 &&
				!ev.readAt.IsZero() &&
				ev.readAt.Before(resHeadWrittenAt)
			if len(genericRequests) == 0 {
				if !answersThis {
					genericResponses = nil
					resHeadWrittenAt = time.Time{}
				}
				reqTimestampMock = ev.readAt
				carriedBySplit = false
			}
			genericRequests = append(genericRequests, encodePayload(ev.bytes, models.FromClient))
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
			// split itself is held to: a response written before the
			// request was read cannot be its answer. In wire order
			// that chunk is the tail of the PREVIOUS reply and is
			// dropped; pairing it here instead records an answer this
			// request never got — and generic mocks are session-scoped
			// (Metadata type "config"), so replay's window filter short
			// -circuits them into the unfiltered pool BEFORE its
			// response-precedes-request check. Nothing downstream
			// catches this; it replays.
			if carriedBySplit && len(reqReadAt) > 0 &&
				!ev.writtenAt.IsZero() && !reqReadAt[0].IsZero() &&
				ev.writtenAt.Before(reqReadAt[0]) {
				if logger != nil {
					logger.Debug("generic v2: dropped a response older than the request a split carried forward",
						zap.Time("responseWrittenAt", ev.writtenAt),
						zap.Time("requestReadAt", reqReadAt[0]))
				}
				continue
			}
			// A client cannot have sent a request until after it saw
			// this response, so on the wire the two are in different
			// exchanges however they reached us. Cut there: keep the
			// requests that precede the response, carry the rest
			// forward. Without this, a reply teed after the request it
			// unblocked merges two commands into one mock and drops
			// the second reply — and replay, which matches on the
			// NUMBER of request buffers, then finds nothing for either
			// command, which is how a recorded app dies at startup.
			//
			// This restores wire order; it does not make an opaque
			// stream understood. The parser still pairs by direction
			// transitions, so bytes the server sent unbidden — a push,
			// the tail of a multi-chunk reply — are attributed to
			// whatever request precedes them, wrongly, exactly as they
			// were before any of this (TestV2_ServerPushIsPairedThe
			// SameEitherWay walks one such stream both ways). Fixing
			// THAT needs a parser that frames the protocol, not a
			// clock.
			//
			// A connection that lost a chunk drops its whole in-flight
			// exchange (flushMock below). Splitting first would keep
			// the tail half of a capture already known to be holed.
			if len(genericRequests) > 1 {
				if i := exchangeSplit(reqReadAt, ev.writtenAt); i > 0 {
					heldReqs := append([]models.Payload(nil), genericRequests[i:]...)
					heldAt := append([]time.Time(nil), reqReadAt[i:]...)
					genericRequests = genericRequests[:i]
					reqReadAt = reqReadAt[:i]
					genericResponses = append(genericResponses, encodePayload(ev.bytes, models.FromServer))
					resTimestampMock = ev.writtenAt
					if flushMock() {
						// The relay holed this capture — possibly
						// between our check and this flush, so the
						// answer has to come from the flush itself.
						// Let the carried half go with it rather
						// than record the surviving half of a
						// recording known to be missing bytes.
						prevChunkWasReq = false
						continue
					}
					genericRequests = heldReqs
					reqReadAt = heldAt
					reqTimestampMock = heldAt[0]
					carriedBySplit = true
					if logger != nil {
						logger.Debug("generic v2: split an exchange the response arrived behind",
							zap.Int("splitAfterRequests", i),
							zap.Int("requestsCarriedForward", len(heldReqs)),
							zap.Time("responseWrittenAt", ev.writtenAt),
							zap.Time("nextRequestReadAt", heldAt[0]))
					}
					continue
				}
			}
			if len(genericResponses) == 0 {
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
	return nil
}

// exchangeSplit reports where a response's arrival proves that the
// requests held in flight are really two exchanges: the index of the
// first request the client cannot have sent until after resWrittenAt,
// the moment that response reached it. It returns 0 for "no evidence",
// which is also the answer whenever any timestamp needed for the
// comparison is missing — an untimestamped chunk cannot be placed on
// either side of a response, and guessing is how a recorder starts
// emitting mocks nobody can explain.
//
// Index 0 is never a split point. A response that predates even the
// FIRST held request is not this exchange's answer, and cutting there
// would emit a pair that never happened on the wire. Replay will not
// catch it: generic mocks are recorded session-scoped, and the window
// filter routes those to the unfiltered pool before it ever compares
// their two timestamps.
func exchangeSplit(reqReadAt []time.Time, resWrittenAt time.Time) int {
	if len(reqReadAt) == 0 {
		return 0
	}
	// Covers an unstamped response too: every real instant is After the
	// zero time, so a zero resWrittenAt returns here.
	if reqReadAt[0].IsZero() || reqReadAt[0].After(resWrittenAt) {
		return 0
	}
	for i := 1; i < len(reqReadAt); i++ {
		if reqReadAt[i].IsZero() {
			return 0
		}
		if reqReadAt[i].After(resWrittenAt) {
			return i
		}
	}
	return 0
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
