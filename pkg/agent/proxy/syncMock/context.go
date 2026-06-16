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

// StaticDeduper is the per-session static-deduplication hook (the enterprise
// "static dedup" feature). It is defined here — a package both the proxy
// reader and the enterprise capture hook already import — so a per-app
// deduper can ride the parser context without either side importing the
// other (avoids an import cycle). The single-session build leaves it unset
// and the capture hook uses its process-wide deduper unchanged.
type StaticDeduper interface {
	IsDuplicate(schema string) bool
	GetCustomFieldsForEndpoint(method, path string, statusCode int) []string
}

type staticDedupKey struct{}

// WithStaticDeduper returns a child of ctx carrying d. Multi-app callers
// wrap the parser/capture context with this so the static-dedup check is
// scoped to the owning app instead of one process-wide deduper.
func WithStaticDeduper(ctx context.Context, d StaticDeduper) context.Context {
	return context.WithValue(ctx, staticDedupKey{}, d)
}

// StaticDeduperFromContext returns the per-app deduper carried by ctx, or
// nil if none (single-app default — caller falls back to its own deduper).
func StaticDeduperFromContext(ctx context.Context) StaticDeduper {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(staticDedupKey{}).(StaticDeduper)
	return d
}
