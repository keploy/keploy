package app

import (
	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.uber.org/zap"
)

// AppRuntimeHooks defines extension points used during app lifecycle.
// Implementations may mutate compose spec in-place.
type AppRuntimeHooks interface {
	BeforeDockerComposeSetup(logger *zap.Logger, compose *docker.Compose, serviceName string) (bool, error)
	BeforeDockerSetup(logger *zap.Logger, cmd string) (string, error)
}

// RuntimeHooks is the singleton used by runtime; can be overridden by other builds.
var RuntimeHooks AppRuntimeHooks = defaultAppRuntimeHooks{}

type defaultAppRuntimeHooks struct{}

func (defaultAppRuntimeHooks) BeforeDockerComposeSetup(logger *zap.Logger, _ *docker.Compose, _ string) (bool, error) {
	// no-op
	return false, nil
}

func (defaultAppRuntimeHooks) BeforeDockerSetup(logger *zap.Logger, cmd string) (string, error) {
	logger.Debug("running before docker setup hook - oss", zap.String("cmd", cmd))
	// no-op
	return cmd, nil
}
