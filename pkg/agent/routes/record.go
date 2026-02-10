// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Agent struct {
	logger *zap.Logger
	svc    agent.Service
}

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

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")

	// Flush headers to ensure the client gets the response immediately
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

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

	// Flush the headers to establish the connection immediately
	flusher.Flush()

	// TODO: make a uniform implementation for both test and mock streaming channels
	// Keep the connection alive and stream data
	for t := range tc {
		select {
		case <-r.Context().Done():
			a.logger.Debug("Client closed the connection or context was cancelled")
			// Client closed the connection or context was cancelled
			return
		default:
			// Stream each test case as JSON
			a.logger.Debug("Sending test case", zap.Any("test_case", t))
			render.JSON(w, r, t)
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
	const readyFile = "/tmp/agent.ready"

	// Create or overwrite the readiness file with a timestamp
	content := []byte(time.Now().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(readyFile, content, 0644); err != nil {
		a.logger.Error("failed to create readiness file", zap.String("file", readyFile), zap.Error(err))
		http.Error(w, "failed to mark agent as ready", http.StatusInternalServerError)
		return
	}

	a.logger.Debug("Agent marked as ready", zap.String("file", readyFile))
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
