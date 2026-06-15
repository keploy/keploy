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
	return &AppContext{
		logger:           logger,
		Key:              key,
		errChannel:       make(chan error, 100),
		dnsCache:         newDNSCache(),
		recordedDNSMocks: newRecordedDNSMocksCache(),
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

// Delete tears down an app: evicts its cache entries and removes it from the
// registry. The caller is responsible for closing per-app resources
// (mockManager, syncMgr, channels) before/after — wired in a later phase.
func (r *AppRegistry) Delete(key agent.AppKey) {
	r.apps.Delete(key)
	r.byPID.Range(func(k, v any) bool {
		if v.(agent.AppKey) == key {
			r.byPID.Delete(k)
		}
		return true
	})
}

// Resolve maps a kernel PID to its owning app key: cache first, then the
// pluggable resolver (whose result is cached), else DefaultAppKey.
func (r *AppRegistry) Resolve(kernelPid uint32) agent.AppKey {
	if v, ok := r.byPID.Load(kernelPid); ok {
		return v.(agent.AppKey)
	}
	r.resolverMu.RLock()
	res := r.resolver
	r.resolverMu.RUnlock()
	if res != nil {
		if key, ok := res(kernelPid); ok {
			r.byPID.Store(kernelPid, key)
			return key
		}
	}
	return agent.DefaultAppKey
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
