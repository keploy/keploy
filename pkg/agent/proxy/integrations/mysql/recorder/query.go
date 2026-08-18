package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// mysqlDecodeItem carries a forwarded chunk to the async decode goroutine.
type mysqlDecodeItem struct {
	fromClient bool
	data       []byte
	ts         time.Time
}

// handleClientQueries handles the MySQL command phase with non-blocking
// forwarding. Raw bytes are relayed at wire speed in the main select loop.
// All packet reassembly, decoding, and mock creation is offloaded to a
// background goroutine via a buffered decode channel.
// tsChunk is a relay chunk tagged with the kernel CLOCK_MONOTONIC wire time of
// the SSL/GoTLS event it came from. handleClientQueries merges the two
// directional relay streams by this ktime so the decoder sees packets in true
// WIRE ORDER — the request→response→next-request order the MySQL state machine
// requires — instead of the goroutine-arrival order a plain select races. On the
// in-memory SimulatedConn loopback that race dropped query mocks for fast
// connection-pool-validation exchanges (SELECT 1).
type tsChunk struct {
	data  []byte
	ktime uint64
}

// ktimeConn is the optional interface a capture-backed net.Conn implements to
// expose each read chunk's kernel wire time (enterprise SimulatedConn does).
// When a conn doesn't implement it (unit tests, non-capture paths) the relay
// falls back to plain Read with ktime 0 and the merge degrades to arrival order
// — i.e. exactly the prior behavior, no regression.
type ktimeConn interface {
	ReadTimestamped(b []byte) (n int, ktimeNs uint64, err error)
}

