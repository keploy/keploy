package manager

import "context"

// ctxKey is the unexported context key under which a per-session
// SyncMockManager is carried. Unexported so only this package can set or
// read it.
type ctxKey struct{}

// NewContext returns a child of ctx carrying mgr. Multi-app callers (e.g.
// the enterprise DaemonSet reader, which runs one manager per app) wrap
// the parser context with NewContext before dispatching, so deep emit
// sites resolve the correct per-session manager instead of the package
// global. A nil mgr is allowed and resolves back to the global default.
func NewContext(ctx context.Context, mgr *SyncMockManager) context.Context {
	return context.WithValue(ctx, ctxKey{}, mgr)
}

// FromContext returns the SyncMockManager carried by ctx, or nil if none
// was set (the single-session / OSS-default case).
func FromContext(ctx context.Context) *SyncMockManager {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(ctxKey{}).(*SyncMockManager)
	return m
}

// FromContextOrGlobal is the single resolution point used by parser emit
// sites: it returns the per-session manager carried by ctx, or the
// package global when none is set. This keeps single-session behaviour
// byte-for-byte identical (no manager on the context ⇒ Get()), while
// letting multi-app callers redirect emits per app via NewContext.
func FromContextOrGlobal(ctx context.Context) *SyncMockManager {
	if m := FromContext(ctx); m != nil {
		return m
	}
	return Get()
}
