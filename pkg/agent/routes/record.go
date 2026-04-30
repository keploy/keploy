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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

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

	if err := agent.ActiveHooks.BeforeTestSetCompose(r.Context(), req.TestRunID); err != nil {
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
	for {
		select {
		case <-r.Context().Done():
			a.logger.Debug("Client closed the connection or context was cancelled")
			return
		case t, ok := <-tc:
			if !ok {
				return
			}
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
				a.logger.Error("gob encode failed", zap.Error(err))
				return
			}
			flusher.Flush()
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

	a.logger.Debug("Agent marked as ready", zap.String("file", agentReadyFilePath))
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