func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {
	// If recording is already paused, pure passthrough.
	if memoryguard.IsRecordingPaused() {
		logger.Debug("memory pressure detected, stopping MySQL recording and falling back to passthrough")
		done := make(chan struct{}, 2)
		cp := func(dst, src net.Conn) {
			_, _ = io.Copy(dst, src)
			done <- struct{}{}
		}
		go cp(destConn, clientConn)
		go cp(clientConn, destConn)
		<-done
		<-done
		return nil
	}

	// Buffered channels for raw byte relay. Each Read() result is sent
	// immediately — no accumulation, no "wait for short read" like ReadBytes.
	clientBuffChan := make(chan tsChunk, 256)
	destBuffChan := make(chan tsChunk, 256)
	errChan := make(chan error, 2)

	// readRelay reads from conn in a loop and sends each chunk to ch.
	// Returns on error, EOF, or context cancellation.
	//
	// Critical invariant: if Read returns bytes, those bytes MUST land
	// on ch even if ctx is already canceled. The previous shape used
	// `select { case ch <- data: case <-ctx.Done(): }` which on a
	// concurrent cancel+ch-ready state would non-deterministically
	// pick the ctx arm and drop the chunk — the dominant root cause of
	// the "record-side packet drop during fast back-to-back ops" bug.
	// Now the send is unconditional onto a buffered channel (cap 256);
	// ctx is checked only AFTER the send so the goroutine still exits
	// promptly without losing the chunk we already pulled off the wire.
	readRelay := func(conn net.Conn, ch chan<- tsChunk) {
		defer close(ch)
		tsc, hasTS := conn.(ktimeConn)
		buf := make([]byte, 32*1024) // reused across reads
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			var n int
			var ktime uint64
			var err error
			if hasTS {
				n, ktime, err = tsc.ReadTimestamped(buf)
			} else {
				n, err = conn.Read(buf)
			}
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				chunk := tsChunk{data: data, ktime: ktime}
				// Buffered channel send; in the rare event the buffer
				// is full we fall back to a select that respects ctx
				// so we don't deadlock on shutdown. The fast path
				// avoids that select entirely so a ctx-cancel that
				// races a successful Read can't preempt the send.
				if len(ch) < cap(ch) {
					ch <- chunk
				} else {
					select {
					case ch <- chunk:
					case <-ctx.Done():
						return
					}
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read from connection")
				}
				select {
				case errChan <- err:
				case <-ctx.Done():
				}
				return
			}
		}
	}

	go func() {
		defer pUtil.Recover(logger, clientConn, destConn)
		readRelay(clientConn, clientBuffChan)
	}()
	go func() {
		defer pUtil.Recover(logger, clientConn, destConn)
		readRelay(destConn, destBuffChan)
	}()

	// Async decode channel and background goroutine.
	//
	// The decoder context is intentionally DETACHED from the parent ctx
	// via context.WithoutCancel so the decoder can finish processing any
	// chunks that were already buffered (in clientBuffChan/destBuffChan
	// or decodeChan) at the moment the parent ctx cancels. recordMock's
	// `select { case mocks <- m: case <-ctx.Done(): }` would otherwise
	// race the parent cancel and drop the in-flight mock — exactly the
	// "record-side packet drop during fast back-to-back operations" bug
	// where a /me handler's COM_STMT_PREPARE + COM_STMT_EXECUTE arrive
	// 2-3 ms before the test harness cancels the parser ctx and end up
	// dropped on the floor.
	//
	// The decoder still exits cleanly: closing decodeChan ends its
	// `for item := range decodeChan` loop after it drains and emits any
	// pending mocks. cancelDecoder is kept as a backstop for the rare
	// case where the mocks channel send blocks indefinitely (e.g. the
	// consumer has stopped reading without closing); it is invoked only
	// after the channel close and after the decoder has had a chance to
	// flush, so it cannot pre-empt a recordMock that would otherwise
	// have succeeded.
	decodeChan := make(chan mysqlDecodeItem, 512)
	decodeDone := make(chan struct{})
	decoderCtx, cancelDecoder := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(decodeDone)
		asyncMySQLDecode(decoderCtx, logger, decodeChan, mocks, decodeCtx, clientConn, opts)
	}()

	// forwardClient/forwardDest: always feed bytes to the decoder for
	// connections that started in recording mode. The entry-point check
	// above handles new connections under pressure (passthrough); once
	// recording started, dropping mid-connection bytes creates orphan TCs.
	// Drain-path forwards: block on the DECODER's context (decoderCtx), not the
	// already-cancelled parent ctx, so the last request/response chunks swept in
	// at teardown are delivered reliably instead of dropped when decodeChan is
	// full. decoderCtx stays live through drainBuffChans (it is cancelled only in
	// cleanup step 4, AFTER the drain + decodeChan close), so this blocks until the
	// decoder makes room and never hangs past decoder shutdown.
	forwardClient := func(buf []byte) {
		if buf == nil {
			return
		}
		_, _ = destConn.Write(buf)
		cp := make([]byte, len(buf))
		copy(cp, buf)
		select {
		case decodeChan <- mysqlDecodeItem{fromClient: true, data: cp, ts: models.CapturedReqTime(ctx)}:
		case <-decoderCtx.Done():
		}
	}
	forwardDest := func(buf []byte) {
		if buf == nil {
			return
		}
		_, _ = clientConn.Write(buf)
		cp := make([]byte, len(buf))
		copy(cp, buf)
		select {
		case decodeChan <- mysqlDecodeItem{fromClient: false, data: cp, ts: models.CapturedRespTime(ctx)}:
		case <-decoderCtx.Done():
		}
	}

	// drainBuffChans gives the relay channels a short grace window to
	// surface any chunks the readRelay goroutines are still pushing
	// from their local Read buffer at the moment cancellation fires,
	// then sweeps whatever is currently buffered into the decoder.
	//
	// The wait shape: we accept up to one chunk per side from the
	// grace timer's first tick onward, then break the moment both
	// chans signal empty. This catches the common case (readRelay
	// already had data buffered locally when ctx.Done fired) without
	// stalling the parser exit if the readRelay is genuinely idle.
	drainBuffChans := func() {
		// Grace window for read-relays to surface chunks that were
		// mid-flight at ctx-cancel time. Empirically 50ms was too tight:
		// when the very last DB op of a fast suite (e.g. a chained-CRUD
		// final DELETE → PREPARE + EXECUTE inside one handler tick) is
		// still being decoded by the parser at cancel time, 50ms wasn't
		// enough for the EXECUTE response to surface. 300ms matches the
		// V1 sandbox record's inter-step pacing budget and reliably
		// captures the tail ops.
		const graceWindow = 300 * time.Millisecond
		deadline := time.NewTimer(graceWindow)
		defer deadline.Stop()
		for {
			select {
			case c, ok := <-clientBuffChan:
				if !ok {
					clientBuffChan = nil
					continue
				}
				forwardClient(c.data)
			case c, ok := <-destBuffChan:
				if !ok {
					destBuffChan = nil
					continue
				}
				forwardDest(c.data)
			case <-deadline.C:
				// Grace expired. Greedy non-blocking sweep of any
				// final bytes the goroutines pushed in the last
				// instant, then return.
				for {
					select {
					case c, ok := <-clientBuffChan:
						if !ok {
							clientBuffChan = nil
							continue
						}
						forwardClient(c.data)
					case c, ok := <-destBuffChan:
						if !ok {
							destBuffChan = nil
							continue
						}
						forwardDest(c.data)
					default:
						return
					}
				}
			}
			if clientBuffChan == nil && destBuffChan == nil {
				return
			}
		}
	}

	// cleanup ensures the decode goroutine is stopped before we return.
	// Order matters:
	//   1. Drain any in-flight chunks from the relay channels into the
	//      decoder so a fast cmd/resp pair that arrived right before
	//      cancellation isn't lost at the buff-chan/decode-chan boundary.
	//   2. Close decodeChan so the decoder's `for ... range` loop exits
	//      cleanly after processing remaining items (each item may emit
	//      a mock via recordMock, and the decoder ctx is detached from
	//      the parent so those emits don't race the parent cancel).
	//   3. Wait for the decoder to finish.
	//   4. Cancel the (detached) decoder ctx as a backstop — at this
	//      point the decoder has already exited, so this is a no-op
	//      under normal operation; it exists only to release the
	//      context resources cleanly.
	cleanup := func() {
		// Drain any chunks the read-relays still hold, close the decode
		// channel, and wait for the decoder to finish so no ordering is lost.
		drainBuffChans()
		close(decodeChan)
		<-decodeDone
		cancelDecoder()
	}

	// Main loop: merge the two relay streams into WIRE ORDER by ktime, then feed
	// the decoder. Each readRelay emits in per-stream wire order; merging by the
	// kernel wire time restores the true request→response→next-request interleave
	// the MySQL state machine needs. The previous plain select over the two
	// channels raced them into goroutine-arrival order — which, on the in-memory
	// SimulatedConn loopback (no network latency), delivered a response before its
	// command (dropped as "no pending command") or a COM_QUIT before the prior
	// response (premature empty flush), silently losing query mocks for fast
	// connection-pool-validation exchanges (SELECT 1) and orphaning replay.
	//
	// Lookahead one chunk per side; emit the earlier ktime. When one side has a
	// chunk and the other is open but momentarily empty, wait a bounded window for
	// it (the loopback reorder is sub-millisecond, so this recovers order; a
	// genuinely idle side just times out and we emit what we have — no stall).
	// ktime 0 (non-capture conns / unit tests) ties as smallest and, with the
	// request-first tie-break, degrades to arrival order exactly as before.
	//
	// mergeWait is paid at most once per *exchange*, not per packet: on the
	// loopback the dest side fills within microseconds of a client request, so
	// the wait almost always returns immediately with the counterpart chunk. It
	// only reaches the full 2ms on a genuinely one-sided lull (idle connection),
	// where emitting slightly late costs nothing — so this is not per-packet
	// added latency.
	const mergeWait = 2 * time.Millisecond
	var cHead, dHead tsChunk
	var cHas, dHas bool
	cOpen, dOpen := true, true

	emit := func(fromClient bool, c tsChunk) error {
		if fromClient {
			if _, err := destConn.Write(c.data); err != nil {
				utils.LogError(logger, err, "failed to write command to the server")
				cleanup()
				return err
			}
		} else {
			if _, err := clientConn.Write(c.data); err != nil {
				utils.LogError(logger, err, "failed to write response to the client")
				cleanup()
				return err
			}
		}
		// c.data is a fresh per-chunk buffer allocated in readRelay (never reused
		// after the send), so hand it to the decoder directly — no re-copy.
		ts := models.CapturedReqTime(ctx)
		if !fromClient {
			ts = models.CapturedRespTime(ctx)
		}
		select {
		case decodeChan <- mysqlDecodeItem{fromClient: fromClient, data: c.data, ts: ts}:
		case <-ctx.Done():
		}
		return nil
	}

	for {
		// Non-blocking top-up of each empty head (also detects channel close).
		if !cHas && cOpen {
			select {
			case c, ok := <-clientBuffChan:
				if ok {
					cHead, cHas = c, true
				} else {
					cOpen = false
				}
			default:
			}
		}
		if !dHas && dOpen {
			select {
			case d, ok := <-destBuffChan:
				if ok {
					dHead, dHas = d, true
				} else {
					dOpen = false
				}
			default:
			}
		}

		switch {
		case cHas && dHas:
			// Emit the earlier-wire-time chunk; tie → request first so a command
			// always precedes its response.
			if dHead.ktime < cHead.ktime {
				if err := emit(false, dHead); err != nil {
					return err
				}
				dHas = false
			} else {
				if err := emit(true, cHead); err != nil {
					return err
				}
				cHas = false
			}
		case cHas && !dOpen:
			if err := emit(true, cHead); err != nil {
				return err
			}
			cHas = false
		case dHas && !cOpen:
			if err := emit(false, dHead); err != nil {
				return err
			}
			dHas = false
		case cHas && dOpen:
			// Have a client chunk; dest is open but empty — a smaller-ktime
			// response may be in flight. Wait bounded, then emit.
			timer := time.NewTimer(mergeWait)
			select {
			case d, ok := <-destBuffChan:
				timer.Stop()
				if ok {
					dHead, dHas = d, true
				} else {
					dOpen = false
				}
			case <-timer.C:
				if err := emit(true, cHead); err != nil {
					return err
				}
				cHas = false
			case <-ctx.Done():
				timer.Stop()
				cleanup()
				return ctx.Err()
			case err, ok := <-errChan:
				timer.Stop()
				if ok && err != nil && !errors.Is(err, io.EOF) {
					cleanup()
					return err
				}
			}
		case dHas && cOpen:
			timer := time.NewTimer(mergeWait)
			select {
			case c, ok := <-clientBuffChan:
				timer.Stop()
				if ok {
					cHead, cHas = c, true
				} else {
					cOpen = false
				}
			case <-timer.C:
				if err := emit(false, dHead); err != nil {
					return err
				}
				dHas = false
			case <-ctx.Done():
				timer.Stop()
				cleanup()
				return ctx.Err()
			case err, ok := <-errChan:
				timer.Stop()
				if ok && err != nil && !errors.Is(err, io.EOF) {
					cleanup()
					return err
				}
			}
		default:
			// Both heads empty.
			if !cOpen && !dOpen {
				cleanup()
				return nil
			}
			var cCh, dCh chan tsChunk
			if cOpen {
				cCh = clientBuffChan
			}
			if dOpen {
				dCh = destBuffChan
			}
			select {
			case c, ok := <-cCh:
				if ok {
					cHead, cHas = c, true
				} else {
					cOpen = false
				}
			case d, ok := <-dCh:
				if ok {
					dHead, dHas = d, true
				} else {
					dOpen = false
				}
			case <-ctx.Done():
				cleanup()
				return ctx.Err()
			case err, ok := <-errChan:
				if ok && err != nil && !errors.Is(err, io.EOF) {
					cleanup()
					return err
				}
			}
		}
	}
}

