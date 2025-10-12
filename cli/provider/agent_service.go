package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent/hooks"
	"go.keploy.io/server/v2/pkg/agent/proxy"
	incoming "go.keploy.io/server/v2/pkg/agent/proxy/incoming"

	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/pkg/platform/storage"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/pkg/service/agent"
	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"
)

type CommonInternalServices struct {
	commonPlatformServices
	Instrumentation *agent.Agent
}

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

	// t := tester.New(logger, h)
	instrumentation := agent.New(logger, h, p, nil, client, ip, cfg)
	storage := storage.New(cfg.APIServerURL, logger)

	commonServices := &CommonInternalServices{
		commonPlatformServices{
			Storage: storage,
		},
		instrumentation,
	}

	switch cmd {
	case "agent":
		return agent.New(logger, commonServices.Instrumentation.Hooks, commonServices.Instrumentation.Proxy, commonServices.Instrumentation.Tester, client, commonServices.Instrumentation.IncomingProxy, cfg), nil
	default:
		return nil, errors.New("invalid command")
	}
}
