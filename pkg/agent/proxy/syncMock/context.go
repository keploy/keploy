package manager

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// New constructs a fresh, independent SyncMockManager. In a multi-tenant agent
// each application owns its own manager (its own mock buffer, output channel,
// firstReqSeen boundary and resolved-window ring) so one app's recording can
// never bleed into another's. The process-global singleton (Get) is now just
// New() called once at package load — the single-tenant / sidecar fallback.
func New() *SyncMockManager {
	return &SyncMockManager{
		buffer:       make([]*models.Mock, 0, defaultMockBufferCapacity),
		firstReqSeen: false,
	}
}

type managerCtxKey struct{}

// NewContext returns a copy of ctx carrying the per-app SyncMockManager. The
// connection router stamps the owning app's manager onto the context before
// dispatching to the protocol parsers; the parsers then resolve it back with
// FromContext when emitting mocks, without threading the manager through every
// RecordOutgoing signature.
func NewContext(ctx context.Context, m *SyncMockManager) context.Context {
	return context.WithValue(ctx, managerCtxKey{}, m)
}

// FromContext returns the per-app SyncMockManager carried on ctx, or the
// process-global instance when none is present. The fallback makes the
// migration safe and behaviour-neutral: an emit site switched from Get() to
// FromContext(ctx) resolves to exactly the global manager until a per-app
// manager is actually stamped onto the context (sidecar / no-key path).
func FromContext(ctx context.Context) *SyncMockManager {
	if ctx != nil {
		if m, ok := ctx.Value(managerCtxKey{}).(*SyncMockManager); ok && m != nil {
			return m
		}
	}
	return instance
}
