package agent

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

var ProxyHook agent.AuxiliaryProxyHook

func RegisterProxyHook(h agent.AuxiliaryProxyHook) {
	ProxyHook = h
}

// Pinnable is implemented by eBPF maps that support pinning to bpffs.
type Pinnable interface {
	Pin(fileName string) error
}

// EbpfMapPinHook is called after eBPF objects are loaded to allow enterprise
// to pin specific maps to bpffs for cross-process access (e.g. sockmap proxy).
// It receives a map of name → Pinnable and returns a cleanup function to unpin
// on shutdown.
type EbpfMapPinHook func(maps map[string]Pinnable) (cleanup func(), err error)

var MapPinHook EbpfMapPinHook

func RegisterMapPinHook(h EbpfMapPinHook) {
	MapPinHook = h
}

var EbpfProxyPortOverride uint32

var ActiveIncomingProxy agent.IncomingProxy

func RegisterIncomingProxy(ip agent.IncomingProxy) {
	ActiveIncomingProxy = ip
}

type SockmapLoadHookFn func(logger *zap.Logger) (cleanup func(), err error)

var SockmapHook SockmapLoadHookFn

func RegisterSockmapLoadHook(h SockmapLoadHookFn) {
	SockmapHook = h
}

type ExtraPassThroughPortsFn func() []uint

var ExtraPassThroughPortsHook ExtraPassThroughPortsFn

func RegisterExtraPassThroughPortsHook(h ExtraPassThroughPortsFn) {
	ExtraPassThroughPortsHook = h
}
