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
// cleanly on either stream reaching EOF or Close, not on ctx cancellation
// (at the recording's stop it reads on to its streams' end, and records what
// its capture holds whole), or on a malformed-HTTP decode error (the error is
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

	// The loop does not end on the context's end. The recording's stop ends
	// it (and the supervisor's abort, which closes the streams), and a
	// parser behind its capture then still holds what the relay captured
	// before the stop: every exchange whole in it is recorded, and the one
	// the stop cut is ended by its stream as anywhere else. The streams end
	// the loop: the relay ends them at its teardown, which the recording's
	// stop begins, once it has handed on what it captured (io.EOF), and the
	// abort closes them (fakeconn.ErrClosed once what they hold is read), so
	// no read waits on the network once the context has ended. A loop that
	// returned on the context's end lost those exchanges with nothing
	// recording, leaving out or reporting them.
	for {
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
			utils.LogError(logger, err, "V2 HTTP record: initial request read failed")
			return err
		}
		reqTs, reqClock := firstChunk.ReadAt, firstChunk.ReadClock
		var finalReq []byte

		// Read the request: its headers and body, and never a byte past its
		// end (readMessageV2), which is the start of the next request of a
		// pipeline when its client sent them in one write.
		if err := h.readRequestV2(sess.ClientStream, &finalReq); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				logger.Debug("V2 HTTP record: client stream closed mid-request", zap.Error(err))
				return nil
			}
			// The error can quote what the application sent (a parser's
			// does): withheld wherever it goes.
			err = utils.WithholdInput(err)
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
			// The error can quote what the application sent (a parser's
			// does): withheld wherever it goes.
			err = utils.WithholdInput(err)
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: initial response read failed: "+err.Error())
			utils.LogError(logger, err, "V2 HTTP record: initial response read failed")
			return err
		}
		var finalResp []byte
		resTs := firstRespChunk.WrittenAt
		// readResponseWrites notes each write it takes bytes of, the first too.
		var respWrites []respWrite

		gotLastWritten, err := h.readResponseWrites(sess.DestStream, &finalResp, requestMethod(finalReq), &respWrites)
		// A 101 (Switching Protocols) is the connection's last HTTP message:
		// the protocol it switched to carries the rest (passUpgradeThrough).
		_, _, status, _ := messageHead(finalResp, true)
		upgraded := status == http.StatusSwitchingProtocols
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
				// The error can quote what the application sent (a parser's
				// does): withheld wherever it goes.
				err = utils.WithholdInput(err)
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
		final := &FinalHTTP{
			Req:              finalReq,
			Resp:             finalResp,
			ReqTimestampMock: reqTs,
			ResTimestampMock: resTs,
			ReqClock:         reqClock,
			RespReadInPart:   streamEnded && sess.Opts.SkipTLSMITM && sess.EndedWithConnection(fakeconn.FromDest),
			RespWrites:       respWrites,
			Fault:            fault,
		}
		mock, err := h.buildHTTPMock(ctx, final, destPort, sess.ClientConnID, sess.Opts)
		if err != nil {
			// The error can quote what the application sent (a parser's
			// does): withheld wherever it goes.
			err = utils.WithholdInput(err)
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
			if emitV2(sess, logger, mock) {
				final.recordOne.giveBack()
			}
		} else {
			// Nothing recorded for this exchange (a passthrough), so a mark
			// set during it must not leave out the next exchange's mock.
			sess.MarkMockComplete()
		}
		if upgraded {
			passUpgradeThrough(sess)
			return nil
		}
	}
}

