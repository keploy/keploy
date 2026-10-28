// Package http provides functionality for handling HTTP outgoing calls.
package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// debugOn reports whether l writes debug entries. A served call builds a
// debug entry's message or fields only when it does (logger.Check): with debug
// off, the copies of the request and the answer they held, the names of every
// mock in a pool and the boxed values were built on every call and dropped,
// a third of what a served 64 KB call allocated.
func debugOn(l *zap.Logger) bool { return l.Core().Enabled(zapcore.DebugLevel) }

// ErrMockNotMatched is returned by decodeHTTP when no recorded mock
// matches the outgoing HTTP request. It aliases models.ErrNoMockMatched so
// callers can errors.Is against either symbol to distinguish a mock-miss
// from a real proxy error.
var ErrMockNotMatched = models.ErrNoMockMatched

// Decodes the mocks in test mode so that they can be sent to the user application.
//
// The requests are read and answered on a goroutine, and decodeHTTP returns
// only once that goroutine has returned, ctx's end included: then its read or
// write of clientConn is ended (pUtil.InterruptOnDone), and it is waited for
// (pUtil.JoinParser). decodeHTTP used to return ctx's error at once and leave
// the goroutine blocked reading clientConn for the next request; once the
// client closed, it logged its failed read after decodeHTTP had returned (in a
// parser test, through the test's logger after the test had ended: a DATA RACE
// under go test -race).
func (h *HTTP) decodeHTTP(ctx context.Context, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	defer pUtil.InterruptOnDone(ctx, clientConn)()
	errCh := make(chan error, 1)
	go func(errCh chan error, reqBuf []byte, opts models.OutgoingOptions) {
		defer close(errCh)
		defer integrations.RecoverIntoCtx(ctx, h.Logger, clientConn, nil, errCh)
		for {
			// The whole header section first: only then is it known whether
			// the client is waiting for a 100 (Continue) before its body.
			if _, _, err := h.readRequestHead(ctx, &reqBuf, clientConn, nil); err != nil {
				errCh <- err
				return
			}
			if awaitsContinue(reqBuf) {
				h.Logger.Debug("the client awaits a 100 Continue before its body; writing it")
				_, err := clientConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(h.Logger, err, "failed to write the 100 continue response to the user application")
					errCh <- err
					return
				}
				// HandleChunkedRequests reads the body the client now sends,
				// by its framing.
			}

			h.Logger.Debug("handling the chunked requests to read the complete request")
			err := h.HandleChunkedRequests(ctx, &reqBuf, clientConn, nil)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to handle chunked requests")
				errCh <- err
				return
			}

			arrived := time.Now()

			//Parse the request buffer
			request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
			if err != nil {
				utils.LogError(h.Logger, err, "failed to parse the http request message")
				errCh <- err
				return
			}

			// Set the host header explicitly because the `http.ReadRequest`` trim the host header
			// func ReadRequest(b *bufio.Reader) (*Request, error) {
			// 	req, err := readRequest(b)
			// 	if err != nil {
			// 		return nil, err
			// 	}

			// 	delete(req.Header, "Host")
			// 	return req, err
			// }
			request.Header.Set("Host", request.Host)

			// A bypassRule that names this call (its host, path and port)
			// passes it to its server, and the server's answer back, unmocked.
			// Only port-only rules reach the kernel, so the proxy applies the
			// rest, here in keploy test as the recorder does in keploy record.
			if dstCfg != nil && utils.IsPassThrough(h.Logger, request, dstCfg.Port, opts) {
				h.Logger.Debug("a bypassRule names this call; passing it through to its server", zap.Any("metadata", utils.GetReqMeta(request)))
				ended, perr := passOneCallThrough(ctx, h.Logger, clientConn, dstCfg, reqBuf, request.Method)
				if perr != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(h.Logger, perr, "failed to pass a call a bypassRule names through to its server", zap.Any("metadata", utils.GetReqMeta(request)))
					errCh <- perr
					return
				}
				if ended || request.Close {
					// The call ended the connection: the client said
					// Connection: close, or the server's answer closes it
					// (or upgraded it, and the upgrade is over). No next
					// call comes on it.
					errCh <- nil
					return
				}
				reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
				if err != nil {
					h.Logger.Debug("bypassRule passthrough: client closed connection", zap.Error(err))
					errCh <- nil
					return
				}
				continue
			}

			reqBody, err := io.ReadAll(request.Body)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to read from request body", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- err
				return
			}

			input := &req{
				method:  request.Method,
				url:     request.URL,
				header:  request.Header,
				body:    reqBody,
				raw:     reqBuf,
				arrived: arrived,
			}

			// Under the proven switch the request's destination is read once,
			// as it comes: a use of its connection's container in the scope
			// open now (containerkey), which every match of it then reads.
			reqCtx := ctx
			if provenswitch.On() {
				reqCtx = withLiveDestination(ctx, liveDestination(ctx, input))
			}

			if input.header.Get("Content-Encoding") != "" {
				input.encoded = input.body
				input.body, err = pkg.Decompress(h.Logger, input.header.Get("Content-Encoding"), input.body, pkg.MaxDecompressedSize)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to decode the http request body", zap.Any("metadata", utils.GetReqMeta(request)))
					errCh <- err
					return
				}
			}

			// The request is customer content: its credentials, cookies,
			// query and body never go to the log, only its shape.
			if ce := h.Logger.Check(zap.DebugLevel, "decodeHTTP debug logs for input"); ce != nil {
				ce.Write(zap.String("method", input.method),
					zap.String("path", input.url.Path),
					zap.Strings("header names", headerNames(input.header)),
					zap.Int("body bytes", len(input.body)))
			}

			noise := requestNoiseFor(h.Logger, opts)
			headerNoise, bodyNoise, urlNoise := noise.Header, noise.Body, noise.URL

			if ce := h.Logger.Check(zap.DebugLevel, "header noise"); ce != nil {
				ce.Write(zap.Any("header noise", headerNoise))
			}
			if ce := h.Logger.Check(zap.DebugLevel, "body noise"); ce != nil {
				ce.Write(zap.Any("body noise", bodyNoise))
			}
			if ce := h.Logger.Check(zap.DebugLevel, "url noise"); ce != nil {
				ce.Write(zap.Any("url noise", urlNoise))
			}

			// Telemetry / noisy-egress passthrough. Configurable via
			// record.passThroughPorts / passThroughHosts (see models.PassThroughRule),
			// with built-in defaults (POST /v1/traces, /ingest) as a "skip" fallback.
			// These are live, fire-and-forget telemetry the app emits every run, not
			// recorded dependencies — matching them falls through to the HTTP
			// fuzzy-matcher, which shingles the large (~150–200 KB) bodies and OOMs
			// the sidecar (measured: ~432 MB of a ~1.1 GB peak). We short-circuit
			// BEFORE h.match:
			//   - skip:      synthesize a bare 200 (OTLP/HTTP & Pyroscope treat any
			//                2xx as success); no mock is consulted.
			//   - recordOne: serve ONE recorded response for (method,path)
			//                body-agnostically for every matching call (never
			//                consumed), falling back to a synthetic 200 when nothing
			//                was recorded — so replay never errors and the body is
			//                never fuzzy-matched.
			ptHost := request.Host
			if ptHost == "" && input.url != nil {
				ptHost = input.url.Host
			}
			var ptPort uint32
			if dstCfg != nil {
				ptPort = uint32(dstCfg.Port)
			}
			if rule, matched := passThroughEgressDecision(opts, ptHost, ptPort, input.method, input.url, request.Header); matched {
				mode := rule.Mode
				out := []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
				served := "synthetic-200"
				// Serve a recorded mock when one exists — for recordOne always, and
				// for skip too: an existing test-set may have legitimately recorded
				// this endpoint (before it became a skip default), and synthesizing a
				// 200 over it would change replay behaviour on unchanged test data.
				// Non-consuming; falls back to the synthetic 200 when nothing matches.
				if recorded := h.serveOnePassThroughMock(mockDb, input, ptHost, ptPort, rule.QueryKeys); recorded != nil {
					out = recorded
					served = "recorded"
				}
				if ce := h.Logger.Check(zap.DebugLevel, "egress passthrough: serving without mock match"); ce != nil {
					ce.Write(zap.String("mode", string(mode)), zap.String("served", served),
						zap.String("path", input.url.Path), zap.Any("metadata", utils.GetReqMeta(request)))
				}
				if _, werr := clientConn.Write(out); werr != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(h.Logger, werr, "egress passthrough: failed to write response", zap.Any("metadata", utils.GetReqMeta(request)))
					errCh <- werr
					return
				}
				// Read the next request on this keep-alive connection and loop,
				// mirroring the matched-response path below.
				reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
				if err != nil {
					h.Logger.Debug("egress passthrough: client closed connection", zap.Error(err))
					errCh <- nil
					return
				}
				continue
			}

			// Async serving short-circuit. When this outgoing request routes to
			// a configured async lane, the transport-agnostic engine — not the
			// normal mock matcher — decides what to serve: either a recorded
			// delivery (once its anchor position has been reached) or a
			// keep-alive payload to keep the client polling. Mirrors the
			// telemetry short-circuit above: write, read the next poll, continue.
			if h.asyncEngine != nil {
				live := liveReqToMock(input)
				if lane, laneOK := h.asyncEngine.LaneFor(live); laneOK {
					// failAsync reports err via errCh unless the context is
					// already cancelled; the caller returns right after.
					failAsync := func(e error, msg string) {
						if ctx.Err() != nil {
							return
						}
						utils.LogError(h.Logger, e, msg, zap.Any("metadata", utils.GetReqMeta(request)))
						errCh <- e
					}
					recorded, keepAlive, derr := h.asyncEngine.Decide(ctx, lane, live)
					if derr != nil {
						failAsync(derr, "async: engine decide failed")
						return
					}
					out := keepAlive
					if recorded != nil {
						if out, derr = h.buildMockResponseBytes(recorded); derr != nil {
							failAsync(derr, "async: failed to serialize recorded delivery")
							return
						}
					}
					if _, werr := clientConn.Write(out); werr != nil {
						failAsync(werr, "async: failed to write delivery/keep-alive to client")
						return
					}
					// Read the next poll on this keep-alive connection and loop,
					// mirroring the telemetry short-circuit and matched-response paths.
					reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
					if err != nil {
						h.Logger.Debug("async: client closed connection after delivery/keep-alive", zap.Error(err))
						errCh <- nil
						return
					}
					continue
				}
			}

			var (
				ok    bool
				stub  *models.Mock
				diag  *matchDiag
				lmiss *learnMiss
				// call is where the request's first read of the round
				// found it made (integrations.CallRound): a miss's lookup
				// of a recording made elsewhere is asked of that round.
				call integrations.CallRound
				// db is the store the request's matcher and its miss read:
				// under the switch held to call's staging
				// (integrations.HoldToRound), so a request is never answered
				// with, nor diagnosed against, a recording of a set the
				// replay staged after its first read. The miss's lookup of a
				// recording made elsewhere (notOwnedRecording) checks the
				// staging last, and says that it moved.
				db = mockDb
			)
			switch {
			case provenswitch.On() && opts.Learn && !integrations.LearnEnded(mockDb):
				// Record's full round: the learn step decides every request
				// (design v14 §7.2), and its first pass is the request's
				// first read of the round. Once the set's learn step ended
				// (a retry cycle, integrations.LearnEnder) a request is
				// served as a replay serves it, below: nothing is paired,
				// nor learned.
				stub, lmiss, err = h.learnMatchIn(reqCtx, input, mockDb, h.newLearnNoise(opts, bodyNoise, urlNoise), opts, &call)
				ok = stub != nil
				if !ok && err == nil {
					// Then the pairing, under the background rules:
					// a call that is exactly another test's recording is
					// answered with it, reported for judging, not missed.
					strict := h.newStrictNoise(headerNoise, bodyNoise, urlNoise)
					if answer, x := h.pairExactlyFrom(reqCtx, input, mockDb, strict, call); answer != nil {
						// Its repeat note is asked as a miss's is: of the
						// store held to the round the call first read
						// (HoldToRound), and none for a call one more than
						// the test recorded (repeatNoteOf).
						held, _ := integrations.HoldToRound(mockDb, call)
						extra := h.extraCall(reqCtx, input, held, headerNoise, bodyNoise, urlNoise, !opts.DisableAutoURLDynamic, opts.NoiseDetection(), opts.NoiseStrict(), !opts.DisableMockCorrelation)
						a, repeat := repeatNoteOf(reqCtx, input, held, strict, extra)
						reportPairing(reqCtx, input, x, a, repeat)
						stub, ok, lmiss = answer, true, nil
					}
				}
				if !ok {
					// The miss is diagnosed in the round the learn step's
					// first pass read.
					db, _ = integrations.HoldToRound(mockDb, call)
				}
			default:
				// Under the switch a replay first honours what the learn step
				// learned onto the recording; anything else goes on to the
				// matcher as before.
				proven := provenswitch.On()
				if proven {
					// The request's first read of the round, before any
					// matcher reads the store: its staging and view, kept
					// in O(1) for a miss to be asked of.
					call = integrations.MarkCallRound(mockDb)
					// Every matcher reads the store held to it: once the
					// replay stages another set, the request finds nothing
					// to be answered with, and misses saying so.
					db, _ = integrations.HoldToRound(mockDb, call)
					stub, err = h.learnedMatch(reqCtx, input, db, h.newLearnNoise(opts, bodyNoise, urlNoise), opts)
					ok = stub != nil
				}
				if !ok && err == nil {
					// Under the switch the learn step of record's full round
					// is the one request-noise learner, and what it learned is
					// always followed (design v14 §7.2 step 10; U18): no other
					// replay learns noise, and a learned correlation is never
					// switched off.
					detect, correlate := opts.NoiseDetection(), !opts.DisableMockCorrelation
					if proven {
						detect, correlate = false, true
					}
					ok, stub, diag, err = h.match(reqCtx, input, db, headerNoise, bodyNoise, urlNoise, !opts.DisableAutoURLDynamic, detect, opts.NoiseStrict(), !opts.DisableStatefulMocks, correlate) // calling match function to match mocks
				}
			}
			if err != nil {
				utils.LogError(h.Logger, err, "error while matching http mocks", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- err
				return
			}
			// The recording matched is named, not dumped: its credentials and
			// body are the application's content too, and the test set holds it.
			if ce := h.Logger.Check(zap.DebugLevel, "after matching the http request"); ce != nil {
				ce.Write(zap.Bool("isMatched", ok), zap.String("mock name", mockNameOf(stub)), zap.Error(err))
			}

			if !ok {
				// Build mismatch report for the user (surfaced in the mismatch
				// table, the report yaml and `keploy report`).
				if lmiss == nil && provenswitch.On() {
					// A call that came with an array in another order is
					// unstable_order in every round, not only the full one.
					lmiss = h.orderMiss(reqCtx, input, db, h.newLearnNoise(opts, bodyNoise, urlNoise))
				}
				if lmiss != nil {
					diag = lmiss.diag()
				}
				// Under exact serving: one call more than the test recorded?
				// Any other miss is diagnosed against the test's answered
				// recordings too (answeredDiag): they are in no read now.
				// Both read the request's destination as it came (reqCtx): a
				// reading now would be a use of its connection's container in
				// the scope open now, the next test's once a test ended.
				n := h.extraCall(reqCtx, input, db, headerNoise, bodyNoise, urlNoise, !opts.DisableAutoURLDynamic, opts.NoiseDetection(), opts.NoiseStrict(), !opts.DisableMockCorrelation)
				if n == 0 && lmiss == nil {
					// (A learn-step miss names the candidate it refused.)
					diag = h.answeredDiag(reqCtx, input, db, diag, headerNoise, bodyNoise, urlNoise)
				}
				report := h.buildHTTPMismatchReport(request, input.body, db, nil, headerNoise, bodyNoise, diag)
				if n > 0 && report != nil {
					report.MissKind, report.Recorded = models.MissExtraCall, n
				}
				// The miss was decided where the request first read the
				// round: whether that read found a staging is told from it.
				integrations.StampMark(report, call)
				// A call that is a recording made elsewhere (another test's,
				// or a worker-scope one not reached yet) is named as that
				// recording: it did not change, so no learn-step reason
				// explains it.
				// It is asked of the round the request's first read found it
				// made in (notOwnedRecording): never a recording that read
				// held as its view's own, nor one of a set the replay staged
				// under the request, which the miss then says.
				var elsewhere *models.Mock
				restaged := false
				if n == 0 && report != nil && provenswitch.On() {
					elsewhere, restaged = notOwnedRecording(reqCtx, input, db, h.newStrictNoise(headerNoise, bodyNoise, urlNoise), call)
				}
				if elsewhere != nil {
					markNotOwned(report, elsewhere)
				} else if lmiss != nil {
					lmiss.explain(report)
				}
				if restaged {
					report.NextSteps = strings.TrimSpace(report.NextSteps + " " + restagedNote)
				} else if why := integrations.UnheldWhy(db); why != "" && report != nil {
					// The store marked the request's round but could not
					// hold it: its matcher read nothing (HoldToRound).
					report.NextSteps = strings.TrimSpace(report.NextSteps + " " + why)
				}
				// A call identical to one an earlier test made, whose
				// recording already answered: the application asked again
				// where its recording did not (models.RepeatNote). In
				// record's full round the learn step finds no recorded call
				// of the test for it (unmocked_call), which the note keeps
				// as judging's code and explains; a learn reason that names
				// a recorded call of the test the request changed from is
				// kept as it is. A call made elsewhere keeps that reason and
				// its closest and gets the note beside it (markRepeat): at
				// record a test in between refilled the cache with the same
				// call, which this replay, inside the cache's span there,
				// never made, so its recording is what the call matches.
				if n == 0 && report != nil && (elsewhere != nil || lmiss == nil || lmiss.res.Reason == models.MissUnmockedCall) {
					if a, ok := repeatNoteOf(reqCtx, input, db, h.newStrictNoise(headerNoise, bodyNoise, urlNoise), n); ok {
						markRepeat(report, a)
					}
				}
				if report != nil {
					// Per-call HTTP detail at Debug; the canonical, protocol-
					// agnostic mock-mismatch WARN is emitted once in
					// proxy.sendMockNotFoundError (where every parser's miss
					// funnels), so this stays Debug to avoid double-logging.
					h.Logger.Debug("no matching http mock found for outgoing request",
						zap.String("protocol", report.Protocol),
						zap.String("destination", report.Destination),
						zap.String("actual", report.ActualSummary),
						zap.String("match_phase", report.MatchPhase),
						zap.Int("candidates", report.CandidateCount),
						zap.String("closest", report.ClosestMock),
						zap.String("diff", report.Diff),
						zap.String("next_step", report.NextSteps))
				}

				// On-miss policy (keploy mock replay --on-miss): passthrough or
				// record dials the real dependency and relays the exchange so an
				// unrecorded call succeeds instead of hard-failing. record also
				// captures it for the CLI to append to the set at end-of-run.
				if opts.OnMiss.PassesThroughOnMiss() {
					handled, phErr := h.serveOnMiss(ctx, clientConn, reqBuf, request, input.body, dstCfg, opts)
					if phErr != nil && ctx.Err() != nil {
						// The call's end cut the passthrough short (its
						// dial, the dependency's answer, which
						// pUtil.InterruptOnDone ends then, or the write to
						// the client): no answer is owed, and it is no miss.
						return
					}
					if phErr != nil {
						h.Logger.Debug("on-miss passthrough failed; falling back to hard miss", zap.Error(phErr), zap.Any("metadata", utils.GetReqMeta(request)))
					} else if handled {
						// Served live — continue the keep-alive loop for the next
						// request on this connection. Not reported as a miss.
						reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
						if err != nil {
							h.Logger.Debug("client closed connection after on-miss passthrough", zap.Error(err))
							errCh <- nil
							return
						}
						continue
					}
				}

				// No mock matched — send a 502 so the application gets a
				// proper HTTP error instead of a silent connection close.
				noMockBody := "keploy: no matching mock found for this HTTP request\n"
				noMock := fmt.Sprintf("HTTP/%d.%d 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
					request.ProtoMajor, request.ProtoMinor, len(noMockBody), noMockBody)
				baseErr := fmt.Errorf("%w for %s %s", ErrMockNotMatched, request.Method, request.URL.Path)
				missErr := models.NewMockMismatchError(baseErr, report)
				// The miss is queued for the test that made it before the app
				// sees the 502. The replay reads a test's misses once the app
				// has answered it (proxy.GetMockErrors), and the app answers
				// only after it has read the 502, so a test that ends as soon
				// as it reads the 502 still has its miss, and the next test is
				// never charged with it. That holds with the proven switch off
				// as well. Queued after the 502, as it was there, the miss
				// raced the app's answer: an app that answered first had its
				// miss charged to the test after it (enterprise pipeline
				// 11651: get-quote-1's GET /fx filed under get-rates-1, and
				// get-quote-1's difference learned as noise).
				reported := models.ReportMockMismatch(ctx, missErr)
				if _, err := clientConn.Write([]byte(noMock)); err != nil {
					h.Logger.Debug("failed to write 502 response to client", zap.Error(err), zap.Any("metadata", utils.GetReqMeta(request)))
				}
				switch {
				case reported && provenswitch.On():
					// Reported, it is not returned again.
					errCh <- nil
				case reported:
					// Returned as well, as reported: the connection ends and
					// the miss is logged as they always were with the switch
					// off, and it is not reported a second time.
					errCh <- models.MismatchReported{Err: missErr}
				default:
					errCh <- missErr
				}
				return
			}

			if stub == nil {
				utils.LogError(h.Logger, nil, "matched mock is nil", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- errors.New("matched mock is nil")
				return
			}

			// Serialize the matched mock's response to wire bytes (hydrates a
			// spilled body, recomputes Content-Length, compresses per
			// Content-Encoding). Shared with the async serving branch above.
			respBytes, err := h.buildMockResponseBytes(stub)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to serialize matched mock response", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- err
				return
			}
			if ce := h.Logger.Check(zap.DebugLevel, "Mock Response sending back to client"); ce != nil {
				ce.Write(zap.String("mock name", stub.Name), zap.Int("status", stub.Spec.HTTPResp.StatusCode),
					zap.Int("answer bytes", len(respBytes)))
			}

			_, err = clientConn.Write(respBytes)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				utils.LogError(h.Logger, err, "failed to write the mock output to the user application", zap.Any("metadata", utils.GetReqMeta(request)))
				errCh <- err
				return
			}

			reqBuf, err = pUtil.ReadBytes(ctx, h.Logger, clientConn)
			if err != nil {
				h.Logger.Debug("failed to read the request buffer from the client", zap.Error(err))
				if ce := h.Logger.Check(zap.DebugLevel, "This was the last response from the server"); ce != nil {
					ce.Write(zap.Int("answer bytes", len(respBytes)))
				}
				errCh <- nil
				return
			}
		}
	}(errCh, reqBuf, opts)

	return pUtil.JoinParser(ctx, errCh)
}

