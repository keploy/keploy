//go:build !unix

package cli

import (
	"context"

	"go.uber.org/zap"
)

// watchParentProcess is a no-op on non-unix platforms. The orphaned-agent
// problem it guards against is specific to the eBPF hooks, DNS takeover and
// proxy/ingress listeners keploy installs on Unix; on Windows the agent is run
// differently (and typically inside a container that bounds its lifetime).
func watchParentProcess(_ context.Context, _ *zap.Logger, _ int) {}
