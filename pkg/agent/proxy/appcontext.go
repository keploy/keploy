package proxy

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// AppContext owns every piece of state that, in the single-app proxy, lived as
// a process singleton on Proxy. One AppContext exists per application
// (agent.AppKey) so that a single agent process can record/replay many apps
// concurrently without cross-tenant interference.
//
// This is the skeleton (issue #4275, phase 2): the struct + registry + PID
// attribution. Wiring the proxy hot path onto it lands in a later phase; until
// then Proxy keeps its own fields and the DefaultAppKey AppContext mirrors the
// historical single-app behaviour.
type AppContext struct {
	logger *zap.Logger

	// Key is the per-app identity; RootPID is the app's process-tree root
	// used for /proc-ancestry attribution on the native path.
	Key     agent.AppKey
	RootPID uint32

	// session + mockManager: the per-app record/replay state. Guarded by
	// sessionMu, mirroring Proxy.{sessionMu,session,mockManager}.
	sessionMu   sync.RWMutex
	session     *agent.Session
	mockManager *MockManager

	// errChannel + activeTestErrors: per-app mock-not-found error plane.
	errChannel       chan error
	activeTestErrors atomic.Pointer[testErrorAccumulator]
	errDrainOnce     sync.Once

	// dnsCache + recordedDNSMocks ("static-dedup" for DNS): per-app.
	dnsCache         *expirable.LRU[string, dnsCacheEntry]
	recordedDNSMocks *expirable.LRU[string, bool]

	// isGracefulShutdown + opportunisticTLSIntercept: per-app flags.
	isGracefulShutdown        atomic.Bool
	opportunisticTLSIntercept bool

	// pcap lifecycle, per-app.
	pcapMu          sync.Mutex
	pcapCancel      context.CancelFunc
	pcapDone        chan struct{}
	pcapBroadcaster *pcapBroadcaster

	// syncMgr is the per-app mock-window manager. Nil here in the skeleton;
	// wired to syncMock.New() once syncMock is de-globalized (phase 3).
	syncMgr *syncMock.SyncMockManager
}

// newAppContext builds an AppContext with its per-app caches/channels
// initialised the same way Proxy.New initialises the singletons today.
func newAppContext(logger *zap.Logger, key agent.AppKey) *AppContext {
	// The DefaultAppKey app reuses the process-global syncMock manager so the
	// single-tenant path — and every existing syncMock.Get() lifecycle site —
	// is byte-for-byte unchanged; only additional apps get their own manager.
	mgr := syncMock.New()
	if key == agent.DefaultAppKey {
		mgr = syncMock.Get()
	}
	mgr.SetLogger(logger)
	return &AppContext{
		logger:           logger,
		Key:              key,
		syncMgr:          mgr,
		errChannel:       make(chan error, 100),
		dnsCache:         newDNSCache(),
		recordedDNSMocks: newRecordedDNSMocksCache(),
	}
}

// SyncMgr returns the app's per-app mock-window manager.
func (a *AppContext) SyncMgr() *syncMock.SyncMockManager { return a.syncMgr }

// close releases the app's per-app resources: stops the mock manager's
// idle-sweeper goroutine and closes the syncMock output channel. Never closes
// the process-global (default-app) manager. Called by AppRegistry.Delete.
func (a *AppContext) close() {
	a.sessionMu.Lock()
	mm := a.mockManager
	a.mockManager = nil
	a.sessionMu.Unlock()
	if mm != nil {
		mm.Close()
	}
	// Non-default apps own a fresh manager; the default app's manager is the
	// shared global and must outlive any single app's teardown.
	if a.syncMgr != nil && a.syncMgr != syncMock.Get() {
		a.syncMgr.CloseOutChan()
	}
}

func (a *AppContext) getSession() *agent.Session {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()
	return a.session
}

func (a *AppContext) setSession(s *agent.Session) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	a.session = s
}

func (a *AppContext) getMockManager() *MockManager {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()
	return a.mockManager
}

// setMockManager swaps in a new manager and Close()s the previous one outside
// the lock — Close() synchronises with the manager's own workers and must not
// run under sessionMu (same contract as Proxy.setMockManager).
func (a *AppContext) setMockManager(m *MockManager) {
	a.sessionMu.Lock()
	prev := a.mockManager
	a.mockManager = m
	a.sessionMu.Unlock()
	if prev != nil {
		prev.Close()
	}
}