// mysqlDecodeState tracks what the async decoder expects next.
type mysqlDecodeState int

const (
	stateExpectCommand          mysqlDecodeState = iota // waiting for a client command
	stateExpectResponse                                 // waiting for first response packet
	stateExpectColumns                                  // reading column definition packets
	stateExpectEOFAfterColumns                          // expecting EOF after column defs
	stateExpectRows                                     // reading row data packets
	stateExpectStmtParams                               // reading param defs for COM_STMT_PREPARE
	stateExpectEOFAfterParams                           // expecting EOF after param defs
	stateExpectStmtColumns                              // reading column defs for COM_STMT_PREPARE
	stateExpectEOFAfterStmtCols                         // expecting EOF after stmt column defs
)

// asyncMySQLDecode runs in a background goroutine and processes forwarded
// chunks in FIFO order. It reassembles MySQL packets, decodes them, pairs
// commands with responses, and records mocks — all off the forwarding path.
func asyncMySQLDecode(ctx context.Context, logger *zap.Logger, decodeChan <-chan mysqlDecodeItem, mocks chan<- *models.Mock, decodeCtx *wire.DecodeContext, clientConn net.Conn, opts models.OutgoingOptions) {
	var clientReassembly mysqlReassemblyBuffer
	var destReassembly mysqlReassemblyBuffer
	var clientOverflowLogged, destOverflowLogged bool

	state := stateExpectCommand

	// maxPipelinedCommands bounds the FIFO below. Real clients pipeline a
	// short burst — Connector/J's streamed-BLOB sequence is two
	// response-bearing commands (RESET then EXECUTE) — so this is generous
	// for legitimate traffic while still bounding a connection whose
	// responses have stopped arriving.
	const maxPipelinedCommands = 32

	type queuedCommand struct {
		bundle       *mysql.PacketBundle
		reqTimestamp time.Time
		op           byte
	}

	// Response-bearing commands wait here until the server replies. MySQL
	// clients may pipeline commands before reading responses (notably
	// Connector/J's RESET -> SEND_LONG_DATA -> EXECUTE sequence), so a single
	// pendingCommand slot is insufficient. No-response commands are recorded
	// immediately and never enter this queue.
	var (
		commandQueue      []queuedCommand
		pendingCommand    *mysql.PacketBundle
		reqTimestamp      time.Time
		resTimestamp      time.Time
		pendingRespBundle *mysql.PacketBundle // accumulated response
		lastOp            byte                // the MySQL command type
		remainingCols     uint64              // columns left to read
		remainingParams   uint16              // params left to read (stmt prepare)
	)

	// Temporary storage for result set assembly.
	var (
		textResultSet   *mysql.TextResultSet
		binaryResultSet *mysql.BinaryProtocolResultSet
		stmtPrepareOk   *mysql.StmtPrepareOkPacket
	)

	resetActiveExchange := func() {
		pendingCommand = nil
		pendingRespBundle = nil
		textResultSet = nil
		binaryResultSet = nil
		stmtPrepareOk = nil
		remainingCols = 0
		remainingParams = 0
		state = stateExpectCommand
	}

	activateNextCommand := func() bool {
		if pendingCommand != nil {
			return true
		}
		if len(commandQueue) == 0 {
			return false
		}

		next := commandQueue[0]
		commandQueue[0] = queuedCommand{}
		commandQueue = commandQueue[1:]

		pendingCommand = next.bundle
		reqTimestamp = next.reqTimestamp
		lastOp = next.op
		pendingRespBundle = nil
		textResultSet = nil
		binaryResultSet = nil
		stmtPrepareOk = nil
		remainingCols = 0
		remainingParams = 0
		state = stateExpectResponse

		// DecodePayload consults LastOp to decide how to decode the response.
		// Client-side decoding may already have advanced LastOp to a later
		// pipelined command, so restore the operation at the head of the FIFO.
		decodeCtx.LastOp.Store(clientConn, lastOp)
		return true
	}

	flushMock := func() {
		if pendingCommand == nil || pendingRespBundle == nil {
			return
		}
		// If the recorder is force-flushing mid-result-set (i.e., a new client
		// command arrived, or the connection closed, before we observed the
		// trailing EOF / OK_with_EOF terminator from the server), the encoded
		// mock would otherwise have FinalResponse == nil and the replay
		// encoder would emit no terminator. Drivers that strictly
		// require the terminator — most notably Connector/J on Java 8 —
		// then block in socketRead0 forever waiting for it.
		//
		// This race is intrinsic to the byte-relay → async-decode split:
		// clientBuffChan / destBuffChan feed asyncMySQLDecode through a
		// single FIFO channel via a non-deterministic select in
		// handleClientQueries, so on a fast loopback the next client
		// command can be enqueued before the server's terminator chunk.
		// Guarding the select doesn't fix it without serializing all
		// recording, so we synthesize a structurally-correct terminator
		// here, matching the recorded server's negotiated capabilities,
		// and stamp it onto the in-memory result set before flushing.
		closeIncompleteResultSetForFlush(state, decodeCtx,
			textResultSet, binaryResultSet, pendingRespBundle)
		requests := []mysql.Request{{PacketBundle: *pendingCommand}}
		responses := []mysql.Response{{PacketBundle: *pendingRespBundle}}
		respOp := pendingRespBundle.Header.Type
		// Lifetime classification at record time: prepared-statement
		// setup (COM_STMT_PREPARE → StmtPrepareOkPacket) is connection-
		// scoped. The executes that reference the statement by id on
		// the same connection may land in a different test's window, so
		// tagging as per-test ("mocks") would have the strict-window
		// filter drop the setup and break replay. Tagging as session
		// ("config") would share it across unrelated connections,
		// which can collide when apps reuse statement names per
		// connection. LifetimeConnection (= "connection" + connID) is
		// the correct scope: not window-filtered, scoped to this
		// connID, matched via GetConnectionMocks(connID) at replay.
		//
		// Connection-alive commands (COM_PING, COM_STATISTICS,
		// COM_DEBUG, COM_RESET_CONNECTION) could semantically be
		// "config" (input-independent responses, session-reusable) but
		// we deliberately keep them as "mocks" for BACKWARD COMPAT:
		// the released keploy replayer skips "config"-tagged mocks at
		// command phase, so tagging them as "config" from this version
		// of the recorder would break the released replayer when it
		// receives a recording made here. The matcher-side
		// isSessionReusableCommandMock helper still dispatches any
		// such mock at command phase if it reaches the session pool
		// (e.g., user-edited recordings), so forward compat is
		// preserved without burning the bridge behind us.
		mockType := "mocks"
		if pendingCommand.Header.Type == "COM_STMT_PREPARE" {
			mockType = "connection"
		}
		// (res>=req order is clamped centrally in recordMock for every mysql mock.)
		recordMock(ctx, requests, responses, mockType, pendingCommand.Header.Type, respOp, mocks, reqTimestamp, resTimestamp, opts)
		resetActiveExchange()
	}

	// heldResp buffers a response that reached the decoder BEFORE its command.
	// The two byte-relay goroutines feed decodeChan via a non-deterministic
	// select, so on the in-memory SimulatedConn loopback a short
	// connection-pool-validation exchange (auth → SELECT 1 → response → QUIT →
	// close in microseconds) can deliver the response ahead of its command. The
	// old code then hit "received MySQL response with no pending command" and
	// DROPPED it — silently losing the query mock and orphaning replay. Instead we
	// hold such a response and, the instant its command arrives (stateExpectResponse
	// below), replay it via `pending` so the exchange pairs correctly.
	var heldResp []mysqlDecodeItem
	var pending []mysqlDecodeItem
	for {
		var item mysqlDecodeItem
		if len(pending) > 0 {
			item = pending[0]
			pending = pending[1:]
		} else {
			it, ok := <-decodeChan
			if !ok {
				break
			}
			item = it
		}
		if item.fromClient {
			// --- Client command chunk ---
			clientReassembly.append(item.data)
			if clientReassembly.didOverflow() && !clientOverflowLogged {
				logger.Debug("MySQL client reassembly buffer exceeded limit")
				clientOverflowLogged = true
			}

			for {
				pkt := clientReassembly.extractCompletePacket()
				if pkt == nil {
					break
				}

				// A partially-read result set does not end on its own: if the
				// trailing EOF/OK terminator is lost, or DeprecateEOF was
				// mis-negotiated so IsResultSetTerminator never fires, the
				// decoder parks in a column/row state forever. activateNextCommand
				// is only reachable from the stateExpectCommand guard, so nothing
				// would ever recover it — every later command would queue and
				// every later RESPONSE would be appended as a row of the stuck
				// result set.
				//
				// A new client command is the signal the server is done. This is
				// deliberately NOT the pre-FIFO "flush any incomplete exchange":
				// that also fired in stateExpectResponse, where no response byte
				// has arrived yet, and silently discarded the command — which is
				// the pipelining bug (#4262) this change exists to fix. Here we
				// hold a genuine partial response, and
				// closeIncompleteResultSetForFlush synthesizes the terminator so
				// the mock is still replayable.
				if pendingCommand != nil {
					switch state {
					case stateExpectColumns, stateExpectEOFAfterColumns, stateExpectRows:
						logger.Debug("MySQL result set never terminated; flushing it before the next command",
							zap.String("nextCommand", "pending"))
						resTimestamp = item.ts
						flushMock()
					}
				}

				// wire.DecodePayload dispatches on LastOp: with COM_QUERY or
				// COM_STMT_EXECUTE still set it decodes the packet as that
				// command's RESPONSE. Fine when commands and responses alternate,
				// wrong the moment a client pipelines — the second command comes
				// back a TextResultSet/BinaryProtocolResultSet, is neither a
				// no-response command nor a 0x unknown, and is queued as a phantom
				// command that eats a real response slot.
				//
				// A client packet is never a response, and LastOp no longer has to
				// survive the client path: activateNextCommand restores the right
				// op per exchange before the response is decoded.
				if op, ok := decodeCtx.LastOp.Load(clientConn); ok &&
					(op == mysql.COM_QUERY || op == mysql.COM_STMT_EXECUTE) {
					decodeCtx.LastOp.Store(clientConn, wire.RESET)
				}

				commandPkt, err := wire.DecodePayload(ctx, logger, pkt, clientConn, decodeCtx)
				if err != nil {
					logger.Debug("failed to decode MySQL command in async decoder", zap.Error(err))
					continue
				}

				// Handle no-response commands — record mock with empty
				// responses, matching the synchronous recorder behavior.
				// No server response exists on the wire, so use the
				// request timestamp for both sides; CapturedRespTime
				// would otherwise carry over the previous exchange's
				// response time on this keep-alive connection and put
				// ResTimestampMock before ReqTimestampMock.
				if wire.IsNoResponseCommand(commandPkt.Header.Type) {
					requests := []mysql.Request{{PacketBundle: *commandPkt}}
					recordMock(ctx, requests, []mysql.Response{}, "mocks", commandPkt.Header.Type, "NO Response Packet", mocks, item.ts, item.ts, opts)
					continue
				}

				// Unknown/unrecognized packet types — treat as no-response.
				// Same timestamp reasoning as the explicit no-response branch.
				if strings.HasPrefix(commandPkt.Header.Type, "0x") {
					logger.Debug("Skipping unknown command packet to avoid stream desync",
						zap.String("type", commandPkt.Header.Type))
					requests := []mysql.Request{{PacketBundle: *commandPkt}}
					recordMock(ctx, requests, []mysql.Response{}, "mocks", commandPkt.Header.Type, "NO Response Packet", mocks, item.ts, item.ts, opts)
					continue
				}

				// Determine the command type for response handling. A miss
				// here is not a reason to drop the command: the old code
				// carried the previous op forward, and losing the mock
				// entirely is worse than decoding its response with a stale
				// hint — the mock is what replay needs to exist at all.
				if op, ok := decodeCtx.LastOp.Load(clientConn); ok {
					lastOp = op
				} else {
					logger.Debug("MySQL command decoded without an operation; carrying the previous one forward",
						zap.String("type", commandPkt.Header.Type))
				}

				// Bounded, like the heldResp guard below it. The queue only
				// holds commands the server has not answered yet, so in
				// healthy operation it is a handful deep at most; growth past
				// this means responses have stopped arriving and the oldest
				// entries are never going to be paired. Dropping the oldest
				// keeps a long-lived pooled connection from accumulating them
				// for the life of the process.
				if len(commandQueue) >= maxPipelinedCommands {
					// Clear it rather than dropping the head. Dropping one entry
					// shifts the FIFO by exactly one, so every response from here
					// on pairs with the wrong command — silently, and for the life
					// of the connection. Overflow already means the pairing is
					// lost, so lose these mocks loudly instead of emitting that
					// many mis-paired ones.
					logger.Warn("MySQL pipelined command queue overflowed; dropping unanswered commands",
						zap.Int("dropped", len(commandQueue)),
						zap.Int("limit", maxPipelinedCommands))
					commandQueue = commandQueue[:0]
				}
				commandQueue = append(commandQueue, queuedCommand{
					bundle:       commandPkt,
					reqTimestamp: item.ts,
					op:           lastOp,
				})

				// If this command's response raced ahead and was held, replay it
				// now — before any further decodeChan items — so it pairs with this
				// command instead of being dropped as "no pending command".
				if len(heldResp) > 0 {
					pending = append(heldResp, pending...)
					heldResp = nil
				}
			}

		} else {
			// --- Server response chunk ---
			// Reorder guard: if this response reached the decoder before its
			// command (the loopback race described at heldResp), hold it and replay
			// it when the command arrives, instead of dropping it below at
			// "received MySQL response with no pending command" — which silently
			// lost the query mock for fast connection-pool-validation exchanges.
			// len(commandQueue) matters as much as pendingCommand: a pipelined
			// burst leaves commands queued with none active, and those responses
			// belong to the head of that queue. Holding them here instead would
			// strand every response after the first in a pipelined exchange.
			if state == stateExpectCommand && pendingCommand == nil && len(commandQueue) == 0 {
				heldResp = append(heldResp, item)
				if len(heldResp) > 128 { // bound: a genuine orphan can't grow unboundedly
					heldResp = heldResp[1:]
				}
				continue
			}
			destReassembly.append(item.data)
			if destReassembly.didOverflow() && !destOverflowLogged {
				logger.Debug("MySQL dest reassembly buffer exceeded limit")
				destOverflowLogged = true
			}

			for {
				pkt := destReassembly.extractCompletePacket()
				if pkt == nil {
					break
				}

				if state == stateExpectCommand {
					if !activateNextCommand() {
						// Unexpected server data without a pending command.
						logger.Debug("received MySQL response with no pending command")
						continue
					}
				}

				switch state {
				case stateExpectResponse:
					state = processFirstResponse(ctx, logger, pkt, decodeCtx, clientConn, lastOp,
						&pendingRespBundle, &textResultSet, &binaryResultSet, &stmtPrepareOk,
						&remainingCols, &remainingParams)
					if state == stateExpectCommand {
						resTimestamp = item.ts
						if pendingRespBundle == nil {
							// The response failed to decode. flushMock would
							// early-return on the nil bundle and leave
							// pendingCommand set with state back at
							// stateExpectCommand — a pair that activateNextCommand
							// reports as "already active" without ever setting
							// stateExpectResponse, so the switch below matches
							// nothing and every later packet on this connection is
							// silently dropped. Drop this one exchange instead and
							// resync on the queue, which is what the pre-FIFO code
							// achieved by reassigning pendingCommand per command.
							resetActiveExchange()
						} else {
							flushMock()
						}
					}

				case stateExpectColumns:
					col, _, err := rowscols.DecodeColumn(ctx, logger, pkt)
					if err != nil {
						logger.Debug("failed to decode column definition in async decoder", zap.Error(err))
						resetActiveExchange()
						continue
					}
					if textResultSet != nil {
						textResultSet.Columns = append(textResultSet.Columns, col)
					} else if binaryResultSet != nil {
						binaryResultSet.Columns = append(binaryResultSet.Columns, col)
					}
					remainingCols--
					if remainingCols == 0 {
						if !decodeCtx.DeprecateEOF() {
							state = stateExpectEOFAfterColumns
						} else {
							state = stateExpectRows
						}
					}

				case stateExpectEOFAfterColumns:
					if !mysqlUtils.IsEOFPacket(pkt) {
						logger.Debug("expected EOF after columns, got something else")
					}
					if textResultSet != nil {
						textResultSet.EOFAfterColumns = pkt
					} else if binaryResultSet != nil {
						binaryResultSet.EOFAfterColumns = pkt
					}
					state = stateExpectRows

				case stateExpectRows:
					if mysqlUtils.IsResultSetTerminator(pkt, decodeCtx.DeprecateEOF()) {
						respType := mysql.StatusToString(mysql.EOF)
						if decodeCtx.DeprecateEOF() && mysqlUtils.IsOKReplacingEOF(pkt) {
							respType = mysql.StatusToString(mysql.OK)
						}
						finalResp := &mysql.GenericResponse{Data: pkt, Type: respType}
						// Preserve the wire Header from the initial response
						// packet (set by processFirstResponse via DecodePayload)
						// so EncodeToBinary gets correct PayloadLength/SequenceID.
						var origHeader *mysql.Header
						if pendingRespBundle != nil && pendingRespBundle.Header != nil {
							origHeader = pendingRespBundle.Header.Header
						}
						if textResultSet != nil {
							textResultSet.FinalResponse = finalResp
							decodeCtx.LastOp.Store(clientConn, wire.RESET)
							pendingRespBundle = &mysql.PacketBundle{
								Header:  &mysql.PacketInfo{Type: string(mysql.Text), Header: origHeader},
								Message: textResultSet,
							}
						} else if binaryResultSet != nil {
							binaryResultSet.FinalResponse = finalResp
							decodeCtx.LastOp.Store(clientConn, wire.RESET)
							pendingRespBundle = &mysql.PacketBundle{
								Header:  &mysql.PacketInfo{Type: string(mysql.Binary), Header: origHeader},
								Message: binaryResultSet,
							}
						}
						resTimestamp = item.ts
						flushMock()
					} else {
						// Row data.
						if textResultSet != nil {
							row, _, err := rowscols.DecodeTextRow(ctx, logger, pkt, textResultSet.Columns)
							if err != nil {
								logger.Debug("failed to decode text row in async decoder", zap.Error(err))
							} else {
								textResultSet.Rows = append(textResultSet.Rows, row)
							}
						} else if binaryResultSet != nil {
							row, _, err := rowscols.DecodeBinaryRow(ctx, logger, pkt, binaryResultSet.Columns)
							if err != nil {
								logger.Debug("failed to decode binary row in async decoder", zap.Error(err))
							} else {
								binaryResultSet.Rows = append(binaryResultSet.Rows, row)
							}
						}
					}

				case stateExpectStmtParams:
					col, _, err := rowscols.DecodeColumn(ctx, logger, pkt)
					if err != nil {
						logger.Debug("failed to decode param definition in async decoder", zap.Error(err))
						resetActiveExchange()
						continue
					}
					if stmtPrepareOk != nil {
						stmtPrepareOk.ParamDefs = append(stmtPrepareOk.ParamDefs, col)
					}
					remainingParams--
					if remainingParams == 0 {
						if !decodeCtx.DeprecateEOF() {
							state = stateExpectEOFAfterParams
						} else {
							if stmtPrepareOk != nil && stmtPrepareOk.NumColumns > 0 {
								remainingCols = uint64(stmtPrepareOk.NumColumns)
								state = stateExpectStmtColumns
							} else {
								decodeCtx.LastOp.Store(clientConn, mysql.OK)
								resTimestamp = item.ts
								flushMock()
							}
						}
					}

				case stateExpectEOFAfterParams:
					if mysqlUtils.IsEOFPacket(pkt) && stmtPrepareOk != nil {
						stmtPrepareOk.EOFAfterParamDefs = pkt
					}
					if stmtPrepareOk != nil && stmtPrepareOk.NumColumns > 0 {
						remainingCols = uint64(stmtPrepareOk.NumColumns)
						state = stateExpectStmtColumns
					} else {
						decodeCtx.LastOp.Store(clientConn, mysql.OK)
						resTimestamp = item.ts
						flushMock()
					}

				case stateExpectStmtColumns:
					col, _, err := rowscols.DecodeColumn(ctx, logger, pkt)
					if err != nil {
						logger.Debug("failed to decode stmt column definition in async decoder", zap.Error(err))
						resetActiveExchange()
						continue
					}
					if stmtPrepareOk != nil {
						stmtPrepareOk.ColumnDefs = append(stmtPrepareOk.ColumnDefs, col)
					}
					remainingCols--
					if remainingCols == 0 {
						if !decodeCtx.DeprecateEOF() {
							state = stateExpectEOFAfterStmtCols
						} else {
							decodeCtx.LastOp.Store(clientConn, mysql.OK)
							resTimestamp = item.ts
							flushMock()
						}
					}

				case stateExpectEOFAfterStmtCols:
					if mysqlUtils.IsEOFPacket(pkt) && stmtPrepareOk != nil {
						stmtPrepareOk.EOFAfterColumnDefs = pkt
					}
					decodeCtx.LastOp.Store(clientConn, mysql.OK)
					resTimestamp = item.ts
					flushMock()
				}
			}
		}
	}

	// Channel closed — flush any remaining exchange.
	flushMock()
}

