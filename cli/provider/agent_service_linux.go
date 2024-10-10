//go:build linux

package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/core/tester"
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

func GetAgent(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, auth service.Auth) (interface{}, error) {

	var client docker.Client
	var err error
	if cfg.InDocker {
		client, err = docker.New(logger)
		if err != nil {
			utils.LogError(logger, err, "failed to create docker client")
		}
	}

	commonServices, err := GetAgentService(ctx, cfg, client, logger)
	if err != nil {
		return nil, err
	}

	switch cmd {
	case "agent":
		return agent.New(logger, commonServices.Instrumentation.Hooks, commonServices.Instrumentation.Proxy, commonServices.Instrumentation.Tester, client), nil
	default:
		return nil, errors.New("invalid command")
	}

}

func GetAgentService(_ context.Context, c *config.Config, client docker.Client, logger *zap.Logger) (*CommonInternalServices, error) {

	h := hooks.NewHooks(logger, c)
	p := proxy.New(logger, h, c)
	//for keploy test bench
	t := tester.New(logger, h)

	instrumentation := agent.New(logger, h, p, t, client)

	storage := storage.New(c.APIServerURL, logger)
	return &CommonInternalServices{
		commonPlatformServices{
			Storage: storage,
		},
		instrumentation,
	}, nil
}
