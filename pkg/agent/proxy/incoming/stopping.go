package proxy

import (
	"context"
	"errors"

	"go.uber.org/zap"
)

// stopping.go: once a recording whose client started the app (and the agent)
// is stopping, the client stops the app the ingress forwards to, on purpose
// and before the agent: it stops the app, drains what the agent captured, and
// only then stops the agent. A client that connects in between (a readiness
// loop, a retry, a health check) finds the app's port refusing, and the
// ingress said so at ERROR, as if the app had failed; a lane that fails on any
// ERROR failed with it. (Under docker compose, or for a k8s sidecar, the app
// goes on running through the stop, and the ingress is not told it stops.)
//
// Only that is said at DEBUG while stopping: a dial to the app that it
// refused. Everything else the ingress logs at ERROR stays an ERROR then: a
// capture that fails while the agent still works through what it captured
// before the stop is a loss, and must be told as one.

// SetStopping says whether the recording the ingress serves is stopping
// and its client stops the app (graceful shutdown, from a client that drains
// after its stop) or a new one has begun. The agent calls it from its
// graceful shutdown, and again as a recording's incoming stream attaches.
func (pm *IngressProxyManager) SetStopping(stopping bool) { pm.stopping.Store(stopping) }

// logAppDialFailure logs a failed dial, or redial, to the app the ingress
// forwards to: at DEBUG when the app refused it while the recording is
// stopping (see above), when keploy itself ended the dial (its context
// canceled: the forwarder or the agent stopping), or when the call's client
// went while the call waited for the app to listen (nothing was sent on to
// the app: dialApp), else at ERROR.
func (pm *IngressProxyManager) logAppDialFailure(logger *zap.Logger, msg string, err error, fields ...zap.Field) {
	fields = append(fields, zap.Error(err))
	if pm.stopping.Load() && isConnRefused(err) {
		logger.Debug(msg, append(fields,
			zap.String("why", "the recording is stopping, and its app refused the connection: expected once the recording has stopped the app it started"))...)
		return
	}
	if errors.Is(err, context.Canceled) {
		logger.Debug(msg, append(fields,
			zap.String("why", "keploy stopped serving the application's port while it dialed the app"))...)
		return
	}
	if errors.Is(err, errClientLeft) {
		logger.Debug(msg, append(fields,
			zap.String("why", "the call's client went while keploy waited for the application to listen"))...)
		return
	}
	logger.Error(msg, fields...)
}