// withResponse returns m with its recorded response in Spec: m itself, or, when
// the agent's disk store kept the response apart from the mock (strict mock
// windows; see Mock.HasSpilledResponse), a copy of m with the response loaded.
// m is a pooled mock that other connections may be matching or copying, and
// HydrateResponse writes the mock it runs on, so it runs on the copy.
func withResponse(m *models.Mock) (*models.Mock, error) {
	if m == nil || !m.HasSpilledResponse() {
		return m, nil
	}
	loaded := m.ShallowCopy()
	if err := loaded.HydrateResponse(); err != nil {
		return nil, fmt.Errorf("http: load the recorded response of mock %q: %w", m.Name, err)
	}
	return loaded, nil
}

// buildMockResponseBytes serializes a recorded HTTP mock's response to raw
// wire bytes (status line + headers + recomputed Content-Length + body,
// compressing the body when Content-Encoding is set; encodeAnswer). It lives
// here (not in async.go) because it is the shared serializer for BOTH the
// ordinary matched-mock path and the async serving branch.
// A response still kept apart from its mock is loaded first (withResponse).
// A response with no body by rule (to a HEAD, a 1xx, 204 or 304) keeps the
// Content-Length it was recorded with: a HEAD's is the size of the body a GET
// gets, which the client takes for that size (an S3 client's Stat).
// An answer served again is written from the bytes built for it before
// (keptAnswers), unless it is served rewritten or read back per serve
// (keepsAnswer). The returned bytes are never to be written to.
func (h *HTTP) buildMockResponseBytes(stub *models.Mock) ([]byte, error) {
	return h.buildAnswer(stub, nil)
}

