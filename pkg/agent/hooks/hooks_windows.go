//go:build windows && amd64

package hooks

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/winshim"
	"go.uber.org/zap"
)

// New builds the Windows interception backend.
//
// There is one: the unprivileged userspace backend in pkg/agent/hooks/winshim.
// It replaced a kernel packet-filter driver that could only load with
// Administrator, which meant a native Windows run failed outright for anyone
// who had not opened an elevated terminal.
func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	return winshim.NewHooks(logger, cfg)
}
