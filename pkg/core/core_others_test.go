//go:build !linux

package core

import (
    "testing"
    "go.uber.org/zap"
)


// Test generated using Keploy
func TestNew_ReturnsCoreInstance(t *testing.T) {
    logger := zap.NewNop()
    core := New(logger)
    if core == nil {
        t.Error("Expected New to return a Core instance, got nil")
    }
    if core.logger != logger {
        t.Error("Expected Core.logger to be set to the provided logger")
    }
}
