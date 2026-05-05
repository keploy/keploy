// Package agent contains methods for setting up hooks and proxy along with registering keploy clients.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type ClientMockStorage struct {
	filtered   []*models.Mock
	unfiltered []*models.Mock
	mu         sync.RWMutex
}

type Agent struct {
	logger          *zap.Logger
	coreAgent.Proxy                // embedding the Proxy interface to transfer the proxy methods to the core object
	coreAgent.Hooks                // embedding the Hooks interface to transfer the hooks methods to the core object
	dockerClient    kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	coreAgent.IncomingProxy
	proxyStarted bool
	config       *config.Config
	// activeClients sync.Map
	// New field for storing client-specific mocks
	clientMocks sync.Map // map[uint64]*ClientMockStorage
	Ip          string

	// strictLogOnce de-dupes the "strict mock window enabled" Info log
	// so it fires at most once per agent process instead of once per
	// test. Operators searching agent logs for strict-mode activity
	// still find a hit; they just don't get swamped by one line per
	// test on large suites.
	strictLogOnce sync.Once
}

func New(logger *zap.Logger, hook coreAgent.Hooks, proxy coreAgent.Proxy, client kdocker.Client, ip coreAgent.IncomingProxy, config *config.Config) *Agent {
	instrumentation := &Agent{
		logger:        logger,
		Hooks:         hook,
		Proxy:         proxy,
		IncomingProxy: ip,
		dockerClient:  client,
		config:        config,
	}
	if coreAgent.ProxyHook != nil && proxy != nil {
		proxy.SetAuxiliaryHook(coreAgent.ProxyHook)
	}
	coreAgent.RegisterIncomingProxy(ip)
	// Propagate the connection-pool idle retention from config into
	// the MockManager package default. Reset unconditionally — whether
	// config is present or nil — so a prior setter invocation
	// (embedder, multi-agent test) does not bleed retention state into
	// this agent instance. The setter itself treats <=0 as "revert to
	// the 5-minute default", so both paths below are safe.
	if config != nil {
		proxyPkg.SetConnectionIdleRetention(config.Test.ConnectionPoolIdleRetention)
	} else {
		proxyPkg.SetConnectionIdleRetention(0)
	}
	return instrumentation
}

// Setup will create a new app and store it in the map, all the setup will be done here
func (a *Agent) Setup(ctx context.Context, startCh chan int) error {

	// Remove stale readiness file from a previous run so the Docker
	// healthcheck (`cat <AgentReadyFile>`) does not pass before the
	// CLI has stored mocks on the agent. The file is re-created later
	// by the MakeAgentReady HTTP handler in pkg/agent/routes/record.go
	// once setup is complete.
	if err := os.Remove(kdocker.AgentReadyFile); err != nil && !os.IsNotExist(err) {
		a.logger.Debug("failed to remove stale agent readiness file", zap.Error(err))
	}

	a.logger.Debug("Starting the agent in ", zap.String("mode", string(a.config.Agent.Mode)))
	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	passPortsUint := a.config.Agent.PassThroughPorts

	rules := make([]models.BypassRule, len(a.config.Agent.PassThroughPorts))
	for i, port := range passPortsUint {
		rules[i] = models.BypassRule{
			Port: port,
		}
	}

	err := a.Hook(ctx, models.HookOptions{
		Mode:          a.config.Agent.Mode,
		IsDocker:      a.config.Agent.IsDocker,
		EnableTesting: a.config.Agent.EnableTesting,
		Rules:         rules,
	})
	if err != nil {
		a.logger.Error("failed to hook into the app", zap.Error(err))
		return err
	}

	if err := memoryguard.Start(ctx, a.logger, a.config.Agent.IsDocker, a.config.Agent.MemoryLimit); err != nil {
		a.logger.Info("Memory guard unavailable, continuing without memory-aware recording. "+
			"Ensure cgroup filesystem is mounted in the container or set --memory-limit=0 to disable.",
			zap.Error(err))
	}

	select {
	case startCh <- int(a.config.Agent.AgentPort):
	case <-ctx.Done():
		a.logger.Info("Context cancelled before agent becomes healthy. Stopping the agent.")
		return nil
	}

	<-ctx.Done()
	err = errGrp.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		utils.LogError(a.logger, err, "error during agent setup")
		return err
	}
	a.logger.Debug("Context cancelled, stopping the agent")
	return context.Canceled

}

