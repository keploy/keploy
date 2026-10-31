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
	BeforeTestSetCompose(ctx context.Context, testSetID string) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error
	BeforeSimulate(ctx context.Context, t time.Time, testSetID string, tcName string) error
	AfterSimulate(ctx context.Context, testSetID string, tcName string) error
	// SetFreezeAnchor advertises the earliest recording-era timestamp that
	// the user app should see during this test run. Hook implementations
	// that drive a clock-mock (e.g. the LD_PRELOAD time-freeze in the
	// enterprise build) should pin the offset file to this instant before
	// the user app's first datetime.now() call so any cached recorded
	// credentials, JWTs, or TLS certs look fresh. A zero anchor means
	// "no anchor available" and should be treated as a no-op.
	SetFreezeAnchor(ctx context.Context, anchor time.Time) error
}

// AppClockReader is an optional AgentHooks capability: AppClock is the
// instant the app's clock showed at the real instant real, as the hooks moved
// it (the enterprise time-freeze), false where they cannot tell. The agent
// hands it to the proxy (coreAgent.AppClockSetter): the learn step reads a
// call's clock reads in the call's own window on it.
type AppClockReader interface {
	AppClock(real time.Time) (time.Time, bool)
}

// AppClockRunReader is an optional AgentHooks capability, beside
// AppClockReader: AppClockRunBegan is the real instant from which the app's
// clock has run as one to the real instant real, as the hooks moved it: the
// end of the last move they wrote at or before real (the first instant
// AppClock reads the moved clock at), told from the moves they made, false
// where AppClock(real) is. The agent hands it to the proxy
// (coreAgent.AppClockRunSetter): a test's first call, whose span on the
// app's clock would otherwise start at the previous test's last serve,
// before the test's begin moved the clock, and so be none, starts its span
// at the move. Hooks without it leave that call with no span, and a replay
// refuses its learned clock reads; and a span from the previous serve is
// then read at its two ends, which takes a stretch the clock left and came
// back to the same offset by (a real test, a frozen one, a real one) for
// one run, and passes a read made before it: hooks that move the clock
// should tell it. Where AppClock(real) is known it must tell it too; a
// false there gives the call no span.
type AppClockRunReader interface {
	AppClockRunBegan(real time.Time) (time.Time, bool)
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
func (n *AgentHook) SetFreezeAnchor(ctx context.Context, anchor time.Time) error {
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

// CaptureSettler settles a capture that reads the application's calls beside
// its path rather than in it (a downstream build's low-latency capture, with
// no keploy proxy for the calls to wait on). Under the proven switch a
// record's test end settles through it instead of through the proxy's
// listener (settle_proven.go): SettleCapture returns once every call the
// application made before it was called has reached keploy's recorder, and
// an error, never nil, when it cannot tell (the capture lost events, or holds
// a source it cannot fence). SettleProgress is a count for each thing the
// settle waits on that moves whenever that thing gets further; the settle's
// watchdog fails it when one of them stops moving (watchSettle).
// NoteTestBegin is called at each such record's test begin, which waits for
// nothing else: the capture counts what it lost from the first one, so what
// it lost before it (the application's start-up) is never the first test's.
type CaptureSettler interface {
	SettleCapture(ctx context.Context) error
	SettleProgress() map[string]uint64
	NoteTestBegin()
}

// StartupLossReporter is a CaptureSettler that says what its capture lost
// before the run's first test began, counted to that begin
// (CaptureSettler.NoteTestBegin): "" for nothing. No test is charged with
// it; the record keeps it for its summary (models.StartupCaptureLoss).
type StartupLossReporter interface {
	StartupCaptureLoss() string
}

// captureSettler is the registered CaptureSettler, nil when none is.
var captureSettler CaptureSettler

// RegisterCaptureSettler registers the capture a proven record's test ends
// settle through (CaptureSettler); nil unregisters it.
func RegisterCaptureSettler(s CaptureSettler) {
	captureSettler = s
}

// SetAgentInfoCustomizer registers a callback that mutates AgentInfo
// before it is written to the BPF map. Use it to set the extensible
// Flags slot from a downstream build.
func SetAgentInfoCustomizer(fn func(info *structs.AgentInfo)) {
	coreAgent.AgentInfoCustomizer = fn
}

// SetInterceptPostgresSSLRequest enables proxy-level handling of the
// Postgres SSLRequest preamble. Leave disabled (default) when a
// Postgres parser is registered — the parser handles the handshake
// through the TLSUpgrader interface and double-handling breaks it.
func SetInterceptPostgresSSLRequest(enabled bool) {
	coreAgent.InterceptPostgresSSLRequest = enabled
}