// buildAnswer is buildMockResponseBytes with rewrite applied to the answer's
// wire headers and body for this serve; nil serves the answer as recorded.
// A rewritten serve never reads or fills keptAnswers: the table's encoder
// (encodeRecordedAnswer) cannot rewrite, so a serve that changes what the
// recording answers builds its bytes for itself, and no serve is given
// another's rewritten answer.
func (h *HTTP) buildAnswer(stub *models.Mock, rewrite answerRewrite) ([]byte, error) {
	if rewrite == nil && keepsAnswer(stub) {
		return keptAnswers.serve(stub, h.encodeRecordedAnswer)
	}
	name := ""
	if stub != nil {
		name = stub.Name
	}
	stub, err := withResponse(stub)
	if err != nil {
		return nil, err
	}
	if stub == nil || stub.Spec.HTTPResp == nil {
		return nil, fmt.Errorf("http: mock %q has no response to serialize", name)
	}
	return h.encodeAnswer(stub, rewrite)
}

// RequestNoise is what of a request may differ from its recording when it is
// matched (decodeHTTP), from the run's options: header noise (the configured
// header noise and, unless --disableAutoHeaderNoise, the headers known to
// change on every call: SigV4's date and signature, an SDK's invocation id,
// a rotating security token), request-body noise (test.globalNoise
// .requestBody) and URL-path noise (test.globalNoise.url). Every parser of
// HTTP calls reads it from here (NewRequestNoise), so none drifts from
// another: the SQS parser's variable pass (VariableMatch) compared every
// header by value, and no signed SDK call fit a recording.
type RequestNoise struct {
	Header map[string][]string
	Body   map[string][]string
	URL    []string
}

