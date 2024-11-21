package docker

import (
    "testing"
    "go.uber.org/zap"
)


// Test generated using Keploy
func TestNew_Success(t *testing.T) {
    logger := zap.NewNop()
    client, err := New(logger)
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
    if client == nil {
        t.Error("Expected a valid client, got nil")
    }
}
