package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/service"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"

	"go.uber.org/zap"
)

func GetAgent(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, _ service.Auth) (interface{}, error) {

	var client docker.Client
	var err error
	if cfg.InDocker {
		client, err = docker.New(logger, cfg)
		if err != nil {
			utils.LogError(logger, err, "failed to create docker client")
		}
	}

	h := hooks.New(logger, cfg)
	p := proxy.New(logger, h, cfg)
	ip := incoming.New(logger, h)

	instrumentation := agent.New(logger, h, p, client, ip, cfg)

	switch cmd {
	case "agent":
		return agent.New(logger, instrumentation.Hooks, instrumentation.Proxy, client, instrumentation.IncomingProxy, cfg), nil
	default:
		return nil, errors.New("invalid command")
	}
}