// NewRequestNoise is the request noise of a call under opts.
func NewRequestNoise(logger *zap.Logger, opts models.OutgoingOptions) RequestNoise {
	// Extract header noise from noise configuration.
	// We make a shallow copy so that injecting default flaky headers
	// below does not mutate the shared opts.NoiseConfig map, which
	// may be accessed concurrently by other outgoing requests.
	// Keys are lowercased during copy because HeadersContainKeys
	// compares against lowercased keys.
	var headerNoise map[string][]string
	if opts.NoiseConfig != nil {
		if hn, ok := opts.NoiseConfig["header"]; ok {
			headerNoise = make(map[string][]string, len(hn))
			for k, v := range hn {
				lk := strings.ToLower(k)
				if existing, ok := headerNoise[lk]; ok {
					// Merge value slices on case-variant collision
					// to keep behaviour deterministic.
					headerNoise[lk] = append(existing, v...)
				} else {
					headerNoise[lk] = v
				}
			}
		}
	}

	// Auto-inject known flaky headers (e.g. AWS SigV4) into the
	// per-request copy so they are ignored during mock matching.
	// This prevents mismatches caused by headers whose values or
	// presence change on every request due to timestamps, signatures,
	// or credential rotation (e.g. X-Amz-Security-Token from IRSA).
	// Disable with --disableAutoHeaderNoise for strict matching.
	if !opts.DisableAutoHeaderNoise {
		if headerNoise == nil {
			headerNoise = make(map[string][]string)
		}
		for hdr, noise := range flakyHeaderNoise() {
			if _, exists := headerNoise[hdr]; !exists {
				headerNoise[hdr] = noise
			}
		}
	}

	// User request-body noise from test.globalNoise.requestBody
	// (root-relative dotted paths). This is a DEDICATED bucket,
	// distinct from the "body" bucket that governs RESPONSE
	// assertions: request-matching noise and response-assertion noise
	// are separate axes. Reusing the response "body" bucket here would
	// let a field noised only because it is dynamic in the response
	// silently soften request matching too (e.g. excusing a wrong
	// user-id under --schema-noise-strict). Keeping them separate makes
	// strict matching a real guarantee — a path can only weaken request
	// matching if the user puts it under requestBody on purpose.
	//
	// Lowercased copy for the same reason as headerNoise: the matcher's
	// noise index matches lowercased paths. Entries are normalized to
	// presence-only (empty regex list): request-body mock matching and
	// drift detection are path-based, so a value-regex cannot gate here.
	var bodyNoise map[string][]string
	if opts.NoiseConfig != nil {
		if bn, ok := opts.NoiseConfig["requestbody"]; ok {
			bodyNoise = make(map[string][]string, len(bn))
			for k, v := range bn {
				if len(v) > 0 {
					logger.Debug("body-noise value regexes are ignored for mock matching; the field is skipped by path",
						zap.String("field", k))
				}
				bodyNoise[strings.ToLower(k)] = []string{}
			}
		}
	}

	// User URL-path noise from test.globalNoise.url. The URL path is the
	// one request field matched by EXACT value with no noise hook, so a
	// non-deterministic path segment (id/uuid/timestamp/object key) made
	// the recorded mock unmatchable and produced a "no matching mock -> 502"
	// on replay. Each key here is a regex that wildcards the matching
	// segment in both the mock and request paths (see MatchURLPath). The
	// value slice is unused — like requestbody, matching is by pattern, not
	// by a value-regex — so an entry is presence-only.
	var urlNoise []string
	if opts.NoiseConfig != nil {
		if un, ok := opts.NoiseConfig["url"]; ok {
			urlNoise = make([]string, 0, len(un))
			for k := range un {
				urlNoise = append(urlNoise, k)
			}
		}
	}
	return RequestNoise{Header: headerNoise, Body: bodyNoise, URL: urlNoise}
}

