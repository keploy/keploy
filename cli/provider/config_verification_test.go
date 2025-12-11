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

	// Verify DebugModules struct is initialized with empty slices
	assert.NotNil(t, updatedCfg.DebugModules)
	assert.Empty(t, updatedCfg.DebugModules.Include, "Include should be empty by default")
	assert.Empty(t, updatedCfg.DebugModules.Exclude, "Exclude should be empty by default")
}
