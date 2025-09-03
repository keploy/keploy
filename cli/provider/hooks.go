package provider

import (
	"context"

	"go.uber.org/zap"
)

// ProviderHooks defines extension points for the provider package.
type ProviderHooks interface {
	// ModifyDockerCommand allows modification of the docker command string
	// that will be executed by RunInDocker. Return the original cmd if no change is needed.
	ModifyDockerCommand(ctx context.Context, cmd string) (string, error)
}

// noopProviderHooks is the default implementation used in the OSS build.
type noopProviderHooks struct {
	logger *zap.Logger
}

func (h noopProviderHooks) ModifyDockerCommand(ctx context.Context, cmd string) (string, error) {
	h.logger.Info("coming from oss modifydockercommand")
	return cmd, nil
}

// Hooks is the active set of hooks used by the provider package. Enterprise
// builds can override this variable during init to supply custom behavior.
// Initialize with a no-op logger to avoid nil dereferences in OSS builds.
var Hooks ProviderHooks = noopProviderHooks{logger: zap.NewNop()}

// SetProviderHooks allows external packages (e.g., enterprise editions) to
// override the default hooks at init-time.
func SetProviderHooks(h ProviderHooks) {
	Hooks = h
}
