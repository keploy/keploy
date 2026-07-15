// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// bindRetryBudget bounds how long StartAgentServer keeps retrying a transient
// "address already in use" before giving up. In docker mode the agent publishes
// its HTTP port to the host; when keploy sessions run back-to-back (e.g. a CI
// driver that records then replays then replays-with-mappings, each a fresh
// keploy process), the previous session's agent — or, more precisely, its
// docker userland-proxy holding the published host port — can still own the
// port for a short window while it tears down. That owner WILL release the
// port, so a transient bind failure must not be fatal: hard-failing here leaves
// the agent's readiness file unwritten and the container healthcheck fails for
// its full ceiling (~300s) before the run aborts. The budget is kept well under
// that ceiling (see pkg/platform/docker) so the retry always has room to land.
const bindRetryBudget = 90 * time.Second

// bindRetryInterval is the delay between bind attempts. A departing agent's port
// frees within a second or two, so a sub-second cadence recovers quickly without
// busy-spinning.
const bindRetryInterval = 500 * time.Millisecond

func StartAgentServer(ctx context.Context, logger *zap.Logger, port int, router http.Handler) {
	logger.Info("Starting Agent's HTTP server on :", zap.Int("port", port))

	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{
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

	// Bind explicitly (with bounded retry on a transiently-held port) so a
	// previous session's not-yet-released host port doesn't permanently fail
	// this agent's startup; then serve on the acquired listener.
	listener, err := listenWithRetry(srvCtx, logger, addr, bindRetryBudget, bindRetryInterval)
	if err != nil {
		logger.Error("failed to start HTTP server; verify port availability and network configuration", zap.Error(err))
		return
	}

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("failed to start HTTP server; verify port availability and network configuration", zap.Error(err))
		return
	}
	logger.Info("HTTP server stopped")
}

// listenWithRetry binds a TCP listener on addr, retrying only the transient
// "address already in use" case until the port frees or budget elapses. Every
// other listen error (and a cancelled context) is returned immediately — the
// retry is strictly for a port a departing owner is still releasing, never a
// mask for a genuine misconfiguration.
func listenWithRetry(ctx context.Context, logger *zap.Logger, addr string, budget, interval time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(budget)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		// Only "address already in use" is transient here; anything else is a
		// real failure the operator must see now.
		if !errors.Is(err, syscall.EADDRINUSE) || !time.Now().Before(deadline) {
			return nil, err
		}
		logger.Warn("agent HTTP port is transiently in use (a previous session's agent is likely still releasing it); retrying bind",
			zap.String("addr", addr),
			zap.Duration("retry_in", interval),
			zap.Error(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}
