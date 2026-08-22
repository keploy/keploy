//go:build windows && amd64

package winshim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/windows"
)

// instrumentationGrace is how long the agent waits for the shim to announce
// itself before warning that the application was never instrumented. It has to
// cover a slow first start — a Go app that compiles on `go run`, a JVM, a Node
// app installing on boot — so it is generous; the warning is only useful if it
// is trustworthy.
const instrumentationGrace = 90 * time.Second

// Hooks is the Windows implementation of agent.Hooks.
//
// It answers the same question the Linux eBPF hooks answer — where was this
// connection really going — but sources it from the injected shim over a named
// pipe instead of from kernel programs. Everything downstream (proxy, TLS,
// parsers, mock matching) is the shared implementation and is unaware of the
// difference.
type Hooks struct {
	logger *zap.Logger
	sess   *agent.Sessions
	conf   *config.Config

	proxyPort         uint32
	dnsPort           uint32
	agentPort         uint32
	incomingProxyPort uint16

	sessionDir string
	shimPath   string
	pipeName   string
	ctrl       *controlServer

	// dests maps a shim-pinned local port to the destination the application
	// actually asked for. This is the Windows-userspace equivalent of the eBPF
	// connection map that DestInfo.Get reads on Linux.
	dests sync.Map // uint16 -> agent.NetworkAddress

	// bindEvents carries ingress events to the incoming proxy.
	//
	// bindMu guards both the send and the close. The senders are control-server
	// goroutines serving live shim requests, so shutdown can land between a
	// dispatch and its send — and a send on a closed channel panics, which a
	// `select` with a default case does NOT protect against. Whoever holds the
	// lock is either sending or closing, never both.
	bindMu     sync.Mutex
	bindEvents chan models.IngressEvent
	bindDone   bool

	// ingress tracks which advertised ports have been moved, so that a
	// dual-stack server binding 8080 on both IPv4 and IPv6 is moved to the same
	// replacement port both times, and which have already been handed to the
	// ingress proxy.
	ingressMu  sync.Mutex
	movedPorts map[uint16]uint16
	published  map[uint16]bool

	// synthetic addresses handed out for names the app could not resolve.
	dnsMu     sync.Mutex
	synthetic map[string]string
	nextSynth uint32

	// armed records that the shim announced itself from at least one process.
	armed atomic.Bool

	mode  models.Mode
	rules []models.BypassRule

	unloadOnce sync.Once
}

// NewHooks builds the unprivileged Windows hooks. cfg is the configuration the
// agent was started with.
func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	incomingProxyPort := cfg.IncomingProxyPort
	if incomingProxyPort == 0 {
		incomingProxyPort = models.DefaultIncomingProxyPort
	}
	return &Hooks{
		logger:            logger,
		sess:              agent.NewSessions(),
		conf:              cfg,
		proxyPort:         cfg.ProxyPort,
		dnsPort:           cfg.DNSPort,
		incomingProxyPort: incomingProxyPort,
		bindEvents:        make(chan models.IngressEvent, 128),
		movedPorts:        make(map[uint16]uint16),
		published:         make(map[uint16]bool),
		synthetic:         make(map[string]string),
	}
}

// Load prepares the interception session: it stages the shim the client injects
// into the application and starts the control pipe the shim talks to.
//
// This needs no privileges. No driver is loaded, nothing is installed
// system-wide, and the blast radius is a single process tree.
func (h *Hooks) Load(ctx context.Context, cfg agent.HookCfg, setupOpts config.Agent) error {
	h.mode = setupOpts.Mode
	if h.mode == "" {
		h.mode = cfg.Mode
	}
	h.rules = cfg.Rules
	h.agentPort = setupOpts.AgentPort
	if h.proxyPort == 0 {
		h.proxyPort = setupOpts.ProxyPort
	}
	if h.dnsPort == 0 {
		h.dnsPort = setupOpts.DnsPort
	}

	// Keyed on the client PID, not on any port: the client has to stage the shim
	// before these ports exist.
	clientPID := setupOpts.ClientNSPID
	if clientPID == 0 {
		clientPID = cfg.Pid
	}
	if clientPID == 0 {
		return errors.New("the unprivileged Windows hooks need the keploy client PID (--client-pid) to locate the shim control pipe")
	}

	sessionDir, err := EnsureSessionDir(clientPID)
	if err != nil {
		return err
	}
	h.sessionDir = sessionDir
	h.pipeName = ControlPipeName(clientPID)

	shimPath, err := StageShim(h.logger, sessionDir, h.pipeName)
	if err != nil {
		return err
	}
	h.shimPath = shimPath

	ctrl, err := newControlServer(h.logger, h, h.pipeName)
	if err != nil {
		return err
	}
	h.ctrl = ctrl

	// A single session, matching the single-app native flow. The proxy's
	// GetSessionFor falls back to this when no resolver is installed.
	h.sess.Set(uint64(0), &agent.Session{
		ID:   uint64(0),
		Mode: h.mode,
	})

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		h.ctrl.close()
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer utils.Recover(h.logger)
		h.ctrl.serve(ctx)
		return nil
	})

	g.Go(func() error {
		defer utils.Recover(h.logger)
		<-ctx.Done()
		h.unLoad()
		return nil
	})

	g.Go(func() error {
		defer utils.Recover(h.logger)
		h.watchForInstrumentation(ctx, instrumentationGrace)
		return nil
	})

	h.logger.Info("Keploy is intercepting this application natively on Windows",
		zap.String("shim", h.shimPath),
		zap.String("control_pipe", h.pipeName),
		zap.Uint32("proxy_port", h.proxyPort))

	return nil
}

