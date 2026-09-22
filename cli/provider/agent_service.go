package provider

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"

	"go.uber.org/zap"
)

func GetAgent(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger) (interface{}, error) {

	var client docker.Client
	var err error
	if cfg.InDocker {
		client, err = docker.New(logger, cfg)
		if err != nil {
			utils.LogError(logger, err, "failed to create docker client")
		}
	}

	if cmd != "agent" {
		return nil, errors.New("invalid command")
	}

	// A downstream build may supply a userspace datapath (see
	// coreAgent.HooksFactory). Checked first so the kernel-backed hooks are not
	// constructed and thrown away on the very path the override exists to serve.
	var h coreAgent.Hooks
	if coreAgent.HooksFactory != nil {
		h = coreAgent.HooksFactory(logger, cfg)
		// Every documented failure mode of an override is silent — zero test
		// cases, or every outgoing connection closed. Without this line triage
		// starts from "eBPF is broken" rather than "the override never resolves".
		logger.Info("using an overridden (non-eBPF) hooks implementation",
			zap.String("impl", fmt.Sprintf("%T", h)))
	}
	if h == nil {
		h = hooks.New(logger, cfg)
	}

	p := proxy.New(logger, h, cfg)
	ip := incoming.New(logger, h, cfg)

	// Built once. agent.New is not side-effect free — it registers the auxiliary
	// proxy hook, the incoming proxy and the connection idle retention — so
	// building it twice fired all three twice per boot.
	return agent.New(logger, h, p, client, ip, cfg), nil
}
