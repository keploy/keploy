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
// injects a Postgres parser (via extraparsers.go) that handles the
// SSLRequest through the TLSUpgrader interface, and double-handling
// breaks the parser-driven flow. Downstream builds that do not
// register a Postgres parser (pure proxy-mode deployments) can opt
// in by setting this to true before proxy start.
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
