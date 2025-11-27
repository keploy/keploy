package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type AgentHooks interface {
	BeforeTestRun(ctx context.Context, testRunID string) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error
	BeforeSimulate(ctx context.Context, t time.Time, testSetID string, tcName string) error
	AfterSimulate(ctx context.Context, testSetID string, tcName string) error
}
type NoOpHooks struct{}

func (n *NoOpHooks) BeforeSimulate(ctx context.Context, t time.Time, testSetID string, tcName string) error {
	return nil
}

func (n *NoOpHooks) AfterSimulate(ctx context.Context, testSetID string, tcName string) error {
	return nil
}
func (n *NoOpHooks) BeforeTestRun(ctx context.Context, id string) error {
	fmt.Println("doing nothing")
	return nil
}
func (n *NoOpHooks) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
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
