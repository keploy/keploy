// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/netnames"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Agent struct {
	logger *zap.Logger
	svc    agent.Service
	// consumed is the consumed history kept for the client between
	// /updatemockparams calls (models.ConsumedSync).
	consumed consumedStore
	// netNames holds the names a docker-mode replay's networks answer
	// themselves, which HandleNetworkNames fills and HandleNetworkNamesAsk
	// serves asks from; nil is the agent process's own (netnames.Default),
	// the one its proxy reads.
	netNames *netnames.Store
}

// agentReadyFilePath is the path MakeAgentReady writes on success. It
// defaults to kdocker.AgentReadyFile (the canonical /tmp/agent.ready
// location consumed by the docker-compose healthcheck) and is a var
// only so unit tests can redirect it into a sandbox without writing
// into /tmp on the host. Production code MUST NOT mutate it.
var agentReadyFilePath = kdocker.AgentReadyFile

// massSuppressionPercent is the share of a session's test cases that must be
// suppressed before the completion summary is raised from Info to Warn.
//
// Not a threshold that changes behaviour — nothing is suppressed differently —
// only one that decides whether the operator is told loudly. Set well above the
// handful of test cases a brief memory-pressure blip costs, and well below the
// point where the recording has quietly become unrepresentative.
const massSuppressionPercent = 25

// maxNamedPressureConns is how many connections the summary's warning of the
// requests a pause of the memory guard cost names, those with the most first.
const maxNamedPressureConns = 8

// notHandedOnSaid is the summary's warning of a session's test cases that
// never reached its stream because the ingress that served their requests
// stopped first (syncMock NoteNotHandedOn).
const notHandedOnSaid = "agent: some test cases were lost: the app served their requests, and the ingress stopped (the agent, or its forwarder for the app's port, was stopping) before their test cases reached the recording"

// unnamedCallerConnsSaid is the summary's warning of a session's connections
// to a listener of the run's own process tree whose callers the agent could
// not name in its PID namespace (syncMock NoteUnnamedCallerConn): it could not
// tell whether a test made the calls they carried, and recorded none as a
// test case. It counts connections: the proxy relays them unread.
const unnamedCallerConnsSaid = "agent: the calls some connections to the app carried were not recorded as test cases: keploy could not name their callers in its PID namespace, so it could not tell whether a test made them"

// tlsRelayedConnsSaid is the summary's warning of a session's connections to
// the app that opened with a TLS handshake (syncMock NoteTLSRelayedConn): the
// ingress relayed each to the app's own TLS unread, so none of the calls they
// carried was recorded as a test case. It counts connections, not calls.
const tlsRelayedConnsSaid = "agent: the calls some connections to the app carried were not recorded as test cases: they were TLS (HTTPS), which keploy relayed to the app's own TLS without reading"

// tlsRelayedNextStep is what the user can do so the calls tlsRelayedConnsSaid
// counts are recorded.
const tlsRelayedNextStep = "serve the app's API over plain HTTP (or h2c gRPC) to the tests while recording, for example with TLS ended in front of the app, so keploy can read and record these calls"

// streamEndMarkerSent is what the agent logs (debug) once an incoming
// stream's end marker is flushed, with the counts it carried: the session's
// summary, deferred, follows it and says the same counts (sessionLost).
const streamEndMarkerSent = "agent: incoming stream ended with its marker"

