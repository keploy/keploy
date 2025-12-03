package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func TestUpdateConfigData_DebugModules(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	c := NewCmdConfigurator(logger, cfg)

	defaultCfg := config.Config{}
	updatedCfg := c.UpdateConfigData(defaultCfg)

	expectedModules := []string{
		"contract",
		"record",
		"test",
		"tools",
		"report",
		"rerecord",
		"hooks",
		"docker",
		"proxy",
		"test-db",
		"mock-db",
		"map-db",
		"openapi-db",
		"report-db",
		"testset-db",
		"storage",
		"agent",
		"gen",
		"coverage",
		"telemetry",
		"auth",
		"proxy.http",
		"proxy.grpc",
		"proxy.generic",
		"proxy.mysql",
		"proxy.postgres_v1",
		"proxy.postgres_v2",
		"proxy.mongo",
		"proxy.redis",
		"proxy.tls",
	}

	for _, module := range expectedModules {
		_, exists := updatedCfg.DebugModules[module]
		assert.True(t, exists, "Module %s should exist in DebugModules", module)
		assert.False(t, updatedCfg.DebugModules[module], "Module %s should be disabled by default", module)
	}
}
