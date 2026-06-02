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
	// chunks followed by one or more dest chunks." The relay only
	// surfaces a dest chunk AFTER the client chunk that caused it has
	// been forwarded (causality), so in production the first event on
	// ClientStream strictly precedes anything on DestStream. We match
	// that invariant here by reading the initial client chunk on the
	// calling goroutine before starting the concurrent reader pair —
	// otherwise a race between the two reader goroutines could observe
	// a dest event before its paired client event on synthetic inputs
	// where both streams are pre-primed.
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

	var (
		genericRequests  []models.Payload
		genericResponses []models.Payload
		reqTimestampMock time.Time
		resTimestampMock time.Time
		// prevChunkWasReq tracks request→response transitions so we know
		// when to flush. Mirrors the legacy encoder's state machine.
		prevChunkWasReq = false
	)

	flushMock := func() {
		if len(genericRequests) == 0 || len(genericResponses) == 0 {
			return
		}
		// Drop the in-flight mock if the relay has flagged it incomplete
		// (dropped chunk upstream, memory pressure, short write, etc.).
		// EmitMock also honours this flag, but checking here avoids
		// building and allocating the mock for nothing.
		if sess.IsMockIncomplete() {
			genericRequests = nil
			genericResponses = nil
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
			return
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
		reqTimestampMock = time.Time{}
		resTimestampMock = time.Time{}
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
			if len(genericRequests) == 0 {
				genericResponses = nil
				reqTimestampMock = ev.readAt
			}
			genericRequests = append(genericRequests, encodePayload(ev.bytes, models.FromClient))
			prevChunkWasReq = true

		case fakeconn.FromDest:
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
