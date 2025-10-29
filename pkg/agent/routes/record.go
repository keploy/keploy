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
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Agent struct {
	logger *zap.Logger
	svc    agent.Service
}

func New(r chi.Router, agent agent.Service, logger *zap.Logger) {
	a := &Agent{
		logger: logger,
		svc:    agent,
	}

	r.Route("/agent", func(r chi.Router) {
		r.Get("/health", a.Health)
		r.Post("/incoming", a.HandleIncoming)
		r.Post("/outgoing", a.HandleOutgoing)
		r.Post("/mock", a.MockOutgoing)
		r.Post("/storemocks", a.StoreMocks)
		r.Post("/updatemockparams", a.UpdateMockParams)
		// r.Post("/testbench", a.SendKtInfo)
		r.Get("/consumedmocks", a.GetConsumedMocks)
		r.Post("/agent/ready", a.MakeAgentReady)
	})
}

func (a *Agent) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	render.JSON(w, r, "OK")
}

func (a *Agent) HandleIncoming(w http.ResponseWriter, r *http.Request) {
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

	a.logger.Info("Agent marked as ready", zap.String("file", readyFile))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Agent is now ready\n"))
}