// resetRecordedDNSMocks clears the per-app DNS dedup tracker for a fresh
// recording session.
func (a *AppContext) resetRecordedDNSMocks() {
	a.recordedDNSMocks = newRecordedDNSMocksCache()
}

// Proxy implements the multi-tenant AppRegistrar extension.
var _ agent.AppRegistrar = (*Proxy)(nil)

// SetAppResolver installs the PID→app attribution function on the proxy's
// registry (satisfies agent.AppRegistrar). Embedders with an authoritative
// PID→app map (e.g. the daemonset controller) override the default
// /proc-ancestry resolver here.
func (p *Proxy) SetAppResolver(resolver func(kernelPid uint32) (agent.AppKey, bool)) {
	p.apps.SetResolver(resolver)
}

// RegisterApp creates (if absent) the AppContext for key and seeds the
// attribution cache with the app's root PID(s), so the data plane
// (handleConnection → apps.Resolve) resolves the same key the control plane
// stamps on ctx. Satisfies agent.AppRegistrar.
func (p *Proxy) RegisterApp(key agent.AppKey, rootPIDs ...uint32) {
	ac := p.apps.GetOrCreate(key)
	for _, pid := range rootPIDs {
		if pid != 0 {
			ac.RootPID = pid
			p.apps.RegisterPID(pid, key)
		}
	}
}

// DeregisterApp tears down the app's per-app state. Satisfies
// agent.AppRegistrar.
func (p *Proxy) DeregisterApp(key agent.AppKey) {
	p.apps.Delete(key)
}

// ResolveAppKey maps an intercepted connection's originating kernel PID to its
// owning app key, reporting whether it was attributed. Used by embedders that
// capture outside handleConnection (the daemonset proxyless reader) to stamp
// the per-app key on the capture context. Satisfies agent.AppRegistrar.
func (p *Proxy) ResolveAppKey(kernelPid uint32) (agent.AppKey, bool) {
	return p.apps.ResolveWithOK(kernelPid)
}

// PIDResolver maps an originating kernel TGID to the owning app key. The
// attribution source is pluggable: the native path uses /proc ancestry
// (resolveAppByProcAncestry); the daemonset supplies an authoritative
// resolver backed by its controller's PID→session map.
type PIDResolver func(kernelPid uint32) (agent.AppKey, bool)

// AppRegistry holds the live AppContexts keyed by AppKey and resolves an
// intercepted connection (by its originating kernel PID) to an app.
type AppRegistry struct {
	logger *zap.Logger

	apps  sync.Map // agent.AppKey -> *AppContext
	byPID sync.Map // uint32 (kernelPid) -> agent.AppKey  (attribution cache)

	// resolver turns a kernelPid into an AppKey when it is not yet cached.
	// When nil, Resolve only consults the cache + DefaultAppKey.
	resolverMu sync.RWMutex
	resolver   PIDResolver
}

func NewAppRegistry(logger *zap.Logger) *AppRegistry {
	r := &AppRegistry{logger: logger}
	// Default native attribution: /proc ancestry. The daemonset overrides
	// this with an authoritative controller-backed resolver via SetResolver.
	r.resolver = func(kernelPid uint32) (agent.AppKey, bool) {
		return resolveAppByProcAncestry(r, kernelPid)
	}
	return r
}

// SetResolver installs the pluggable PID→app attribution function.
func (r *AppRegistry) SetResolver(res PIDResolver) {
	r.resolverMu.Lock()
	r.resolver = res
	r.resolverMu.Unlock()
}

// Get returns the AppContext for key, if present.
func (r *AppRegistry) Get(key agent.AppKey) (*AppContext, bool) {
	v, ok := r.apps.Load(key)
	if !ok {
		return nil, false
	}
	return v.(*AppContext), true
}

// GetOrCreate returns the AppContext for key, creating it on first use.
func (r *AppRegistry) GetOrCreate(key agent.AppKey) *AppContext {
	if v, ok := r.apps.Load(key); ok {
		return v.(*AppContext)
	}
	ac := newAppContext(r.logger, key)
	actual, loaded := r.apps.LoadOrStore(key, ac)
	if loaded {
		return actual.(*AppContext)
	}
	return ac
}

// RegisterPID seeds the attribution cache so an app's RootPID (and any TGIDs
// the caller already knows) resolve to its key without a /proc walk.
func (r *AppRegistry) RegisterPID(kernelPid uint32, key agent.AppKey) {
	r.byPID.Store(kernelPid, key)
}

