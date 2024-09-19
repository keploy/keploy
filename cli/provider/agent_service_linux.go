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
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/pkg/service/agent"

	"go.uber.org/zap"
)

type CommonInternalServices struct {
	commonPlatformServices
	Instrumentation *agent.Agent
}

func GetAgent(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry, auth service.Auth) (interface{}, error) {
	
	commonServices, err := GetAgentService(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}

	switch cmd {
	case "agent":
		return agent.New(logger, commonServices.Instrumentation.Hooks, commonServices.Instrumentation.Proxy, commonServices.Instrumentation.Tester, nil), nil
	default:
		return nil, errors.New("invalid command")
	}

}

func GetAgentService(_ context.Context, c *config.Config, logger *zap.Logger) (*CommonInternalServices, error) {

	h := hooks.NewHooks(logger, c)
	p := proxy.New(logger, h, c)
	//for keploy test bench
	t := tester.New(logger, h)

	var client docker.Client
	// var err error
	// fixed port for docker - 26789
	// fixed port for native - 16789

	instrumentation := agent.New(logger, h, p, t, client)

	storage := storage.New(c.APIServerURL, logger)
	return &CommonInternalServices{
		commonPlatformServices{
			Storage: storage,
		},
		instrumentation,
	}, nil
}
