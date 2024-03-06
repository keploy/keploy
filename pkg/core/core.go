package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Core struct {
	logger       *zap.Logger
	id           utils.AutoInc
	apps         sync.Map
	hook         Hooks
	proxy        Proxy
	proxyStarted bool
}

func New(logger *zap.Logger, hook Hooks, proxy Proxy) *Core {
	return &Core{
		logger: logger,
		hook:   hook,
		proxy:  proxy,
	}
}

func (c *Core) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	// create a new app and store it in the map
	id := uint64(c.id.Next())
	a := app.NewApp(c.logger, id, cmd)
	c.apps.Store(id, a)

	err := a.Setup(ctx, app.Options{
		DockerNetwork: opts.DockerNetwork,
	})
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
	if appKind == utils.Docker || appKind == utils.DockerCompose {
		isDocker = true
	}

	select {
	case <-ctx.Done():
		println("context cancelled before loading hooks")
		return ctx.Err()
	default:
	}

	// TODO: ensure right values are passed to the hook
	//load hooks
	err = c.hook.Load(ctx, id, HookCfg{
		AppID:      id,
		Pid:        0,
		IsDocker:   isDocker,
		KeployIPV4: a.KeployIPv4Addr(),
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
		println("context cancelled before starting proxy")
		return ctx.Err()
	default:
	}

	// TODO: Hooks can be loaded multiple times but proxy should be started only once
	// if there is another containerized app, then we need to pass new (ip:port) of proxy to the eBPF
	// as the network namespace is different for each container and so is the keploy/proxy IP to communicate with the app.
	//start proxy
	err = c.proxy.StartProxy(ctx, ProxyOptions{
		DnsIPv4Addr: a.KeployIPv4Addr(),
		//DnsIPv6Addr: ""
	})
	if err != nil {
		utils.LogError(c.logger, err, "failed to start proxy")
		return hookErr
	}

	c.proxyStarted = true
	return nil
}

func (c *Core) Run(ctx context.Context, id uint64, opts models.RunOptions) models.AppError {
	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	}

	//send inode to the hook
	inodeChan := make(chan uint64)
	go func(inodeChan chan uint64) {
		defer utils.Recover(c.logger)
		defer close(inodeChan)

		inode := <-inodeChan
		err := c.hook.SendInode(ctx, id, inode)
		if err != nil {
			utils.LogError(c.logger, err, "failed to send inode")
		}
	}(inodeChan)

	return a.Run(ctx, inodeChan, app.Options{DockerDelay: opts.DockerDelay})
}

func (c *Core) GetAppIp(ctx context.Context, id uint64) (string, error) {

	a, err := c.getApp(id)
	if err != nil {
		utils.LogError(c.logger, err, "failed to get app")
		return "", err
	}

	return a.ContainerIPv4Addr(), nil
}
