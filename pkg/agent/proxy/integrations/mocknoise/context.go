package mocknoise

import "context"

// engineKey carries the per-replay mock-noise Engine on a connection's context.
// The proxy builds the Engine from a parser's declared adapter (Integrations.
// MockNoise()) once per outgoing connection and attaches it here; the parser
// reads it back with EngineFrom instead of constructing its own. This is what
// makes MockNoise() the single switch that turns detection/strict on: a parser
// that declares nil gets no Engine (noise off), one that declares an adapter
// gets an Engine built from exactly that adapter.
type engineKey struct{}

// WithEngine returns a copy of ctx carrying e. A nil e is stored as-is; EngineFrom
// then returns nil and every Engine method is nil-safe, so "no engine" cleanly
// means "noise disabled".
func WithEngine(ctx context.Context, e *Engine) context.Context {
	return context.WithValue(ctx, engineKey{}, e)
}

// EngineFrom returns the Engine the proxy attached for this connection, or nil
// when none was attached (e.g. a parser whose MockNoise() is nil, or a test that
// drives the match path directly without injecting one). All Engine methods are
// nil-safe, so callers use the result without a guard.
func EngineFrom(ctx context.Context) *Engine {
	e, _ := ctx.Value(engineKey{}).(*Engine)
	return e
}