func (a *Agent) StartIncomingProxy(ctx context.Context, opts models.IncomingOptions) (chan *models.TestCase, error) {
	tc := a.IncomingProxy.Start(ctx, opts)
	a.logger.Debug("Ingress proxy manager started and is listening for bind events.")
	return tc, nil
}

// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
// When this flag is set, connection errors will be logged as debug instead of error.
func (a *Agent) SetGracefulShutdown(ctx context.Context) error {
	a.logger.Debug("Setting graceful shutdown flag on proxy")
	return a.Proxy.SetGracefulShutdown(ctx)
}

// outgoingMockBufferBytes is the byte budget for the agent → CLI mock
// channel. This is the *design* unit — operators (and the comment
// block in pkg/agent/proxy/syncMock/syncMock.go's drop log) reason
// about memory, not slot count, and PR #4172's relay-layer cap
// (DefaultPerConnCap, also bytes) uses the same convention. At
// 64 MiB this matches PR #4172's per-connection budget, which keeps
// the two layers symmetric and easy to audit against the agent's
// cgroup limit.
//
// Implementation detail: a Go channel is slot-counted, so the
// runtime cap is derived as outgoingMockBufferBytes / nominalMockSizeBytes.
// nominalMockSizeBytes is an *estimate* of "what one mock typically
// costs in memory" — it is NOT a measured average, and the runtime
// neither tracks per-mock byte size nor enforces this value as a
// per-mock ceiling. It exists solely to translate the design-unit
// byte budget into the slot count Go channels actually need.
//
// 4 KiB was picked from the per-mock size distribution observed in
// production bundles (provider-engagement test EKS, 2026-05-04:
// median ~1 KiB, mean ~2.3 KiB, p95 ~10 KiB, max ~27 KiB). 4 KiB
// sits between mean and p95 — high enough that workloads with
// mostly-typical mocks fit comfortably under the byte budget, low
// enough that workloads skewed toward p95 still get a useful number
// of slots. A workload with consistently-larger mocks (e.g. mostly
// p95+ Postgres rows) will pin proportionally more memory at slot
// cap than the nominal 64 MiB; this is the same expected-vs-worst
// tradeoff every slot-counted Go channel makes.
//
// Bumped (effectively) from 100-slot to 64 MiB-equivalent after
// the same production bundles showed the old 100-slot cap silently
// dropping ≥2048 mocks per pod within minutes of recording start.
// At the bundles' MEAN per-mock size of ~2.3 KiB (computed as
// 1.0 MiB total / 450 mocks for the 8eafdd8e bundle, ~2.2 KiB
// for 452c57e7, ~2.5 KiB for c48b2e08), 100 slots held only
// ~230 KiB worst-case — less than 1/4 of the 1.0 MiB the
// *successful* part of those sessions ended up writing to disk.
// The mean is what matters here, not the median (~1 KiB), because
// the producer fills slots with whatever it produces — bigger
// individual mocks weigh more on the channel even at slot cap.
//
// Why "byte budget then derive" instead of "two independent caps
// (slots AND bytes) like PR #4172"? PR #4172 needs the two-cap shape
// because individual chunks at the relay layer can be up to 32 KiB
// each (forward buffer size) and a single connection might emit
// thousands of them — the slot cap protects against pathological
// chunk-count explosions even when bytes are fine. At the
// outChan layer downstream of the relay, mocks are already
// per-protocol-message granularity (one HTTP req/resp pair, one
// PG query, etc.) — count and bytes track each other much more
// tightly, so a single byte budget is sufficient and avoids the
// "two knobs, both must be set, can drift" maintenance burden.
//
// The drop path itself is unchanged and still intentional: it is
// far better to lose a mock and keep recording than to OOM-kill the
// agent and lose every mock captured in the run. We just stop hitting
// the cliff at trivial loads.
const (
	outgoingMockBufferBytes int64 = 64 * 1024 * 1024 // 64 MiB
	nominalMockSizeBytes        int64 = 4 * 1024         // 4 KiB
	outgoingMockChanCap           = int(outgoingMockBufferBytes / nominalMockSizeBytes)
)

