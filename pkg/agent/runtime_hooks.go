package agent

import (
	"sync"

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
// It is called once, as the agent starts loading its hooks, and its ports go into
// the kernel's pass-through list before the user's bypassRules ports.
var ExtraPassThroughPortsHook ExtraPassThroughPortsFn

func RegisterExtraPassThroughPortsHook(h ExtraPassThroughPortsFn) {
	ExtraPassThroughPortsHook = h
}

// extraLossReporters are the capture-loss counts a downstream build adds to
// the agent's own (RegisterCaptureLossReporter), under extraLossMu, each with
// the token its unregister removes it by.
var (
	extraLossMu        sync.Mutex
	extraLossReporters []registeredLossReporter
	extraLossNext      uint64
)

type registeredLossReporter struct {
	r     CaptureLossReporter
	token uint64
}

// RegisterCaptureLossReporter adds r's counts to every read of the agent's
// capture losses, beside the proxy's and the hooks' own: a downstream build
// whose capture neither of them sees (one of its own kernel programs, its own
// ring buffer) counts what that capture lost here, so that a session under
// the proven switch charges it to its tests as it charges keploy's own. r's
// counts must only ever grow for the agent's life, as every count there does
// (models.CaptureLosses). r may also be a CaptureLossDrainer (drained before
// each read) and a CaptureLossErrReporter (its counts unknown while it errs),
// as the hooks may. Call it before the agent serves. unregister takes r out
// again (a test's cleanup).
func RegisterCaptureLossReporter(r CaptureLossReporter) (unregister func()) {
	if r == nil {
		return func() {}
	}
	extraLossMu.Lock()
	defer extraLossMu.Unlock()
	extraLossNext++
	token := extraLossNext
	extraLossReporters = append(extraLossReporters, registeredLossReporter{r: r, token: token})
	return func() {
		extraLossMu.Lock()
		defer extraLossMu.Unlock()
		for i, x := range extraLossReporters {
			if x.token == token {
				extraLossReporters = append(extraLossReporters[:i:i], extraLossReporters[i+1:]...)
				return
			}
		}
	}
}

// CaptureLossReporters are the reporters RegisterCaptureLossReporter added,
// in the order it added them.
func CaptureLossReporters() []CaptureLossReporter {
	extraLossMu.Lock()
	defer extraLossMu.Unlock()
	out := make([]CaptureLossReporter, 0, len(extraLossReporters))
	for _, x := range extraLossReporters {
		out = append(out, x.r)
	}
	return out
}