// processFirstResponse handles the first response packet of a MySQL
// command-response exchange. It returns the new decoder state.
func processFirstResponse(
	ctx context.Context,
	logger *zap.Logger,
	pkt []byte,
	decodeCtx *wire.DecodeContext,
	clientConn net.Conn,
	lastOp byte,
	pendingRespBundle **mysql.PacketBundle,
	textResultSet **mysql.TextResultSet,
	binaryResultSet **mysql.BinaryProtocolResultSet,
	stmtPrepareOk **mysql.StmtPrepareOkPacket,
	remainingCols *uint64,
	remainingParams *uint16,
) mysqlDecodeState {
	// Try to decode the response packet.
	commandRespPkt, err := wire.DecodePayload(ctx, logger, pkt, clientConn, decodeCtx)
	if err != nil {
		logger.Debug("failed to decode MySQL response in async decoder", zap.Error(err))
		return stateExpectCommand
	}

	// Check if response is OK or ERR — simple single-packet response.
	if commandRespPkt.Header.Type == mysql.StatusToString(mysql.ERR) ||
		commandRespPkt.Header.Type == mysql.StatusToString(mysql.OK) {
		*pendingRespBundle = commandRespPkt
		return stateExpectCommand
	}

	// Guard: if response was decoded as a command packet, streams are desynced.
	if isCommandPacket(commandRespPkt.Message) {
		logger.Debug("Response decoded as command packet — stream desync detected",
			zap.String("responseType", commandRespPkt.Header.Type))
		decodeCtx.LastOp.Store(clientConn, wire.RESET)
		*pendingRespBundle = commandRespPkt
		return stateExpectCommand
	}

	// Multi-packet response — determine type based on lastOp.
	switch lastOp {
	case mysql.COM_QUERY:
		ts, ok := commandRespPkt.Message.(*mysql.TextResultSet)
		if !ok {
			logger.Debug("expected TextResultSet",
				zap.String("got", fmt.Sprintf("%T", commandRespPkt.Message)))
			*pendingRespBundle = commandRespPkt
			return stateExpectCommand
		}
		*textResultSet = ts
		*binaryResultSet = nil
		*stmtPrepareOk = nil
		*remainingCols = ts.ColumnCount
		*pendingRespBundle = commandRespPkt
		return stateExpectColumns

	case mysql.COM_STMT_PREPARE:
		sp, ok := commandRespPkt.Message.(*mysql.StmtPrepareOkPacket)
		if !ok {
			logger.Debug("expected StmtPrepareOkPacket",
				zap.String("got", fmt.Sprintf("%T", commandRespPkt.Message)))
			*pendingRespBundle = commandRespPkt
			return stateExpectCommand
		}
		*stmtPrepareOk = sp
		*textResultSet = nil
		*binaryResultSet = nil
		*pendingRespBundle = commandRespPkt
		if sp.NumParams > 0 {
			*remainingParams = sp.NumParams
			return stateExpectStmtParams
		}
		if sp.NumColumns > 0 {
			*remainingCols = uint64(sp.NumColumns)
			return stateExpectStmtColumns
		}
		// No params, no columns — done.
		decodeCtx.LastOp.Store(clientConn, mysql.OK)
		return stateExpectCommand

	case mysql.COM_STMT_EXECUTE:
		bs, ok := commandRespPkt.Message.(*mysql.BinaryProtocolResultSet)
		if !ok {
			logger.Debug("expected BinaryProtocolResultSet",
				zap.String("got", fmt.Sprintf("%T", commandRespPkt.Message)))
			*pendingRespBundle = commandRespPkt
			return stateExpectCommand
		}
		*binaryResultSet = bs
		*textResultSet = nil
		*stmtPrepareOk = nil
		*remainingCols = bs.ColumnCount
		*pendingRespBundle = commandRespPkt
		return stateExpectColumns

	default:
		logger.Debug("unsupported lastOp in async decoder",
			zap.String("op", fmt.Sprintf("%x", lastOp)))
		*pendingRespBundle = commandRespPkt
		return stateExpectCommand
	}
}