// unLoad tears down the control pipe.
func (h *Hooks) unLoad() {
	h.unloadOnce.Do(func() {
		if h.ctrl != nil {
			h.ctrl.close()
		}
		h.closeIngress()
		h.sess.Delete(uint64(0))
		h.logger.Debug("unloaded the unprivileged Windows hooks")
	})
}

// Record is not used here; ingress test cases are produced by the shared
// incoming proxy, driven by WatchBindEvents.
func (h *Hooks) Record(_ context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	return nil, nil
}

// WatchBindEvents streams the app's server binds as they are intercepted, so the
// incoming proxy can take over each advertised port.
//
// A kernel filter could leave the application on its advertised port and
// redirect inbound packets to a fixed proxy port. User space has no way to
// redirect a port the application already owns, so — exactly like the macOS
// backend — the application's listener is moved and Keploy binds the port it
// advertises, publishing one event per real server socket.
func (h *Hooks) WatchBindEvents(_ context.Context) (<-chan models.IngressEvent, error) {
	return h.bindEvents, nil
}

// Get returns the destination the application originally asked for on the
// connection whose local port is srcPort.
func (h *Hooks) Get(_ context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	v, ok := h.dests.Load(srcPort)
	if !ok {
		return nil, fmt.Errorf("no destination recorded for source port %d", srcPort)
	}
	addr, ok := v.(agent.NetworkAddress)
	if !ok {
		return nil, fmt.Errorf("corrupt destination entry for source port %d", srcPort)
	}
	return &addr, nil
}

// Delete releases the destination entry once the proxy has consumed it.
func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	h.dests.Delete(srcPort)
	return nil
}

// CleanProxyEntry releases a destination entry by source port.
func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	h.dests.Delete(srcPort)
	return nil
}

// SendAgentInfo is part of the hooks surface; there is nothing to send here.
func (h *Hooks) SendAgentInfo(_ structs.AgentInfo) error { return nil }

// Armed reports whether the shim has announced itself from at least one process.
func (h *Hooks) Armed() bool { return h.armed.Load() }

// ---------------------------------------------------------------------------
// controlDecider — the policy the shim consults
// ---------------------------------------------------------------------------

// onConnect records an outgoing connection and returns the proxy port the shim
// should dial in its place.
func (h *Hooks) onConnect(srcPort uint16, version uint32, destIP string, destPort uint16) (uint16, bool) {
	if h.proxyPort == 0 || h.proxyPort > 65535 {
		return 0, false
	}

	// Never intercept keploy's own control plane. Redirecting the proxy port
	// would loop the proxy into itself; the agent and DNS ports are keploy's own
	// listeners and are checked here because only the agent knows them.
	if uint32(destPort) == h.agentPort || uint32(destPort) == h.proxyPort ||
		(h.dnsPort != 0 && uint32(destPort) == h.dnsPort) {
		return 0, false
	}

	ip := net.ParseIP(destIP)
	if ip == nil {
		return 0, false
	}

	if h.isBypassed(ip, destPort) {
		h.logger.Debug("leaving a bypassed destination alone",
			zap.String("dest", net.JoinHostPort(destIP, fmt.Sprint(destPort))))
		return 0, false
	}

	addr, err := toNetworkAddress(ip, destPort, version)
	if err != nil {
		h.logger.Debug("could not encode a destination; letting the connection through",
			zap.String("ip", destIP), zap.Error(err))
		return 0, false
	}

	h.dests.Store(srcPort, addr)
	h.logger.Debug("captured an outgoing connection",
		zap.Uint16("src_port", srcPort),
		zap.String("dest", net.JoinHostPort(destIP, fmt.Sprint(destPort))))
	return uint16(h.proxyPort), true
}

