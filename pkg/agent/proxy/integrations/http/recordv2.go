package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// recordV2 is the FakeConn-based record path. The relay owns and writes the
// real sockets; recordV2 only observes the teed chunks via sess.ClientStream
// / sess.DestStream and emits mocks with timestamps derived from the chunks'
// ReadAt / WrittenAt fields (never from time.Now()).
//
// The mock payload (headers, body, metadata, URL, method, status) is
// constructed identically to the legacy path via buildHTTPMock, so
// mock.Spec.HTTPReq / HTTPResp are byte-equivalent between the two paths
// for the same input traffic. Delivery differs: V2 goes through
// sess.EmitMock (which runs the post-record hook chain and respects the
// incomplete-mock gate) rather than the syncMock / mocks channel shim
// the legacy path uses.
//
// recordV2 loops over request/response pairs for HTTP/1.1 keepalive /
// pipelining. Each request starts where Session.NextRequest finds it, so its
// response is what the server sent after the request was captured: server
// bytes captured before it (the answer to a request in flight when the capture
// began or the agent restarted, a 408 the server sent on its own) are never
// paired with it, and NextRequest says which of them it reports: it ends
// with Session.EndExchanges, however it ends, so they are reported though it
// never reads the server's stream past them. It exits
// cleanly on either stream reaching EOF or Close,
// on ctx cancellation, or on a malformed-HTTP decode error (the error is
// returned, and the supervisor falls through to passthrough). An exchange
// it stops on and cannot record (a request or response that does not
// decode, a mock that cannot be built) is reported first, as every parser
// reports the exchange it stops on (Session.ReportStoppedOn). A request the
// server's stream ends without answering is not: with no response there is
// no mock to lose.
func (h *HTTP) recordV2(ctx context.Context, sess *supervisor.Session) error {
	if sess == nil {
		return errors.New("recordV2: nil supervisor session")
	}
	defer sess.EndExchanges()
	logger := sess.Logger
	if logger == nil {
		logger = h.Logger
	}

	destPort := destPortFromAddr(sess.DestStream)

	// watchdogSuspended latches once this connection's watchdog has been
	// disarmed for a poll lane, so we don't re-parse and re-match every
	// subsequent request on a keep-alive poll-poll-poll connection.
	watchdogSuspended := false
	// exchanges counts the requests read on this connection, so the
	// upstream's end before any answer is recorded on its first one only
	// (noAnswerFault).
	exchanges := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// --- Request side ---------------------------------------------
		//
		// The ReadAt of the chunk the request starts in is the request
		// arrival timestamp. It is shown, not taken: readRequestV2 takes the
		// request's bytes of it, so a request that starts partway through a
		// chunk, after the request before it, carries that chunk's time.
		firstChunk, err := nextRequestV2(sess)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				logger.Debug("V2 HTTP record: client stream ended at start of request", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(logger, err, "V2 HTTP record: initial request read failed")
			return err
		}
		reqTs, reqClock := firstChunk.ReadAt, firstChunk.ReadClock
		var finalReq []byte

		// Read the request: its headers and body, and never a byte past its
		// end (readMessageV2), which is the start of the next request of a
		// pipeline when its client sent them in one write.
		if err := h.readRequestV2(ctx, sess.ClientStream, &finalReq); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				logger.Debug("V2 HTTP record: client stream closed mid-request", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: request read failed: "+err.Error())
			utils.LogError(logger, err, "V2 HTTP record: failed to read full request")
			return err
		}

		exchanges++

		// If this request routes to a long-poll async lane, disarm the
		// supervisor's hang watchdog for this connection before we block on
		// the response: a poll legitimately holds the connection open with no
		// byte progress for far longer than the hang budget, and would
		// otherwise be aborted (falling through to passthrough) before the
		// delivery arrives — so the poll's mock would never be recorded.
		if !watchdogSuspended && sess.SuspendWatchdog != nil && h.isPollLaneRequest(finalReq) {
			logger.Debug("V2 HTTP record: request matched a poll lane; suspending hang watchdog for this connection")
			sess.SuspendWatchdog()
			watchdogSuspended = true
		}

		// --- Response side --------------------------------------------
		//
		// Shown, not taken, as the request's first chunk is: readResponseV2
		// takes the response's bytes of it, and leaves the next response's
		// for the next request.
		firstRespChunk, err := sess.DestStream.Peek()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				// The stream ended with nothing of a response. A side of the
				// connection ended it (the relay's read found its FIN or its
				// reset), and the call is recorded with that fault
				// (noAnswerFault), so replay gives the application what the
				// upstream gave it, or holds the call unanswered until the
				// application gives up again: the client gave up and closed
				// (a call a Go http.Client timed out ends so, where the relay
				// kept from the parser a late answer the closed client
				// refused: Session.EndedWhereClientStopped), or the upstream
				// reset or closed the connection on its first request.
				//
				// Else there is no mock to lose and nothing is reported. The
				// server closed without answering a later request, or after
				// bytes no request asked for (its 408): the keep-alive
				// idle-close race, in which the app's HTTP client retries
				// the request on a new connection, where it is recorded. Or
				// the session closed the stream: the supervisor's abort,
				// whose fallthrough leaves out the rest of the connection,
				// or the recording's stop.
				//
				// A mark on the incomplete-mock flag does not make it a loss
				// either. Response bytes the capture lost stopped this
				// connection's capture, as this parser cannot re-align
				// after a hole, and the capture leaves out the test cases
				// the connection carries from the loss on itself (the
				// relay's OnCaptureDesync). A write the relay could not
				// make (write_error) is a request the server never got:
				// the same race, when the app writes its request in more
				// than one piece.
				if fault := noAnswerFault(sess, err, exchanges); fault != nil {
					return h.recordFault(ctx, sess, logger, &FinalHTTP{
						Req:              finalReq,
						ReqTimestampMock: reqTs,
						ResTimestampMock: reqTs,
						ReqClock:         reqClock,
						Fault:            fault,
					}, destPort, reqTs)
				}
				if sess.EndedWhereClientStopped() {
					// Said apart, as the one way this ends that can hide a
					// response the application read: a half-closed client
					// that reset before its acknowledgement went out
					// (relay/client_took.go).
					logger.Debug("V2 HTTP record: the client closed the connection before its stack acknowledged any of the response; "+
						"nothing is recorded for the call", zap.Error(err))
					return nil
				}
				logger.Debug("V2 HTTP record: dest stream ended before response", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: initial response read failed: "+err.Error())
			utils.LogError(logger, err, "V2 HTTP record: initial response read failed")
			return err
		}
		var finalResp []byte
		resTs := firstRespChunk.WrittenAt

		gotLastWritten, err := h.readResponseV2(ctx, sess.DestStream, &finalResp, requestMethod(finalReq))
		// The stream ended before readResponseV2 found the response's end.
		// Where keploy relays the connection it has read all the server sent,
		// so that is the server's doing, unless the relay ended the stream
		// where the client stopped taking it (EndedWhereClientStopped): the
		// client closed its socket partway through the response, and refused
		// the rest. That exchange is left out, and said so
		// (LeftOutClientClosedPartway): the client gave up on the call (its
		// timeout) or wanted no more of it, and nothing tells which. Recorded
		// as far as the client had it, a call that timed out at record would
		// be served a complete-looking shorter response at replay. Unless
		// what it refused, of what the destination sent before the client's
		// FIN reached the proxy, ended the response with framing alone
		// (chunkedEndRefused): then it had every byte of content, and the
		// exchange is whole. An
		// observe-only capture carries only what the client read, so when its
		// stream ended with the connection (EndedWithConnection) it is the
		// client that stopped reading and closed, and the response is
		// recorded as the client had it (FinalHTTP.RespReadInPart).
		streamEnded := false
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				// Legacy encodeHTTP treats EOF after some bytes as
				// end-of-response. Respect that shape: emit what we have.
				// Only a stream that ran out (io.EOF) ended where its capture
				// did: ErrClosed is its reader being closed while the capture
				// could still send. A capture that dropped chunks before it
				// ended the stream (its tee gave up on a consumer) marked the
				// mock incomplete for them.
				streamEnded = errors.Is(err, io.EOF)
				if !gotLastWritten.IsZero() {
					resTs = gotLastWritten
				}
			} else {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: response read failed: "+err.Error())
				utils.LogError(logger, err, "V2 HTTP record: failed to read full response")
				return err
			}
		} else if !gotLastWritten.IsZero() {
			resTs = gotLastWritten
		}
		if streamEnded && sess.EndedWhereClientStopped() {
			whole, ok := chunkedEndRefused(finalResp, sess.NotTakenWhereClientStopped())
			if !ok || sess.IsMockIncomplete() {
				// The client closed the connection partway through the
				// response: no whole exchange to record. Its stream has
				// ended, and so has every answer after it. A mark on the
				// incomplete-mock flag is a chunk the capture lost, the
				// likelier cause, so the exchange is reported under it;
				// and what the client refused does not follow on from
				// what the stream holds across a chunk the capture lost,
				// so it does not end the response then.
				reason := supervisor.LeftOutClientClosedPartway
				if mark, ok := sess.TakeMockIncomplete(); ok {
					reason = mark + ": " + reason
				}
				logger.Debug("V2 HTTP record: the client closed the connection partway through the response; the exchange is left out")
				sess.ReportLeftOut(&models.Mock{
					Kind: models.HTTP,
					Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: resTs},
				}, reason)
				return nil
			}
			// What the client refused was the end of the chunked body's
			// framing alone, the last chunk, which a server that flushes
			// its body writes only as its handler returns, and it reached
			// the proxy before the client's FIN: the client had all the
			// response carries, and closed before the last chunk came,
			// which the destination wrote before it could know. The
			// response is whole, and recorded so. A client whose own
			// timeout fired in that gap closed the same way, and its call
			// failed: nothing its stack did tells the two apart, so its
			// call is recorded whole too, and at replay the app gets the
			// response its call timed out on at record. A last chunk that
			// reached the proxy after the client's FIN, as a streaming
			// server writes one once its client has gone, ends nothing
			// here: that exchange is left out above.
			finalResp, streamEnded = whole, false
		}

		// Build and emit the mock. Identical shape to the legacy path.
		var fault *models.Fault
		if streamEnded {
			if reason, ok := partAnswerLeftOut(sess); ok {
				// The client's end came first, partway through the response,
				// and records no fault: the response is not known to be
				// whole, and no fault is known to record it with. Recorded
				// as far as it came, a call cut short would be served a
				// complete-looking shorter response at replay.
				if mark, ok := sess.TakeMockIncomplete(); ok {
					reason = mark + ": " + reason
				}
				logger.Debug("V2 HTTP record: the client's end came partway through the response, and records no fault; the exchange is left out")
				sess.ReportLeftOut(&models.Mock{
					Kind: models.HTTP,
					Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: resTs},
				}, reason)
				return nil
			}
			fault = partAnswerFault(sess)
		}
		mock, err := h.buildHTTPMock(&FinalHTTP{
			Req:              finalReq,
			Resp:             finalResp,
			ReqTimestampMock: reqTs,
			ResTimestampMock: resTs,
			ReqClock:         reqClock,
			RespReadInPart:   streamEnded && sess.Opts.SkipTLSMITM && sess.EndedWithConnection(fakeconn.FromDest),
			Fault:            fault,
		}, destPort, sess.ClientConnID, sess.Opts)
		if err != nil {
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: "+err.Error())
			utils.LogError(logger, err, "V2 HTTP record: failed to build mock")
			return err
		}
		if mock != nil && mock.Spec.Fault != nil {
			noteFaultRecorded(logger, mock)
		}
		if mock != nil {
			// Recorded, not passed through: under the proven switch the
			// connection's local destination is listed mocked
			// (integrations.CallServed).
			integrations.CallServed(ctx)
			// EmitMock takes the incomplete-mock flag itself. Clearing it
			// again here would wipe a mark set while EmitMock delivered
			// (on another goroutine, meanwhile): that mark is the
			// next exchange's, whose mock would then be recorded with
			// nothing reporting it.
			if emitErr := sess.EmitMock(mock); emitErr != nil {
				return emitErr
			}
		} else {
			// Nothing recorded for this exchange (a passthrough), so a mark
			// set during it must not leave out the next exchange's mock.
			sess.MarkMockComplete()
		}
	}
}

