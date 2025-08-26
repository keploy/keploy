//go:build !linux

package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestCore_GetHookUnloadDone_NonLinux(t *testing.T) {
	logger := zap.NewNop()

	// Create a new Core instance using the non-Linux constructor
	core := New(logger)

	appID := uint64(12345)

	// Test that GetHookUnloadDone returns a channel
	ch := core.GetHookUnloadDone(appID)
	assert.NotNil(t, ch, "GetHookUnloadDone should return a channel")

	// For non-Linux platforms, the channel should be immediately closed
	// since no actual hooks are loaded
	select {
	case <-ch:
		// Expected behavior for non-Linux - channel is immediately closed
	case <-time.After(100 * time.Millisecond):
		t.Error("Channel should be immediately closed on non-Linux platforms")
	}
}

func TestCore_GetHookUnloadDone_NonLinux_MultipleCalls(t *testing.T) {
	logger := zap.NewNop()
	core := New(logger)

	appID := uint64(12345)

	// Get multiple channels - each should be immediately closed
	ch1 := core.GetHookUnloadDone(appID)
	ch2 := core.GetHookUnloadDone(appID)

	assert.NotNil(t, ch1, "First call should return a channel")
	assert.NotNil(t, ch2, "Second call should return a channel")

	// Both channels should be immediately closed
	select {
	case <-ch1:
		// Expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("First channel should be immediately closed")
	}

	select {
	case <-ch2:
		// Expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("Second channel should be immediately closed")
	}
}

func TestCore_GetHookUnloadDone_NonLinux_DifferentApps(t *testing.T) {
	logger := zap.NewNop()
	core := New(logger)

	appID1 := uint64(12345)
	appID2 := uint64(67890)

	// Get channels for different app IDs
	ch1 := core.GetHookUnloadDone(appID1)
	ch2 := core.GetHookUnloadDone(appID2)

	assert.NotNil(t, ch1, "First app should return a channel")
	assert.NotNil(t, ch2, "Second app should return a channel")

	// Both should be immediately closed
	select {
	case <-ch1:
		// Expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("Channel for app1 should be immediately closed")
	}

	select {
	case <-ch2:
		// Expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("Channel for app2 should be immediately closed")
	}
}