// onHello records that the shim armed inside a process.
func (h *Hooks) onHello(pid uint32, progName string) {
	if h.armed.CompareAndSwap(false, true) {
		h.logger.Debug("the interception shim armed inside the application",
			zap.Uint32("pid", pid), zap.String("program", progName))
	}
}

// onBind moves an application bind so Keploy can own the advertised port and
// forward to the app.
//
// Record only, mirroring Linux, where the bind4/bind6 cgroup programs are
// attached inside `if opts.Mode == models.MODE_RECORD`. Moving the listener
// during replay would take the app off its advertised port with nothing to
// forward it back: the ingress proxy that owns that port is only started on the
// record path, so the replayer would aim at the recorded app port and every
// simulated request would be refused.
func (h *Hooks) onBind(_ uint32, origPort uint16) uint16 {
	if h.mode != models.MODE_RECORD {
		return 0
	}

	// Never move keploy's own listeners, in case the agent and the app end up
	// sharing a process tree.
	if uint32(origPort) == h.agentPort || uint32(origPort) == h.proxyPort ||
		(h.dnsPort != 0 && uint32(origPort) == h.dnsPort) ||
		origPort == h.incomingProxyPort {
		return 0
	}

	h.ingressMu.Lock()
	defer h.ingressMu.Unlock()

	// A dual-stack server binds the same port twice (0.0.0.0 and [::]). Both
	// binds must land on the SAME replacement port or half the app's listeners
	// end up somewhere keploy is not forwarding to.
	if existing, ok := h.movedPorts[origPort]; ok {
		return existing
	}

	newPort, err := utils.GetAvailablePort()
	if err != nil {
		h.logger.Error("could not allocate a port to move the application's listener to; recording incoming requests on this port will be skipped",
			zap.Uint16("app_port", origPort), zap.Error(err))
		return 0
	}
	if newPort == 0 || newPort > 65535 {
		return 0
	}

	moved := uint16(newPort)
	h.movedPorts[origPort] = moved

	// Deliberately no ingress event yet. At bind time a server socket and a
	// client that pinned an explicit source port are indistinguishable, and
	// standing up an ingress forwarder for the latter would occupy a port for
	// nothing. onListen publishes it once the socket proves it is a server.
	h.logger.Debug("moved an application bind so keploy can own the port",
		zap.Uint16("app_port", origPort), zap.Uint16("moved_to", moved))
	return moved
}

// onListen publishes the ingress event for a moved socket that has now proved
// itself a server by calling listen().
func (h *Hooks) onListen(pid uint32, origPort, movedPort uint16) {
	if h.mode != models.MODE_RECORD {
		return
	}

	h.ingressMu.Lock()
	expected, known := h.movedPorts[origPort]
	alreadyPublished := h.published[origPort]
	if known && expected == movedPort && !alreadyPublished {
		h.published[origPort] = true
	}
	h.ingressMu.Unlock()

	if !known || expected != movedPort {
		// A listen on a port we never moved, or moved differently. Nothing to
		// forward — keploy does not own the advertised port in that case.
		h.logger.Debug("ignoring a listen on a port keploy did not move",
			zap.Uint16("orig_port", origPort), zap.Uint16("moved_port", movedPort))
		return
	}
	if alreadyPublished {
		// A dual-stack server listens twice on the same moved port.
		return
	}

	if !h.publishIngress(models.IngressEvent{
		PID:         pid,
		Family:      windows.AF_INET,
		OrigAppPort: origPort,
		NewAppPort:  movedPort,
	}) {
		// Worse than a missed recording: the bind was already moved, so with no
		// forwarder there is nothing on the port the application advertises —
		// its API is unreachable. Say that, rather than implying the only cost
		// is coverage.
		h.ingressMu.Lock()
		delete(h.published, origPort)
		h.ingressMu.Unlock()
		h.logger.Warn("Keploy moved your application's listener but could not take over the port it advertises, so nothing is serving that port and requests to it will fail.",
			zap.Uint16("app_port", origPort),
			zap.Uint16("app_is_listening_on", movedPort),
			zap.String("next_step", "Stop the run and start it again; if it repeats, run with --debug and share the agent log."))
		return
	}

	h.logger.Debug("published an ingress event for an application listener",
		zap.Uint16("app_port", origPort), zap.Uint16("moved_to", movedPort))
}

// publishIngress sends an event unless the channel has already been closed.
// Reports whether the event was accepted.
func (h *Hooks) publishIngress(e models.IngressEvent) bool {
	h.bindMu.Lock()
	defer h.bindMu.Unlock()

	if h.bindDone {
		return false
	}
	select {
	case h.bindEvents <- e:
		return true
	default:
		return false
	}
}

