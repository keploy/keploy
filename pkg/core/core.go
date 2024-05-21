// Package core provides functionality for managing core functionalities in Keploy.
package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Core struct {
	Proxy        // embedding the Proxy interface to transfer the proxy methods to the core object
	Hooks        // embedding the Hooks interface to transfer the hooks methods to the core object
	Tester       // embedding the Tester interface to transfer the tester methods to the core object
	logger       *zap.Logger
	id           utils.AutoInc
	apps         sync.Map
	proxyStarted bool
}

func New(logger *zap.Logger, hook Hooks, proxy Proxy, tester Tester) *Core {
	return &Core{
		logger: logger,
		Hooks:  hook,
		Proxy:  proxy,
		Tester: tester,
	}
}

func (c *Core) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	// create a new app and store it in the map
	id := uint64(c.id.Next())
	a := app.NewApp(c.logger, id, cmd, app.Options{
		DockerNetwork: opts.DockerNetwork,
		Container:     opts.Container,
		DockerDelay:   opts.DockerDelay,
	})
	c.apps.Store(id, a)

	err := a.Setup(ctx)
	if err != nil {
		utils.LogError(c.logger, err, "failed to setup app")
		return 0, err
	}
	return id, nil
}

func (c *Core) getApp(id uint64) (*app.App, error) {
	a, ok := c.apps.Load(id)
	if !ok {
		return nil, fmt.Errorf("app with id:%v not found", id)
	}

	// type assertion on the app
	h, ok := a.(*app.App)
	if !ok {
		return nil, fmt.Errorf("failed to type assert app with id:%v", id)
	}

	return h, nil
}

func (c *Core) Hook(ctx context.Context, id uint64, opts models.HookOptions) error {
	hookErr := errors.New("failed to hook into the app")

	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return hookErr
	}

	isDocker := false
	appKind := a.Kind(ctx)
	//check if the app is docker/docker-compose or native
	if utils.IsDockerKind(appKind) {
		isDocker = true
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
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

	g.Go(func() error {
		<-ctx.Done()

		proxyCtxCancel()
		err = proxyErrGrp.Wait()
		if err != nil {
			utils.LogError(c.logger, err, "failed to stop the proxy")
		}

		hookCtxCancel()
		err := hookErrGrp.Wait()
		if err != nil {
			utils.LogError(c.logger, err, "failed to unload the hooks")
		}

		return nil
	})

	//load hooks
	err = c.Hooks.Load(hookCtx, id, HookCfg{
		AppID:      id,
		Pid:        0,
		IsDocker:   isDocker,
		KeployIPV4: a.KeployIPv4Addr(),
		Mode:       opts.Mode,
	})
	if err != nil {
		utils.LogError(c.logger, err, "failed to load hooks")
		return hookErr
	}

	if c.proxyStarted {
		c.logger.Debug("Proxy already started")
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// TODO: Hooks can be loaded multiple times but proxy should be started only once
	// if there is another containerized app, then we need to pass new (ip:port) of proxy to the eBPF
	// as the network namespace is different for each container and so is the keploy/proxy IP to communicate with the app.
	// start proxy
	err = c.Proxy.StartProxy(proxyCtx, ProxyOptions{
		DNSIPv4Addr: a.KeployIPv4Addr(),
		//DnsIPv6Addr: ""
	})
	if err != nil {
		utils.LogError(c.logger, err, "failed to start proxy")
		return hookErr
	}

	c.proxyStarted = true

	// For keploy test bench
	if opts.EnableTesting {

		// enable testing in the app
		a.EnableTesting = true
		a.Mode = opts.Mode

		// Setting up the test bench
		err := c.Tester.Setup(ctx, models.TestingOptions{Mode: opts.Mode})
		if err != nil {
			utils.LogError(c.logger, err, "error while setting up the test bench environment")
			return errors.New("failed to setup the test bench")
		}
	}

	return nil
}

func (c *Core) Run(ctx context.Context, id uint64, _ models.RunOptions) models.AppError {
	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}

	runAppErrGrp, runAppCtx := errgroup.WithContext(ctx)

	inodeErrCh := make(chan error, 1)
	appErrCh := make(chan models.AppError, 1)
	inodeChan := make(chan uint64, 1) //send inode to the hook

	defer func() {
		err := runAppErrGrp.Wait()
		defer close(inodeErrCh)
		defer close(inodeChan)
		if err != nil {
			utils.LogError(c.logger, err, "failed to stop the app")
		}
	}()

	runAppErrGrp.Go(func() error {
		defer utils.Recover(c.logger)
		if a.Kind(ctx) == utils.Native {
			return nil
		}
		select {
		case inode := <-inodeChan:
			err := c.Hooks.SendInode(ctx, id, inode)
			if err != nil {
				utils.LogError(c.logger, err, "")

				inodeErrCh <- errors.New("failed to send inode to the kernel")
			}
		case <-ctx.Done():
			return nil
		}
		return nil
	})

	runAppErrGrp.Go(func() error {
		defer utils.Recover(c.logger)
		defer close(appErrCh)
		appErr := a.Run(runAppCtx, inodeChan)
		if appErr.Err != nil {
			utils.LogError(c.logger, appErr.Err, "error while running the app")
			appErrCh <- appErr
		}
		return nil
	})

	select {
	case <-runAppCtx.Done():
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: nil}
	case appErr := <-appErrCh:
		return appErr
	case inodeErr := <-inodeErrCh:
		return models.AppError{AppErrorType: models.ErrInternal, Err: inodeErr}
	}
}

func (c *Core) GetContainerIP(_ context.Context, id uint64) (string, error) {

	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return "", err
	}

	ip := a.ContainerIPv4Addr()
	if ip == "" {
		return "", fmt.Errorf("failed to get the IP address of the app container. Try increasing --delay (in seconds)")
	}

	return ip, nil
}
