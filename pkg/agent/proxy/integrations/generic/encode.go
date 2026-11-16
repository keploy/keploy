package generic

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// capturedChunk is a chunk one direction of the connection carried.
type capturedChunk struct {
	fromClient bool
	buf        []byte
}

// captureStream is the connection's captured chunks, both directions on one
// channel in the order the tees queued them. Each tee queues a chunk before
// it forwards it (captureTeeWriter.Write), and an answer can only come after
// what it answers was forwarded, so the stream holds a request before its
// answer. Two channels, one per direction, did not: a tee that forwarded a
// request and was then outrun by the answer's tee queued the request second,
// and the mock maker, reading the answer first, dropped the exchange.
//
// A chunk that does not fit, or a recording that pauses, stops the capture
// of both directions: an exchange missing a chunk of either is not one. The
// channel is closed once, when capture stops or both directions have ended.
type captureStream struct {
	ch chan capturedChunk

	mu      sync.Mutex
	stopped bool // nothing more is queued; ch is closed
	open    int  // directions not ended yet
}

func newCaptureStream(size, directions int) *captureStream {
	return &captureStream{ch: make(chan capturedChunk, size), open: directions}
}

// add queues a copy of p, carried in the direction fromClient says, without
// waiting: a full channel stops the capture.
func (c *captureStream) add(fromClient bool, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if memoryguard.IsRecordingPaused() {
		c.stopLocked()
		return
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case c.ch <- capturedChunk{fromClient: fromClient, buf: buf}:
	default:
		c.stopLocked()
	}
}

// stopLocked ends the capture, c.mu held: the mock maker gets what was
// queued, then the end.
func (c *captureStream) stopLocked() {
	if !c.stopped {
		c.stopped = true
		close(c.ch)
	}
}

// ended notes that one direction has ended; the last to end ends the capture.
func (c *captureStream) ended() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open--; c.open <= 0 {
		c.stopLocked()
	}
}

// captureTeeWriter forwards data to dest (critical path) and tees a copy to
// the connection's capture stream without waiting (captureStream.add).
type captureTeeWriter struct {
	dest       io.Writer
	stream     *captureStream
	fromClient bool

	// n counts bytes that actually reached dest. The drain polls it from
	// the caller's goroutine while this tee is still being written by its
	// own copy goroutine, so it has to be atomic.
	n atomic.Int64
}

// delivered reports how many bytes this direction has handed on.
func (w *captureTeeWriter) delivered() int64 { return w.n.Load() }

func (w *captureTeeWriter) Write(p []byte) (int, error) {
	// Queued before it is forwarded, so that its answer, which can only
	// come once it is forwarded, is queued after it (captureStream). A chunk
	// whose forwarding then fails is captured all the same: the exchange it
	// begins has no answer, and no mock is made of it.
	w.stream.add(w.fromClient, p)
	n, err := w.dest.Write(p)
	w.n.Add(int64(n))
	return n, err
}

func encodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	return encodeGenericWithDrain(ctx, logger, reqBuf, clientConn, destConn, mocks, opts, proxyutil.RelayDrainIdle)
}

