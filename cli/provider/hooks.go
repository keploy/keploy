package provider

import (
	"context"
)

// ProviderHooks defines extension points for the provider package.
// Enterprise builds can replace the default implementation to customize behavior.
type ProviderHooks interface {
	// ModifyDockerCommand allows modification of the docker command string
	// that will be executed by RunInDocker. Return the original cmd if no change is needed.
	ModifyDockerCommand(ctx context.Context, cmd string) (string, error)
}

// noopProviderHooks is the default implementation used in the OSS build.
type noopProviderHooks struct{}

func (noopProviderHooks) ModifyDockerCommand(ctx context.Context, cmd string) (string, error) {
	return cmd, nil
}

// Hooks is the active set of hooks used by the provider package. Enterprise
// builds can override this variable during init to supply custom behavior.
var Hooks ProviderHooks = noopProviderHooks{}
