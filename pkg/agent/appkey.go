package agent

import "context"

// AppKey is the per-application (per-recording-session) identity that keys all
// per-app state in a multi-tenant agent. One agent process can serve many
// applications concurrently; every piece of state that used to be a process
// singleton (proxy session, mock manager, syncMock output, dedup, mode, etc.)
// is partitioned by this key.
//
// It is deliberately an opaque string so the two attribution sources can share
// one identity type:
//   - sidecar/native: derived from the app (e.g. app name or a hash of
//     appName+clientPID).
//   - daemonset: the per-session key the controller resolves from a target's
//     {namespace, deployment, testSetID}.
//
// The zero value (DefaultAppKey) is the single-tenant / sidecar fallback: when
// no key is present on the context, callers operate on the default app so the
// existing one-process-one-app behaviour is preserved byte-for-byte.
type AppKey string

// DefaultAppKey is the fallback key used when no per-app key is supplied. It
// preserves the historical single-app behaviour: all state lands under one
// default AppContext.
const DefaultAppKey AppKey = ""

type appKeyCtxKey struct{}

// WithAppKey returns a copy of ctx carrying the per-app key. Producers (HTTP
// route middleware on the control path, the connection router on the data
// path) stamp the key; consumers deep in the proxy/parsers read it back with
// AppKeyFromContext without having to thread an extra parameter through every
// call.
func WithAppKey(ctx context.Context, key AppKey) context.Context {
	return context.WithValue(ctx, appKeyCtxKey{}, key)
}

// AppKeyFromContext returns the per-app key on ctx and whether one was set.
// When ok is false the caller should use DefaultAppKey (single-tenant path).
func AppKeyFromContext(ctx context.Context) (AppKey, bool) {
	if ctx == nil {
		return DefaultAppKey, false
	}
	key, ok := ctx.Value(appKeyCtxKey{}).(AppKey)
	return key, ok
}

// AppKeyOrDefault is the convenience accessor: returns the per-app key on ctx
// or DefaultAppKey when none is present.
func AppKeyOrDefault(ctx context.Context) AppKey {
	key, ok := AppKeyFromContext(ctx)
	if !ok {
		return DefaultAppKey
	}
	return key
}
