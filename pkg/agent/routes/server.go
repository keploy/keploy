// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

func StartAgentServer(ctx context.Context, logger *zap.Logger, port int, router http.Handler) {
	logger.Info("Starting Agent's HTTP server on :", zap.Int("port", port))

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
		logger.Info("DIAG/agent-shutdown-begin: srvCtx cancelled (SIGTERM or main ctx cancel)",
			zap.Int64("ts_ms", time.Now().UnixMilli()))

		// CRITICAL FIX (docker mode) — flush syncMock BEFORE shutting
		// down the HTTP server.
		//
		// Why this is here and not just in the /stop handler:
		// In docker-compose mode, `docker compose down` sends SIGTERM
		// directly to the agent container. The host never gets to POST
		// /stop — the agent shutdown is triggered by the OS signal that
		// utils.NewCtx's handler turns into a ctx.Cancel. That path
		// bypasses /stop entirely, so the flush we wired into
		// routes.record.go::Stop never runs in this CI lane.
		//
		// srvCtx.Done() is the single chokepoint that fires for BOTH
		// shutdown triggers (HTTP /stop AND OS SIGTERM), so flushing
		// here guarantees the buffered mongo teardown tail reaches the
		// host regardless of which path initiated the shutdown.
		//
		// CloseOutChan runs FlushOwnedWindows internally, pushing every
		// still-attributable buffered mock through the live outChan
		// (= mockChan = the live HTTP stream to the host). The outgoing
		// stream handler is still alive at this point — srv.Shutdown
		// hasn't been called yet — so it drains the channel through
		// gob.Encode to the host. CloseOutChan then closes outChan,
		// which lets the stream handler exit cleanly on the next read
		// (ok=false). Only after that completes do we call srv.Shutdown,
		// which waits for the handler to fully return before sealing
		// the server.
		if mgr := syncmgr.Get(); mgr != nil {
			logger.Info("DIAG/agent-shutdown-flush-begin: calling SyncMock.CloseOutChan before HTTP shutdown",
				zap.Int64("ts_ms", time.Now().UnixMilli()))
			flushStart := time.Now()
			mgr.CloseOutChan()
			logger.Info("DIAG/agent-shutdown-flush-end: SyncMock.CloseOutChan returned",
				zap.Duration("flush_duration", time.Since(flushStart)),
				zap.Int64("ts_ms", time.Now().UnixMilli()))
		} else {
			logger.Info("DIAG/agent-shutdown-flush-skipped: no SyncMockManager (likely test/replay mode)")
		}

		// Was 10 s; raised to 30 s because the post-flush HTTP graceful
		// shutdown now has to wait for the /outgoing stream handler to
		// finish draining mockChan (~5-15 s for 800-1,200 buffered
		// mocks gob-encoded over the wire on a 2-vCPU runner). 10 s
		// would cut the drain short on heavy-load lanes and resurrect
		// the same orphan-TC bug.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		logger.Info("DIAG/agent-shutdown-srv-begin: calling srv.Shutdown",
			zap.Duration("shutdown_grace", 30*time.Second),
			zap.Int64("ts_ms", time.Now().UnixMilli()))
		srvShutdownStart := time.Now()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server shutdown did not complete; check for long-running handlers or increase shutdown timeout",
				zap.Error(err),
				zap.Duration("srv_shutdown_duration", time.Since(srvShutdownStart)))
		} else {
			logger.Info("DIAG/agent-shutdown-srv-end: srv.Shutdown completed",
				zap.Duration("srv_shutdown_duration", time.Since(srvShutdownStart)),
				zap.Int64("ts_ms", time.Now().UnixMilli()))
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("failed to start HTTP server; verify port availability and network configuration", zap.Error(err))
		return
	}
	logger.Info("HTTP server stopped")
}