// noAnswerFault is the fault a call ended in that got no byte of an answer, as
// the relay saw the connection end (Session.FirstPeerEnded), or nil when the
// call is recorded as nothing, as before:
//   - the server sent bytes no request asked for before this request
//     (Session.ServerSpokeUnasked: its 408 on a keep-alive connection that
//     idled out as the client sent it): the end that follows is its idle
//     close, which the application's HTTP client meets by retrying the
//     request on a new connection, where it is recorded;
//   - the client's side ended first: the application gave up on the call (its
//     client's timeout) and closed, with no answer (FaultTimeout), on any
//     request of the connection;
//   - the destination's side ended first: it reset the connection
//     (FaultReset) or closed it (FaultEOF), on the connection's first request
//     only. On a later one that is the keep-alive idle-close race (the
//     server's close met the request), and the client retries as above; and
//     a capture that joined the connection partway
//     (Session.JoinedMidConnection) does not know its first request;
//   - neither side ended it: the relay ended the stream (the recording's
//     stop), the session closed it (err is not io.EOF: the supervisor's
//     abort), or the capture lost part of it (Session.EndedAtHole);
//   - the first end was the application's stop (Session.EndFollowedStop):
//     read after the recording's stop began (keploy stops the application),
//     or the client's end as its process exited (the command's own exit
//     included). The close of a call it had in flight is the stop's, not its
//     client's timeout. So is a client's end the hooks cannot tell from its
//     process's exit (the macOS and Windows hooks): no fault is recorded on
//     an end nothing says was the application's giving up. One partway
//     through the answer is left out (partAnswerLeftOut).
func noAnswerFault(sess *supervisor.Session, err error, exchanges int) *models.Fault {
	if !errors.Is(err, io.EOF) {
		return nil
	}
	if _, hole := sess.EndedAtHole(fakeconn.FromDest); hole {
		return nil
	}
	if sess.ServerSpokeUnasked() {
		return nil
	}
	first, ok := sess.FirstPeerEnded()
	if !ok || sess.EndFollowedStop() {
		return nil
	}
	if first == fakeconn.FromClient {
		return &models.Fault{Kind: models.FaultTimeout, Observed: "the application closed the connection with no answer"}
	}
	if exchanges != 1 || sess.JoinedMidConnection {
		return nil
	}
	if sess.PeerEnded(fakeconn.FromDest) == supervisor.PeerReset {
		return &models.Fault{Kind: models.FaultReset, Observed: "the upstream reset the connection with no answer"}
	}
	return &models.Fault{Kind: models.FaultEOF, Observed: "the upstream closed the connection with no answer"}
}