// requestNoiseMemo is the request noise requestNoiseFor built last, beside
// what it was built from: a copy of the options' noise configuration, their
// auto-header-noise setting and the proven switch's state (flakyHeaderNoise).
type requestNoiseMemo struct {
	cfg         map[string]map[string][]string
	disableAuto bool
	proven      bool
	noise       RequestNoise
}

var lastRequestNoise atomic.Pointer[requestNoiseMemo]

// requestNoiseBuilds counts the request noise requestNoiseFor built.
var requestNoiseBuilds atomic.Int64

// requestNoiseFor is NewRequestNoise(logger, opts), built once per options
// value: a call whose options carry the noise configuration, the
// auto-header-noise setting and the proven switch's state the last build was
// made from gets that build. The configuration is compared by content, not by
// the map's identity, so options changed in place get a build of their own.
// The noise is shared by every call it is handed to, so no reader writes it
// (the matchers only read it). Building it on every request was about 15% of
// a served call's CPU.
func requestNoiseFor(logger *zap.Logger, opts models.OutgoingOptions) RequestNoise {
	proven := provenswitch.On()
	if m := lastRequestNoise.Load(); m != nil && m.proven == proven &&
		m.disableAuto == opts.DisableAutoHeaderNoise && sameNoiseConfig(m.cfg, opts.NoiseConfig) {
		return m.noise
	}
	requestNoiseBuilds.Add(1)
	// Built from a copy of its own, so nothing the options' maps share with
	// the noise (a header's patterns) is written through the options later.
	noise := NewRequestNoise(logger, models.OutgoingOptions{
		NoiseConfig:            cloneNoiseConfig(opts.NoiseConfig),
		DisableAutoHeaderNoise: opts.DisableAutoHeaderNoise,
	})
	lastRequestNoise.Store(&requestNoiseMemo{cfg: cloneNoiseConfig(opts.NoiseConfig),
		disableAuto: opts.DisableAutoHeaderNoise, proven: proven, noise: noise})
	return noise
}