// encodeGenericWithDrain is encodeGeneric with the survivor drain's idle
// window as a parameter, so tests can drive the drain without waiting out
// the production window. Production has exactly one caller, above, and it
// always passes proxyutil.RelayDrainIdle.
func encodeGenericWithDrain(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions, drainIdle time.Duration) error {

	// Forward initial request buffer to destination immediately.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}

	// If recording is already paused, pure passthrough.
	if memoryguard.IsRecordingPaused() {
		return forwardBidirectional(clientConn, destConn)
	}

	// The capture stream, both directions in order (captureStream); a
	// background goroutine reads it to create mocks. Buffered to absorb
	// brief processing delays without blocking forwarding.
	stream := newCaptureStream(512, 2)

	// The initial request buffer, forwarded above before either copy began,
	// so before any answer to it was read: first on the stream.
	stream.add(true, reqBuf)

	// Start background mock creator.
	go createGenericMocksAsync(ctx, logger, stream.ch)

	// Forward bidirectionally at wire speed with non-blocking capture tee.
	// Each direction ends on the stream once, by whichever comes first: its
	// copy's end, or the deferred end of a copy that panicked.
	clientEnded := &sync.Once{}
	destEnded := &sync.Once{}

	// Buffered and error-carrying: a broken direction must not strand the
	// other. The two directions do NOT necessarily fail together — these are
	// SafeConns over *tls.Conn on TLS-intercepted paths, and tls.Conn keeps
	// its read and write halves' errors separate — so the survivor may still
	// have decrypted bytes to deliver. Give it an idle window rather than
	// waiting forever or cutting it off at once.
	//
	// The result carries the direction's index, not just its error. The
	// survivor to drain is whichever direction did NOT report, and a bare
	// error cannot say which that is — guessing watches the byte counter
	// of the direction that just died, which never moves again, and
	// collapses the idle drain back into a single fixed wait.
	done := make(chan copyOutcome, 2)
	// Built before either goroutine starts: the drain reads whichever tee
	// belongs to the survivor.
	tees := [2]*captureTeeWriter{
		{dest: destConn, stream: stream, fromClient: true},
		{dest: clientConn, stream: stream},
	}
	go func() {
		defer clientEnded.Do(stream.ended)
		_, err := io.Copy(tees[0], clientConn)
		// End the direction on the stream BEFORE the FIN: forwardFIN can
		// write a close_notify on a TLS conn, and SafeConn's write deadline
		// is a no-op, so a stalled peer would otherwise hold
		// createGenericMocksAsync open behind it. The deferred Do is a
		// sync.Once, so this is the early half of the same end.
		clientEnded.Do(stream.ended)
		forwardFIN(destConn, err)
		done <- copyOutcome{idx: 0, err: err}
	}()
	go func() {
		defer destEnded.Do(stream.ended)
		_, err := io.Copy(tees[1], destConn)
		destEnded.Do(stream.ended)
		forwardFIN(clientConn, err)
		done <- copyOutcome{idx: 1, err: err}
	}()

	for i := 0; i < 2; i++ {
		res := <-done
		if res.err == nil {
			continue
		}
		// Both results are already in hand on the second iteration — there
		// is nothing left to drain, and waiting would hold the caller for a
		// full idle window on the very common "one side ends cleanly, the
		// other resets".
		if i == 1 {
			return nil
		}
		// Give the surviving direction a chance to finish. Returning at once
		// would let the caller's deferred close truncate it, and on this
		// path that costs the user's bytes AND the mocks the tee captured.
		// Waiting a fixed grace instead is the same bug wearing a longer
		// coat: it cuts off a response that is still arriving, just later.
		// Wait while it is delivering, and stop when it stalls.
		//
		// A copy error is a peer/socket condition, not a parser fault, so
		// the dispatcher still sees nil.
		proxyutil.WaitForSurvivor(done, tees[1-res.idx].delivered, drainIdle)
		return nil
	}

	return nil
}

// copyOutcome reports which forwarding direction ended, and how.
type copyOutcome struct {
	idx int
	err error
}

// forwardFIN half-closes dst when the copy that fed it ended cleanly.
//
// A finished io.Copy means this side sent shutdown(SHUT_WR) — it has no
// more to say — not that the connection is over. An EOF-delimited
// protocol only learns the request ended when the FIN arrives, so
// without this the peer waits for bytes that will never come while the
// client waits for a reply it will never send, and the exchange
// deadlocks until something external tears it down.
//
// copyErr is the gate, and it is not decoration. io.Copy returns nil
// only on a clean EOF; a read error, a reset, or a failed write to the
// real peer all exit the same loop. Forwarding a FIN for those would
// tell the peer "the request is complete" about a message that was
// truncated — and generic is the catch-all for mongo wire and arbitrary
// binary RPC, where acting on a partial message can mean committing a
// partial operation. A broken copy falls through to the ordinary
// teardown instead.
func forwardFIN(dst net.Conn, copyErr error) {
	if copyErr != nil {
		return
	}
	_ = proxyutil.CloseWriteIfPossible(dst)
}

