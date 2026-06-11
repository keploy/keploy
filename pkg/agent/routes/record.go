// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// OutgoingHandlerStats are package-level atomic counters that the
// black-box recorder in cli/agent.go reads every second to track the
// /outgoing HTTP stream handler's state without needing to thread a
// handle through the cobra command graph. All fields are monotonic
// counters that can be safely read concurrently.
//
// These together with syncMock.ShutdownSnapshot() answer the question
// "where in the agent-side pipeline are mocks being lost?":
//
//	syncmock.totalAdded - outgoingForwardedTotal  =  mocks that entered
//	                                                 syncMock but never
//	                                                 reached the host
//	                                                 HTTP stream
//
//	outgoingHandlerStarted - outgoingHandlerExited  =  number of
//	                                                 /outgoing stream
//	                                                 handlers currently
//	                                                 in flight (= 1 in
//	                                                 normal recording, 0
//	                                                 when the host has
//	                                                 disconnected)
var (
	outgoingForwardedTotal atomic.Int64
	outgoingHandlerStarted atomic.Int64
	outgoingHandlerExited  atomic.Int64
	outgoingLastForwardMs  atomic.Int64 // wall-clock UnixMilli of last successful gob.Encode
)

// OutgoingForwardedTotal returns the lifetime count of mocks
// successfully gob-encoded onto the /outgoing HTTP response stream
// across every handler invocation in this process.
func OutgoingForwardedTotal() int64 { return outgoingForwardedTotal.Load() }

// OutgoingHandlerInFlight returns the number of currently-active
// /outgoing stream handlers (started minus exited). Normal value is
// 1 while the host is recording, 0 when the host has disconnected
// and not yet reconnected.
func OutgoingHandlerInFlight() int64 {
	return outgoingHandlerStarted.Load() - outgoingHandlerExited.Load()
}

// OutgoingHandlerStartedTotal returns the lifetime count of
// /outgoing stream handler invocations.
func OutgoingHandlerStartedTotal() int64 { return outgoingHandlerStarted.Load() }

// OutgoingHandlerExitedTotal returns the lifetime count of
// /outgoing stream handler returns.
func OutgoingHandlerExitedTotal() int64 { return outgoingHandlerExited.Load() }

// OutgoingLastForwardUnixMs returns the wall-clock millisecond
// timestamp of the most recent successful mock forward, or 0 if
// none has happened yet. The recorder uses this to flag "stream is
// alive but stalled" — when the value stops advancing while the
// host is still recording.
func OutgoingLastForwardUnixMs() int64 { return outgoingLastForwardMs.Load() }

type Agent struct {
	logger *zap.Logger
	svc    agent.Service
}

// agentReadyFilePath is the path MakeAgentReady writes on success. It
// defaults to kdocker.AgentReadyFile (the canonical /tmp/agent.ready
// location consumed by the docker-compose healthcheck) and is a var
// only so unit tests can redirect it into a sandbox without writing
// into /tmp on the host. Production code MUST NOT mutate it.
var agentReadyFilePath = kdocker.AgentReadyFile

// firstCARefusalLog ensures we emit exactly one Info-level line the
// first time /agent/ready is called before the CA bundle is written.
// This is the observability signal operators rely on — subsequent
// calls log at Debug to avoid flooding logs when docker-compose /
// kubelet polls readiness aggressively during boot.
var firstCARefusalLog sync.Once

// firstCAFailureLog ensures we emit exactly one Error-level line the
// first time /agent/ready observes a terminal SetupCA failure. The
// condition is latched for the process lifetime (MarkCAFailed records
// an error that never clears without an agent restart), so every
// subsequent readiness poll would re-emit the same Error — which
// floods operator logs and can dominate aggregators during incident
// response. After the first emission, repeated polls log at Debug.
var firstCAFailureLog sync.Once