// cloneNoiseConfig copies a noise configuration whole, keeping which maps and
// slices are nil.
func cloneNoiseConfig(cfg map[string]map[string][]string) map[string]map[string][]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]map[string][]string, len(cfg))
	for bucket, entries := range cfg {
		if entries == nil {
			out[bucket] = nil
			continue
		}
		c := make(map[string][]string, len(entries))
		for k, v := range entries {
			if v != nil {
				v = append(make([]string, 0, len(v)), v...)
			}
			c[k] = v
		}
		out[bucket] = c
	}
	return out
}

// sameNoiseConfig reports whether a and b hold the same noise configuration,
// down to which maps and slices are nil (NewRequestNoise reads a bucket that is
// present and empty unlike one that is absent).
func sameNoiseConfig(a, b map[string]map[string][]string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for bucket, ae := range a {
		be, ok := b[bucket]
		if !ok || (ae == nil) != (be == nil) || len(ae) != len(be) {
			return false
		}
		for k, av := range ae {
			bv, ok := be[k]
			if !ok || (av == nil) != (bv == nil) || len(av) != len(bv) {
				return false
			}
			for i := range av {
				if av[i] != bv[i] {
					return false
				}
			}
		}
	}
	return true
}

// headerNames are h's header names, sorted, without their values: a live
// request's (http.Header) or a recording's.
func headerNames[V any](h map[string]V) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// mockNameOf is m's name, or "" for no mock.
func mockNameOf(m *models.Mock) string {
	if m == nil {
		return ""
	}
	return m.Name
}

