// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"fmt"
	"net/http"

	"go.uber.org/zap"
)

func StartAgentServer(ctx context.Context, logger *zap.Logger, port int, router http.Handler) {
	logger.Info("Starting Agent's HTTP server on :", zap.Int("port", port))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router,
	}

	// Shut down the HTTP server when context is cancelled.
	go func() {
		<-ctx.Done()
		logger.Info("Shutting down agent HTTP server")
		if err := srv.Close(); err != nil {
			logger.Error("failed to close HTTP server", zap.Error(err))
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("failed to start HTTP server", zap.Error(err))
		return
	}
	logger.Info("HTTP server stopped")
}