func (d DefaultRoutes) New(r chi.Router, agent agent.Service, logger *zap.Logger) {
	a := &Agent{
		logger: logger,
		svc:    agent,
	}

	r.Route("/agent", func(r chi.Router) {
		r.Get("/health", a.Health)
		r.Post("/incoming", a.HandleIncoming)
		r.Post("/outgoing", a.HandleOutgoing)
		r.Post("/mappings", a.HandleMappings)
		r.Post("/mock", a.MockOutgoing)
		r.Post("/storemocks", a.StoreMocks)
		r.Post("/updatemockparams", a.UpdateMockParams)
		r.Post("/stop", a.Stop)
		// r.Post("/testbench", a.SendKtInfo)
		r.Get("/consumedmocks", a.GetConsumedMocks)
		r.Get("/mockerrors", a.GetMockErrors)
		r.Post("/agent/ready", a.MakeAgentReady)
		r.Post("/graceful-shutdown", a.HandleGracefulShutdown)
		// Long-lived streaming endpoints. /pcap/traffic emits a
		// pcap-format byte stream (file header + one record per
		// packet) over chunked transfer-encoding; /pcap/keylog
		// emits NSS keylog lines as TLS handshakes happen. The
		// recorder holds these connections open for the entire
		// recording session — they exit only when the recorder
		// closes the request or capture stops.
		r.Get("/pcap/traffic", a.HandlePcapStream)
		r.Get("/pcap/keylog", a.HandleKeylogStream)
		r.Post("/hooks/before-simulate", a.HandleBeforeSimulate)
		r.Post("/hooks/after-simulate", a.HandleAfterSimulate)
		r.Post("/hooks/before-test-run", a.HandleBeforeTestRun)
		r.Post("/hooks/before-test-set-compose", a.HandleBeforeTestSetCompose)
		r.Post("/hooks/after-test-run", a.HandleAfterTestRun)
	})
}

type DefaultRoutes struct{}

type RouteHook interface {
	New(r chi.Router, agent agent.Service, logger *zap.Logger)
}

var (
	ActiveHooks RouteHook = &DefaultRoutes{}
)

func RegisterHooks(h RouteHook) {
	ActiveHooks = h
}