// Delete tears down an app: closes its per-app resources (mock manager
// idle-sweeper goroutine + syncMock output channel), evicts its PID-attribution
// cache entries, and removes it from the registry. Mandatory under the
// long-lived shared daemon — without the close, every session teardown leaks
// the per-app MockManager's sweeper goroutine.
func (r *AppRegistry) Delete(key agent.AppKey) {
	if v, ok := r.apps.Load(key); ok {
		v.(*AppContext).close()
	}
	r.apps.Delete(key)
	r.byPID.Range(func(k, v any) bool {
		if v.(agent.AppKey) == key {
			r.byPID.Delete(k)
		}
		return true
	})
}

// Range calls f for each live AppContext (stopping if f returns false). Used by
// the proxy's per-app syncMock flush/close lifecycle.
func (r *AppRegistry) Range(f func(*AppContext) bool) {
	r.apps.Range(func(_, v any) bool {
		return f(v.(*AppContext))
	})
}

// Resolve maps a kernel PID to its owning app key: cache first, then the
// pluggable resolver (whose result is cached), else DefaultAppKey.
func (r *AppRegistry) Resolve(kernelPid uint32) agent.AppKey {
	key, _ := r.ResolveWithOK(kernelPid)
	return key
}

// ResolveWithOK is Resolve but reports whether the PID was actually attributed
// to a registered app. ok=false (with DefaultAppKey) means unresolved
// (cold-start / not a target / ambiguous overlap) — callers on the data path
// use this to drop unattributed captures rather than mis-file them.
func (r *AppRegistry) ResolveWithOK(kernelPid uint32) (agent.AppKey, bool) {
	if v, ok := r.byPID.Load(kernelPid); ok {
		return v.(agent.AppKey), true
	}
	r.resolverMu.RLock()
	res := r.resolver
	r.resolverMu.RUnlock()
	if res != nil {
		if key, ok := res(kernelPid); ok {
			r.byPID.Store(kernelPid, key)
			return key, true
		}
	}
	return agent.DefaultAppKey, false
}

// resolveAppByProcAncestry is the default native attribution: walk the /proc
// parent chain of kernelPid and return the key of the first registered app
// whose RootPID is an ancestor. Used to build a PIDResolver over an
// AppRegistry on the sidecar/native path.
func resolveAppByProcAncestry(reg *AppRegistry, kernelPid uint32) (agent.AppKey, bool) {
	// Build RootPID -> key once per call (apps are few).
	roots := map[uint32]agent.AppKey{}
	reg.apps.Range(func(_, v any) bool {
		ac := v.(*AppContext)
		if ac.RootPID != 0 {
			roots[ac.RootPID] = ac.Key
		}
		return true
	})
	if len(roots) == 0 {
		return agent.DefaultAppKey, false
	}
	pid := kernelPid
	for i := 0; i < 64 && pid > 1; i++ {
		if key, ok := roots[pid]; ok {
			return key, true
		}
		ppid, ok := readParentPID(pid)
		if !ok {
			break
		}
		pid = ppid
	}
	return agent.DefaultAppKey, false
}

// readParentPID reads PPid from /proc/<pid>/status. Returns false when the
// process is gone or the field is unreadable.
func readParentPID(pid uint32) (uint32, bool) {
	data, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/status")
	if err != nil {
		return 0, false
	}
	const key = "PPid:"
	s := string(data)
	idx := indexLine(s, key)
	if idx < 0 {
		return 0, false
	}
	field := s[idx+len(key):]
	// trim leading spaces/tabs, read digits up to newline
	j := 0
	for j < len(field) && (field[j] == ' ' || field[j] == '\t') {
		j++
	}
	start := j
	for j < len(field) && field[j] >= '0' && field[j] <= '9' {
		j++
	}
	if start == j {
		return 0, false
	}
	ppid, err := strconv.ParseUint(field[start:j], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(ppid), true
}

// indexLine returns the byte index of prefix at the start of any line in s, or
// -1. Avoids a regexp/strings.Split allocation on the attribution hot path.
func indexLine(s, prefix string) int {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return 0
	}
	for i := 0; i+len(prefix) < len(s); i++ {
		if s[i] == '\n' && s[i+1:i+1+len(prefix)] == prefix {
			return i + 1
		}
	}
	return -1
}