// partAnswerLeftOut reports the reason an exchange is left out for whose
// response stream ended before the response did (recordV2's streamEnded), when
// the connection's first end was the client's and records no fault
// (Session.FirstEndNoFault): the recording's stop, the client's process's own
// exit, or an end the hooks cannot judge. Its response is not known to be
// whole: its framing had not ended, or its body is framed by the upstream's
// close, which had not come. And no fault is known to record it with. ok is
// false for any other end, and for a stream that ended at a capture hole
// (Session.EndedAtHole), as in partAnswerFault.
func partAnswerLeftOut(sess *supervisor.Session) (reason string, ok bool) {
	if _, hole := sess.EndedAtHole(fakeconn.FromDest); hole {
		return "", false
	}
	if first, ok := sess.FirstPeerEnded(); !ok || first != fakeconn.FromClient {
		return "", false
	}
	switch sess.FirstEndNoFault() {
	case supervisor.EndAtStop:
		return supervisor.LeftOutPartwayAtStop, true
	case supervisor.EndAtExit:
		return supervisor.LeftOutPartwayAtExit, true
	case supervisor.EndUnjudged:
		return supervisor.LeftOutPartwayUnjudged, true
	}
	return "", false
}

// partAnswerFault is the fault a call ends in whose response stream ended
// before the response did, as the relay saw the connection end, or nil: the
// destination reset the connection (FaultReset after the head) or closed it
// (FaultTruncate), or the client closed first, the upstream silent, and gave
// up on the rest (FaultTimeout after the head). Where the client closed while
// the destination still sent (Session.EndedWhereClientStopped), or its end came
// first and records no fault (partAnswerLeftOut), the exchange is left out
// before this is asked. The upstream's end read once the recording's stop had
// begun (Session.EndFollowedStop) records no fault, as in noAnswerFault.
// buildHTTPMock records the fault only when the response is in fact
// unfinished (FinalHTTP.Fault).
func partAnswerFault(sess *supervisor.Session) *models.Fault {
	if _, hole := sess.EndedAtHole(fakeconn.FromDest); hole {
		return nil
	}
	first, ok := sess.FirstPeerEnded()
	if !ok || sess.EndFollowedStop() {
		return nil
	}
	if first == fakeconn.FromClient {
		return &models.Fault{Kind: models.FaultTimeout, AfterHead: true, Observed: "the application closed the connection partway through the answer"}
	}
	if sess.PeerEnded(fakeconn.FromDest) == supervisor.PeerReset {
		return &models.Fault{Kind: models.FaultReset, AfterHead: true, Observed: "the upstream reset the connection partway through the answer"}
	}
	return &models.Fault{Kind: models.FaultTruncate, Observed: "the upstream closed the connection partway through the answer"}
}