func (a *Agent) HandleBeforeTestRun(w http.ResponseWriter, r *http.Request) {
	var req models.BeforeTestRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := agent.ActiveHooks.BeforeTestRun(r.Context(), req.TestRunID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) HandleBeforeTestSetCompose(w http.ResponseWriter, r *http.Request) {
	var req models.BeforeTestSetCompose
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Rotate the debug-file sink (if attached via KEPLOY_DEBUG_FILE) to
	// a per-test-set scope. This is the per-test-set boundary — fires
	// exactly once for the test set when the agent comes up in
	// DockerCompose mode, before any test case runs. Rotating here
	// (rather than per test case via BeforeSimulate) means each test
	// set gets exactly one rotation and the per-set file captures the
	// agent's pre-simulate work (mock load, store-mocks, etc.), not
	// just the simulate path.
	if req.TestSetID != "" {
		if err := log.RotateDebugFileForTestSet(req.TestSetID); err != nil {
			a.logger.Warn("debug file rotation for test set failed; continuing without rotation",
				zap.String("testSetID", req.TestSetID), zap.Error(err))
		}
	}

	if err := agent.ActiveHooks.BeforeTestSetCompose(r.Context(), req.TestSetID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) HandleAfterTestRun(w http.ResponseWriter, r *http.Request) {
	var req models.AfterTestRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := agent.ActiveHooks.AfterTestRun(r.Context(), req.TestRunID, req.TestSetIDs, req.Coverage); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) HandleBeforeSimulate(w http.ResponseWriter, r *http.Request) {
	var req models.BeforeSimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	if err := agent.ActiveHooks.BeforeSimulate(r.Context(), req.TimeStamp, req.TestSetID, req.TestCaseName); err != nil {
		a.logger.Error("failed to execute before simulate hook", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) HandleAfterSimulate(w http.ResponseWriter, r *http.Request) {
	var req models.AfterSimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	if err := agent.ActiveHooks.AfterSimulate(r.Context(), req.TestSetID, req.TestCaseName); err != nil {
		a.logger.Error("failed to execute after simulate hook", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) Stop(w http.ResponseWriter, _ *http.Request) {
	// Stop the agent first
	if err := utils.Stop(a.logger, "stop requested via agent API"); err != nil {
		a.logger.Error("failed to stop agent", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		if _, writeErr := w.Write([]byte("Failed to stop agent\n")); writeErr != nil {
			a.logger.Error("failed to write error response", zap.Error(writeErr))
		}
		return
	}

	// Send response after agent has stopped successfully
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("Agent stopped successfully\n")); err != nil {
		a.logger.Error("failed to write response", zap.Error(err))
	}
}

func (a *Agent) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	render.JSON(w, r, "OK")
}

// HandlePcapStream is a long-lived chunked response that emits a
// pcap-format byte stream — the file header followed by one record
// per captured frame, flushed to the wire as packets arrive.
// Returns 503 when capture is not active so the recorder can
// distinguish "feature off" from a transport failure.
func (a *Agent) HandlePcapStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by transport", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Subscribe synchronously BEFORE committing the status code.
	// SubscribePcap writes the pcap file header into w, which
	// triggers an implicit 200 in net/http — this is intentional.
	// If capture is not active it returns an error without writing
	// to w, so we can still return 503 cleanly.
	unsub, err := a.svc.SubscribePcap(w, flusher.Flush)
	if err != nil {
		http.Error(w, "packet capture not active: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer unsub()
	flusher.Flush() // push the pcap file header to the client
	<-r.Context().Done()
}

// HandleKeylogStream is a long-lived chunked response that emits
// NSS keylog lines as stdlib crypto/tls writes them during
// handshakes. Subscriber-style: lines arrive only while the
// connection is held; recorders that connect late see only future
// handshakes, not historical ones.
func (a *Agent) HandleKeylogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by transport", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	// Wrap so each Write flushes immediately — keylog lines are
	// short and bursty, and Wireshark wants them as soon as the
	// matching encrypted frames arrive in the parallel pcap stream.
	if err := a.svc.StreamKeylog(r.Context(), &flushOnWrite{w: w, flusher: flusher}); err != nil {
		a.logger.Warn("keylog stream ended with error", zap.Error(err))
	}
}

// flushOnWrite invokes http.Flusher.Flush after every Write so
// streamed bytes leave the agent's buffer immediately.
type flushOnWrite struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (f *flushOnWrite) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 {
		f.flusher.Flush()
	}
	return n, err
}

func (a *Agent) HandleIncoming(w http.ResponseWriter, r *http.Request) {
	a.logger.Debug("Received request to handle incoming test cases")

	// Flush headers to ensure the client gets the response immediately
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	// Set up Multipart Writer
	mw := multipart.NewWriter(w)
	defer func() {
		if err := mw.Close(); err != nil {
			a.logger.Error("failed to close multipart writer", zap.Error(err))
		}
		// Flush the final boundary so the client sees a clean EOF
		// instead of "unexpected EOF" when the connection tears down.
		flusher.Flush()
	}()
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.Header().Set("Cache-Control", "no-cache")

	// Create a context with the request's context to manage cancellation
	errGrp, _ := errgroup.WithContext(r.Context())
	ctx := context.WithValue(r.Context(), models.ErrGroupKey, errGrp)

	// decode request body
	var incomingReq models.IncomingReq
	err := json.NewDecoder(r.Body).Decode(&incomingReq)
	if err != nil {
		http.Error(w, "Error decoding request", http.StatusBadRequest)
		return
	}

	tc, err := a.svc.StartIncomingProxy(ctx, incomingReq.IncomingOptions)
	if err != nil {
		stopReason := "failed to start the ingress proxy"
		a.logger.Error(stopReason, zap.Error(err))
		http.Error(w, "Error starting incoming proxy", http.StatusInternalServerError)
		return // Important: return after handling the error
	}

	a.logger.Debug("Streaming incoming test cases to client")

	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	a.logger.Debug("Incoming stream connection established and headers flushed")

	// Keep the connection alive and stream data.
	// Use select (not for-range) so context cancellation is checked
	// concurrently with channel receive — otherwise the handler blocks
	// forever during shutdown when no test cases are arriving.
	var tcsSentSoFar int       // TCs sent to CLI this session
	var tcsSuppressedSoFar int // TCs suppressed because pressure overlapped the TC's HTTP window
	// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
	// var diagRangesDumped bool  // DIAG: ensure the pressure-ranges snapshot is logged only once
	for {
		select {
		case <-r.Context().Done():
			a.logger.Debug("Client closed the connection or context was cancelled")
			return
		case t, ok := <-tc:
			if !ok {
				// Channel closed = recording session over.
				_, finalDropped, finalAdded, _ := syncmgr.Get().GetDropStats()
				a.logger.Info("agent: recording complete",
					zap.Int("tcs_sent_to_cli", tcsSentSoFar),
					zap.Int("tcs_suppressed_pressure_overlap", tcsSuppressedSoFar),
					zap.Int("pressure_ranges_total", syncmgr.Get().PressureRangeCount()),
					zap.Int64("mocks_dropped_by_pressure", finalDropped),
					zap.Int64("mocks_added_successfully", finalAdded),
				)
				return
			}

			// Bug 0 fix: suppress this TC if memory pressure was active at any
			// moment during its HTTP window [HTTPReq.Timestamp, HTTPResp.Timestamp].
			// We use pressure-range overlap (not per-mock-drop timestamps) because
			// the Mongo-mock goroutine and the HTTP-TC goroutine race: a paired
			// mock can be dropped AFTER the TC has already reached this handler,
			// in which case a per-mock ledger would still be empty. Pressure
			// ranges, however, are recorded the instant SetMemoryPressure(true)
			// flips memoryPause — happens-before any mock drop it causes — so an
			// overlap check is race-free.
			tcRespTime := t.HTTPResp.Timestamp
			if tcRespTime.IsZero() {
				// Defensive: HTTPResp.Timestamp is normally set by the proxy
				// hook before the TC enters this channel. If it's missing for
				// some reason, widen the window with a 30s ceiling so we still
				// catch most concurrent pressure events without going unbounded.
				tcRespTime = t.HTTPReq.Timestamp.Add(30 * time.Second)
			}
			hasOverlap, overlapCount := syncmgr.Get().WasPressureActiveInWindow(t.HTTPReq.Timestamp, tcRespTime)

			// DIAG (temporary): once any pressure range exists, log every TC's
			// window check in plain Unix-ms integers (the Docker log renderer
			// garbles zap.Time but not int64). On the FIRST such TC, also dump
			// every recorded pressure range so overlap can be verified by hand.
			// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
			// if rangeCount := syncmgr.Get().PressureRangeCount(); rangeCount > 0 {
			// 	if !diagRangesDumped {
			// 		diagRangesDumped = true
			// 		ranges := syncmgr.Get().PressureRangesUnixMilli()
			// 		rf := make([]zap.Field, 0, len(ranges)+1)
			// 		rf = append(rf, zap.Int("range_count", len(ranges)))
			// 		for i, r := range ranges {
			// 			rf = append(rf, zap.Int64s(fmt.Sprintf("range_%d_start_end_ms", i), []int64{r[0], r[1]}))
			// 		}
			// 		a.logger.Info("DIAG/pressure-ranges-snapshot", rf...)
			// 	}
			// 	a.logger.Info("DIAG/window-check",
			// 		zap.Int64("tc_req_ms", t.HTTPReq.Timestamp.UnixMilli()),
			// 		zap.Int64("tc_resp_ms", tcRespTime.UnixMilli()),
			// 		zap.Int64("tc_window_ms", tcRespTime.Sub(t.HTTPReq.Timestamp).Milliseconds()),
			// 		zap.Bool("req_is_zero", t.HTTPReq.Timestamp.IsZero()),
			// 		zap.Bool("resp_is_zero", t.HTTPResp.Timestamp.IsZero()),
			// 		zap.Int("range_count", rangeCount),
			// 		zap.Int("overlap_count", overlapCount),
			// 		zap.Bool("would_suppress", hasOverlap),
			// 	)
			// }

			if hasOverlap {
				tcsSuppressedSoFar++
				a.logger.Info("agent: TC suppressed — memory pressure overlapped TC window, not sent to CLI",
					zap.String("tc_name", t.Name),
					zap.Int64("tc_req_ms", t.HTTPReq.Timestamp.UnixMilli()),
					zap.Int64("tc_resp_ms", tcRespTime.UnixMilli()),
					zap.Int("pressure_overlaps", overlapCount),
					zap.Int("tcs_suppressed_so_far", tcsSuppressedSoFar),
				)
				continue
			}

			tcsSentSoFar++
			// Stream each test case as JSON
			// 1. Write metadata (JSON)
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", `form-data; name="metadata"`)
			header.Set("Content-Type", "application/json")
			part, err := mw.CreatePart(header)
			if err != nil {
				a.logger.Error("failed to create metadata part", zap.Error(err))
				return
			}
			if err := json.NewEncoder(part).Encode(t); err != nil {
				a.logger.Error("failed to encode metadata", zap.Error(err))
				return
			}

			// 2. Write file part if exists
			if t.HasBinaryFile {
				a.logger.Debug("Starting binary file streaming for test case", zap.String("name", t.Name))
				for _, form := range t.HTTPReq.Form {
					for i, path := range form.Paths {
						if path == "" {
							continue
						}

						// Get filename from FileNames if available, or base of path
						fileName := "binary_file"
						if i < len(form.FileNames) {
							fileName = form.FileNames[i]
						} else {
							fileName = filepath.Base(path)
						}

						fileHeader := textproto.MIMEHeader{}
						fileHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, fileName))
						filePart, err := mw.CreatePart(fileHeader)
						if err != nil {
							a.logger.Error("failed to create file part", zap.Error(err))
							return
						}

						f, err := os.Open(path)
						if err != nil {
							a.logger.Error("failed to open file for streaming", zap.String("path", path), zap.Error(err))
							return
						}
						if _, err := io.Copy(filePart, f); err != nil {
							f.Close()
							a.logger.Error("failed to copy file to stream", zap.Error(err))
							return
						}
						f.Close()
						a.logger.Debug("Successfully streamed file part", zap.String("file", fileName))

						// Cleanup temp file
						os.Remove(path)
					}
				}
			}

			// 3. Write delimiter part to force closure of previous part (file)
			// This is critical: The client reads the file part until it sees the *next* boundary.
			// Without this delimiter, the client blocks waiting for the *next testcase* to create a boundary,
			// causing a deadlock if testcases are infrequent.
			delimiterHeader := textproto.MIMEHeader{}
			delimiterHeader.Set("Content-Disposition", `form-data; name="delimiter"`)
			if _, err := mw.CreatePart(delimiterHeader); err != nil {
				a.logger.Error("failed to create delimiter part", zap.Error(err))
				return
			}

			flusher.Flush() // Immediately send data to the client
		}
	}
}

func (a *Agent) HandleOutgoing(w http.ResponseWriter, r *http.Request) {

	a.logger.Debug("Received request to handle outgoing mocks")

	// Track handler lifetime for the black-box recorder (cli/agent.go).
	// Started/Exited are monotonic counters; (Started - Exited) gives the
	// current in-flight count. Deferred Exited increment guarantees we
	// account for every exit, including early returns below.
	outgoingHandlerStarted.Add(1)
	defer outgoingHandlerExited.Add(1)
	// RTRACE: TEMP single-buffer experiment — N (agent forwarded). Compare
	// against host_decoded (RTRACE/host-recv-final) and disk-written
	// (RTRACE/mockdb-written) to locate any loss. Remove before merge.
	defer func() {
		fmt.Fprintf(os.Stderr, "RTRACE/agent-forwarded-final: outgoing_forwarded_total=%d\n", outgoingForwardedTotal.Load())
		_ = os.Stderr.Sync()
	}()

	// Headers for a binary gob stream
	w.Header().Set("Content-Type", "application/x-gob")
	w.Header().Set("Cache-Control", "no-cache")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// If you had an SSE/JSON client, this changes: they'll need a gob client now.
	errGrp, _ := errgroup.WithContext(r.Context())
	ctx := context.WithValue(r.Context(), models.ErrGroupKey, errGrp)

	var outgoingReq models.OutgoingReq
	if err := json.NewDecoder(r.Body).Decode(&outgoingReq); err != nil {
		http.Error(w, "Error decoding request", http.StatusBadRequest)
		return
	}

	mockChan, err := a.svc.GetOutgoing(ctx, outgoingReq.OutgoingOptions)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get outgoing: %v", err), http.StatusInternalServerError)
		a.logger.Error("failed to get outgoing", zap.Error(err))
		return
	}

	a.logger.Debug("Streaming outgoing mocks to client")

	// Flush the headers to establish the connection immediately
	flusher.Flush()

	enc := gob.NewEncoder(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case m, ok := <-mockChan:
			if !ok {
				return
			}
			if err := enc.Encode(m); err != nil {
				// enc.Encode(m) folds two distinct failure modes into
				// one error: (a) per-mock serialization errors (an
				// un-encodable Go type inside m's Spec, e.g. a postgres
				// cell whose type isn't yet covered by
				// PostgresV3Cell.GobEncode) and (b) write errors
				// against w (client gone, broken pipe, connection
				// reset, use of closed network connection). They MUST
				// be handled differently:
				//
				//   (a) skip + continue — the producer keeps pushing
				//       onto mockChan and a return here would silently
				//       drop every subsequent mock for the rest of the
				//       recording. RCA May 2026: a single [16]uint8
				//       case in a uuid[] column truncated 22 tests'
				//       worth of mocks because this handler exited at
				//       the first encode error.
				//
				//   (b) return — the stream is no longer viable;
				//       continuing would tight-loop draining mockChan
				//       while every Encode reproduces the same write
				//       error, spamming logs.
				//
				// utils.IsShutdownError matches the (b) cases (EOF,
				// connection refused/reset, broken pipe, use of closed
				// network connection); everything else is treated as
				// (a).
				if utils.IsShutdownError(err) {
					a.logger.Debug("outgoing stream client gone; ending mock stream",
						zap.Error(err),
					)
					return
				}
				a.logger.Error("gob encode failed; skipping this mock and continuing the stream",
					zap.Error(err),
					zap.String("mockKind", string(m.Kind)),
					zap.String("mockName", m.Name),
					zap.String("next_step", "the named mock was DROPPED from this recording. Inspect the error to identify the unsupported Go type, then extend the gob encoder for that mock kind (e.g. PostgresV3Cell.Gob{En,De}code + the codec catalogue for postgres mocks) and re-record. Until that lands, replays of test cases that depend on this mock will report no candidates from the matcher."),
				)
				continue
			}
			flusher.Flush()
			// Track successful forward for the black-box recorder.
			// Bump after Flush so the counter reflects "host has at
			// least had this mock pushed at it", not "encoded into
			// the response buffer but maybe still trapped behind a
			// blocked write".
			outgoingForwardedTotal.Add(1)
			outgoingLastForwardMs.Store(time.Now().UnixMilli())
		}
	}
}

