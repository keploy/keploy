// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/agent"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type AgentRequest struct {
	logger *zap.Logger
	agent  agent.Service
}

func New(r chi.Router, agent agent.Service, logger *zap.Logger) {
	a := &AgentRequest{
		logger: logger,
		agent:  agent,
	}
	r.Route("/agent", func(r chi.Router) {
		r.Get("/health", a.Health)
		r.Post("/incoming", a.HandleIncoming)
		r.Post("/outgoing", a.HandleOutgoing)
		r.Post("/mock", a.MockOutgoing)
		r.Post("/setmocks", a.SetMocks)
		r.Post("/register", a.RegisterClients)
		r.Get("/consumedmocks", a.GetConsumedMocks)
		r.Post("/unregister", a.DeRegisterClients)
	})

}

func (a *AgentRequest) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	render.JSON(w, r, "OK")
}

func (a *AgentRequest) HandleIncoming(w http.ResponseWriter, r *http.Request) {
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

	// Call GetIncoming to get the channel
	tc, err := a.agent.GetIncoming(ctx, incomingReq.ClientID, incomingReq.IncomingOptions)
	if err != nil {
		http.Error(w, "Error retrieving test cases", http.StatusInternalServerError)
		return
	}

	// Keep the connection alive and stream data
	for t := range tc {
		select {
		case <-r.Context().Done():
			// Client closed the connection or context was cancelled
			return
		default:
			// Stream each test case as JSON
			fmt.Printf("Sending Test case: %v\n", t)
			render.JSON(w, r, t)
			flusher.Flush() // Immediately send data to the client
		}
	}
}

func (a *AgentRequest) HandleOutgoing(w http.ResponseWriter, r *http.Request) {
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

	var outgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&outgoingReq)
	if err != nil {
		http.Error(w, "Error decoding request", http.StatusBadRequest)
		return
	}

	// Call GetOutgoing to get the channel
	mockChan, err := a.agent.GetOutgoing(ctx, outgoingReq.ClientID, outgoingReq.OutgoingOptions)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	for m := range mockChan {
		select {
		case <-r.Context().Done():
			fmt.Println("Context done in HandleOutgoing")
			if m != nil {
				render.JSON(w, r, m)
				flusher.Flush()
			} else {
				render.JSON(w, r, "No more mocks")
				flusher.Flush()
			}
			return
		default:
			// Stream each mock as JSON
			render.JSON(w, r, m)
			flusher.Flush()
		}
	}
}

func (a *AgentRequest) RegisterClients(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var registerReq models.RegisterReq
	err := json.NewDecoder(r.Body).Decode(&registerReq)

	register := models.AgentResp{
		ClientID: registerReq.SetupOptions.ClientID,
		Error:    nil,
	}

	if err != nil {
		register.Error = err
		render.JSON(w, r, register)
		render.Status(r, http.StatusBadRequest)
		return
	}

	fmt.Printf("SetupRequest: %v\n", registerReq.SetupOptions.ClientNsPid)

	if registerReq.SetupOptions.ClientNsPid == 0 {
		register.Error = fmt.Errorf("Client pid is required")
		render.JSON(w, r, register)
		render.Status(r, http.StatusBadRequest)
		return
	}
	fmt.Printf("Register Client req: %v\n", registerReq.SetupOptions)

	err = a.agent.RegisterClient(r.Context(), registerReq.SetupOptions)
	if err != nil {
		register.Error = err
		render.JSON(w, r, register)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, register)
	render.Status(r, http.StatusOK)
}

func (a *AgentRequest) DeRegisterClients(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var OutgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&OutgoingReq)

	mockRes := models.AgentResp{
		ClientID:  OutgoingReq.ClientID,
		Error:     nil,
		IsSuccess: true,
	}

	if err != nil {
		mockRes.Error = err
		mockRes.IsSuccess = false
		render.JSON(w, r, mockRes)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.agent.DeRegisterClient(r.Context(), OutgoingReq.ClientID)
	if err != nil {
		mockRes.Error = err
		mockRes.IsSuccess = false
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	render.JSON(w, r, "Client De-registered")
}
