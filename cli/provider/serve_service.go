package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/service/serve"
	"go.uber.org/zap"
)

func GetServe(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger) (interface{}, error) {
	switch cmd {
	case "serve":
		serveCfg := *cfg
		if serveCfg.ProxyPort == 0 {
			serveCfg.ProxyPort = config.DefaultServeProxyPort
		}
		serveCfg.DNSPort = 0 // Disable DNS interception - users connect directly to proxy port

		h := hooks.New(logger, &serveCfg)
		p := proxy.New(logger, h, &serveCfg)
		return serve.New(logger, &serveCfg, p), nil
	default:
		return nil, errors.New("invalid command")
	}
}