func (a *Agent) HandleMappings(w http.ResponseWriter, r *http.Request) {
	a.logger.Debug("Received request to handle mappings stream")
	w.Header().Set("Content-Type", "application/json")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Connect to the service to get the channel
	mappingChan, err := a.svc.GetMapping(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	enc := json.NewEncoder(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case mapping, ok := <-mappingChan:
			if !ok {
				return
			}
			if err := enc.Encode(mapping); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// MakeAgentReady marks the agent as ready by creating a readiness file.
// This file can be used by Docker Compose healthchecks to verify the agent's readiness.
//
// Example usage in docker-compose.yml:
//
//	healthcheck:
//	  test: ["CMD", "cat", "/tmp/agent.ready"]
func (a *Agent) MakeAgentReady(w http.ResponseWriter, r *http.Request) {
	// The Keploy CA bundle MUST be installed before the agent signals
	// ready — Kubernetes postStart hooks and docker-compose healthchecks
	// gate app-container start on /tmp/agent.ready, and apps may make
	// HTTPS calls immediately on boot. Refuse readiness until SetupCA
	// has completed (see pkg/agent/proxy/tls.CAStatus).
	//
	// The install location is mode-dependent: the shared volume at
	// /tmp/keploy-tls/ca.crt in Docker/k8s mode, the system trust store
	// (distro-specific path under /etc or /usr/local) in native Linux
	// mode, or a Windows temp file that is removed on shutdown. We
	// deliberately do NOT log a ca_path here because no single value is
	// correct in all modes; operators investigating a 503 should
	// cross-reference the earlier Error log from pkg/agent/proxy (which
	// names the actual path that failed).
	//
	// Today the ordering is race-free because SetupCA runs synchronously
	// inside Hook() before the HTTP server starts. This explicit gate
	// protects against a future refactor that moves SetupCA into a
	// goroutine — without this check, app containers could boot before
	// the CA bundle exists and silently fail HTTPS egress.
	//
	// CAStatus distinguishes three states: ready, not-yet-ready, and
	// terminal setup failure. On failure we surface the underlying
	// error so operators see "CA setup failed: ..." instead of
	// polling forever on an opaque 503. The log line is Debug (not
	// Warn) because this endpoint is routinely polled by docker-compose
	// / kubelet during boot and the 503 itself is the signal; spamming
	// Warn would drown real warnings.
	if ready, setupErr := pTls.CAStatus(); !ready {
		if setupErr != nil {
			// MarkCAFailed is a latch — operator-driven restart is the
			// only way to clear it — so the first Error is the
			// actionable signal. Emit it at Error (with a next_step
			// field for operator guidance) exactly once, then degrade
			// to Debug for repeat polls so readiness-poller churn
			// doesn't drown the aggregator during incident response.
			firstCAFailureLog.Do(func() {
				a.logger.Error(
					"/agent/ready: CA setup failed; readiness will not recover without agent restart",
					zap.String("next_step",
						"Restart the agent after fixing the underlying "+
							"SetupCA failure (see the earlier Error log "+
							"line from pkg/agent/proxy for the specific "+
							"install path and next_step — typically "+
							"write-access to /tmp/keploy-tls in shared-"+
							"volume mode or to the host CA trust store "+
							"in native mode)."),
					zap.Error(setupErr),
				)
			})
			a.logger.Debug(
				"/agent/ready: CA setup failure still latched",
				zap.Error(setupErr),
			)
			http.Error(
				w,
				fmt.Sprintf("CA setup failed: %v", setupErr),
				http.StatusServiceUnavailable,
			)
			return
		}
		// Log the first refusal at Info so the gate's behaviour is
		// visible in default-level logs; subsequent polls during the
		// boot window go to Debug to avoid flooding.
		firstCARefusalLog.Do(func() {
			a.logger.Info(
				"/agent/ready called before CA bundle is installed; refusing",
			)
		})
		a.logger.Debug(
			"/agent/ready still refusing: CA bundle not yet installed",
		)
		http.Error(w, "CA bundle not yet installed", http.StatusServiceUnavailable)
		return
	}

	// Create or overwrite the readiness file with a timestamp
	content := []byte(time.Now().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(agentReadyFilePath, content, 0644); err != nil {
		// This path blocks docker-compose / kubelet startup, so the
		// log line must point operators at the common root causes:
		// read-only mount, missing parent dir, or disk pressure.
		a.logger.Error(
			"failed to create readiness file",
			zap.String("file", agentReadyFilePath),
			zap.String("parent_dir", filepath.Dir(agentReadyFilePath)),
			zap.String("next_step",
				"ensure the parent directory exists, is writable by the agent "+
					"user, and that the container filesystem / volume is not "+
					"read-only or out of space"),
			zap.Error(err),
		)
		http.Error(w, "failed to mark agent as ready", http.StatusInternalServerError)
		return
	}

	// RTRACE: TEMP diagnostic (agent-ready/replay-hang investigation) — remove before merge.
	a.logger.Info("RTRACE/agent: wrote readiness file (agent now healthy)", zap.String("file", agentReadyFilePath))
	w.WriteHeader(http.StatusOK)
	a.logger.Debug("Keploy Agent is ready from the ...")
	_, _ = w.Write([]byte("Agent is now ready\n"))
}

// HandleGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
// When this flag is set, connection errors will be logged as debug instead of error.
func (a *Agent) HandleGracefulShutdown(w http.ResponseWriter, r *http.Request) {
	a.logger.Debug("Received graceful shutdown notification")

	if err := a.svc.SetGracefulShutdown(r.Context()); err != nil {
		a.logger.Error("failed to set graceful shutdown flag", zap.Error(err))
		http.Error(w, "failed to set graceful shutdown", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Graceful shutdown flag set\n"))
}