// recordFault builds and emits the mock of a call that got no answer, with the
// fault it ended in, and ends the connection's recording: its stream has
// ended.
func (h *HTTP) recordFault(ctx context.Context, sess *supervisor.Session, logger *zap.Logger, m *FinalHTTP, destPort uint, reqTs time.Time) error {
	mock, err := h.buildHTTPMock(m, destPort, sess.ClientConnID, sess.Opts)
	if err != nil {
		sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: "+err.Error())
		utils.LogError(logger, err, "V2 HTTP record: failed to build the mock of a call that ended in a fault")
		return err
	}
	if mock == nil {
		sess.MarkMockComplete()
		return nil
	}
	noteFaultRecorded(logger, mock)
	integrations.CallServed(ctx)
	return sess.EmitMock(mock)
}

// faultNoted makes noteFaultRecorded's note once per process.
var faultNoted sync.Once

// noteFaultRecorded says, once per process, at the first call recorded with a
// fault, that keploy records an upstream's fault as itself: a recording made
// before holds what was made of it instead (nothing, or a 504/502 marked
// Keploy-Recorded-Upstream-Error), and replays as before.
func noteFaultRecorded(logger *zap.Logger, mock *models.Mock) {
	faultNoted.Do(func() {
		logger.Warn("keploy now records a call that ended in a fault with the fault itself (a reset, a close, a cut-short "+
			"answer, or the application giving up), and replay gives the application the same fault; recordings made "+
			"before keep what they hold and replay as before",
			zap.String("fault", mock.Spec.Fault.String()))
	})
}

// nextRequestV2 starts the next exchange (Session.NextRequest) and returns the
// chunk its request starts in, not taken (readRequestV2 takes it): what is
// left of a chunk the request before it ended in, when the client pipelined
// them, or the client's next chunk. A chunk with no request bytes, nothing but
// empty lines (emptyLinesBefore), is no part of a request, so it is taken
// first, and the request starts at the client's next chunk: its response is
// what the server sent after that one was captured, and it is the connection's
// first request when the capture began after the CRLF of a request it does not
// have. The request's first chunk therefore always holds request bytes.
func nextRequestV2(sess *supervisor.Session) (fakeconn.Chunk, error) {
	for {
		c, err := sess.ClientStream.Peek()
		if err != nil {
			return fakeconn.Chunk{}, err
		}
		if emptyLinesBefore(c.Bytes) < len(c.Bytes) {
			break
		}
		if _, err := sess.ClientStream.ReadChunk(); err != nil {
			return fakeconn.Chunk{}, err
		}
	}
	return sess.NextRequest(models.HTTP, false)
}

// readRequestV2 is the V2 counterpart of HandleChunkedRequests. It reads the
// rest of an HTTP/1 request from stream onto *finalReq, which holds what of it
// was read before, and no byte past its end (readMessageV2): the bytes after it
// are the next request's, when the client pipelined them. The empty lines
// before its request line are dropped (dropEmptyLinesBeforeRequest).
//
// Returns io.EOF if stream closes before the body is fully consumed.
// Returns a decode error for malformed Content-Length / body framing.
func (h *HTTP) readRequestV2(ctx context.Context, stream *fakeconn.FakeConn, finalReq *[]byte) error {
	if _, err := readMessageV2(ctx, stream, finalReq, &messageFrame{}); err != nil {
		return err
	}
	dropEmptyLinesBeforeRequest(finalReq)
	return nil
}

// readResponseV2 is the V2 counterpart of handleChunkedResponses. It reads the
// rest of an HTTP/1 response from stream onto *finalResp, as readRequestV2
// reads a request (messageFrame frames it: past any interim 1xx, by its
// Content-Length, its chunked framing, or to the stream's end), and returns
// the WrittenAt of the last chunk it took bytes of. It does NOT write to any
// client connection — the relay owns forwarding on the V2 path.
//
// Returns (lastWrittenAt, err). err may be io.EOF for the legitimate
// "server closed after sending a full response" case; the caller
// decides whether to emit a mock.
func (h *HTTP) readResponseV2(ctx context.Context, stream *fakeconn.FakeConn, finalResp *[]byte, reqMethod string) (time.Time, error) {
	return readMessageV2(ctx, stream, finalResp, &messageFrame{response: true, reqMethod: reqMethod})
}

