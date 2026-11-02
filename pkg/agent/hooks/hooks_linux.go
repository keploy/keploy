//go:build linux

package hooks

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/linux"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
	"go.uber.org/zap"
)

// The ingress manager takes the hooks' ingress hold by a type assertion
// (incoming.New): a Hooks that stopped satisfying IngressPortHolder would
// leave the hold's park unserved, silently. This makes it a build error.
var _ incoming.IngressPortHolder = (*linux.Hooks)(nil)

func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	return linux.NewHooks(logger, cfg)
}
