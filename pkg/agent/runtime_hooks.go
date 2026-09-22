package agent

import (
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// Pinnable is implemented by eBPF maps that support pinning to bpffs.
type Pinnable interface {
	Pin(fileName string) error
}

// EbpfLoadedHook is called after eBPF objects are loaded. The callback
// receives a lookup function that resolves map names to Pinnable references.
// Downstream builds use this to pin only the maps they need.
var EbpfLoadedHook func(getMap func(string) Pinnable) error

// EbpfProxyPortOverride can override the proxy port used in eBPF hook config.
var EbpfProxyPortOverride uint32

// SkipProxyListener disables the proxy TCP accept loop. When true, the
// proxy does not bind a port for outgoing traffic interception. DNS
// servers and session management still operate normally.
var SkipProxyListener bool

// HooksFactory, when non-nil, replaces the kernel-backed Hooks implementation
// that the agent composition root would otherwise construct. Downstream builds
// set it to run the datapath entirely in userspace — e.g. the JVM-fed sidecar
// path, where an in-process javaagent reports connection destinations instead of
// cgroup/connect4 writing redirectProxyMap — on clusters that do not permit
// eBPF.
//
// It is a FACTORY rather than a pre-built value on purpose. The composition root
// owns the process logger and the fully-parsed config, so a factory receives
// both instead of forcing downstream to re-plumb them through its own globals.
// It also makes "no override" unambiguous: a nil func is nil, whereas a nil
// implementation pointer stored in an interface is non-nil and would install a
// Hooks that panics on first use.
//
// The returned value MUST satisfy the whole Hooks contract:
//
//   - Get: returning (nil, nil) is NOT permitted. Return a non-nil error for an
//     unknown source port; the proxy closes such connections gracefully, but it
//     dereferences the result whenever err is nil.
//   - Delete: MUST be idempotent and return nil for a port that was never
//     registered. The proxy treats a Delete error as fatal to the connection,
//     unlike Get, so "not found" here tears down live traffic.
//   - Load: HookCfg.Port is only populated when EbpfProxyPortOverride is set;
//     read the proxy port from the config passed to the factory instead.
//   - WatchBindEvents: the returned channel MUST be closed when ctx is done, or
//     the ingress manager never runs StopAll and the app's port stays bound.
//     An error return is permanent — the consumer starts once and never retries.
//
// The value is read ONCE, at composition. Setting it afterwards has no effect.
var HooksFactory func(logger *zap.Logger, cfg *config.Config) Hooks

// AgentInfoCustomizer is called after the base AgentInfo has been
// populated but before it is written to the eBPF map. Downstream
// builds can use this to set the extensible Flags slot on AgentInfo,
// which the BPF cgroup hooks consume to branch their behavior.
var AgentInfoCustomizer func(info *structs.AgentInfo)

// InterceptPostgresSSLRequest controls whether the proxy itself
// responds to the Postgres SSLRequest preamble (by replying 'S' and
// upgrading to TLS). Disabled by default: the default keploy build
// may ship with a Postgres parser registered via
// pkg/agent/proxy/extraparsers.go (blank-imported by the
// setup-private-parsers composite action at CI time) that already
// handles the SSLRequest through the TLSUpgrader interface, and
// double-handling breaks that parser-driven flow.
//
// Scope when enabled: the flag covers both sides of the handshake.
//   - Client side: read SSLRequest, reply 'S', MITM TLS with the client.
//   - Upstream side: when the destination port is 5432, the proxy
//     dials plain TCP, writes the SSLRequest preamble, reads the
//     'S'/'N' response from the upstream Postgres server, and only
//     then upgrades the existing socket to TLS via tls.Client. This
//     is what a real Postgres server expects; tls.Dial directly on
//     5432 would be rejected by the server. Non-5432 destinations
//     still go through the plain tls.Dial path — if a downstream
//     deployment runs Postgres on a non-standard port, it needs to
//     either (a) accept direct TLS, or (b) register a Postgres parser
//     via the TLSUpgrader path.
//
// End-to-end MITM against a vanilla Postgres now works under this
// flag. A parser-driven TLSUpgrader, when one is registered, remains
// the richer option when you want protocol-aware mocking.
var InterceptPostgresSSLRequest bool

// ProxyHook allows an optional auxiliary proxy hook to run after proxy startup.
var ProxyHook AuxiliaryProxyHook

func RegisterProxyHook(h AuxiliaryProxyHook) {
	ProxyHook = h
}

// ActiveIncomingProxy is set when the active incoming proxy implementation is registered.
var ActiveIncomingProxy IncomingProxy

func RegisterIncomingProxy(ip IncomingProxy) {
	ActiveIncomingProxy = ip
}

type ExtraPassThroughPortsFn func() []uint

// ExtraPassThroughPortsHook allows external providers to append passthrough ports.
var ExtraPassThroughPortsHook ExtraPassThroughPortsFn

func RegisterExtraPassThroughPortsHook(h ExtraPassThroughPortsFn) {
	ExtraPassThroughPortsHook = h
}
