// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

func StartAgentServer(ctx context.Context, logger *zap.Logger, port int, router http.Handler) {
	logger.Info("Starting Agent's HTTP server on :", zap.Int("port", port))

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router,
	}

	// Start server in a goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("failed to start HTTP server", zap.Error(err))
		}
	}()

	logger.Info("HTTP server started successfully on port ", zap.Int("port", port))

	// Wait for context cancellation (SIGTERM/SIGINT)
	<-ctx.Done()
	logger.Info("Shutting down agent HTTP server...")

	// Create a deadline for graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	} else {
		logger.Info("HTTP server stopped gracefully")
	}
}