// passOneCallThrough sends one HTTP call, raw as the client sent it, to its
// server (dstCfg: its address, and its TLS config when the call came over TLS),
// and streams the server's answer to the client. The server's connection is
// closed after the answer. ended is whether the answer ended the client's
// connection too: the server closes after it (Connection: close, or a body
// that runs to the close), or upgraded it (101) and the upgraded connection is
// over, both closed here. Otherwise the client's stays open for its
// next call.
func passOneCallThrough(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, raw []byte, method string) (ended bool, err error) {
	if dstCfg.Addr == "" {
		return false, errors.New("the call has no destination address to pass it through to")
	}
	target := pUtil.DialTarget{Addr: dstCfg.Addr, Fabricated: dstCfg.AddrFabricated}
	var conn net.Conn
	if dstCfg.TLSCfg != nil {
		conn, err = pUtil.DialDestinationTLS(ctx, logger, "tcp", target, dstCfg.TLSCfg)
	} else {
		conn, err = pUtil.DialDestination(ctx, logger, "tcp", target)
	}
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", dstCfg.Addr, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := conn.Write(raw); err != nil {
		return false, fmt.Errorf("write the call to %s: %w", dstCfg.Addr, err)
	}
	br := bufio.NewReader(conn)
	for {
		resp, err := http.ReadResponse(br, &http.Request{Method: method})
		if err != nil {
			return false, fmt.Errorf("read the answer of %s: %w", dstCfg.Addr, err)
		}
		switch {
		case resp.StatusCode == http.StatusSwitchingProtocols:
			// An upgrade (a WebSocket): the connection is the server's from
			// here on, both ways, until either side ends it. Then both are
			// closed, as a reverse proxy does, so neither copy outlives the
			// call: a server that neither reads nor writes once the client
			// has gone would otherwise hold it open.
			if err := resp.Write(clientConn); err != nil {
				return false, fmt.Errorf("write the answer of %s to the client: %w", dstCfg.Addr, err)
			}
			copied := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(conn, clientConn); copied <- struct{}{} }()
			go func() { _, _ = io.Copy(clientConn, br); copied <- struct{}{} }()
			<-copied
			_ = conn.Close()
			_ = clientConn.Close()
			<-copied
			return true, nil
		case resp.StatusCode >= 100 && resp.StatusCode < 200:
			// An interim answer (100 Continue): the decode loop has
			// answered the client's Expect itself, so it is not relayed.
			continue
		}
		werr := resp.Write(clientConn)
		_ = resp.Body.Close()
		if werr != nil {
			return false, fmt.Errorf("write the answer of %s to the client: %w", dstCfg.Addr, werr)
		}
		// resp.Close: the server said Connection: close, or its body ran to
		// the close, which resp.Write tells the client as Connection: close.
		return resp.Close, nil
	}
}