func (a *Agent) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	m := make(chan *models.Mock, outgoingMockChanCap)

	err := a.Proxy.Record(ctx, m, opts)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (a *Agent) GetMapping(ctx context.Context) (<-chan models.TestMockMapping, error) {
	mappingCh := make(chan models.TestMockMapping, 100)
	a.Proxy.Mapping(ctx, mappingCh)

	return mappingCh, nil
}

func (a *Agent) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	a.logger.Debug("MockOutgoing function called", zap.Any("options", opts))

	err := a.Proxy.Mock(ctx, opts)
	if err != nil {
		return err
	}

	return nil
}

func (a *Agent) Hook(ctx context.Context, opts models.HookOptions) error {
	hookErr := errors.New("failed to hook into the app")

	parentErrGrp := ctx.Value(models.ErrGroupKey).(*errgroup.Group)

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// create a new error group for the hooks
	hookErrGrp, _ := errgroup.WithContext(ctx)
	hookCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the hookCtx to control the lifecycle of the hooks
	hookCtx, hookCtxCancel := context.WithCancel(hookCtx)
	hookCtx = context.WithValue(hookCtx, models.ErrGroupKey, hookErrGrp)

	// create a new error group for the proxy
	proxyErrGrp, _ := errgroup.WithContext(ctx)
	proxyCtx := context.WithoutCancel(ctx) //so that main context doesn't cancel the proxyCtx to control the lifecycle of the proxy
	proxyCtx, proxyCtxCancel := context.WithCancel(proxyCtx)
	proxyCtx = context.WithValue(proxyCtx, models.ErrGroupKey, proxyErrGrp)

	parentErrGrp.Go(func() error {
		<-ctx.Done()

		proxyCtxCancel()
		err := proxyErrGrp.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to stop the proxy")
		}

		hookCtxCancel()
		err = hookErrGrp.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to unload the hooks")
		}
		return nil
	})

	// load hooks if the mode changes ..
	hookCfg := coreAgent.HookCfg{
		Pid:      0,
		IsDocker: opts.IsDocker,
		Mode:     opts.Mode,
		Rules:    opts.Rules,
	}

	if coreAgent.EbpfProxyPortOverride != 0 {
		hookCfg.Port = coreAgent.EbpfProxyPortOverride
	}
	err := a.Hooks.Load(hookCtx, hookCfg, a.config.Agent)

	if err != nil {
		utils.LogError(a.logger, err, "failed to load hooks")
		return hookErr
	}

	if a.proxyStarted {
		a.logger.Debug("Proxy already started")
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	DNSIPv4, err := utils.GetContainerIPv4()
	if err != nil {
		utils.LogError(a.logger, err, "failed to get container IP")
		return hookErr
	}
	if coreAgent.ProxyHook != nil {
		a.Proxy.SetAuxiliaryHook(coreAgent.ProxyHook)
	}

	err = a.Proxy.StartProxy(proxyCtx, coreAgent.ProxyOptions{
		DNSIPv4Addr: DNSIPv4,
		//DnsIPv6Addr: ""
	})

	if err != nil {
		utils.LogError(a.logger, err, "failed to start proxy")
		// StartProxy may have spawned proxy goroutines before the
		// failure point (TCP accept loop, TCP DNS, UDP DNS) — they
		// stay live until proxyCtx is cancelled. Before this fix
		// they leaked until the outer agent context was cancelled,
		// holding ports and consuming resources. Cancel here so the
		// partial proxy is torn down immediately. Matters now that
		// StartProxy propagates auxiliary-hook failures (keploy#4078)
		// rather than swallowing them.
		proxyCtxCancel()
		return hookErr
	}

	a.proxyStarted = true
	return nil
}

func (a *Agent) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	return a.Proxy.GetConsumedMocks(ctx)
}

func (a *Agent) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	return a.Proxy.GetMockErrors(ctx)
}

