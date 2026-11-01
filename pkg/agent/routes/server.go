// Package routes defines the routes for the agent service.
package routes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/plaintcp"
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
// A variable for tests.
var bindRetryBudget = 90 * time.Second

// bindRetryInterval is the delay between bind attempts. A departing agent's port
// frees within a second or two, so a sub-second cadence recovers quickly without
// busy-spinning. A variable for tests.
var bindRetryInterval = 500 * time.Millisecond

// StartAgentServer binds the agent's control-plane HTTP server. Its routes are
// guarded by a bearer token (see Authenticate); what it serves —
// /agent/pcap/keylog streams live TLS session keys, /agent/stop and
// /agent/storemocks mutate a running session — is why the bind address is kept
// as narrow as each mode allows on top of that.
//
// In native mode the agent and CLI share a network namespace, so loopback is
// both sufficient and necessary: binding 0.0.0.0 would expose the port to
// every host interface, and loopback already reaches every legitimate caller.
// In docker mode the CLI runs on the host and reaches the
// agent through its published port, so the server must stay reachable on the
// container's non-loopback interface for the docker proxy to forward into —
// exposure to the host network is instead closed off at the publish step
// (see GenerateKeployAgentService, which now publishes only to the host's
// loopback).
func StartAgentServer(ctx context.Context, logger *zap.Logger, port int, isDocker bool, router http.Handler) {
	addr := agentBindAddr(port, isDocker)
	logger.Info("Starting Agent's HTTP server", serverStartFields(addr)...)
	srv := newAgentHTTPServer(router)

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
		logger.Error("failed to start HTTP server; verify port availability and network configuration", zap.String("addr", addr), zap.Error(err))
		// The agent's start has failed: it lives on without its server, and
		// the CLI that waits for it fails the start with this at once. Not
		// for a port still held when the retries' budget ran out: that is no
		// failure a fresh start (which the CLI's retry is, on ports of its
		// own) would meet again, and the start, taking no step while it
		// waits for the port, is given up on as stalled, which retries.
		if srvCtx.Err() == nil && !isAddrInUse(err) {
			utils.FailBoot(fmt.Errorf("failed to start the agent's HTTP server on %s: %w", addr, err))
		}
		return
	}
	// The agent's start is over: its health check answers from here
	// (utils.BootProgress.Ready).
	utils.MarkBootReady()

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("failed to start HTTP server; verify port availability and network configuration", zap.String("addr", addr), zap.Error(err))
		return
	}
	logger.Info("HTTP server stopped")
}

// serverStartFields are the fields of the line the agent's HTTP server logs as
// it starts: its address and, under the proven switch, the name this agent
// process gives itself in every answer (processInstance,
// models.AgentInstanceHeader) and its pid. A verdict that says keploy's agent
// was started again under the running application names the process before
// and the one after by that name ("agent process X, then Y",
// models.CaptureLossAgentStartedAgain); this line, once per start in the
// agent's log, says which start each was, so a user can find both starts in
// the log and match each one's time to a restart of the agent or of its
// container. The pid is the one in the agent's own PID namespace: under
// docker that is not the host's pid a kernel log line names. With the switch
// off the line is as it was: no answer names the process then.
func serverStartFields(addr string) []zap.Field {
	fields := []zap.Field{zap.String("addr", addr)}
	if provenswitch.On() {
		fields = append(fields, zap.String("agent process", processInstance), zap.Int("pid", os.Getpid()))
	}
	return fields
}

// newAgentHTTPServer builds the control-plane HTTP server around router.
func newAgentHTTPServer(router http.Handler) *http.Server {
	return &http.Server{
		Handler: router,
		// A connection that is opened and then never finishes its request
		// line, or that completes a request and then goes quiet, would
		// otherwise hold a goroutine indefinitely - on a port any local
		// process can reach. These two cover both, and neither touches a
		// request already in flight.
		//
		// Deliberately no ReadTimeout or WriteTimeout: /agent/pcap/keylog and
		// /agent/pcap/traffic are long-lived streams that either would cut.
		// IdleTimeout applies only BETWEEN requests on a keep-alive
		// connection, so it leaves those streams alone; a client that finds a
		// reused connection closed simply dials again.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       5 * time.Minute,
		// The net/http default, pinned rather than changed, so the bound on
		// header memory is stated here rather than inherited silently.
		MaxHeaderBytes: 1 << 20,
	}
}

// agentBindAddr picks the agent control-plane server's bind address. Native
// mode binds loopback-only to narrow who can route to endpoints like
// /agent/pcap/keylog, which streams live TLS session keys; the bearer token
// enforced in Authenticate is what actually guards them, since loopback is not
// a privilege boundary between local users. Docker mode
// must still bind every interface inside the container so the docker proxy
// can forward the published port in; that publish is restricted to the
// host's own loopback in GenerateKeployAgentService instead.
func agentBindAddr(port int, isDocker bool) string {
	if isDocker {
		return fmt.Sprintf(":%d", port)
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// listenWithRetry binds a TCP listener on addr, retrying only the transient
// "address already in use" case until the port frees or budget elapses. Every
// other listen error (and a cancelled context) is returned immediately — the
// retry is strictly for a port a departing owner is still releasing, never a
// mask for a genuine misconfiguration.
func listenWithRetry(ctx context.Context, logger *zap.Logger, addr string, budget, interval time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(budget)
	for {
		// Plain TCP, never Go's default MPTCP: in docker the agent listens on
		// every interface of its container.
		ln, err := plaintcp.Listen(context.Background(), "tcp", addr)
		if err == nil {
			return ln, nil
		}
		// Only "address already in use" is transient here; anything else is a
		// real failure the operator must see now.
		if !isAddrInUse(err) || !time.Now().Before(deadline) {
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
