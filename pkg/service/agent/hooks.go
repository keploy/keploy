package agent

import (
	"context"
	"sync"
	"time"
)

type AgentHooks interface {
	BeforeSimulate(ctx context.Context, t time.Time) error
	AfterSimulate(ctx context.Context) error
}
type NoOpHooks struct{}

func (n *NoOpHooks) BeforeSimulate(ctx context.Context, t time.Time) error {
	return nil
}

func (n *NoOpHooks) AfterSimulate(ctx context.Context) error {
	return nil
}

var (
	activeHooks AgentHooks = &NoOpHooks{}
	hookMu      sync.RWMutex
)

func RegisterHooks(h AgentHooks) {
	hookMu.Lock()
	defer hookMu.Unlock()
	activeHooks = h
}

func GetHooks() AgentHooks {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return activeHooks
}

type StartupHook interface {
    GetArgs(ctx context.Context) []string
}

// Default NoOp implementation
type NoOpStartupHook struct{}
func (h *NoOpStartupHook) GetArgs(ctx context.Context) []string { return nil }

var startupHook StartupHook = &NoOpStartupHook{}

func RegisterStartupHook(h StartupHook) {
    startupHook = h
}

func GetStartupHook() StartupHook {
    return startupHook
}