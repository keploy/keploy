package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"

	app "go.keploy.io/server/v2/pkg/core/app"
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

func (c *Core) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (int, error) {
	id := c.id.Next()
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

func (c *Core) getApp(id int) (App, error) {
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

func (c *Core) Hook(ctx context.Context, id int, opts models.HookOptions) error {
	a, err := c.getApp(id)
	if err != nil {
		return err
	}

	opts.KeployIPv4 = a.KeployIPv4Addr()
	c.hook.Load(ctx, id, opts)
}

func (c *Core) Run(ctx context.Context, id int, opts models.RunOptions) error {
	a, err := c.getApp(id)
	if err != nil {
		return err
	}
	switch a.Kind(ctx) {
	case utils.Docker, utils.DockerCompose:
		// process ebpf hooks
	case utils.Native:
		// process native hooks
	}

	return a.Run(ctx)
	// TODO: send the docker inode to the hook
}

// IPv4ToUint32 converts a string representation of an IPv4 address to a 32-bit integer.
func IPv4ToUint32(ipStr string) (uint32, error) {
	ipAddr := net.ParseIP(ipStr)
	if ipAddr != nil {
		ipAddr = ipAddr.To4()
		if ipAddr != nil {
			return binary.BigEndian.Uint32(ipAddr), nil
		} else {
			return 0, errors.New("not a valid IPv4 address")
		}
	} else {
		return 0, errors.New("failed to parse IP address")
	}
}
