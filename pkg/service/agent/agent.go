//go:build linux

// Package agent contains methods for setting up hooks and proxy along with registering keploy clients.
package agent

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/agent/hooks"
	"go.keploy.io/server/v2/pkg/agent/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	kdocker "go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Agent struct {
	logger       *zap.Logger
	agent.Proxy                 // embedding the Proxy interface to transfer the proxy methods to the core object
	agent.Hooks                 // embedding the Hooks interface to transfer the hooks methods to the core object
	agent.Tester                // embedding the Tester interface to transfer the tester methods to the core object
	dockerClient kdocker.Client //embedding the docker client to transfer the docker client methods to the core object
	proxyStarted bool
}

func New(logger *zap.Logger, hook agent.Hooks, proxy agent.Proxy, tester agent.Tester, client kdocker.Client) *Agent {
	return &Agent{
		logger:       logger,
		Hooks:        hook,
		Proxy:        proxy,
		Tester:       tester,
		dockerClient: client,
	}
}

// Setup will create a new app and store it in the map, all the setup will be done here
func (a *Agent) Setup(ctx context.Context, _ string, opts models.SetupOptions) error {

	a.logger.Info("Starting the agent in ", zap.String(string(opts.Mode), "mode"))
	err := a.Hook(ctx, 0, models.HookOptions{Mode: opts.Mode, IsDocker: opts.IsDocker})
	if err != nil {
		a.logger.Error("failed to hook into the app", zap.Error(err))
	}

	<-ctx.Done()
	a.logger.Info("Context cancelled, stopping the agent")
	return context.Canceled

}

func (a *Agent) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	return a.Hooks.Record(ctx, id, opts)
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

func (a *Agent) Hook(ctx context.Context, id uint64, opts models.HookOptions) error {
	hookErr := errors.New("failed to hook into the app")

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

	hookErrGrp.Go(func() error {
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
	})

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
		fmt.Println("Hooks context cancelled, stopping Hook")
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

	// For keploy test bench
	// Doubt: if this is enabled automatically
	fmt.Println("opts.EnableTesting", opts.EnableTesting)
	if opts.EnableTesting {

		// enable testing in the app
		// a.EnableTesting = true
		// a.Mode = opts.Mode

		// Setting up the test bench
		err := a.Tester.Setup(ctx, models.TestingOptions{Mode: opts.Mode})
		if err != nil {
			utils.LogError(a.logger, err, "error while setting up the test bench environment")
			return errors.New("failed to setup the test bench")
		}
	}

	return nil
}

func (a *Agent) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	a.logger.Debug("Inside SetMocks of agent binary !!")
	return a.Proxy.SetMocks(ctx, id, filtered, unFiltered)
}

func (a *Agent) GetConsumedMocks(ctx context.Context, id uint64) ([]string, error) {
	return a.Proxy.GetConsumedMocks(ctx, id)
}

func (a *Agent) DeRegisterClient(ctx context.Context, id uint64) error {
	fmt.Println("Inside DeRegisterClient of agent binary !!")
	err := a.Proxy.MakeClientDeRegisterd(ctx)
	if err != nil {
		return err
	}
	err = a.Hooks.DeleteKeployClientInfo(id)
	if err != nil {
		return err
	}

	return nil
}

func (a *Agent) RegisterClient(ctx context.Context, opts models.SetupOptions) error {

	a.logger.Info("Registering the client with the keploy server")
	// send the network info to the kernel
	err := a.SendNetworkInfo(ctx, opts)
	if err != nil {
		a.logger.Error("failed to send network info to the kernel", zap.Error(err))
		return err
	}

	clientInfo := structs.ClientInfo{
		KeployClientNsPid: opts.ClientNsPid,
		IsDockerApp:       0,
		KeployClientInode: opts.ClientInode,
		AppInode:          opts.AppInode,
	}

	switch opts.Mode {
	case models.MODE_RECORD:
		clientInfo.Mode = uint32(1)
	case models.MODE_TEST:
		clientInfo.Mode = uint32(2)
	default:
		clientInfo.Mode = uint32(0)
	}

	if opts.IsDocker {
		clientInfo.IsDockerApp = 1
	}

	return a.Hooks.SendKeployClientInfo(opts.ClientID, clientInfo)
}

func (a *Agent) SendNetworkInfo(ctx context.Context, opts models.SetupOptions) error {
	if !opts.IsDocker {
		proxyIP, err := hooks.IPv4ToUint32("127.0.0.1")
		if err != nil {
			return err
		}
		proxyInfo := structs.ProxyInfo{
			IP4:  proxyIP,
			IP6:  [4]uint32{0, 0, 0, 0},
			Port: 16789,
		}
		err = a.Hooks.SendClientProxyInfo(opts.ClientID, proxyInfo)
		if err != nil {
			return err
		}
		return nil
	}

	inspect, err := a.dockerClient.ContainerInspect(ctx, "keploy-v2")
	if err != nil {
		utils.LogError(a.logger, nil, fmt.Sprintf("failed to get inspect keploy container:%v", inspect))
		return err
	}

	keployNetworks := inspect.NetworkSettings.Networks
	var keployIPv4 string
	for n, settings := range keployNetworks {
		if n == opts.DockerNetwork {
			keployIPv4 = settings.IPAddress //keploy container IP
			break
		}
	}

	ipv4, err := hooks.IPv4ToUint32(keployIPv4)
	if err != nil {
		return err
	}

	var ipv6 [4]uint32
	if opts.IsDocker {
		ipv6, err := hooks.ToIPv4MappedIPv6(keployIPv4)
		if err != nil {
			return fmt.Errorf("failed to convert ipv4:%v to ipv4 mapped ipv6 in docker env:%v", ipv4, err)
		}
		a.logger.Debug(fmt.Sprintf("IPv4-mapped IPv6 for %s is: %08x:%08x:%08x:%08x\n", keployIPv4, ipv6[0], ipv6[1], ipv6[2], ipv6[3]))

	}

	proxyInfo := structs.ProxyInfo{
		IP4:  ipv4,
		IP6:  ipv6,
		Port: 36789,
	}

	err = a.Hooks.SendClientProxyInfo(opts.ClientID, proxyInfo)
	if err != nil {
		return err
	}
	return nil
}