// isCommandPacket returns true if the decoded message is a client command type
// rather than a server response type. This is used to detect stream desync
// where DecodePayload misidentifies a server response as a client command.
func isCommandPacket(msg interface{}) bool {
	switch msg.(type) {
	case *mysql.QueryPacket,
		*mysql.StmtPreparePacket,
		*mysql.StmtExecutePacket,
		*mysql.StmtClosePacket,
		*mysql.StmtResetPacket,
		*mysql.StmtSendLongDataPacket,
		*mysql.QuitPacket,
		*mysql.InitDBPacket,
		*mysql.PingPacket,
		*mysql.StatisticsPacket,
		*mysql.DebugPacket,
		*mysql.ChangeUserPacket,
		*mysql.ResetConnectionPacket:
		return true
	default:
		return false
	}
}

// closeIncompleteResultSetForFlush patches an in-progress result-set
// response so that flushMock writes a structurally-complete mock even
// when the recorder is forced to flush before the server has finished
// streaming the result set. Three race shapes are repaired:
//
//  1. stateExpectRows / stateExpectEOFAfterColumns — all column defs
//     were captured but the trailing EOF / OK_with_EOF terminator
//     wasn't. Synthesize the terminator and stamp it on FinalResponse.
//
//  2. stateExpectColumns with cols < columnCount — the column count
//     packet was processed and some column defs were captured, but the
//     decoder lost the rest of the column flight before the next client
//     command preempted. Truncate columnCount to the number of columns
//     actually captured, then synthesize the terminator. Replay clients
//     deserialize this as an empty result set (columns < expected, zero
//     rows) and proceed cleanly to the next command instead of blocking
//     on a column def that will never arrive.
//
//  3. Already-complete result sets (FinalResponse already populated by
//     the natural terminator path) — no-op.
//
// For any other state — single-packet OK/ERR, prepare-phase
// intermediate states, etc. — this is a no-op.
//
// The synthesized terminator copies the negotiated capabilities so the
// replay-time encoder hands the driver a packet shape it accepts:
//
//   - DEPRECATE_EOF negotiated     → 0xFE-prefixed OK_with_EOF
//   - DEPRECATE_EOF NOT negotiated → legacy 5-byte EOF
//   - SESSION_TRACK negotiated     → trailing lenenc info string
//
// Sequence ID is one past the highest seq observed across the recorded
// columns / EOFAfterColumns / rows, falling back to the column-count
// packet's seq + 1 when the result set has no columns at all. Status
// flags carry just SERVER_STATUS_AUTOCOMMIT (0x0002), warnings = 0,
// info = empty — the idle-connection terminator a client sees after a
// successful result set.
//
// The bundle's Header.Type is also rewritten to TextResultSet /
// BinaryProtocolResultSet so wire.EncodeToBinary's type-switch routes
// the encode through the result-set encoder rather than the column-
// count head packet that processFirstResponse originally stored.
func closeIncompleteResultSetForFlush(
	state mysqlDecodeState,
	decodeCtx *wire.DecodeContext,
	textRs *mysql.TextResultSet,
	binaryRs *mysql.BinaryProtocolResultSet,
	bundle *mysql.PacketBundle,
) {
	if state != stateExpectRows &&
		state != stateExpectEOFAfterColumns &&
		state != stateExpectColumns {
		return
	}
	if bundle == nil {
		return
	}

	useDeprecateEOF := decodeCtx != nil && decodeCtx.DeprecateEOF()
	// Recorder reads the live client's negotiated caps off ClientCaps
	// (set by handleInitialHandshake when it decoded the live
	// HandshakeResponse41). PreferRecordedCaps isn't relevant on this
	// path — that flag is for replay where the live client may differ
	// from what was recorded.
	useSessionTrack := decodeCtx != nil &&
		(decodeCtx.ServerCaps&uint32(mysql.CLIENT_SESSION_TRACK)) != 0 &&
		(decodeCtx.ClientCaps&uint32(mysql.CLIENT_SESSION_TRACK)) != 0

	switch {
	case textRs != nil:
		if textRs.FinalResponse != nil &&
			(mysqlUtils.IsEOFPacket(textRs.FinalResponse.Data) ||
				mysqlUtils.IsOKReplacingEOF(textRs.FinalResponse.Data)) {
			return // already complete
		}
		// Repair shape 2: declared more columns than we captured. Keep
		// only what we have and rewrite the column count to match so
		// the encoder doesn't promise a column def that isn't there.
		if uint64(len(textRs.Columns)) < textRs.ColumnCount {
			textRs.ColumnCount = uint64(len(textRs.Columns))
			// In legacy (!DEPRECATE_EOF) mode the column flight ends
			// with an intermediate EOF packet; if we never saw it, fill
			// in a synthesized one so the structural sequence
			// "columns → EOF → rows → terminator" stays valid.
			if !useDeprecateEOF && len(textRs.EOFAfterColumns) == 0 {
				eofSeq := nextSeqIDForResultSet(
					columnHeadersOf(textRs.Columns), nil, nil,
				)
				textRs.EOFAfterColumns = []byte{
					0x05, 0x00, 0x00, eofSeq,
					0xFE, 0x00, 0x00, 0x02, 0x00,
				}
			}
		}
		seq := nextSeqIDForResultSet(
			columnHeadersOf(textRs.Columns),
			textRs.EOFAfterColumns,
			textRowHeadersOf(textRs.Rows),
		)
		data, respType := buildResultSetTerminator(seq, useDeprecateEOF, useSessionTrack)
		textRs.FinalResponse = &mysql.GenericResponse{Data: data, Type: respType}
		bundle.Header.Type = string(mysql.Text)
		bundle.Message = textRs

	case binaryRs != nil:
		if binaryRs.FinalResponse != nil &&
			(mysqlUtils.IsEOFPacket(binaryRs.FinalResponse.Data) ||
				mysqlUtils.IsOKReplacingEOF(binaryRs.FinalResponse.Data)) {
			return
		}
		if uint64(len(binaryRs.Columns)) < binaryRs.ColumnCount {
			binaryRs.ColumnCount = uint64(len(binaryRs.Columns))
			if !useDeprecateEOF && len(binaryRs.EOFAfterColumns) == 0 {
				eofSeq := nextSeqIDForResultSet(
					columnHeadersOf(binaryRs.Columns), nil, nil,
				)
				binaryRs.EOFAfterColumns = []byte{
					0x05, 0x00, 0x00, eofSeq,
					0xFE, 0x00, 0x00, 0x02, 0x00,
				}
			}
		}
		seq := nextSeqIDForResultSet(
			columnHeadersOf(binaryRs.Columns),
			binaryRs.EOFAfterColumns,
			binaryRowHeadersOf(binaryRs.Rows),
		)
		data, respType := buildResultSetTerminator(seq, useDeprecateEOF, useSessionTrack)
		binaryRs.FinalResponse = &mysql.GenericResponse{Data: data, Type: respType}
		bundle.Header.Type = string(mysql.Binary)
		bundle.Message = binaryRs
	}
}

