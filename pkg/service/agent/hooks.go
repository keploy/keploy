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