// StoreMocks stores the filtered and unfiltered mocks for a client ID.
//
// Unification (Phase 1): every mock is run through DeriveLifetime on
// entry so TestModeInfo.Lifetime is populated before any matcher reads
// it. This is a second safety-net — the mockdb disk loader already
// derives at load time, but StoreMocks is also reachable from other
// paths (grpc instrumentation transport, in-memory tests, etc.) where
// no prior DeriveLifetime has run. Calling it again is idempotent
// thanks to the LifetimeDerived-bool short-circuit in DeriveLifetime
// (LifetimePerTest is the zero value of Lifetime, so the bool is the
// only reliable "already classified" signal), so re-deriving here
// never double-bumps the legacyKindFallbackFires counter or races
// with concurrent matchers.
//
// Caveat: because the stored slices carry pointer copies (not deep
// copies) of the caller's mocks, DeriveLifetime's write to
// TestModeInfo.Lifetime is visible through the caller's slice as
// well. This is the intended semantic — there's exactly ONE Mock
// object per name per session and every consumer of it (including
// the caller) benefits from the cached Lifetime. Do NOT introduce a
// deep copy here unless a concrete mutation-safety regression lands
// first.
func (a *Agent) StoreMocks(ctx context.Context, filtered []*models.Mock, unfiltered []*models.Mock) error {
	storage := &ClientMockStorage{
		filtered:   make([]*models.Mock, len(filtered)),
		unfiltered: make([]*models.Mock, len(unfiltered)),
	}

	// Shallow copy the slices — only the outer backing array is
	// duplicated, the *models.Mock pointers are shared with the
	// caller's slices. This is INTENTIONAL and load-bearing: matchers
	// look up mocks via pointer identity in MockManager's trees and
	// per-connID pools, and HitCount/Lifetime are bumped on the shared
	// Mock object so observability is consistent across the stack (see
	// the caveat block before this function for the full rationale).
	// Do NOT switch to a deep copy without coordinated updates at
	// every downstream site.
	copy(storage.filtered, filtered)
	copy(storage.unfiltered, unfiltered)

	// Derive Lifetime once per mock before they enter the runtime pool.
	// Idempotent: DeriveLifetime short-circuits when the Lifetime
	// field is already set (i.e. the disk loader has already run),
	// so re-deriving here is safe and never double-counts the
	// legacyKindFallbackFires telemetry.
	for _, m := range storage.filtered {
		if m != nil {
			m.DeriveLifetime()
		}
	}
	for _, m := range storage.unfiltered {
		if m != nil {
			m.DeriveLifetime()
		}
	}

	a.clientMocks.Store(uint64(0), storage)

	a.logger.Debug("Successfully stored mocks for client")
	return nil
}