// buildResultSetTerminator returns the wire bytes (with header) of a
// minimal result-set terminator packet matching the negotiated caps.
func buildResultSetTerminator(seqID byte, deprecateEOF, sessionTrack bool) ([]byte, string) {
	if deprecateEOF {
		// OK_with_EOF: 0xFE | affected=0 | last_id=0 | status=2 | warn=0
		// Plus an optional lenenc info string when SESSION_TRACK was
		// negotiated (without it, Connector/J 8.x rejects the packet).
		payload := []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
		if sessionTrack {
			payload = append(payload, 0x00) // info: empty lenenc string
		}
		out := make([]byte, 4+len(payload))
		out[0] = byte(len(payload))
		out[1] = byte(len(payload) >> 8)
		out[2] = byte(len(payload) >> 16)
		out[3] = seqID
		copy(out[4:], payload)
		return out, mysql.StatusToString(mysql.OK)
	}
	// Legacy EOF: 0xFE | warnings(2) | status_flags(2)
	return []byte{
		0x05, 0x00, 0x00, seqID,
		0xFE, 0x00, 0x00, 0x02, 0x00,
	}, mysql.StatusToString(mysql.EOF)
}

// nextSeqIDForResultSet picks the seq id one past the highest observed
// across the recorded columns / EOFAfterColumns / rows. Falls back to 2
// (one past the column-count head packet) if nothing is recorded.
func nextSeqIDForResultSet(columnHdrs []mysql.Header, eofAfterCols []byte, rowHdrs []mysql.Header) byte {
	max := byte(0)
	for _, h := range columnHdrs {
		if h.SequenceID > max {
			max = h.SequenceID
		}
	}
	if len(eofAfterCols) >= 4 && eofAfterCols[3] > max {
		max = eofAfterCols[3]
	}
	for _, h := range rowHdrs {
		if h.SequenceID > max {
			max = h.SequenceID
		}
	}
	if max == 0 {
		max = 1
	}
	return max + 1
}

func columnHeadersOf(cols []*mysql.ColumnDefinition41) []mysql.Header {
	out := make([]mysql.Header, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Header)
	}
	return out
}

func textRowHeadersOf(rows []*mysql.TextRow) []mysql.Header {
	out := make([]mysql.Header, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Header)
	}
	return out
}

func binaryRowHeadersOf(rows []*mysql.BinaryRow) []mysql.Header {
	out := make([]mysql.Header, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Header)
	}
	return out
}
