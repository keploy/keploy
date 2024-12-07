package provider

import (
    "testing"
    "go.keploy.io/server/v2/config"
    "go.keploy.io/server/v2/pkg/service"
    "go.uber.org/zap"
)


// Test generated using Keploy
func TestNewServiceProvider_ReturnsValidServiceProvider(t *testing.T) {
    logger := zap.NewNop()
    cfg := &config.Config{}
    var auth service.Auth = nil

    sp := NewServiceProvider(logger, cfg, auth)

    if sp == nil {
        t.Errorf("Expected ServiceProvider to be non-nil")
    }
    if sp.logger != logger {
        t.Errorf("Expected logger to be set correctly")
    }
    if sp.cfg != cfg {
        t.Errorf("Expected cfg to be set correctly")
    }
    if sp.auth != auth {
        t.Errorf("Expected auth to be set correctly")
    }
}