// closeIngress closes the event channel exactly once, excluding concurrent
// senders for the duration.
func (h *Hooks) closeIngress() {
	h.bindMu.Lock()
	defer h.bindMu.Unlock()

	if h.bindDone {
		return
	}
	h.bindDone = true
	close(h.bindEvents)
}

// watchForInstrumentation warns, once, if nothing ever arms.
//
// Injection is best-effort by design — a child process keploy could not open, or
// one of a different architecture, simply runs uninstrumented rather than
// failing. That is the right trade, but it makes a completely un-instrumented
// run silently indistinguishable from an application that made no dependency
// calls: the recording completes, the report is green, and there are simply no
// mocks. Saying so explicitly is the difference between a five-minute fix and an
// afternoon of confusion.
func (h *Hooks) watchForInstrumentation(ctx context.Context, within time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(within):
	}

	if h.armed.Load() {
		return
	}

	h.logger.Warn("Keploy never got loaded into your application, so no traffic can be recorded or mocked.",
		zap.Duration("waited", within),
		zap.String("next_step", "Run the application's real executable directly rather than through a launcher that replaces its own process, "+
			"and make sure the application is a 64-bit program. Re-run with --debug to see the shim's own log."))
}

// onDNS hands back a synthetic loopback address for a name the application could
// not resolve.
//
// Only during replay: a recorded dependency may no longer exist (or the machine
// may be offline), and without an address the app fails inside the resolver,
// before it ever reaches connect() — so the mock that would have answered is
// never consulted. During record a resolution failure is a real failure and must
// surface as one.
func (h *Hooks) onDNS(hostname string) string {
	if h.mode != models.MODE_TEST {
		return ""
	}

	h.dnsMu.Lock()
	defer h.dnsMu.Unlock()

	if existing, ok := h.synthetic[hostname]; ok {
		return existing
	}

	// 127.0.0.0/8 is entirely routed to loopback, so any address in it reaches
	// the local machine — and every connection to it is redirected to the proxy
	// by the shim anyway. Start above 127.0.0.1 to avoid colliding with a real
	// localhost dependency.
	const syntheticBase = 2
	offset := syntheticBase + h.nextSynth + 1
	if offset > 0x00FFFFFF {
		h.logger.Warn("exhausted the synthetic loopback address range", zap.String("hostname", hostname))
		return ""
	}
	h.nextSynth++
	addr := net.IPv4(127, byte(offset>>16), byte(offset>>8), byte(offset)).String()

	h.synthetic[hostname] = addr
	h.logger.Debug("handed the application a synthetic address for an unresolvable dependency",
		zap.String("hostname", hostname), zap.String("address", addr))
	return addr
}

// isBypassed reports whether a destination matches one of the configured bypass
// rules, in which case keploy must not intercept it.
func (h *Hooks) isBypassed(ip net.IP, port uint16) bool {
	for _, rule := range h.rules {
		if rule.Port != 0 && rule.Port != uint(port) {
			continue
		}
		if rule.Host != "" {
			if ruleIP := net.ParseIP(rule.Host); ruleIP == nil || !ruleIP.Equal(ip) {
				continue
			}
		}
		if rule.Host == "" && rule.Port == 0 {
			continue
		}
		return true
	}
	return false
}

// toNetworkAddress encodes an IP the way the proxy decodes it.
//
// util.ToIP4AddressStr treats IPv4Addr as a big-endian 32-bit number, and
// util.ToIPv6AddressStr reads each of the four words most-significant byte
// first. Encoding these the other way round produces a plausible-looking address
// that dials the wrong host, so the pairing matters.
func toNetworkAddress(ip net.IP, port uint16, version uint32) (agent.NetworkAddress, error) {
	addr := agent.NetworkAddress{Port: uint32(port)}

	if v4 := ip.To4(); v4 != nil {
		addr.Version = 4
		addr.IPv4Addr = binary.BigEndian.Uint32(v4)
		return addr, nil
	}

	v6 := ip.To16()
	if v6 == nil {
		return agent.NetworkAddress{}, fmt.Errorf("%q is neither an IPv4 nor an IPv6 address", ip)
	}
	if version == 4 {
		return agent.NetworkAddress{}, fmt.Errorf("%q was reported as IPv4 but is not", ip)
	}
	addr.Version = 6
	for i := 0; i < 4; i++ {
		addr.IPv6Addr[i] = binary.BigEndian.Uint32(v6[i*4 : (i+1)*4])
	}
	return addr, nil
}