// forwardBidirectional does raw TCP passthrough without any capture.
func forwardBidirectional(clientConn, destConn net.Conn) error {
	// This is generic's memory-pressure fallback and was a verbatim copy of
	// http's and mysql's. It gets the shared implementation — including the
	// bounded drain, without which a broken direction either wedges the
	// caller or truncates the surviving one.
	proxyutil.RelayRawPassthrough(clientConn, destConn)
	return nil
}

// createGenericMocksAsync reads captured data from both channels and creates
// mock entries based on request-response alternation. Runs in a background
// goroutine — never blocks the forwarding path.
//
// Exchange-boundary detection: a request-response pair is flushed to the
// syncMock buffer the instant the first response chunk arrives, not when
// the next request starts. Flushing on "next request" is too late for
// pooled connections — the next request can be seconds away (mongo driver
// checkout/heartbeat pools), by which point the enterprise capture has
// already run ResolveRange for the test and the 7-second buffer cutoff in
// syncMock.ResolveRange discards the pending mock. The consequence was
// that in static-dedup mode every test after the first recorded zero
// mocks.
//
// The tradeoff: a server reply split into multiple TCP read chunks lands
// as a single mock here (only the first chunk), with trailing chunks
// dropped. For the protocols generic is a fallback for (mongo wire,
// various binary RPC), responses are almost always a single write and
// therefore one io.Copy chunk. A future protocol-aware parser should
// supersede generic where framing matters.
func createGenericMocksAsync(ctx context.Context, logger *zap.Logger, chunks <-chan capturedChunk) {
	var genericRequests []models.Payload
	var genericResponses []models.Payload
	prevChunkWasReq := true // first chunk is always a request (initial reqBuf)
	reqTimestampMock := models.CapturedReqTime(ctx)
	var resTimestampMock time.Time
	// exchanges counts the connection's exchanges flushed so far
	// (exchangeLifetime).
	exchanges := 0
	emit := func(m *models.Mock) {
		if mgr := syncMock.FromContextOrGlobal(ctx); mgr != nil {
			mgr.AddMock(m)
		}
	}
	// hold is the connection's first exchange, and those after it, while the
	// test that owns its request may own the whole connection (preambleHold).
	// lastAt is the connection's latest chunk time.
	var hold *preambleHold
	var lastAt time.Time
	// A parser that leaves holding (a panic the supervisor recovers) takes
	// its hold off the stop's count: what it held is lost with it, and the
	// stop is not to wait on it forever.
	defer func() {
		if hold != nil {
			hold.handedOn()
		}
	}()
	release := func(wholly bool) {
		if hold == nil {
			return
		}
		hold.handOn(wholly, emit)
		hold = nil
	}

	flushMock := func() {
		if len(genericRequests) == 0 || len(genericResponses) == 0 {
			return
		}
		n := exchanges
		exchanges++
		metadata := make(map[string]string)
		metadata["type"] = "config"
		metadata[models.MetadataLifetimeKey] = exchangeLifetime(n)
		if connID, ok := ctx.Value(models.ClientConnectionIDKey).(string); ok {
			metadata["connID"] = connID
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
			// Generic TCP has no well-defined protocol for the recorder to
			// classify commands against; the legacy recorder tags every
			// exchange Metadata["type"]="config" which DeriveLifetime
			// previously resolved to LifetimeSession. Stamp that explicitly
			// so the filter layer's authoritative routing (Lifetime-first,
			// metadata.type fallback) short-circuits on a single enum
			// compare — no map probe, no kind fallback.
			TestModeInfo: models.TestModeInfo{
				Lifetime:        models.LifetimeSession,
				LifetimeDerived: true,
			},
		}
		switch {
		case hold != nil:
			hold.add(mock)
			if hold.outlived() {
				release(false)
			}
		case n == 0:
			if hold = holdPreamble(ctx, mock, mock.Spec.ReqTimestampMock, lastAt); hold == nil {
				emit(mock)
			} else if hold.outlived() {
				release(false)
			}
		default:
			emit(mock)
		}
		genericRequests = nil
		genericResponses = nil
	}

	// saw records a chunk at at, and releases a held first exchange once the
	// connection has outlived the test that owns it.
	saw := func(at time.Time) {
		if at.After(lastAt) {
			lastAt = at
		}
		if hold != nil {
			hold.saw(at)
			if hold.outlived() {
				release(false)
			}
		}
	}

	// fromClient takes a chunk the client sent.
	fromClient := func(buf []byte) {
		// Back-stop for the rare case where the previous completed
		// request/response exchange was not flushed when its first
		// response chunk arrived. When the next client chunk shows
		// up, flush that already-paired exchange before starting a
		// new one. The common case is flushed below on the first
		// response chunk, so this branch is normally a no-op.
		if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
			flushMock()
		}
		// Starting a brand-new exchange: drop any orphaned response
		// chunks from a previous multi-chunk server reply whose
		// head chunk was already flushed, and reset the request
		// timestamp to this request's arrival time.
		if len(genericRequests) == 0 {
			genericResponses = nil
			reqTimestampMock = models.CapturedReqTime(ctx)
		}
		genericRequests = append(genericRequests, encodePayload(buf, models.FromClient))
		prevChunkWasReq = true
		saw(models.CapturedReqTime(ctx))
	}
	// fromDest takes a chunk the destination sent.
	fromDest := func(buf []byte) {
		genericResponses = append(genericResponses, encodePayload(buf, models.FromServer))
		resTimestampMock = models.CapturedRespTime(ctx)
		saw(resTimestampMock)

		// Flush the moment the first response chunk for an
		// outstanding request arrives. This makes the mock visible
		// to the syncMock buffer BEFORE the enterprise capture's
		// ResolveRange call fires for the current test, which in
		// static-dedup mode is what associates the mock with the
		// right test window. Waiting any longer (for the next
		// client chunk or an idle timer) misses the window on
		// pooled connections where the next request is seconds
		// away.
		if prevChunkWasReq && len(genericRequests) > 0 {
			flushMock()
		}
		prevChunkWasReq = false
	}

	// take takes the stream's next chunk.
	take := func(c capturedChunk) {
		if c.fromClient {
			fromClient(c.buf)
		} else {
			fromDest(c.buf)
		}
	}

	for chunks != nil {
		var changes, stopping <-chan struct{}
		if hold != nil {
			changes, stopping = hold.changes(), hold.stopping
		}
		select {
		case <-changes:
			hold.refresh()
			if hold.outlived() {
				release(false)
			}
		case <-stopping:
			// The recording's stop has begun: decided with what is known.
			release(hold.wholly())
		case <-ctx.Done():
			// The connection's context can end while chunks its copies
			// captured are still queued here: a short connection whose
			// caller returns, and cancels, as soon as both copies end.
			// They are its last exchange, so they are taken first, in the
			// stream's order. Taking only what is queued, it never waits
			// on a copy still running.
			drainBuffered(chunks, take)
			flushMock()
			if hold != nil {
				release(hold.wholly())
			}
			return
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			take(c)
		}
	}
	flushMock()
	// The connection has ended: its held first exchange is the test's when
	// its whole lay inside it, known once the test's answer is.
	if hold != nil {
		hold.settle(ctx)
		release(hold.wholly())
	}
}

// drainBuffered hands take every chunk ch holds now, without waiting for
// more; a nil ch holds none.
func drainBuffered(ch <-chan capturedChunk, take func(capturedChunk)) {
	for {
		select {
		case buf, ok := <-ch:
			if !ok {
				return
			}
			take(buf)
		default:
			return
		}
	}
}

// binaryPayload is the Type of a payload whose Data is its bytes in base64
// (encodePayload): any piece that is not wholly ASCII.
const binaryPayload = "binary"

func encodePayload(buf []byte, origin models.OriginType) models.Payload {
	bufStr := string(buf)
	dataType := models.String
	if !util.IsASCII(string(buf)) {
		bufStr = util.EncodeBase64(buf)
		dataType = binaryPayload
	}
	return models.Payload{
		Origin: origin,
		Message: []models.OutputBinary{
			{
				Type: dataType,
				Data: bufStr,
			},
		},
	}
}