// massSuppressed reports whether a session that sent sent test cases and
// suppressed suppressed of them suppressed a large share
// (massSuppressionPercent): its summary says so at WARN.
func massSuppressed(sent, suppressed int) bool {
	total := sent + suppressed
	return total > 0 && suppressed*100/total >= massSuppressionPercent
}

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

	r.Route(agentRoutePrefix, func(r chi.Router) {
		// Under the proven switch every answer names this agent process, so a
		// client can tell an agent started again in its place, which holds
		// none of what this one saw, from this one (models.AgentInstanceHeader).
		// With the switch off the answers are as they were.
		if provenswitch.On() {
			r.Use(answersAs(processInstance))
		}
		r.Get("/health", a.Health)
		r.Get("/ca", a.CACert)
		r.Post("/incoming", a.HandleIncoming)
		r.Post("/outgoing", a.HandleOutgoing)
		r.Post("/mappings", a.HandleMappings)
		r.Post("/mock", a.MockOutgoing)
		r.Post("/storemocks", a.StoreMocks)
		r.Post("/updatemockparams", a.UpdateMockParams)
		r.Post("/stop", a.Stop)
		r.Get("/record/pending", a.HandlePending)
		r.Post(models.StreamEndPath, a.HandleEndStreams)
		r.Get(models.StopNoticePath, a.HandleStopNotice)
		// r.Post("/testbench", a.SendKtInfo)
		r.Get("/consumedmocks", a.GetConsumedMocks)
		r.Get("/mockerrors", a.GetMockErrors)
		r.Get("/ids", a.HandleIDs)
		r.Get("/variables", a.HandleTestVariables)
		r.Post("/test-capture/begin", a.BeginTestErrorCapture)
		// Per-test scope API for `keploy mock record|replay` — a user's test
		// runner marks per-test boundaries so mocks are attributed / restricted
		// per test. See pkg/agent/routes/scope.go.
		r.Post("/scope/begin", a.HandleScopeBegin)
		r.Post("/scope/end", a.HandleScopeEnd)
		// A value the open test gave the application (scope_proven.go).
		// Any method: with the proven switch off the plain 404.
		r.HandleFunc("/scope/given", a.HandleScopeGiven)
		r.Get("/scope/windows", a.HandleScopeWindows)
		r.Post("/scope/table", a.HandleScopeTable)
		r.Post("/replay/gate", a.HandleScopeGate)
		// The proven wire's instance claim (scope_proven.go): a test
		// process's keploy integration claims the run before its first test.
		// Every method is routed to it, so with the switch off the path is
		// the plain 404 it was before the wire existed.
		r.HandleFunc("/instance", a.HandleInstance)
		// What an integration refused, for the run's verdict to name
		// (scope_proven.go). The plain 404 with the switch off.
		r.HandleFunc("/refusal", a.HandleRefusal)
		// Any method: with the proven switch off the handler answers the
		// 404 this path had before it existed.
		r.HandleFunc("/replay/clock", a.HandleTestBegins)
		// The keploy CLI's container list, for a docker-mode agent that cannot
		// read Docker itself (containerkey); only under the proven switch.
		if provenswitch.On() {
			r.Get("/containers", a.HandleTakesContainers)
			r.Post("/containers", a.HandleContainers)
			r.Get("/containers/settle", a.HandleSettleContainers)
		}
		// The run's mock.real, and what the run saw of it beyond its tests
		// (real.go). Any method: with the switch off each answers the 404
		// its path had before it existed.
		r.HandleFunc("/real", a.HandleRealDeps)
		r.HandleFunc("/real/run", a.HandleRealRun)
		r.HandleFunc("/real/test", a.HandleRealTest)
		r.Post("/app/start", a.HandleAppStart)
		// The names the replay's docker networks answer themselves, from
		// the keploy CLI (a docker-mode agent has no Docker socket), and the
		// agent's held ask for a fresh list (netnames).
		r.Post(models.NetworkNamesPath, a.HandleNetworkNames)
		r.Get(models.NetworkNamesAskPath, a.HandleNetworkNamesAsk)
		r.Get("/mock/stats", a.HandleMockStats)
		// The refused inserts into the hooks' PID maps, each a process they
		// could not track; a session during which one was refused is capture
		// incomplete. See pkg/agent/routes/pidmaps.go.
		r.Get("/pidmaps", a.HandlePIDMapRefusals)
		// Every kind of capture loss, for the per-test taint under the
		// proven switch. See pkg/agent/routes/capturelosses.go.
		r.Get("/capturelosses", a.HandleCaptureLosses)
		// What of it no test was charged with (a loss after the last test).
		r.Get("/capturelosses/uncharged", a.HandleUnchargedCaptureLosses)
		r.Get("/app/listen-addrs", a.HandleAppListenAddrs)
		// The application's listens and its start's progress, for keploy
		// test's readiness under the proven switch (readiness.go).
		r.Get("/app/readiness", a.HandleAppReadiness)
		r.Post("/app/readiness/mark", a.HandleAppReadinessMark)
		r.Get("/mock/captured", a.HandleCapturedMocks)
		// Pollable, non-draining view of which mocks have been served. Distinct
		// from /consumedmocks, which drains and is read once at end of run.
		r.Get("/mock/served", a.HandleServedMocks)
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
		http.Error(w, fmt.Sprintf("invalid before-simulate request: %v", err), http.StatusBadRequest)
		return
	}

	real := provenswitch.On() && len(req.Real) > 0
	// A mock.real case without a recorded request time has nothing to
	// freeze to: only its move onto real time below.
	if !real || !req.TimeStamp.IsZero() {
		if err := agent.ActiveHooks.BeforeSimulate(r.Context(), req.TimeStamp, req.TestSetID, req.TestCaseName); err != nil {
			a.logger.Error("failed to execute before simulate hook", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if real {
		// A test case that uses mock.real runs on the real clock (F3): after
		// the rest of the hook (a coverage window, say), its clock moves off
		// the recorded request onto real time.
		if err := agent.BeginRealTestCase(r.Context(), req.TestCaseName); err != nil {
			a.logger.Error("could not put a test case that uses mock.real on the real clock", zap.String("testCase", req.TestCaseName), zap.Error(err))
			code := http.StatusInternalServerError
			if errors.Is(err, agent.ErrNoRealTestClock) {
				code = http.StatusNotImplemented
			}
			http.Error(w, err.Error(), code)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) HandleAfterSimulate(w http.ResponseWriter, r *http.Request) {
	var req models.AfterSimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid after-simulate request: %v", err), http.StatusBadRequest)
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
	// Tells the client that /storemocks takes a zstd-compressed stream
	// (RFC 7694). An agent without this header gets the stream uncompressed.
	w.Header().Set("Accept-Encoding", models.MockStreamEncodingZstd)
	// Whether this agent's proven switch is on, as it pinned it from its
	// --proven flag; the CLI refuses an agent whose switch is not its own
	// (provenswitch.CheckAgent).
	w.Header().Set(provenswitch.HealthHeader, provenswitch.State())
	w.WriteHeader(http.StatusOK)
	render.JSON(w, r, "OK")
}

// CACert serves this run's MITM CA public certificate (PEM) to the native
// client, which points the app's trust env vars at it. The MITM CA is generated
// per run and its private key never leaves the agent; only the public
// certificate is returned here. The route is token-guarded (it is NOT in
// isAuthExempt), so only the keploy client that holds the session token can read
// it. A 503 before SetupCA has established the CA lets the client distinguish
// "not ready yet" from a transport failure.
func (a *Agent) CACert(w http.ResponseWriter, _ *http.Request) {
	certPEM := pTls.ActiveCACertPEM()
	if len(certPEM) == 0 {
		http.Error(w, "CA not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(certPEM); err != nil {
		a.logger.Debug("failed to write CA certificate response", zap.Error(err))
	}
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

// testCaseHoldTick is how often HandleIncoming looks at the test cases it holds
// for their verdict again (syncmgr.TestCaseHoldTick). A variable so a test can
// tell what the stream does at once from what its tick does.
var testCaseHoldTick = syncmgr.TestCaseHoldTick

// testCaseHoldMax is how long HandleIncoming holds a test case for its verdict
// while what holds it makes no progress, in the time the process could run
// (syncMock.TestCaseHoldMax). A variable so a test can reach the bound.
var testCaseHoldMax = syncmgr.TestCaseHoldMax

// streamWriteFailed logs a failed write to a recording's test-case stream: at
// DEBUG once the stream has ended (net/http ends the request's context on a
// failed write, as on its client going away; the summary's ended_by and
// tcs_held_at_end say what the end cost), at ERROR when the failure is the
// handler's own (a test case that does not encode, an upload it cannot read).
func streamWriteFailed(logger *zap.Logger, r *http.Request, msg string, err error) {
	if r.Context().Err() != nil {
		logger.Debug(msg, zap.Error(err))
		return
	}
	logger.Error(msg, zap.Error(err))
}

func (a *Agent) HandleIncoming(w http.ResponseWriter, r *http.Request) {
	// A recording's client that drains this stream after its stop holds it
	// open until it has: a stop signal waits for that (holdForDrain).
	defer holdForDrain(r)()
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
			streamWriteFailed(a.logger, r, "failed to close multipart writer", err)
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
		http.Error(w, fmt.Sprintf("failed to decode incoming request: %v", err), http.StatusBadRequest)
		return
	}

	// keploy record names each test case's mock.real destinations by when
	// they were used: the uses are kept while this stream is open, and none
	// after (proven switch only).
	if k, ok := a.svc.(realUseKeeper); ok && provenswitch.On() {
		k.KeepRealUses(true)
		defer k.KeepRealUses(false)
	}
	tc, err := a.svc.StartIncomingProxy(ctx, incomingReq.IncomingOptions)
	if err != nil {
		stopReason := "failed to start the ingress proxy"
		a.logger.Error(stopReason, zap.Error(err))
		http.Error(w, fmt.Sprintf("%s: %v", stopReason, err), http.StatusInternalServerError)
		return // Important: return after handling the error
	}
	// Test cases built and not yet taken here wait in tc: a stop's drain
	// waits for them (syncMock BacklogBefore).
	defer syncmgr.Get().WatchTestCaseQueue(func() int { return len(tc) })()

	// A recording whose stop was noted (syncMock.NoteStop) has ended once
	// the next one's stream attaches, and once this one's stream ends: the
	// requests its stop found in flight are no requests of the next
	// (EndStoppedRecording). Here, before this session's tally opens, so it
	// tells no loss of theirs.
	syncmgr.Get().EndStoppedRecording()
	defer syncmgr.Get().EndStoppedRecording()
	// A request the stop of a recording that has ended found in flight is no
	// request of this session's: a test case of one resolved as that
	// recording ended, or captured as its window was given up, which that
	// recording's stream no longer took, is left out here, as the recording it
	// began in left it out. This session is the first since that end, which
	// is where those arrive.
	leftovers := syncmgr.Get().TakeLeftovers()
	// Kept requests the mock manager leaves out this session because their
	// window was given up while they were in flight (syncMock.Window.Keep),
	// less those a later request is recorded in the place of
	// (Window.Replaced). The manager serves every session of the agent's
	// life, and a loss told in an earlier one can be taken back in this one,
	// so this session's are on a tally of its own. Opened before the headers
	// go out: once they are flushed the client may act on the open session,
	// and a loss told then is this session's.
	leftOut := syncmgr.Get().OpenLossTally()
	defer leftOut.Close()
	// The mocks parsers leave out this session (Session.ReportLeftOut), from
	// the same point and for the same reason: the manager counts them over its
	// life. None is ever taken back, so this session's are what that count
	// grows by from here.
	mocksLeftOutBefore := syncmgr.Get().MocksLeftOut()
	// The requests the ingress did not record because the memory guard had
	// paused recording (NoteNotRecordedUnderPressure), counted the same way.
	notRecordedBefore := syncmgr.Get().NotRecordedUnderPressure()
	// The requests the ingress served and was recording whose test cases
	// never reached this stream's queue because the ingress stopped first
	// (NoteNotHandedOn), counted the same way.
	notHandedOnBefore := syncmgr.Get().NotHandedOn()
	// The connections to a listener of the run's own process tree whose
	// callers the agent could not name in its PID namespace, none of whose
	// calls was recorded as a test case (NoteUnnamedCallerConn), counted the
	// same way.
	unnamedBefore := syncmgr.Get().UnnamedCallerConns()
	// The connections to the app that opened with a TLS handshake, relayed to
	// the app's TLS unread, none of whose calls was recorded as a test case
	// (NoteTLSRelayedConn), counted the same way.
	tlsRelayedBefore := syncmgr.Get().TLSRelayedConns()
	// sessionLost reads, once, what this session lost to its ingress stopping
	// (NoteNotHandedOn), to callers the agent could not name
	// (NoteUnnamedCallerConn) and to TLS connections relayed unread
	// (NoteTLSRelayedConn): the end marker, the summary and its warnings say
	// the same counts, whichever of them reads first. The handler's own
	// goroutine calls it (the marker, and the summary it defers).
	var lostRead bool
	var notHandedOnLost, unnamedLost, tlsRelayedLost uint64
	sessionLost := func() (notHandedOn, unnamed, tlsRelayed uint64) {
		if !lostRead {
			lostRead = true
			notHandedOnLost = syncmgr.Get().NotHandedOn() - notHandedOnBefore
			unnamedLost = syncmgr.Get().UnnamedCallerConns() - unnamedBefore
			tlsRelayedLost = syncmgr.Get().TLSRelayedConns() - tlsRelayedBefore
		}
		return notHandedOnLost, unnamedLost, tlsRelayedLost
	}
	// The ingress requests that come during the memory guard's pauses, from
	// here: counted by the order of events (the request count read at each
	// pause's start and end), apart from what the capture then decided, so
	// the summary can be held to it.
	pauseTally := memoryguard.OpenPauseTally()
	defer pauseTally.Close()

	a.logger.Debug("Streaming incoming test cases to client")

	// Closed once this stream's client, told the agent holds nothing more
	// from before its stop, asks the agent to end its streams. Taken before
	// the headers go out: once they are flushed the client may ask.
	ending, endingClosed := streamEnding(r)
	defer endingClosed()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	a.logger.Debug("Incoming stream connection established and headers flushed")

	// Keep the connection alive and stream data.
	// Use select (not for-range) so context cancellation is checked
	// concurrently with channel receive — otherwise the handler blocks
	// forever during shutdown when no test cases are arriving.
	var tcsSentSoFar int       // TCs sent to CLI this session
	var tcsSuppressedSoFar int // TCs suppressed: a pressure or orphan span overlapped the TC's HTTP window, or its mock was capacity-dropped
	var tcsUnsettled int       // TCs the hold's bound released before their verdict was final
	// send checks one test case and streams it unless it is left out. It
	// reports false once the stream is broken.
	send := func(t *models.TestCase) bool {
		// Whether t is streamed, left out or lost to a broken stream, this
		// is the last that sees it.
		defer discardUpload(a.logger, t)
		// The test case's window, by its kind: a gRPC test case's times are
		// its GrpcReq's and GrpcResp's. Read as an HTTP one's, they were zero,
		// on which the span checks below make no claim, so a gRPC test case a
		// pause or a hole overlapped was streamed without the mocks it lost.
		tcReqTime, tcRespTime := testCaseWindow(t)
		if _, left := leftovers[tcReqTime.UnixNano()]; left && !tcReqTime.IsZero() {
			a.logger.Debug("Test case left out: its request was in flight across the stop of a recording that has ended",
				zap.Time("request_time", tcReqTime))
			return true
		}
		// Skip this test case if memory pressure overlapped its window
		// [request, response]: under pressure the paired mock may have been
		// dropped, so sending the TC would orphan it at replay. We check the
		// TC window against recorded pressure ranges (not individual mock
		// drops) because that check is race-free regardless of when the
		// mock's goroutine runs relative to this handler.
		hasPressure, pressureOverlaps := syncmgr.Get().WasPressureActiveInWindow(tcReqTime, tcRespTime)
		// An exchange a connection carried but did not record as a mock
		// strands its TC mock-less, whether or not a pressure range covers
		// it: a parser's hole (a mongo/v2 reassembly resync), an exchange a
		// parser left out (a MySQL response it cannot frame), a mock the
		// incomplete flag left out, a connection that can no longer be
		// recorded. Each is reported as an orphan span
		// (Session.RecordOrphanWindow lists them); suppress any TC whose
		// window overlaps one, same as for pressure. A span its parser
		// reports once it gets to the exchange, behind the traffic, reaches
		// only the TCs checked after it: without a watermark (the hold
		// below), a TC streamed before it is saved without the mock, as the
		// WARN that reports the exchange says.
		hasOrphan, orphanOverlaps := syncmgr.Get().WasMockOrphanedInWindow(tcReqTime, tcRespTime)
		// A capacity drop (outChan overflow / already-closed channel)
		// feeds nothing into pressureRanges, so the pressure-overlap check
		// above cannot catch it. Suppress by EXACT owning test name so a
		// TC whose mock was capacity-dropped is not streamed mock-less
		// (replay: match_phase=no_mocks), without over-suppressing any
		// concurrent TC that kept all its mocks.
		mockDropped := syncmgr.Get().WasMockDroppedForTC(t.Name)

		if hasPressure || hasOrphan || mockDropped {
			tcsSuppressedSoFar++
			a.logger.Debug("agent: TC suppressed — memory pressure or an exchange a connection did not record overlapped TC window, or a mock was capacity-dropped; not sent to CLI",
				zap.String("tc_name", t.Name),
				zap.Int64("tc_req_ms", tcReqTime.UnixMilli()),
				zap.Int64("tc_resp_ms", tcRespTime.UnixMilli()),
				zap.Int("pressure_overlaps", pressureOverlaps),
				zap.Int("orphan_overlaps", orphanOverlaps),
				zap.Bool("capacity_drop", mockDropped),
				zap.Int("tcs_suppressed_so_far", tcsSuppressedSoFar),
			)
			return true
		}

		// Under the proven switch, the mock.real destinations its request
		// used, named as its mocks are, by time (design v14 §4): the
		// test case's recording lists them, and its replay runs on the
		// real clock.
		if u, ok := a.svc.(realUseReader); ok && provenswitch.On() {
			from, to := testCaseWindow(t)
			t.Real = u.RealUsedDuring(from, to)
		}

		tcsSentSoFar++
		// From here the test case is this stream's to deliver, no longer the
		// agent's backlog (NoticeClientsHold).
		carriedFrame(r)
		// Stream each test case as JSON
		// 1. Write metadata (JSON)
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="metadata"`)
		header.Set("Content-Type", "application/json")
		part, err := mw.CreatePart(header)
		if err != nil {
			streamWriteFailed(a.logger, r, "failed to create metadata part", err)
			return false
		}
		if err := json.NewEncoder(part).Encode(t); err != nil {
			streamWriteFailed(a.logger, r, "failed to encode metadata", err)
			return false
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
						streamWriteFailed(a.logger, r, "failed to create file part", err)
						return false
					}

					f, err := os.Open(path)
					if err != nil {
						a.logger.Error("failed to open file for streaming", zap.String("path", path), zap.Error(err))
						return false
					}
					if _, err := io.Copy(filePart, f); err != nil {
						f.Close()
						streamWriteFailed(a.logger, r, "failed to copy file to stream", err)
						return false
					}
					f.Close()
					a.logger.Debug("Successfully streamed file part", zap.String("file", fileName))
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
			streamWriteFailed(a.logger, r, "failed to create delimiter part", err)
			return false
		}

		flusher.Flush() // Immediately send data to the client
		return true
	}

	// A capture that can tell when a test case's verdict under the rule for
	// unrecorded traffic is final (the enterprise proxyless agent) holds each
	// test case until it is: a span that overlaps it can become known after it
	// completes (syncMock settle.go). Without one, test cases go at once.
	var hold *syncmgr.TestCaseHold
	if w := syncmgr.Get().Watermark(); w != nil {
		hold = syncmgr.NewTestCaseHold(w, testCaseHoldMax, syncmgr.TestCaseHoldBytes)
		// A stream that ends while it holds test cases (its client went
		// away, or it broke) sends none of them.
		defer func() {
			for _, t := range hold.Drain() {
				discardUpload(a.logger, t)
			}
		}()
	}
	var holdTick *time.Ticker
	defer func() {
		if holdTick != nil {
			holdTick.Stop()
		}
		syncmgr.Get().NoteHeld(time.Time{})
	}()
	// release sends what the hold lets go, and keeps a ticker going while it
	// holds any. It reports false once the stream is broken.
	release := func() bool {
		released := hold.Release()
		for i, h := range released {
			if h.Unsettled {
				tcsUnsettled++
				// What held it, for the first of this recording and then
				// one in 1,024: the bound can let many go.
				if h.HeldBy != nil && (tcsUnsettled <= 64 || tcsUnsettled%1024 == 0) {
					if ce := a.logger.Check(zap.DebugLevel, "agent: test case let go by the hold's bound before its verdict was final"); ce != nil {
						ce.Write(zap.String("tc_name", h.TC.Name), zap.Int("unsettled", tcsUnsettled),
							zap.Stringer("held_by", h.HeldBy))
					}
				}
			}
			if !send(h.TC) {
				for _, rest := range released[i+1:] {
					discardUpload(a.logger, rest.TC)
				}
				return false
			}
		}
		// Every test case still held starts no earlier than the earliest:
		// past the cap, the spans before it are the ones joined (Spans).
		hold.EarliestStarts(func(_, _ string, start time.Time) { syncmgr.Get().CheckedBefore(start) })
		if hold.Len() > 0 && holdTick == nil {
			holdTick = time.NewTicker(testCaseHoldTick)
		} else if hold.Len() == 0 && holdTick != nil {
			holdTick.Stop()
			holdTick = nil
		}
		// What the stream still holds keeps a stop's drain going (PendingBefore).
		syncmgr.Get().NoteHeld(hold.Oldest())
		return true
	}
	// summarize says what this session recorded, and what it lost, however the
	// stream ends (endedBy): the capture closing tc, its client asking the
	// agent to end it (HandleEndStreams), or the stream ending under it, which is how a native or docker recording ends (the ingress
	// never closes tc): its client closes it once its stop's drain is done. A
	// write that fails as the client goes cannot be told from a client that
	// went away (net/http ends the request's context on either), and with an
	// app that serves on through the drain the handler is as often writing as
	// waiting when it goes: tcs_held_at_end, not a guess at why, says what an
	// end left unsent.
	endedBy := "client"
	summarize := func() {
		_, finalDropped, finalAdded, _ := syncmgr.Get().GetDropStats()
		orphanRecorded, orphanClosed, orphanOpen := syncmgr.Get().OrphanRangeCount()
		pressureRecorded, pressureSpans := syncmgr.Get().PressureRangeCount()
		// Suppression protects replay from test cases whose mocks
		// were never captured, but it is coarse: the windows are
		// timestamp-based and session-wide, so a connection that
		// stays busy while un-capturable can take a large share of
		// the run with it — including test cases that other,
		// healthy connections served completely. At a few percent
		// that is the intended trade; at a third of the session the
		// operator has a materially thinner recording than they
		// think, and must be told at a level they actually see.
		logRecordingComplete := a.logger.Info
		if massSuppressed(tcsSentSoFar, tcsSuppressedSoFar) {
			logRecordingComplete = a.logger.Warn
		}
		// Read once: the warning and the summary say the same.
		leftOutInFlight := leftOut.LeftOut()
		if leftOutInFlight > 0 {
			// Never captured, so never counted above: each stayed in
			// flight while the mocks the agent held for the requests
			// in flight passed their budget, or memory pressure let
			// them go, and was left out rather than recorded without
			// its mocks. The manager warns of the first and every
			// 1024th; this is the session's count.
			a.logger.Warn("agent: some test cases were left out of the recording: each stayed in flight (a slow endpoint, a long poll, a stream) while mocks it may have made had to be let go",
				zap.Uint64("test_cases", leftOutInFlight),
				zap.Int64("hold_bound_bytes", syncmgr.MaxHeldBytes))
		}
		// Read once: the warning and the summary say the same.
		notRecorded := syncmgr.Get().NotRecordedUnderPressure().Since(notRecordedBefore)
		duringPauses := pauseTally.Read()
		if notRecorded.Total > 0 {
			// Each came while the memory guard had paused recording, so it
			// was forwarded and not recorded. The pause cost these and no
			// more: each connection's requests after it are recorded.
			a.logger.Warn("agent: some requests were not recorded: the agent was short of memory when they came, and the memory guard had paused recording; each connection's requests after the pause are recorded",
				zap.Uint64("requests", notRecorded.Total),
				zap.Uint64("requests_during_pauses", duringPauses.DuringPauses),
				zap.Int("connections", notRecorded.Connections()),
				zap.Strings("connections_most", notRecorded.Most(maxNamedPressureConns)))
		}
		// Read once (sessionLost): the warnings, the summary and the end
		// marker say the same.
		notHandedOn, unnamedConns, tlsRelayedConns := sessionLost()
		if notHandedOn > 0 {
			// The app served each, and the ingress stopped before its test
			// case reached this stream: the manager warns of the first and
			// every 1024th as they come; this is the session's count.
			a.logger.Warn(notHandedOnSaid,
				zap.Uint64("test_cases", notHandedOn))
		}
		if unnamedConns > 0 {
			// Each was passed to the app's listener and none of its calls
			// recorded: the proxy warned of the agent's first as it came;
			// this is the session's count.
			a.logger.Warn(unnamedCallerConnsSaid,
				zap.Uint64("connections", unnamedConns),
				zap.String("next_step", "run keploy in the PID namespace of the app's tests, or in the host's (--pid=host, or hostPID in Kubernetes)"))
		}
		if tlsRelayedConns > 0 {
			// Each reached the app's TLS and none of its calls was
			// recorded: the ingress warned of the agent's first to each
			// app port as it came; this is the session's count.
			a.logger.Warn(tlsRelayedConnsSaid,
				zap.Uint64("connections", tlsRelayedConns),
				zap.String("next_step", tlsRelayedNextStep))
		}
		if tcsUnsettled > 0 {
			// The capture's parsers ran more than the hold's bound
			// behind: these were checked against what was known then.
			a.logger.Warn("agent: some test cases were saved before the agent had parsed all the traffic they overlap; if a connection's parser failed on it afterwards, they lack mocks",
				zap.Int("test_cases", tcsUnsettled),
				zap.Duration("held_without_progress_for", testCaseHoldMax))
		}
		logRecordingComplete("agent: recording complete",
			// How the stream ended: "capture" (it closed its channel),
			// "end" (its client asked the agent to end it, after it was
			// told the agent held nothing more from before its stop), or
			// "client" (the stream ended under it).
			zap.String("ended_by", endedBy),
			zap.Int("tcs_sent_to_cli", tcsSentSoFar),
			zap.Int("tcs_suppressed_total", tcsSuppressedSoFar),
			// _total is how often each suppressor fired; the spans
			// are what they were kept as, after overlapping and (past
			// the cap) joining (syncMock.Spans).
			zap.Int("pressure_ranges_total", pressureRecorded),
			zap.Int("pressure_spans", pressureSpans),
			zap.Int("orphan_ranges_total", orphanRecorded),
			zap.Int("orphan_spans_closed", orphanClosed),
			zap.Int("orphan_ranges_still_open", orphanOpen),
			zap.Int64("mocks_dropped_by_pressure", finalDropped),
			// Requests the ingress did not record because the memory guard
			// had paused recording when they came (the WARN above names
			// their connections).
			zap.Uint64("tcs_not_recorded_under_pressure", notRecorded.Total),
			// The HTTP/1 ingress requests that came while recording was
			// paused (memoryguard.PauseTally), whatever their capture
			// did: a pause costs these and those in flight at its edges,
			// so tcs_not_recorded_under_pressure far above them says a
			// pause did not end for some connection. And those that came
			// before the first pause: all a recording would hold whose
			// capture never came back after it.
			zap.Uint64("requests_during_pauses", duringPauses.DuringPauses),
			zap.Uint64("requests_before_first_pause", duringPauses.BeforeFirstPause),
			// Every mock a parser left out, the exchange it stopped
			// on included, each reported with Session.ReportLeftOut
			// (Session.ReportStoppedOn for the one it stopped on):
			// for the incomplete-mock flag (a chunk the relay
			// dropped, a short write, a decode error), for a reason
			// of the parser's own, or for the server bytes that
			// start a connection the capture joined mid-way, the
			// answer to a request it does not have
			// (Session.NextRequest). A parser's own reasons: a
			// MySQL command that does not decode, that the replayer
			// cannot serve, or whose response cannot be framed is
			// left out alone, and the connection's recording goes
			// on; an HTTP/1 request or response that does not
			// decode stops the parser, and so does a MySQL command
			// in which the framing of the client's stream is lost,
			// or whose response cannot be framed with no way to
			// take the connection up again after it (a client that
			// pipelines its commands, say); an HTTP/2 stream reset
			// before it completed is left out, which the capture
			// did not lose: its call did not complete. Their WARN
			// is rate-limited, so this is where each is counted:
			// the ones reported while this session was open, as
			// tcs_left_out_in_flight counts its test cases left out
			// in flight, so a later session's summary does not
			// count them again. What a connection carried after its
			// recording stopped is in the orphan spans above, not
			// here: it was never parsed into mocks to count.
			zap.Int64("mocks_left_out", syncmgr.Get().MocksLeftOut()-mocksLeftOutBefore),
			zap.Int64("mocks_added_successfully", finalAdded),
			zap.Uint64("mocks_dropped_capacity", syncmgr.Get().DropCount()),
			zap.Int("tcs_dropped_capacity", syncmgr.Get().DroppedTCCount()),
			zap.Int("tcs_released_unsettled", tcsUnsettled),
			zap.Uint64("tcs_left_out_in_flight", leftOutInFlight),
			// Served by the app and never handed to this stream: the
			// ingress that served them stopped first (the WARN above).
			zap.Uint64("tcs_not_handed_on", notHandedOn),
			// Calls to the app none of which was recorded: the agent could
			// not name their callers (the WARN above).
			zap.Uint64("connections_from_unnamed_callers", unnamedConns),
			// TLS connections relayed to the app unread, none of whose
			// calls was recorded (the WARN above).
			zap.Uint64("connections_relayed_tls", tlsRelayedConns),
			// Held for their verdicts when the stream ended, and not sent
			// (the hold's Drain). A client ends its stream holding none from
			// before its stop unless its drain gave up, which it says: these
			// are what the app served after the stop, or what that drain lost.
			zap.Int("tcs_held_at_end", heldLen(hold)),
		)
	}
	// Before the hold's Drain (deferred earlier, so run later): what it still
	// holds is counted first.
	defer summarize()
	// take hands a test case the ingress built to the hold, or, without one,
	// to the stream. It reports false once the stream is broken.
	take := func(t *models.TestCase) bool {
		if hold == nil {
			return send(t)
		}
		hold.Add(t, "", "")
		return true
	}
	for {
		var tickC <-chan time.Time
		if holdTick != nil {
			tickC = holdTick.C
		}
		select {
		case <-r.Context().Done():
			a.logger.Debug("Client closed the connection or context was cancelled")
			return
		case <-ending:
			// What the ingress already queued goes first, and what the hold
			// lets go, then the marker: the client ends its drain on it, not
			// on a quiet bound that a test case still on its way here could
			// outlast. What the hold still holds is not final, and is what
			// the app served after the client's stop (the agent said it held
			// nothing from before it): the summary counts it.
		drain:
			for {
				select {
				case t, ok := <-tc:
					if !ok {
						break drain
					}
					if !take(t) {
						return
					}
				default:
					break drain
				}
			}
			if hold != nil && !release() {
				return
			}
			// The marker carries what the session lost (the summary's
			// warnings): a client tears an agent's compose stack down once
			// its drains end at the markers, and compose can drop the
			// agent's last log lines with it.
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", `form-data; name="`+models.StreamEndPart+`"`)
			part, err := mw.CreatePart(header)
			if err != nil {
				streamWriteFailed(a.logger, r, "failed to end the incoming stream with its marker", err)
				return
			}
			notHandedOn, unnamedConns, tlsRelayedConns := sessionLost()
			if err := json.NewEncoder(part).Encode(models.StreamEndSummary{
				TestCasesSent:       tcsSentSoFar,
				TestCasesSuppressed: tcsSuppressedSoFar,
				LeftOutInFlight:     leftOut.LeftOut(),
				NotRecordedUnderPressure: func() models.PressureSkipSummary {
					n := syncmgr.Get().NotRecordedUnderPressure().Since(notRecordedBefore)
					return models.PressureSkipSummary{Requests: n.Total, Connections: n.Connections()}
				}(),
				NotHandedOn:        notHandedOn,
				UnnamedCallerConns: unnamedConns,
				TLSRelayedConns:    tlsRelayedConns,
				Unsettled:          tcsUnsettled,
				MassSuppressed:     massSuppressed(tcsSentSoFar, tcsSuppressedSoFar),
			}); err != nil {
				streamWriteFailed(a.logger, r, "failed to end the incoming stream with its marker", err)
				return
			}
			flusher.Flush()
			a.logger.Debug(streamEndMarkerSent,
				zap.Int("tcs_sent_to_cli", tcsSentSoFar),
				zap.Uint64("tcs_not_handed_on", notHandedOn),
				zap.Uint64("connections_from_unnamed_callers", unnamedConns),
				zap.Uint64("connections_relayed_tls", tlsRelayedConns))
			endedBy = "end"
			return
		case <-tickC:
			if !release() {
				return
			}
		case t, ok := <-tc:
			if !ok {
				// Channel closed = recording session over. What the hold
				// still has goes as its verdicts settle, or its bound ends.
				for hold != nil && hold.Len() > 0 {
					if holdTick == nil {
						holdTick = time.NewTicker(testCaseHoldTick)
					}
					select {
					case <-r.Context().Done():
						return
					case <-holdTick.C:
					}
					if !release() {
						return
					}
				}
				endedBy = "capture"
				return
			}

			if hold == nil {
				if !send(t) {
					return
				}
				continue
			}
			// One that is final at once goes at once; behind held ones, it
			// waits for the tick, which looks at them all.
			wasEmpty := hold.Len() == 0
			hold.Add(t, "", "")
			if wasEmpty {
				if !release() {
					return
				}
				continue
			}
			// Held from here: a stop's drain must know before the tick.
			syncmgr.Get().NoteHeld(hold.Oldest())
		}
	}
}

// heldLen is how many test cases hold holds: none without a hold.
func heldLen(hold *syncmgr.TestCaseHold) int {
	if hold == nil {
		return 0
	}
	return hold.Len()
}

// discardUpload deletes the files t's upload was written to (conn.Capture): t
// owns them, and once its stream is done with it, streamed or not, nothing
// else will.
func discardUpload(logger *zap.Logger, t *models.TestCase) {
	if t != nil && t.HasBinaryFile {
		conn.RemoveFormFiles(logger, t.HTTPReq.Form)
	}
}

// PendingResponse answers GET /agent/record/pending.
type PendingResponse struct {
	// Pending: the agent may still hand over a test case or a mock captured
	// before the time asked about.
	Pending bool `json:"pending"`
	// Progress is a count that moves whenever the capture gets further (a
	// parser consumes bytes or returns, a test case is built): a stop that
	// waits on a pending backlog tells one being worked through from one
	// that is not moving. Absent from an agent that predates it.
	Progress uint64 `json:"progress"`
	// WaitingOn says what is pending, when it can be told.
	WaitingOn string `json:"waitingOn,omitempty"`
	// EndMarkers: the answer counts the mappings owed for tests from before
	// the time asked about, and the agent ends a recording client's frame
	// streams, each with a marker after all it had, when it asks
	// (models.StreamEndPath, HandleEndStreams). Absent from an agent that
	// predates it, whose client ends its drains on quiet.
	EndMarkers bool `json:"endMarkers,omitempty"`
}

// HandlePending tells a recording's stop whether the agent may still hand over
// a test case or a mock captured before ?before= (Unix nanoseconds): its
// capture has not got past that time, or a test case it holds for its verdict,
// builds, or has queued ended before it. The stop drains the agent's streams
// until it may not, and not only until they fall quiet: a parser can be
// behind the traffic, and quiet, for longer than any fixed grace. It also says
// how far the capture has got (Progress), so the stop can tell a backlog being
// worked through from one that is not moving. 501 when the agent cannot tell
// (its capture has no watermark); the stop then goes by quiet alone.
func (a *Agent) HandlePending(w http.ResponseWriter, r *http.Request) {
	ns, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil {
		http.Error(w, "before must be a Unix time in nanoseconds", http.StatusBadRequest)
		return
	}
	st := syncmgr.Get().BacklogBefore(time.Unix(0, ns))
	if !st.Known {
		http.Error(w, "this agent's capture cannot tell what it has yet to hand over", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PendingResponse{Pending: st.Pending, Progress: st.Progress, WaitingOn: st.WaitingOn, EndMarkers: true})
}

// HandleEndStreams ends a recording's frame streams (POST
// models.StreamEndPath?token=, the token its streams carry): each puts on its
// stream what it still has, then its end marker, and ends. The client asks
// once the agent has said it holds nothing more from before its stop
// (HandlePending), so its drains end on those markers, every frame it was
// owed read, and not on a stream's quiet.
func (a *Agent) HandleEndStreams(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		http.Error(w, "token names the recording whose streams to end", http.StatusBadRequest)
		return
	}
	endRecordingStreams(tok)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) HandleOutgoing(w http.ResponseWriter, r *http.Request) {
	// A recording's client that drains this stream after its stop holds it
	// open until it has: a stop signal waits for that (holdForDrain).
	defer holdForDrain(r)()

	a.logger.Debug("Received request to handle outgoing mocks")

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
		http.Error(w, fmt.Sprintf("failed to decode outgoing request: %v", err), http.StatusBadRequest)
		return
	}

	mockChan, err := a.svc.GetOutgoing(ctx, outgoingReq.OutgoingOptions)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get outgoing: %v", err), http.StatusInternalServerError)
		a.logger.Error("failed to get outgoing", zap.Error(err))
		return
	}

	a.logger.Debug("Streaming outgoing mocks to client")

	// Closed once this stream's client, told the agent holds nothing more
	// from before its stop, asks the agent to end its streams. Taken before
	// the headers go out: once they are flushed the client may ask.
	ending, endingClosed := streamEnding(r)
	defer endingClosed()
	// Flush the headers to establish the connection immediately
	flusher.Flush()

	enc := gob.NewEncoder(w)
	// The record log names, once each, the processes its calls came from
	// (starts.Registry.StampOrigin).
	starts.Default.SetLogger(a.logger)

	// put streams m. It reports false once the stream is no longer viable.
	put := func(m *models.Mock) bool {
		starts.Default.Stamp(m)
		// From here the mock is this stream's to deliver, no longer the
		// agent's backlog (NoticeClientsHold).
		carriedFrame(r)
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
				return false
			}
			a.logger.Error("gob encode failed; skipping this mock and continuing the stream",
				zap.Error(err),
				zap.String("mockKind", string(m.Kind)),
				zap.String("mockName", m.Name),
				zap.String("next_step", "the named mock was DROPPED from this recording. Inspect the error to identify the unsupported Go type, then extend the gob encoder for that mock kind (e.g. PostgresV3Cell.Gob{En,De}code + the codec catalogue for postgres mocks) and re-record. Until that lands, replays of test cases that depend on this mock will report no candidates from the matcher."),
			)
			return true
		}
		flusher.Flush()
		return true
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ending:
			// What the channel already holds goes first, then the marker:
			// the client ends its drain on the marker, not on a quiet bound
			// that a mock still on its way here could outlast.
		drain:
			for {
				select {
				case m, ok := <-mockChan:
					if !ok {
						break drain
					}
					if !put(m) {
						return
					}
				default:
					break drain
				}
			}
			if err := enc.Encode(&models.Mock{Kind: models.StreamEndKind}); err != nil {
				a.logger.Debug("could not end the outgoing stream with its marker", zap.Error(err))
				return
			}
			flusher.Flush()
			return
		case m, ok := <-mockChan:
			if !ok {
				return
			}
			if !put(m) {
				return
			}
		}
	}
}

func (a *Agent) HandleMappings(w http.ResponseWriter, r *http.Request) {
	// A recording's client that drains this stream after its stop holds it
	// open until it has: a stop signal waits for that (holdForDrain).
	defer holdForDrain(r)()
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

	// Closed once this stream's client, told the agent holds nothing more
	// from before its stop, mappings included, asks the agent to end its
	// streams. Taken before the headers go out: once they are flushed the
	// client may ask.
	ending, endingClosed := streamEnding(r)
	defer endingClosed()
	// Establish the stream now, as the test-case and mock streams do, not
	// with the first mapping: the client's request returns only once the
	// headers arrive. A recording that emits no mappings (a DaemonSet or
	// proxyless one) otherwise left the client's GetMappings blocked for the
	// whole session, and its stop waited out the mapping drain's cap.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ending:
			// What the channel already holds goes first, then the marker:
			// the client ends its mapping drain on it, not on a quiet bound
			// that a mapping still on its way here could outlast.
		drain:
			for {
				select {
				case mapping, ok := <-mappingChan:
					if !ok {
						break drain
					}
					if err := enc.Encode(mapping); err != nil {
						return
					}
				default:
					break drain
				}
			}
			if err := enc.Encode(models.TestMockMapping{StreamEnd: true}); err != nil {
				a.logger.Debug("could not end the mapping stream with its marker", zap.Error(err))
				return
			}
			flusher.Flush()
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
		http.Error(w, fmt.Sprintf("failed to mark agent as ready: %v", err), http.StatusInternalServerError)
		return
	}

	a.logger.Debug("Agent marked as ready", zap.String("file", agentReadyFilePath))
	w.WriteHeader(http.StatusOK)
	a.logger.Debug("Keploy Agent is ready from the ...")
	_, _ = w.Write([]byte("Agent is now ready\n"))
}

// HandleGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
// When this flag is set, connection errors will be logged as debug instead of error.
func (a *Agent) HandleGracefulShutdown(w http.ResponseWriter, r *http.Request) {
	a.logger.Debug("Received graceful shutdown notification")

	// A recording client that holds a frame stream it drains after the stop
	// stops the app it started before it stops the agent: what the ingress
	// finds refusing from here is that app gone on purpose (incoming
	// stopping.go). Any other client's app (docker compose, a k8s sidecar's)
	// goes on running through the stop.
	ctx := r.Context()
	if RecordingClients() > 0 {
		ctx = models.WithDrainsAfterStop(ctx)
	}
	if err := a.svc.SetGracefulShutdown(ctx); err != nil {
		a.logger.Error("failed to set graceful shutdown flag", zap.Error(err))
		http.Error(w, fmt.Sprintf("failed to set graceful shutdown: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Graceful shutdown flag set\n"))
}
