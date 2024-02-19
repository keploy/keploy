package core

import (
	"context"
	"fmt"
	"sync"

	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Core struct {
	logger *zap.Logger
	id     utils.AutoInc
	apps   sync.Map
	hook   Hooks
}

func (c *Core) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	id := uint64(c.id.Next())
	a := app.NewApp(c.logger, id, cmd)
	err := a.Setup(ctx, AppOptions{
		DockerNetwork: opts.DockerNetwork,
	})
	if err != nil {
		c.logger.Error("Failed to create app", zap.Error(err))
		return 0, err
	}

	if a.KeployIPv4Addr() == "" {
		// TODO implement me
	}
	return id, nil
}

func (c *Core) getApp(id uint64) (App, error) {
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
	a, err := c.getApp(id)
	if err != nil {
		return err
	}

	opts.KeployIPv4 = a.KeployIPv4Addr()
	// TODO: ensure right values are passed to the hook
	return c.hook.Load(ctx, id, HookOptions{
		AppID:      id,
		Pid:        0,
		IsDocker:   false,
		KeployIPV4: "",
	})
}

func (c *Core) Run(ctx context.Context, id uint64, opts models.RunOptions) error {
	a, err := c.getApp(id)
	if err != nil {
		return err
	}
	// TODO: send the docker inode to the hook
	switch a.Kind(ctx) {
	case utils.Docker, utils.DockerCompose:
		// process ebpf hooks
	case utils.Native:
		// process native hooks
	}

	return a.Run(ctx)

}