// readMessageV2 takes the rest of one HTTP/1 message off stream onto *msg,
// which holds what of it was taken before, chunk by chunk, and no byte past
// the message's end, which frame finds. A client that pipelines its requests
// (HTTP/1.1: it sends the next before the answer to the one before comes)
// writes them in one write as often as not, and a server writes its answers
// back to back, so a chunk can hold the end of one message and the start of
// the next: what follows the end stays on the stream, with its chunk's times,
// for the next message's read. Taking whole chunks recorded only the first
// message of such a chunk, and the rest of the pipeline was lost.
//
// It returns the WrittenAt of the last chunk it took bytes of (zero when none
// carried one), and frame's error or the stream's, the message unfinished.
func readMessageV2(ctx context.Context, stream *fakeconn.FakeConn, msg *[]byte, frame *messageFrame) (time.Time, error) {
	var lastWr time.Time
	_, done, err := frame.end(*msg)
	for !done && err == nil {
		if err := ctx.Err(); err != nil {
			return lastWr, err
		}
		// The chunk is looked at before it is taken: only the end of the
		// message, framed with it, says how much of it is the message's.
		c, perr := stream.Peek()
		if perr != nil {
			return lastWr, perr
		}
		from := len(*msg)
		*msg = append(*msg, c.Bytes...)
		var end int
		if end, done, err = frame.end(*msg); done {
			*msg = (*msg)[:end]
		}
		// Take what msg holds of the chunk off the stream: a Read takes it
		// from the chunk Peek showed (the same bytes, into the same place),
		// and leaves the rest of the chunk for the next read. The whole of
		// it is taken as a chunk, one with no bytes too, which a Read never
		// takes. Neither fails: the chunk Peek showed is in hand, and only
		// the reader moves the stream (fakeconn.FakeConn).
		if len(*msg)-from < len(c.Bytes) {
			_, _ = stream.Read((*msg)[from:])
		} else {
			_, _ = stream.ReadChunk()
		}
		if !c.WrittenAt.IsZero() {
			lastWr = c.WrittenAt
		}
	}
	return lastWr, err
}

// messageFrame finds where one HTTP/1 message ends as its bytes arrive. The
// header section that governs its body (a response's final one, past any
// interim 1xx: messageHead) says how the body is framed: no body (the answer
// to a HEAD, a 204 or a 304: responseHasNoBody; a request with neither field
// below), a Content-Length, chunked framing (chunkedBody), or, for a response
// with neither, the stream's end.
type messageFrame struct {
	response  bool
	reqMethod string      // a response's request's method
	body      chunkedBody // frames a chunked body as it arrives
}

// end reports the length of the message msg starts with, and true, once all
// of it is in msg (bytes after it are the next message's), or false while it
// is not. A body that runs to the stream's end never ends here. msg must only
// grow between calls. A Content-Length that does not parse, or is negative,
// and a malformed chunked body are errors.
func (f *messageFrame) end(msg []byte) (int, bool, error) {
	head, bodyStart, status, ok := messageHead(msg, f.response)
	if !ok {
		return 0, false, nil
	}
	// A request's status is 0 and its method unset here: no rule for an
	// answer without a body holds for it.
	if responseHasNoBody(f.reqMethod, status) {
		return bodyStart, true, nil
	}
	contentLength, transferEncoding := parseHeaders(head)
	switch {
	case contentLength != "":
		n, err := contentLengthValue(contentLength)
		if err != nil {
			return 0, false, err
		}
		if n < 0 {
			// No length: the message would end inside its own head.
			return 0, false, fmt.Errorf("%w (a negative value)", errBadContentLength)
		}
		if len(msg)-bodyStart < n {
			return 0, false, nil
		}
		return bodyStart + n, true, nil
	case strings.Contains(strings.ToLower(transferEncoding), "chunked"):
		done, err := f.body.feed(msg[bodyStart:])
		return bodyStart + f.body.end, done, err
	}
	// Neither field: a request has no body, and a response's runs until the
	// server closes (RFC 9112 §6.3: HTTP/1.0 and "Connection: close").
	return bodyStart, !f.response, nil
}

// parseHeaders extracts the body-framing Content-Length and
// Transfer-Encoding header values (if any) from an HTTP message whose
// headers end with CRLFCRLF. The parse is the same loose style the chunk.go
// helpers use: split on '\n', trim '\r', skip malformed lines. It reads the
// bytes in place: every request a replay serves is framed by it twice
// (HandleChunkedRequests, then messageFrame), and a recording's every chunk,
// so a copy of each line was an allocation per header field per read.
//
// A chunked Transfer-Encoding overrides a Content-Length sent with it (RFC
// 9112 §6.3), so contentLength is "" then: framing such a message by its
// Content-Length would end it where its peer does not.
func parseHeaders(msg []byte) (contentLength string, transferEncoding string) {
	var cl, te []byte
	for rest := msg; len(rest) > 0; {
		line, after, _ := bytes.Cut(rest, newline)
		rest = after
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break // end of headers
		}
		key, val, ok := bytes.Cut(line, colon)
		if !ok {
			continue
		}
		key = bytes.TrimSpace(key)
		switch {
		case equalFoldASCII(key, contentLengthName):
			cl = bytes.TrimSpace(val)
		case equalFoldASCII(key, transferEncodingName):
			te = bytes.TrimSpace(val)
		}
	}
	transferEncoding = string(te)
	if strings.Contains(strings.ToLower(transferEncoding), "chunked") {
		return "", transferEncoding
	}
	return string(cl), transferEncoding
}

var (
	newline              = []byte("\n")
	colon                = []byte(":")
	contentLengthName    = []byte("content-length")
	transferEncodingName = []byte("transfer-encoding")
)

