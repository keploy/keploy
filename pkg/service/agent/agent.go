//go:build linux

// Package agent contains methods for setting up hooks and proxy along with registering keploy clients.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/models"
	kdocker "go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
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
	agent.Tester                // embedding the Tester interface to transfer the tester methods to the core object
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	agent.IncomingProxy
	proxyStarted bool
	// activeClients sync.Map
	// New field for storing client-specific mocks
	clientMocks sync.Map // map[uint64]*ClientMockStorage
	Ip          string
}

func New(logger *zap.Logger, hook agent.Hooks, proxy agent.Proxy, tester agent.Tester, client kdocker.Client, ip agent.IncomingProxy) *Agent {
	return &Agent{
		logger:        logger,
		Hooks:         hook,
		Proxy:         proxy,
		IncomingProxy: ip,
		Tester:        tester,
		dockerClient:  client,
	}
}

// Setup will create a new app and store it in the map, all the setup will be done here
func (a *Agent) Setup(ctx context.Context, opts models.SetupOptions, startCh chan struct{}) error {
	a.logger.Info("Starting the agent in ", zap.String("mode", string(opts.Mode)))
	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	err := a.Hook(ctx, 0, models.HookOptions{
		Mode:          opts.Mode,
		IsDocker:      opts.IsDocker,
		EnableTesting: opts.EnableTesting,
	}, opts)
	if err != nil {
		a.logger.Error("failed to hook into the app", zap.Error(err))
		return err
	}

	startCh <- struct{}{}

	<-ctx.Done()
	errGrp.Wait()
	a.logger.Info("Context cancelled, stopping the agent")
	return context.Canceled

}

// func (a *Agent) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) error {
// 	return nil
// }

func (a *Agent) StartIncomingProxy(ctx context.Context, opts models.IncomingOptions) (chan *models.TestCase, error) {
	tc := a.IncomingProxy.Start(ctx, opts)
	a.logger.Debug("Ingress proxy manager started and is listening for bind events.")
	return tc, nil
}

func (a *Agent) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	m := make(chan *models.Mock, 500)

	err := a.Proxy.Record(ctx, id, m, opts)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (a *Agent) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	a.logger.Debug("Inside MockOutgoing of agent binary !!")

	err := a.Proxy.Mock(ctx, id, opts)
	if err != nil {
		return err
	}

	return nil
}

func (a *Agent) Hook(ctx context.Context, id uint64, opts models.HookOptions, setupOpts models.SetupOptions) error {
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
	err := a.Hooks.Load(hookCtx, id, agent.HookCfg{
		ClientID:   id,
		Pid:        0,
		IsDocker:   opts.IsDocker,
		KeployIPV4: "172.18.0.2",
		Mode:       opts.Mode,
	}, setupOpts)

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

	err = a.Proxy.StartProxy(proxyCtx, agent.ProxyOptions{
		DNSIPv4Addr: "172.18.0.2",
		//DnsIPv6Addr: ""
	})

	if err != nil {
		utils.LogError(a.logger, err, "failed to start proxy")
		return hookErr
	}

	a.proxyStarted = true
	// if opts.EnableTesting {
	// 	// Setting up the test bench
	// 	err := a.Tester.Setup(ctx, models.TestingOptions{Mode: opts.Mode})
	// 	if err != nil {
	// 		utils.LogError(a.logger, err, "error while setting up the test bench environment")
	// 		return errors.New("failed to setup the test bench")
	// 	}
	// }

	return nil
}

func (a *Agent) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	a.logger.Debug("Inside SetMocks of agent binary !!")
	return a.Proxy.SetMocks(ctx, id, filtered, unFiltered)
}

func (a *Agent) GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error) {
	return a.Proxy.GetConsumedMocks(ctx, id)
}

// StoreMocks stores the filtered and unfiltered mocks for a client ID
func (a *Agent) StoreMocks(ctx context.Context, id uint64, filtered []*models.Mock, unfiltered []*models.Mock) error {
	storage := &ClientMockStorage{
		filtered:   make([]*models.Mock, len(filtered)),
		unfiltered: make([]*models.Mock, len(unfiltered)),
	}

	// Deep copy the mocks to avoid data races
	copy(storage.filtered, filtered)
	copy(storage.unfiltered, unfiltered)

	a.clientMocks.Store(id, storage)

	a.logger.Info("Successfully stored mocks for client", zap.Uint64("clientID", id))
	return nil
}

// UpdateMockParams applies filtering parameters and updates the agent's mock manager
func (a *Agent) UpdateMockParams(ctx context.Context, id uint64, params models.MockFilterParams) error {

	// Get stored mocks for the
	storageInterface, exists := a.clientMocks.Load(id)
	if !exists {
		return fmt.Errorf("no mocks stored for client ID %d", id)
	}

	storage := storageInterface.(*ClientMockStorage)
	storage.mu.RLock()
	originalFiltered := make([]*models.Mock, len(storage.filtered))
	originalUnfiltered := make([]*models.Mock, len(storage.unfiltered))
	copy(originalFiltered, storage.filtered)
	copy(originalUnfiltered, storage.unfiltered)
	storage.mu.RUnlock()

	var filteredMocks, unfilteredMocks []*models.Mock

	// Apply filtering based on parameters
	if params.UseMappingBased && len(params.MockMapping) > 0 {
		filteredMocks = pkg.FilterTcsMocksMapping(ctx, a.logger, originalFiltered, params.MockMapping)
		unfilteredMocks = pkg.FilterConfigMocksMapping(ctx, a.logger, originalUnfiltered, params.MockMapping)
	} else {
		filteredMocks = pkg.FilterTcsMocks(ctx, a.logger, originalFiltered, params.AfterTime, params.BeforeTime)
		unfilteredMocks = pkg.FilterConfigMocks(ctx, a.logger, originalUnfiltered, params.AfterTime, params.BeforeTime)
	}

	// Filter out deleted mocks if totalConsumedMocks is provided
	if params.TotalConsumedMocks != nil {
		filteredMocks = a.filterOutDeleted(filteredMocks, params.TotalConsumedMocks)
		unfilteredMocks = a.filterOutDeleted(unfilteredMocks, params.TotalConsumedMocks)
	}

	// Set the filtered mocks to the proxy
	err := a.Proxy.SetMocks(ctx, id, filteredMocks, unfilteredMocks)
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
