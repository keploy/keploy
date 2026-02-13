// Package agent contains methods for setting up hooks and proxy along with registering keploy clients.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent"
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
	logger       *zap.Logger
	agent.Proxy                 // embedding the Proxy interface to transfer the proxy methods to the core object
	agent.Hooks                 // embedding the Hooks interface to transfer the hooks methods to the core object
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	agent.IncomingProxy
	proxyStarted bool
	config       *config.Config
	// activeClients sync.Map
	// New field for storing client-specific mocks
	clientMocks sync.Map // map[uint64]*ClientMockStorage
	Ip          string
}

func New(logger *zap.Logger, hook agent.Hooks, proxy agent.Proxy, client kdocker.Client, ip agent.IncomingProxy, config *config.Config) *Agent {
	return &Agent{
		logger:        logger,
		Hooks:         hook,
		Proxy:         proxy,
		IncomingProxy: ip,
		dockerClient:  client,
		config:        config,
	}
}

// Setup will create a new app and store it in the map, all the setup will be done here
func (a *Agent) Setup(ctx context.Context, startCh chan int) error {

	a.logger.Info("Starting the agent in ", zap.String("mode", string(a.config.Agent.Mode)))
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

func (a *Agent) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	m := make(chan *models.Mock, 1000)

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
	err := a.Hooks.Load(hookCtx, agent.HookCfg{
		Pid:      0,
		IsDocker: opts.IsDocker,
		Mode:     opts.Mode,
		Rules:    opts.Rules,
	}, a.config.Agent)

	if err != nil {
		utils.LogError(a.logger, err, "failed to load hooks")
		return hookErr
	}

	if a.proxyStarted {
		a.logger.Info("Proxy already started")
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
	err = a.Proxy.StartProxy(proxyCtx, agent.ProxyOptions{
		DNSIPv4Addr: DNSIPv4,
		//DnsIPv6Addr: ""
	})

	if err != nil {
		utils.LogError(a.logger, err, "failed to start proxy")
		return hookErr
	}

	a.proxyStarted = true
	return nil
}

func (a *Agent) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	return a.Proxy.GetConsumedMocks(ctx)
}

// StoreMocks stores the filtered and unfiltered mocks for a client ID
func (a *Agent) StoreMocks(ctx context.Context, filtered []*models.Mock, unfiltered []*models.Mock) error {
	storage := &ClientMockStorage{
		filtered:   make([]*models.Mock, len(filtered)),
		unfiltered: make([]*models.Mock, len(unfiltered)),
	}

	// Deep copy the mocks to avoid data races
	copy(storage.filtered, filtered)
	copy(storage.unfiltered, unfiltered)

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
		zap.Int("mockMappingCount", len(params.MockMapping)))

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

	// Apply filtering based on parameters
	if params.UseMappingBased && len(params.MockMapping) > 0 {
		filteredMocks = pkg.FilterTcsMocksMapping(ctx, a.logger, originalFiltered, params.MockMapping)
		unfilteredMocks = pkg.FilterConfigMocksMapping(ctx, a.logger, originalUnfiltered, params.MockMapping)
	} else {
		filteredMocks = pkg.FilterTcsMocks(ctx, a.logger, originalFiltered, params.AfterTime, params.BeforeTime)
		unfilteredMocks = pkg.FilterConfigMocks(ctx, a.logger, originalUnfiltered, params.AfterTime, params.BeforeTime)
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

	// Set the filtered mocks to the proxy
	err := a.Proxy.SetMocks(ctx, filteredMocks, unfilteredMocks)
	if err != nil {
		utils.LogError(a.logger, err, "failed to set mocks on proxy")
		return err
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