// passUpgradeThrough reads what sess's streams carry after a 101 (Switching
// Protocols) to their ends, and records none of it: from the 101's head on,
// the connection carries the protocol it switched to (a WebSocket's frames),
// both ways, and no HTTP message. The relay forwards those bytes as it
// forwards every byte. Read here, the client's are no request the parser left
// unread, so no test case is left out over them, and the capture holds none
// of them for no one. keploy read the server's as the 101's body, to the
// connection's close, and only then recorded the 101: at the recording's
// stop, partway through that "body", the exchange was left out.
func passUpgradeThrough(sess *supervisor.Session) {
	dest := make(chan struct{})
	go func() {
		defer close(dest)
		for {
			if _, err := sess.DestStream.ReadChunk(); err != nil {
				return
			}
		}
	}()
	for {
		if _, err := sess.ClientStream.ReadChunk(); err != nil {
			break
		}
	}
	<-dest
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
	mock, err := h.buildHTTPMock(ctx, m, destPort, sess.ClientConnID, sess.Opts)
	if err != nil {
		// The error can quote what the application sent (a parser's
		// does): withheld wherever it goes.
		err = utils.WithholdInput(err)
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
	if emitV2(sess, logger, mock) {
		m.recordOne.giveBack()
	}
	return nil
}

// leftOutRefusedAtStop is the reason emitV2 leaves out a mock the session
// refused as its context ended.
const leftOutRefusedAtStop = "the recording stopped as the mock of an exchange read whole was handed over"

// emitV2 hands the session m, the mock of an exchange recordV2 read whole,
// whether or not the session's context (Session.Ctx) has ended: EmitMock
// while it lasts, EmitMockOnShutdown once it has ended, as at the recording's
// stop, where EmitMock refuses every mock. A mock EmitMock refuses all the
// same, as the context ends between the check and its own, is left out and
// said so (leftOutRefusedAtStop), not dropped: EmitMock refuses a mock only
// for its context's end, after which the recording goes on with
// EmitMockOnShutdown. It returns whether m was lost: left out, by the session
// or here, or dropped by the session's manager as it was handed over
// (EmitMockOutcome), which counted it. A recordOne representative lost so is
// not one: its caller gives its mark back (ptMark.giveBack).
func emitV2(sess *supervisor.Session, logger *zap.Logger, m *models.Mock) (lost bool) {
	if c := sess.Ctx; c != nil && c.Err() != nil {
		lost, _ = sess.EmitMockOutcome(m, true)
		return lost
	}
	lost, err := sess.EmitMockOutcome(m, false)
	if err != nil {
		logger.Debug("V2 HTTP record: the session refused a mock as its context ended", zap.Error(err))
		sess.ReportLeftOut(m, leftOutRefusedAtStop)
		return true
	}
	return lost
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
func (h *HTTP) readRequestV2(stream *fakeconn.FakeConn, finalReq *[]byte) error {
	if _, err := readMessageV2(stream, finalReq, &messageFrame{}, nil); err != nil {
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
func (h *HTTP) readResponseV2(stream *fakeconn.FakeConn, finalResp *[]byte, reqMethod string) (time.Time, error) {
	var writes []respWrite
	return h.readResponseWrites(stream, finalResp, reqMethod, &writes)
}

// readResponseWrites is readResponseV2 that also notes, in writes, where in
// *finalResp each chunk it took bytes of ended and when the relay wrote it to
// the client (respWrite): the answer's marks (answerMarks). The answer to a
// HEAD, a 204, a 304 and a 101 is its head alone whatever its headers say
// (framedHeadOnly): what came after the head in its read is no part of it (a
// 101's: the switched-to protocol's), and stays on the stream.
func (h *HTTP) readResponseWrites(stream *fakeconn.FakeConn, finalResp *[]byte, reqMethod string, writes *[]respWrite) (time.Time, error) {
	return readMessageV2(stream, finalResp, &messageFrame{response: true, reqMethod: reqMethod, body: chunkedBody{response: true}}, writes)
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
// When writes is not nil, it notes where in *msg each chunk it took bytes of
// ended, and that chunk's WrittenAt (noteWrite). It returns the WrittenAt of
// the last chunk it took bytes of (zero when none carried one), and frame's
// error or the stream's, the message unfinished.
func readMessageV2(stream *fakeconn.FakeConn, msg *[]byte, frame *messageFrame, writes *[]respWrite) (time.Time, error) {
	var lastWr time.Time
	_, done, err := frame.end(*msg)
	for !done && err == nil {
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
		if writes != nil {
			noteWrite(writes, len(*msg), c.WrittenAt)
		}
	}
	return lastWr, err
}

// messageFrame finds where one HTTP/1 message ends as its bytes arrive. The
// header section that governs its body (a response's final one, past any
// interim 1xx: messageHead) says how the body is framed: none (the answer to a
// HEAD, a 204, a 304 or a 101: framedHeadOnly; a request with neither field
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
// grow between calls. A header section net/http's reader refuses
// (parseHeaders: a Content-Length that is not one length, a negative one
// among them) and a malformed chunked body are errors. A head-only answer's
// framing fields are read too, as net/http reads them.
func (f *messageFrame) end(msg []byte) (int, bool, error) {
	head, bodyStart, status, ok := messageHead(msg, f.response)
	if !ok {
		return 0, false, nil
	}
	contentLength, chunked, err := parseHeaders(head, f.response)
	if err != nil {
		return 0, false, err
	}
	if f.response && framedHeadOnly(f.reqMethod, status) {
		return bodyStart, true, nil
	}
	if contentLength >= 0 {
		if len(msg)-bodyStart < contentLength {
			return 0, false, nil
		}
		return bodyStart + contentLength, true, nil
	}
	if chunked {
		done, err := f.body.feed(msg[bodyStart:])
		return bodyStart + f.body.end, done, err
	}
	// Neither field: a request has no body, and a response's runs until the
	// server closes (RFC 9112 §6.3: HTTP/1.0 and "Connection: close").
	return bodyStart, !f.response, nil
}

// errFramingRefused is returned for a message whose header section net/http's
// reader refuses, or frames otherwise than keploy would: keploy does not frame
// it, and it is passed through whole. It names the fields only, not their
// values, which are the app's bytes (see contentLengthValue).
var errFramingRefused = errors.New("the message's header section is one net/http refuses or frames otherwise")

// parseHeaders reads the body framing of the HTTP message whose header section
// is head, as messageHead returns it: a start line (a status line when
// response is true, a request line when not), field lines read as
// headerFields reads them, and the empty line that ends it. contentLength is
// the length its Content-Length gives, or -1 when it has none; chunked is
// whether a "Transfer-Encoding: chunked" frames it, which overrides a
// Content-Length sent with it (RFC 9112 §6.3), so contentLength is -1 then.
// With an error it gives no framing: -1 and false. It reads the framing
// fields of an answer that has no body (a HEAD's, a 1xx, 204 or 304) too, as
// net/http's reader does before it looks at the status: the caller drops that
// framing, and passes through one parseHeaders refuses.
//
// It frames a message only as net/http's reader (ReadRequest, ReadResponse)
// frames it, and refuses (errFramingRefused, or errBadContentLength for a
// value that is no length) one it refuses, so the caller passes it through
// whole rather than end it where its peer does not:
//   - a start line (startLine) or a header section (headerFields) it refuses;
//   - a Content-Length that is not decimal digits below 2^63 ("+5", "-5",
//     "5, 5", "", "9223372036854775808"), or two that differ ("5" and "6",
//     or "5" and "05"); two that are the same are one length;
//   - on HTTP/1.1 and later (HTTP/0.0 is read as HTTP/1.1), a
//     Transfer-Encoding that is not the one coding chunked ("gzip, chunked",
//     or two Transfer-Encoding fields), and a chunked message whose Trailer
//     names Content-Length, Transfer-Encoding or Trailer;
//   - a request with two Host fields.
//
// It also refuses two messages it could frame as net/http does, where that
// framing is not one every peer shares:
//   - a Content-Length or a Transfer-Encoding written with a space before its
//     colon: net/http's server refuses the request, and its client reads the
//     field as another one;
//   - a Transfer-Encoding on a message below HTTP/1.1, which net/http ignores
//     and RFC 9112 §6.1 has a recipient treat as faulty framing.
func parseHeaders(head []byte, response bool) (contentLength int, chunked bool, err error) {
	major, minor, _, ok := startLine(head, response)
	if !ok {
		return -1, false, fmt.Errorf("%w: a start line net/http's reader refuses", errFramingRefused)
	}
	if major == 0 && minor == 0 {
		major, minor = 1, 1 // net/http's reader frames HTTP/0.0 as HTTP/1.1
	}
	contentLength = -1
	var length, coding []byte
	var trailers [][]byte
	lengths, codings, hosts := 0, 0, 0
	var fieldErr error
	err = headerFields(head, func(name, value []byte) {
		field := strings.ToLower(string(bytes.TrimRight(name, " "))) // token bytes: ASCII
		if len(field) != len(name) {
			// A name no other field has, to net/http's reader.
			if field == "content-length" || field == "transfer-encoding" {
				fieldErr = fmt.Errorf("%w: a space before a %s field's colon", errFramingRefused, field)
			}
			return
		}
		switch field {
		case "host":
			hosts++
		case "trailer":
			trailers = append(trailers, value)
		case "transfer-encoding":
			coding = value
			codings++
		case "content-length":
			value = trimWhiteSpace(value) // a value's end, which a fold can leave
			n, valueErr := contentLengthValue(string(value))
			switch {
			case valueErr != nil:
				fieldErr = valueErr
			case lengths > 0 && !bytes.Equal(value, length):
				fieldErr = fmt.Errorf("%w: two Content-Length fields that differ", errFramingRefused)
			default:
				contentLength, length = n, value
				lengths++
			}
		}
	})
	if err == nil {
		err = fieldErr
	}
	if err == nil && !response && hosts > 1 {
		err = fmt.Errorf("%w: a request with two Host fields", errFramingRefused)
	}
	if err != nil {
		return -1, false, err
	}
	if codings == 0 {
		return contentLength, false, nil
	}
	if major < 1 || major == 1 && minor < 1 {
		return -1, false, fmt.Errorf("%w: a Transfer-Encoding below HTTP/1.1", errFramingRefused)
	}
	if codings > 1 || !asciiEqualFold(coding, "chunked") {
		return -1, false, fmt.Errorf("%w: a Transfer-Encoding other than the one coding chunked", errFramingRefused)
	}
	for _, names := range trailers {
		if trailerNamesFraming(names) {
			return -1, false, fmt.Errorf("%w: a Trailer that names a framing field", errFramingRefused)
		}
	}
	return -1, true, nil
}

// trailerNamesFraming reports whether names, a Trailer field's value (a list
// of field names), names Content-Length, Transfer-Encoding or Trailer, which
// net/http's reader refuses in a chunked message: a trailer section cannot
// frame the message it ends.
func trailerNamesFraming(names []byte) bool {
	for _, name := range bytes.Split(names, []byte(",")) {
		name = trimWhiteSpace(name)
		if asciiEqualFold(name, "content-length") || asciiEqualFold(name, "transfer-encoding") || asciiEqualFold(name, "trailer") {
			return true
		}
	}
	return false
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
func (h *HTTP) buildHTTPMock(ctx context.Context, m *FinalHTTP, destPort uint, connID string, opts models.OutgoingOptions) (*models.Mock, error) {
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
		// The parser's error quotes the request: withheld, as the error
		// is logged and reported.
		return nil, fmt.Errorf("parse request: %w", utils.WithholdInput(err))
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
			return h.buildFaultAfterHead(ctx, m, req, respParsed, destPort, connID, opts)
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
	// When each part of the answer reached the client: replay writes each
	// part at its place. A whole answer the upstream ended in a fault after
	// it is recorded with
	// no fault, and is placed as any whole answer; one its client read in
	// part (the only answer cut short that is recorded) has no place for the
	// rest it did not read.
	var marks *models.HTTPAnswerMarks
	if fault == nil && !m.RespReadInPart {
		marks = answerMarks(m.Resp, m.RespWrites, respParsed.Header.Get("Content-Encoding"), len(respBody))
	}

	meta := map[string]string{
		"name":      "Http",
		"type":      models.HTTPClient,
		"operation": req.Method,
		"connID":    connID,
	}
	if utils.IsPassThrough(h.Logger, req, destPort, opts) {
		h.Logger.Debug("V2: request is a passThrough, skipping mock emit",
			zap.Any("metadata", utils.GetReqMeta(ctx, req)))
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
				h.Logger.Debug("egress passthrough: skip mode, not recording", zap.Any("metadata", utils.GetReqMeta(ctx, req)))
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
				Marks:             marks,
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
	// It is m's (FinalHTTP.recordOne), given back if the recording loses the
	// mock as it is handed over.
	if ptRecordOne && h.ptRecorder != nil {
		if m.recordOne = h.ptRecorder.take(ptKey, respParsed.StatusCode); m.recordOne == nil {
			h.Logger.Debug("egress passthrough: recordOne, dropping duplicate telemetry mock", zap.Any("metadata", utils.GetReqMeta(ctx, req)))
			return nil, nil
		}
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
func (h *HTTP) buildFaultAfterHead(ctx context.Context, m *FinalHTTP, req *http.Request, resp *http.Response, destPort uint, connID string, opts models.OutgoingOptions) (*models.Mock, error) {
	_, bodyStart, _, ok := messageHead(m.Resp, true)
	if !ok {
		return nil, fmt.Errorf("parse response: the head of an answer cut short did not arrive whole: %w", io.ErrUnexpectedEOF)
	}
	raw := m.Resp[bodyStart:]
	fault := *m.Fault
	fault.BodyBytes = int64(len(raw))
	noAnswer := &FinalHTTP{
		Req:              m.Req,
		ReqTimestampMock: m.ReqTimestampMock,
		ResTimestampMock: m.ResTimestampMock,
		ReqClock:         m.ReqClock,
		Fault:            &models.Fault{Kind: models.FaultEOF},
	}
	mock, err := h.buildHTTPMock(ctx, noAnswer, destPort, connID, opts)
	m.recordOne = noAnswer.recordOne
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
