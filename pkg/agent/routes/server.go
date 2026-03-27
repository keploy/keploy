// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

func StartAgentServer(ctx context.Context, logger *zap.Logger, port int, router http.Handler) {
	logger.Info("Starting Agent's HTTP server on :", zap.Int("port", port))
	os.Setenv("KEPLOY_AGENT_PORT", fmt.Sprintf("%d", port))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router,
	}

	// Derive a context tied to both the parent context and the lifetime of this function,
	// so the shutdown goroutine will always terminate when the server stops or fails to start.
	srvCtx, srvCancel := context.WithCancel(ctx)
	defer srvCancel()

	// Shut down the HTTP server when context is cancelled.
	go func() {
		<-srvCtx.Done()
		logger.Info("Shutting down agent HTTP server")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server shutdown did not complete; check for long-running handlers or increase shutdown timeout", zap.Error(err))
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("failed to start HTTP server; verify port availability and network configuration", zap.Error(err))
		return
	}
	logger.Info("HTTP server stopped")
}