// UpdateMockParams applies filtering parameters and updates the agent's mock manager
func (a *Agent) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {

	a.logger.Debug("UpdateMockParams called",
		zap.Time("afterTime", params.AfterTime),
		zap.Time("beforeTime", params.BeforeTime),
		zap.Bool("useMappingBased", params.UseMappingBased),
		zap.Int("mockMappingCount", len(params.MockMapping)),
		zap.Bool("strictMockWindow", params.StrictMockWindow))

	// Strict mock-window is OPT-IN via either the per-call flag
	// (params.StrictMockWindow) or the process-wide KEPLOY_STRICT_MOCK_WINDOW
	// env override. Surface the EFFECTIVE state so operators searching
	// agent logs for "strict mock window enabled" find hits regardless of
	// which route they used — previously the log fired only on the per-call
	// flag, making env-only opt-ins invisible.
	if strictEnabled := pkg.IsStrictMockWindow(params.StrictMockWindow); strictEnabled {
		// Fire the activation message at most once per agent process
		// (sync.Once). Info (project logging policy disallows Warn for
		// expected-default state); the escape-hatch names are embedded
		// inline so operators hitting unexpected "missing mock" errors
		// after an upgrade can opt out without digging through docs.
		// Per-test diagnostics drop to Debug.
		a.strictLogOnce.Do(func() {
			a.logger.Info(
				"strict mock-window containment is ACTIVE for this session — per-test mocks whose request "+
					"timestamp falls outside the outer test window will be dropped rather than promoted "+
					"across tests. If your replays start reporting missing mocks after an upgrade, this "+
					"is likely why. To opt out: set KEPLOY_STRICT_MOCK_WINDOW=0 in the environment, OR "+
					"test.strictMockWindow: false in keploy.yaml.",
				zap.Bool("viaPerCallFlag", params.StrictMockWindow),
				zap.Bool("viaEnvOverride", !params.StrictMockWindow && strictEnabled),
				zap.String("escape_hatch_env", "KEPLOY_STRICT_MOCK_WINDOW=0"),
				zap.String("escape_hatch_config", "test.strictMockWindow: false"))
		})
		a.logger.Debug("strict mock window active for test",
			zap.Time("windowStart", params.AfterTime),
			zap.Time("windowEnd", params.BeforeTime))
	}

	// Get stored mocks for the client
	storageInterface, exists := a.clientMocks.Load(uint64(0))
	if !exists {
		return fmt.Errorf("no mocks stored for client ID")
	}
	storage := storageInterface.(*ClientMockStorage)

	storage.mu.RLock()
	originalFiltered := make([]*models.Mock, len(storage.filtered))
	originalUnfiltered := make([]*models.Mock, len(storage.unfiltered))
	copy(originalFiltered, storage.filtered)
	copy(originalUnfiltered, storage.unfiltered)
	storage.mu.RUnlock()

	a.logger.Debug("Original mocks before filtering",
		zap.Int("originalFiltered", len(originalFiltered)),
		zap.Int("originalUnfiltered", len(originalUnfiltered)))

	var filteredMocks, unfilteredMocks []*models.Mock

	// When the proxy supports the windowed extension, MockManager's
	// SetMocksWithWindow applies the authoritative per-test-window
	// filter (including startup-init promotion for req < firstWindowStart
	// — bootstrap traffic like HikariCP pool warm-up that fires before
	// any test request). Pre-dropping out-of-window per-test mocks at
	// the agent under strict mode would hide those startup-init mocks
	// from MockManager and break replay of app-bootstrap DB calls under
	// strict. So: agent-level strict pre-filtering runs only on the
	// legacy SetMocks fallback path. For WindowedProxy callers we
	// pass strict=false to FilterPerTestAndLaxPromoted and let
	// MockManager.SetMocksWithWindow enforce strict semantics.
	_, isWindowedProxy := a.Proxy.(coreAgent.WindowedProxy)
	agentStrict := params.StrictMockWindow && !isWindowedProxy

	// Tier-aware strictMockWindow: when the proxy exposes the optional
	// FirstWindowStartReader extension, read the earliest test-window
	// start the MockManager has observed so filterByTimeStamp can keep
	// per-test mocks with req < firstWindowStart in the filtered slice
	// (they are startup-tier, not cross-test bleed). Stale previous-test
	// mocks (firstWindowStart <= req < currentStart, or req > currentEnd)
	// are still dropped — that's the containment guarantee strict mode
	// exists to provide. A zero return from the reader (no test has
	// fired yet) reverts to the legacy blanket-drop contract so callers
	// observe behaviour strictly no worse than before.
	var firstWindowStart time.Time
	if reader, ok := a.Proxy.(coreAgent.FirstWindowStartReader); ok {
		firstWindowStart = reader.FirstTestWindowStart()
	}

	// Apply filtering based on parameters
	if params.UseMappingBased && len(params.MockMapping) > 0 {
		filteredMocks = pkg.FilterTcsMocksMapping(ctx, a.logger, originalFiltered, params.MockMapping)
		unfilteredMocks = pkg.FilterConfigMocksMapping(ctx, a.logger, originalUnfiltered, params.MockMapping)
	} else {
		// Lax-mode promotion restoration: filterByTimeStamp moves
		// out-of-window non-config per-test mocks into its "unfiltered"
		// return slice so they remain reusable across tests (the
		// pre-Phase-2 kind-fallback used to do this implicitly by
		// over-promoting anything MySQL/Postgres/... to LifetimeSession
		// at DeriveLifetime). Round-5 correctly narrowed DeriveLifetime
		// but FilterTcsMocks was still discarding the promoted slice —
		// so shared-fixture mocks (SELECT * FROM pet used by every
		// test in spring-petclinic) were lost at the agent. We now
		// drain those promoted-to-session mocks out of the per-test
		// filter and merge them into the session pool passed to the
		// proxy. Under strict mode this branch doesn't fire because
		// filterByTimeStamp drops rather than promotes.
		perTestIn, promotedToSession := pkg.FilterPerTestAndLaxPromotedTierAware(ctx, a.logger, originalFiltered, params.AfterTime, params.BeforeTime, agentStrict, firstWindowStart)
		filteredMocks = perTestIn
		unfilteredMocks = pkg.FilterConfigMocksTierAware(ctx, a.logger, originalUnfiltered, params.AfterTime, params.BeforeTime, agentStrict, firstWindowStart)
		if len(promotedToSession) > 0 {
			unfilteredMocks = append(unfilteredMocks, promotedToSession...)
			// Ordering: both FilterConfigMocks and FilterPerTestAndLaxPromoted
			// already sort their outputs by ReqTimestampMock deterministically,
			// so the concatenated slice is stable across runs. We deliberately
			// DO NOT re-sort the merged pool here — the Mongo v2 matcher
			// relies on config-tagged session mocks preceding lax-promoted
			// per-test mocks in the unfiltered iteration order (config tier
			// wins handshake/ping ties before data-plane mocks get a chance
			// to steal the match and drop the driver connection mid-handshake).
			// Re-sorting by timestamp interleaves them and causes the
			// Mongo fuzzer cross-version replay to hang.
			a.logger.Debug("lax-promoted per-test mocks into session pool for cross-test reuse",
				zap.Int("count", len(promotedToSession)))
		}
	}

	// Count IsFiltered distribution for debugging
	var filteredCount, unfilteredCount int
	for _, m := range unfilteredMocks {
		if m.TestModeInfo.IsFiltered {
			filteredCount++
		} else {
			unfilteredCount++
		}
	}
	a.logger.Debug("After filtering",
		zap.Int("filteredMocks", len(filteredMocks)),
		zap.Int("unfilteredMocks", len(unfilteredMocks)),
		zap.Int("unfilteredWithIsFilteredTrue", filteredCount),
		zap.Int("unfilteredWithIsFilteredFalse", unfilteredCount))

	// Filter out deleted mocks if totalConsumedMocks is provided
	if params.TotalConsumedMocks != nil {
		filteredMocks = a.filterOutDeleted(filteredMocks, params.TotalConsumedMocks)
	}

	// Atomically update mocks AND the active test window when the proxy
	// supports the WindowedProxy extension. Otherwise fall back to the
	// stable SetMocks contract — third-party proxies without window
	// support keep working in legacy lax mode.
	if wp, ok := a.Proxy.(coreAgent.WindowedProxy); ok {
		if err := wp.SetMocksWithWindow(ctx, filteredMocks, unfilteredMocks, params.AfterTime, params.BeforeTime); err != nil {
			utils.LogError(a.logger, err, "failed to set mocks and test window on proxy; verify the proxy implements WindowedProxy and the agent/proxy versions are in sync, or retry via the plain SetMocks fallback path")
			return err
		}
	} else {
		if err := a.Proxy.SetMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
			utils.LogError(a.logger, err, "failed to set mocks on proxy")
			return err
		}
	}

	return nil
}

// filterOutDeleted filters out deleted mocks based on totalConsumedMocks
func (a *Agent) filterOutDeleted(mocks []*models.Mock, totalConsumedMocks map[string]models.MockState) []*models.Mock {
	filtered := make([]*models.Mock, 0, len(mocks))
	for _, m := range mocks {
		// treat empty/missing names as never consumed
		if m == nil || m.Name == "" {
			filtered = append(filtered, m)
			continue
		}
		// we are picking mocks that are not consumed till now (not present in map),
		// and, mocks that are updated.
		if k, ok := totalConsumedMocks[m.Name]; !ok || k.Usage != models.Deleted {
			if ok {
				m.TestModeInfo.IsFiltered = k.IsFiltered
				m.TestModeInfo.SortOrder = k.SortOrder
			}
			filtered = append(filtered, m)
		}
	}
	return filtered
}
