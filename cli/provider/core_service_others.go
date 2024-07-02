//go:build !linux

package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.uber.org/zap"
)

func Get(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry) (interface{}, error) {
	return nil, errors.New("command not supported in non linux os")
}
