package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

func TestAppContextSyncMgrPerApp(t *testing.T) {
	r := NewAppRegistry(zap.NewNop())
	// Default app reuses the process-global manager (single-tenant parity).
	def := r.GetOrCreate(agent.DefaultAppKey)
	if def.SyncMgr() != syncMock.Get() {
		t.Fatalf("default app must reuse the global syncMock manager")
	}
	// A named app gets its own, distinct manager.
	a := r.GetOrCreate(agent.AppKey("app-a"))
	if a.SyncMgr() == nil || a.SyncMgr() == syncMock.Get() {
		t.Fatalf("named app must get its own (non-global) syncMock manager")
	}
	b := r.GetOrCreate(agent.AppKey("app-b"))
	if a.SyncMgr() == b.SyncMgr() {
		t.Fatalf("distinct apps must get distinct syncMock managers")
	}
}

func TestAppRegistryDeleteClosesResources(t *testing.T) {
	r := NewAppRegistry(zap.NewNop())
	a := r.GetOrCreate(agent.AppKey("app-a"))
	a.setMockManager(NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop()))
	// Delete must not panic, must evict, and must clear the manager (closing
	// its idle-sweeper goroutine) — the daemonset-churn leak fix.
	r.Delete(agent.AppKey("app-a"))
	if _, ok := r.Get(agent.AppKey("app-a")); ok {
		t.Fatalf("Delete did not evict the app")
	}
	if a.getMockManager() != nil {
		t.Fatalf("Delete did not close/clear the mock manager")
	}
	// Deleting the default app must NOT close the shared global manager's
	// output channel (defensive: close() guards against it).
	_ = r.GetOrCreate(agent.DefaultAppKey)
	r.Delete(agent.DefaultAppKey) // must not panic / must not close global
}

func TestAppRegistryGetOrCreateIsIdempotent(t *testing.T) {
	r := NewAppRegistry(zap.NewNop())
	a := r.GetOrCreate(agent.AppKey("app-a"))
	b := r.GetOrCreate(agent.AppKey("app-a"))
	if a != b {
		t.Fatalf("GetOrCreate returned distinct AppContexts for the same key")
	}
	if got, ok := r.Get(agent.AppKey("app-a")); !ok || got != a {
		t.Fatalf("Get did not return the created AppContext")
	}
	if a.errChannel == nil || a.dnsCache == nil || a.recordedDNSMocks == nil {
		t.Fatalf("AppContext caches/channels not initialised")
	}
}

func TestAppRegistryResolveUsesCacheThenResolver(t *testing.T) {
	r := NewAppRegistry(zap.NewNop())

	// No cache entry and the default /proc resolver finds no registered app
	// → DefaultAppKey.
	if got := r.Resolve(424242); got != agent.DefaultAppKey {
		t.Fatalf("expected DefaultAppKey for unknown pid, got %q", got)
	}

	// A custom resolver is consulted on a miss and its result is cached.
	calls := 0
	r.SetResolver(func(pid uint32) (agent.AppKey, bool) {
		calls++
		if pid == 100 {
			return agent.AppKey("app-x"), true
		}
		return agent.DefaultAppKey, false
	})
	if got := r.Resolve(100); got != agent.AppKey("app-x") {
		t.Fatalf("resolver result not used, got %q", got)
	}
	if got := r.Resolve(100); got != agent.AppKey("app-x") {
		t.Fatalf("cached result not used, got %q", got)
	}
	if calls != 1 {
		t.Fatalf("resolver should be hit once (then cached), hit %d times", calls)
	}
}

func TestAppRegistryRegisterAndDeleteEvictsPIDCache(t *testing.T) {
	r := NewAppRegistry(zap.NewNop())
	r.GetOrCreate(agent.AppKey("app-a"))
	r.RegisterPID(777, agent.AppKey("app-a"))
	if got := r.Resolve(777); got != agent.AppKey("app-a") {
		t.Fatalf("RegisterPID not honoured by Resolve, got %q", got)
	}
	r.Delete(agent.AppKey("app-a"))
	if _, ok := r.Get(agent.AppKey("app-a")); ok {
		t.Fatalf("Delete did not remove the AppContext")
	}
	if got := r.Resolve(777); got != agent.DefaultAppKey {
		t.Fatalf("Delete did not evict the PID cache entry, got %q", got)
	}
}