// equalFoldASCII reports whether name, a header field's name, is lower (a
// name in lower-case ASCII) without regard to case. Only ASCII letters fold,
// as net/http folds a field name: bytes.EqualFold also takes Unicode's simple
// folds, so U+017F (ſ) matched s and a field net/http does not read as
// Transfer-Encoding framed the message as chunked.
func equalFoldASCII(name, lower []byte) bool {
	if len(name) != len(lower) {
		return false
	}
	for i, c := range name {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// destPortFromAddr extracts the destination TCP port from a FakeConn's
// RemoteAddr, falling back to 0 if the address is not a TCP address
// (e.g. a net.Pipe under tests). Port is only used for passthrough
// rule matching — 0 reliably matches no rule so tests without real
// sockets still exercise the non-passthrough path.
func destPortFromAddr(conn net.Conn) uint {
	if conn == nil {
		return 0
	}
	addr := conn.RemoteAddr()
	if addr == nil {
		return 0
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return uint(tcp.Port)
	}
	return 0
}

// isPollLaneRequest reports whether the raw HTTP request routes to a
// configured async lane whose type is a poll variant (lane.IsPoll()).
// Matching is by request shape only (host/path/query, via the engine's
// LaneFor), so it is valid at record time before any mock is emitted.
// recordV2 uses it to disarm the supervisor's hang watchdog for long-poll
// connections. Returns false when no async engine is configured or the
// request is malformed.
func (h *HTTP) isPollLaneRequest(rawReq []byte) bool {
	if h.asyncEngine == nil || !h.asyncEngine.HasPollLanes() {
		return false
	}
	parsed, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(rawReq)))
	if err != nil {
		return false
	}
	// Reuse liveReqToMock (the same request→mock shaping the replay LaneFor
	// path uses in decode.go) so record- and replay-time lane matching stay
	// consistent — in particular it leaves URLParams unset so queryOf does the
	// full multi-value query parse rather than a truncated first-value view.
	lane, ok := h.asyncEngine.LaneFor(liveReqToMock(&req{
		method: parsed.Method,
		url:    parsed.URL,
		header: parsed.Header,
	}))
	return ok && lane.IsPoll()
}

// parseFinalResponse parses the response m records: the final one, past any
// interim 1xx responses in front of it (net/http does not skip them), which are
// read off the same reader one after another, so their line endings do not
// matter. One net/http will not read that way (an interim with a malformed
// header line) is framed by its blank lines instead (finalResponse), as it was
// before the interims were read. A response its client read only in part
// (m.RespReadInPart) that does not parse whole is parsed as the client had it
// (responseAsRead, from where the final response begins), and cut reports so.
// What parses as it stands is never read that way: a bare-LF header section,
// which responseAsRead cannot frame, still parses whole.
//
// An interim response is never recorded as the answer: one whose final
// response never arrived (the stream ended after it, in its head, or in the
// final one's status line) is an error wrapping io.ErrUnexpectedEOF, as a
// response cut short is.
func parseFinalResponse(m *FinalHTTP, req *http.Request) (resp *http.Response, cut bool, err error) {
	rd := bytes.NewReader(m.Resp)
	br := bufio.NewReader(rd)
	interim, final := 0, 0 // final: where the response after the interims begins
	for {
		resp, err = http.ReadResponse(br, req)
		if err != nil || !pUtil.IsInterimStatus(resp.StatusCode) {
			break
		}
		interim = resp.StatusCode // no body: the next response follows it
		final = len(m.Resp) - rd.Len() - br.Buffered()
	}
	if err == nil {
		return resp, false, nil
	}
	if framed, ferr := http.ReadResponse(bufio.NewReader(bytes.NewReader(finalResponse(m.Resp))), req); ferr == nil && !pUtil.IsInterimStatus(framed.StatusCode) {
		return framed, false, nil
	}
	if m.RespReadInPart {
		asRead, rerr := http.ReadResponse(bufio.NewReader(bytes.NewReader(responseAsRead(m.Resp[final:]))), req)
		if rerr == nil && !pUtil.IsInterimStatus(asRead.StatusCode) {
			return asRead, true, nil
		}
	}
	if interim != 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return nil, false, fmt.Errorf("parse response: the final response after a %d never arrived whole: %w",
			interim, io.ErrUnexpectedEOF)
	}
	return nil, false, fmt.Errorf("parse response: %w", err)
}

// decompressReadInPart is pkg.Decompress for a body its client read only in
// part: what decodes before the cut, which is as much as the client could
// decode. A body cut before any of it decodes (in the gzip header, or none of
// it read) decodes to nothing. The cut is expected here, so it is not logged as
// a failure. An encoding it does not decode is kept as it is, as Decompress
// keeps it.
func decompressReadInPart(encoding string, data []byte, limit int64) ([]byte, error) {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return []byte{}, nil // cut in its header
		}
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case "br":
		r = brotli.NewReader(bytes.NewReader(data))
	case "", "identity":
		return data, nil
	default:
		return data, nil
	}
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%w of %d bytes (possible decompression bomb)", pkg.ErrDecompressedTooLarge, limit)
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return out, nil
}

