package agent

import "go.keploy.io/server/v3/pkg/agent/hooks/structs"

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
