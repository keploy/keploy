package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/agent"
	"golang.org/x/sync/errgroup"

	// "go.keploy.io/server/v2/pkg/service/agent"
	"go.uber.org/zap"
)

type AgentRequest struct {
	logger *zap.Logger
	agent  agent.Service
}

// handlers -> agent service
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

	// Call GetIncoming to get the channel
	tc, err := a.agent.GetIncoming(ctx, 0, models.IncomingOptions{})
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

	// Call GetOutgoing to get the channel
	mockChan, err := a.agent.GetOutgoing(ctx, 0, models.OutgoingOptions{})
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	
	for m := range mockChan {
		select {
		case <-r.Context().Done():
			// Client closed the connection or context was cancelled
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

	var SetupRequest models.RegisterReq
	err := json.NewDecoder(r.Body).Decode(&SetupRequest)

	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusBadRequest)
		return
	}

	fmt.Printf("SetupRequest: %v\n", SetupRequest.SetupOptions.ClientNsPid)

	if SetupRequest.SetupOptions.ClientNsPid == 0 {
		render.JSON(w, r, "Client pid is required")
		render.Status(r, http.StatusBadRequest)
		return
	}
	fmt.Printf("Register Client req: %v\n", SetupRequest.SetupOptions)

	err = a.agent.RegisterClient(r.Context(), SetupRequest.SetupOptions)
	if err != nil {
		render.JSON(w, r, err)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	var SetupResponse models.RegisterResp
	SetupResponse = models.RegisterResp{
		AppId:      0,
		IsRunnning: true,
	}

	render.JSON(w, r, SetupResponse)
	render.Status(r, http.StatusOK)
}
