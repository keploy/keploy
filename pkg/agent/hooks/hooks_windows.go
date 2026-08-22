//go:build windows && amd64

package hooks

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/windows"
	"go.keploy.io/server/v3/pkg/agent/hooks/winshim"
	"go.uber.org/zap"
)

// New selects the Windows interception backend.
//
// WinDivert (pkg/agent/hooks/windows) stays the default wherever it can work: it
// is a kernel filter, so it intercepts a process tree keploy did not create and
// needs no injection at all. But it needs Administrator to load its driver, and
// without that a native Windows run used to fail outright.
//
// So when keploy is not elevated — or the operator asked for it explicitly — the
// unprivileged userspace backend (pkg/agent/hooks/winshim) is used instead. An
// elevated run reaches exactly the code it always did.
func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	if winshim.Selected(logger) {
		return winshim.NewHooks(logger, cfg)
	}
	return windows.NewHooks(logger, cfg)
}