// buildHTTPMock constructs a *models.Mock with the same shape the legacy
// parseFinalHTTP produces. Returns (nil, nil) when the request is a
// pass-through (IsPassThrough true) so the caller skips emission.
// Returns a decode error only for malformed HTTP; timestamps are carried
// through unchanged from `m`.
//
// The construction must stay byte-equivalent to parseFinalHTTP. If the
// legacy helper gains a new field, mirror it here. A parity test in
// recordv2_test.go asserts field-by-field equivalence on identical
// input bytes (modulo timestamps, which are allowed to differ because
// legacy uses time.Now() and V2 uses chunk timestamps).
func (h *HTTP) buildHTTPMock(m *FinalHTTP, destPort uint, connID string, opts models.OutgoingOptions) (*models.Mock, error) {
	// Prefer the resolved destination port. The V2 recorder derives destPort from
	// the DestStream FakeConn, which in proxyless (observe-only) capture has no
	// real remote port (0) — breaking port-keyed telemetry-passthrough matching.
	// routeEgressToParser sets DstCfg.Port authoritatively; equal to the FakeConn
	// port on the proxy path, so this is a no-op there.
	if opts.DstCfg != nil && opts.DstCfg.Port != 0 {
		destPort = opts.DstCfg.Port
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(m.Req)))
	if err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	req.Header.Set("Host", req.Host)

	var reqBody []byte
	if req.Body != nil {
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		if req.Header.Get("Content-Encoding") != "" {
			reqBody, err = pkg.Decompress(h.Logger, req.Header.Get("Content-Encoding"), reqBody, pkg.MaxDecompressedSize)
			if err != nil {
				return nil, fmt.Errorf("decompress request body: %w", err)
			}
		}
	}

	// fault is the fault the mock is recorded with (FinalHTTP.Fault): one
	// before any answer always, one after the head only once the response
	// is known to be unfinished.
	var fault *models.Fault
	var respParsed *http.Response
	var cut bool
	if m.Fault != nil && !m.Fault.SendsHead() {
		// No answer: nothing to parse, and no response to record.
		fault = m.Fault
		respParsed = &http.Response{Header: http.Header{}}
	} else if respParsed, cut, err = parseFinalResponse(m, req); err != nil {
		return nil, err
	}

	var respBody []byte
	// wire is the body as the server sent it, encoded (respBody once decoded):
	// what a digest over the encoded bytes was computed over.
	var wire []byte
	if respParsed.Body != nil {
		respBody, err = io.ReadAll(respParsed.Body)
		// The answer ended unfinished, in a fault: its body was cut short of
		// its framing, or it is close-delimited and the upstream reset it or
		// the client gave up on it (a close is its end). Recorded as the
		// upstream sent it, its head as it came (Content-Length and
		// Transfer-Encoding as sent) and its body as the bytes on the wire,
		// undecoded, so replay sends the same bytes and then the fault.
		if m.Fault != nil && (errors.Is(err, io.ErrUnexpectedEOF) ||
			err == nil && m.Fault.Kind != models.FaultTruncate && closeDelimited(respParsed, req.Method)) {
			return h.buildFaultAfterHead(m, req, respParsed, destPort, connID, opts)
		}
		// A body its client read in part is recorded as far as it read it:
		// ReadAll returns what it read before the cut. Replay serves it with
		// its own length (the Content-Length set below).
		if err != nil {
			if !m.RespReadInPart || !errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("read response body: %w", err)
			}
			cut = true
		}
		wire = respBody
		if enc := respParsed.Header.Get("Content-Encoding"); enc != "" {
			// Read in part, a body with no length of its own (close-delimited)
			// cannot be known to be whole either: it decodes as far as it goes.
			// A whole one decodes the same either way.
			//
			// Not a response with no body by rule (to a HEAD, a 204, a 304):
			// its encoding describes a body it does not carry, and it is
			// recorded, or not, as a whole one is.
			if (cut || m.RespReadInPart) && !responseHasNoBody(req.Method, respParsed.StatusCode) {
				respBody, err = decompressReadInPart(enc, respBody, pkg.MaxDecompressedSize)
			} else {
				respBody, err = pkg.Decompress(h.Logger, enc, respBody, pkg.MaxDecompressedSize)
			}
			if err != nil {
				return nil, fmt.Errorf("decompress response body: %w", err)
			}
		}
		// A response with no body by rule (to a HEAD, a 1xx, 204 or 304)
		// keeps the Content-Length it was sent with: a HEAD's is the size of
		// the body a GET gets (an S3 client's Stat), not its own 0.
		if !responseHasNoBody(req.Method, respParsed.StatusCode) {
			respParsed.Header.Set("Content-Length", strconv.Itoa(len(respBody)))
		}
	}

	meta := map[string]string{
		"name":      "Http",
		"type":      models.HTTPClient,
		"operation": req.Method,
		"connID":    connID,
	}
	if utils.IsPassThrough(h.Logger, req, destPort, opts) {
		h.Logger.Debug("V2: request is a passThrough, skipping mock emit",
			zap.Any("metadata", utils.GetReqMeta(req)))
		return nil, nil
	}

	// Telemetry / noisy-egress passthrough (config-driven via
	// opts.PassThroughPorts/Hosts, with built-in defaults as "skip"):
	//   - skip:      do not record (live telemetry, not a dependency).
	//   - recordOne: keep ONE representative exchange per (method,host,port,path),
	//                preferring the first 2xx, tagged type:config so it becomes a
	//                session-lifetime mock served body-agnostically on replay.
	var ptRecordOne bool
	var ptKey string
	{
		ptHost := req.Host
		if ptHost == "" && req.URL != nil {
			ptHost = req.URL.Host
		}
		if rule, matched := passThroughRecordDecision(opts, ptHost, uint32(destPort), req.Method, req.URL, req.Header); matched {
			if rule.Mode == models.PassThroughSkip {
				h.Logger.Debug("egress passthrough: skip mode, not recording", zap.Any("metadata", utils.GetReqMeta(req)))
				return nil, nil
			}
			// recordOne: defer the dedup decision to emit time (below) so a later
			// drop can't burn the one representative and leave zero mocks.
			ptRecordOne = true
			ptKey = ptRecordKey(opts.PassThroughScope, req.Method, ptHost, uint32(destPort), req.URL.Path, rule.QueryKeys, req.URL.Query())
			meta["type"] = "config"
			meta["passthrough"] = string(models.PassThroughRecordOne)
		}
	}
	// A CORS preflight the server allowed, under the proven switch, is a
	// phase of the call it guards (corsPreflightPhase): protocol-phase, served
	// to any test as often as asked, owned by none.
	preflight := corsPreflightPhase(req, respParsed.StatusCode)
	if preflight {
		meta["type"] = "config"
	}

	// The digest headers the body matched as the server sent it, which a
	// replay recomputes over the body it sends (models.MetadataBodyDigestsKey),
	// once the exchange is known to be recorded: passthrough traffic is not.
	// Not of a body its client read only in part: no digest is of a part.
	if !cut && !m.RespReadInPart {
		if note := bodydigest.Note(pkg.ToYamlHTTPHeader(respParsed.Header), respBody, wire); note != "" {
			meta[models.MetadataBodyDigestsKey] = note
		}
	}

	// The lifetime the mock is stated (models.MetadataLifetimeKey), beside the
	// type tag older replayers read: an exchange is the data of the test it
	// was made in, a recordOne representative and an allowed CORS preflight
	// (corsPreflightPhase) are served to every test.
	ptLifetime := models.LifetimePerTest
	meta[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	if ptRecordOne || preflight {
		ptLifetime = models.LifetimeSession
		meta[models.MetadataLifetimeKey] = models.MetadataLifetimeSession
	}
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: meta,
			HTTPReq: &models.HTTPReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHTTPHeader(req.Header),
				Body:       string(reqBody),
				URLParams:  pkg.URLParams(req),
			},
			HTTPResp: &models.HTTPResp{
				StatusCode:    respParsed.StatusCode,
				StatusMessage: customReason(respParsed),
				Header:        pkg.ToYamlHTTPHeader(respParsed.Header),
				Body:          string(respBody),

				// Replay serves this response: keep what it takes to serve a
				// repeated header (Set-Cookie) on its own lines.
				HeaderLineLengths: pkg.ToYamlHTTPHeaderLineLengths(respParsed.Header),
			},
			Fault:            fault,
			Created:          time.Now().Unix(),
			ReqTimestampMock: m.ReqTimestampMock,
			ResTimestampMock: m.ResTimestampMock,
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        ptLifetime,
			LifetimeDerived: true,
		},
	}
	// Under the proven switch, the clock the request's stamp is on
	// (FinalHTTP.ReqClock), as keploy/integrations' Mongo v2 recorder names
	// its own: so a set that holds recordings of two record runs (a partial
	// record keeps the tests it did not record anew) orders this call only
	// against its own run's recordings (the replay's teardown bound). A
	// stamp the producer did not take names none, and the mock gets no
	// clock: an untrusted stamp never routes. Without the switch nothing is
	// written. The origin is the mock's own: the agent adds the process that
	// made the call to it, keeping its clock (starts.Registry.StampOrigin).
	if provenswitch.On() {
		if id := fakeconn.ClockID(m.ReqClock); id != "" {
			mock.Origin = &models.Origin{Clock: id}
		}
	}
	// recordOne dedup, committed only now that a mock is actually being emitted.
	if ptRecordOne && h.ptRecorder != nil && !h.ptRecorder.shouldRecord(ptKey, respParsed.StatusCode) {
		h.Logger.Debug("egress passthrough: recordOne, dropping duplicate telemetry mock", zap.Any("metadata", utils.GetReqMeta(req)))
		return nil, nil
	}
	return mock, nil
}

