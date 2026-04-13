package agent

import (
	"context"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
)

type AgentHooks interface {
	BeforeTestRun(ctx context.Context, testRunID string) error
	BeforeTestSetCompose(ctx context.Context, testRunID string) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error
	BeforeSimulate(ctx context.Context, t time.Time, testSetID string, tcName string) error
	AfterSimulate(ctx context.Context, testSetID string, tcName string) error
}
type AgentHook struct{}

func (n *AgentHook) BeforeSimulate(ctx context.Context, t time.Time, testSetID string, tcName string) error {
	return nil
}

func (n *AgentHook) AfterSimulate(ctx context.Context, testSetID string, tcName string) error {
	return nil
}
func (n *AgentHook) BeforeTestRun(ctx context.Context, id string) error {
	return nil
}
func (n *AgentHook) BeforeTestSetCompose(ctx context.Context, id string) error {
	return nil
}
func (n *AgentHook) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	return nil
}

var (
	ActiveHooks AgentHooks = &AgentHook{}
)

func RegisterHooks(h AgentHooks) {
	ActiveHooks = h
}

type StartupHook interface {
	GetArgs(ctx context.Context) []string
}

// Default NoOp implementation
type StartupHooks struct{}

func (h *StartupHooks) GetArgs(ctx context.Context) []string { return nil }

var StartupAgentHook StartupHook = &StartupHooks{}

func RegisterStartupHook(h StartupHook) {
	StartupAgentHook = h
}

type SetupHooks interface {
	AfterSetup(ctx context.Context) error
}

type SetupHook struct{}

func (s *SetupHook) AfterSetup(ctx context.Context) error {
	return nil
}

var SetupAgentHook SetupHooks = &SetupHook{}

func RegisterSetupHook(h SetupHooks) {
	SetupAgentHook = h
}

func RegisterProxyHook(h coreAgent.AuxiliaryProxyHook) {
	coreAgent.RegisterProxyHook(h)
}

func RegisterIncomingProxy(ip coreAgent.IncomingProxy) {
	coreAgent.RegisterIncomingProxy(ip)
}

type ExtraPassThroughPortsFn = coreAgent.ExtraPassThroughPortsFn

func RegisterExtraPassThroughPortsHook(h ExtraPassThroughPortsFn) {
	coreAgent.RegisterExtraPassThroughPortsHook(h)
}

func RegisterEbpfLoadedHook(h func(getMap func(string) coreAgent.Pinnable) error) {
	coreAgent.EbpfLoadedHook = h
}

func SetEbpfProxyPortOverride(port uint32) {
	coreAgent.EbpfProxyPortOverride = port
}

func SetSkipProxyListener(skip bool) {
	coreAgent.SkipProxyListener = skip
}

// SetAgentInfoCustomizer registers a callback that mutates AgentInfo
// before it is written to the BPF map. Use it to set the extensible
// Flags slot from a downstream build.
func SetAgentInfoCustomizer(fn func(info *structs.AgentInfo)) {
	coreAgent.AgentInfoCustomizer = fn
}
