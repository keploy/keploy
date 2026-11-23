//go:build !unix && !windows

package cli

import (
	"context"

	"go.uber.org/zap"
)

// watchParentProcess is a no-op where keploy runs no native agent (neither
// unix nor Windows): there is no agent process to outlive its client.
func watchParentProcess(_ context.Context, _ *zap.Logger, _ int) {}