// buildFaultAfterHead builds the mock of a call whose answer the upstream cut
// short after its head (FinalHTTP.Fault, Fault.SendsHead): the response's head
// as it came, Content-Length and Transfer-Encoding as sent, its body as the
// bytes the upstream sent on the wire, undecoded, and the fault, with
// BodyBytes their count. Replay sends the head and those bytes, then enacts the
// fault. It is buildHTTPMock again with no answer to read, so the mock is
// what buildHTTPMock makes of the call (or nothing, for a passthrough), with
// the cut-short answer put on it.
func (h *HTTP) buildFaultAfterHead(m *FinalHTTP, req *http.Request, resp *http.Response, destPort uint, connID string, opts models.OutgoingOptions) (*models.Mock, error) {
	_, bodyStart, _, ok := messageHead(m.Resp, true)
	if !ok {
		return nil, fmt.Errorf("parse response: the head of an answer cut short did not arrive whole: %w", io.ErrUnexpectedEOF)
	}
	raw := m.Resp[bodyStart:]
	fault := *m.Fault
	fault.BodyBytes = int64(len(raw))
	mock, err := h.buildHTTPMock(&FinalHTTP{
		Req:              m.Req,
		ReqTimestampMock: m.ReqTimestampMock,
		ResTimestampMock: m.ResTimestampMock,
		ReqClock:         m.ReqClock,
		Fault:            &models.Fault{Kind: models.FaultEOF},
	}, destPort, connID, opts)
	if mock == nil || err != nil {
		return mock, err
	}
	header := resp.Header.Clone()
	if len(resp.TransferEncoding) > 0 {
		header.Set("Transfer-Encoding", strings.Join(resp.TransferEncoding, ", "))
	}
	mock.Spec.HTTPResp = &models.HTTPResp{
		StatusCode:        resp.StatusCode,
		StatusMessage:     customReason(resp),
		Header:            pkg.ToYamlHTTPHeader(header),
		Body:              string(raw),
		HeaderLineLengths: pkg.ToYamlHTTPHeaderLineLengths(header),
	}
	mock.Spec.Fault = &fault
	return mock, nil
}

// closeDelimited reports whether resp's body is framed by the connection's
// close (neither a Content-Length nor chunked), for a response that has a body.
func closeDelimited(resp *http.Response, method string) bool {
	return resp.ContentLength < 0 && len(resp.TransferEncoding) == 0 && !responseHasNoBody(method, resp.StatusCode)
}

// customReason is resp's reason phrase when it is not the standard one for its
// status code ("402 Card Declined"), for HTTPResp.StatusMessage, which replay
// serves: "" for the standard phrase, so a recording of one stays as before.
func customReason(resp *http.Response) string {
	reason := strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode))
	reason = strings.TrimPrefix(reason, " ")
	if reason == http.StatusText(resp.StatusCode) {
		return ""
	}
	return reason
}
