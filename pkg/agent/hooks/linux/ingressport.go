//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/utils/plaintcp"
)

// ingressHold holds an application's moved ports from inside its bind, so a
// call to one after the app listens and before keploy's ingress forwarder
// does is not refused but served and recorded (ingress_lookup.c, keploy/ebpf).
//
// While recording, the bind hooks move the app's bind of port P to P' and
// write app_port_info[P] = P' inside its bind(2). A BPF sk_lookup program,
// attached to the app's network namespace, then steers a connection to a port
// in app_port_info to keploy's forwarder for it once it listens
// (ingressForwarders, by port), or, until then, to one park listener keploy
// opens before the app starts (ingressPark). A parked connection keeps its
// original destination as its local address, so the forwarder path serves it
// the same way.
//
// It holds only what the app's own socket would have taken: with
// structs.FlagIngressHold set (ingressHoldFlag), the bind hooks note each
// moved socket's family and address (app_port_binds, app_bind_socks,
// pending_holds: the main object's maps, which this object shares), and
// k_ingress_release, on the app's cgroup, takes a socket's note back when it
// goes. A connection to a moved port on an address or family the app did not
// bind is the kernel's, as without keploy.
type ingressHold struct {
	coll       *ebpf.Collection
	netnsLink  link.Link
	release    link.Link     // k_ingress_release on the app's cgroup
	ownConnect link.Link     // k_ingress_own_connect (sock_ops) on the app's cgroup, which holds the agent too
	agentExit  link.Link     // k_ingress_agent_exit on sched_process_exit: the agent's exit stops the steering
	agentKill  link.Link     // k_ingress_agent_killed on signal_generate: a SIGKILL sent to the agent does, as it is sent
	seed       *ebpf.Program // k_ingress_seed, a restarted agent's seeding (BPF_PROG_TEST_RUN)
	park       net.Listener  // dual-stack, MPTCP off, so a sockmap takes it
	forwarders *ebpf.Map     // ingress_forwarders, a sockhash by port
	parkMap    *ebpf.Map     // ingress_park, the park's sockmap
	yielded    *ebpf.Map     // ingress_yielded, the ports the agent yields
	appPort    *ebpf.Map     // app_port_info (P -> P'), the hold's own copy
	appBinds   *ebpf.Map     // app_port_binds, the bind hooks' notes, the hold's own copy
	appSocks   *ebpf.Map     // app_bind_socks, each noted socket's note by its cookie, the hold's own copy
	own        *ebpf.Map     // ingress_own, the agent's own connections to the application it steers for, by four-tuple
	ownSocks   *ebpf.Map     // ingress_own_socks, the agent's sockets that connect to the application, by cookie
	stats      *ebpf.Map     // ingress_lookup_stats
}

// appBindKey and appBindCount mirror struct app_bind_key and struct
// app_bind_count (keploy/ebpf headers/k_ingress_hold.h): the bind hooks'
// note of what one of the app's moved sockets bound, and how many of its
// sockets hold that note now.
type appBindKey struct {
	Port uint16   // the port the app asked for, P
	Kind uint16   // appBind*
	Addr [16]byte // network order; appBindV4's in the first 4 bytes; all zero: any address
}

type appBindCount struct {
	Lock uint32
	N    uint32
}

// The kinds of note (APP_BIND_*), by what the socket takes calls on.
const (
	appBindV4     = 4  // an IPv4 socket, or an IPv6 one on a v4-mapped address: IPv4 only
	appBindV6     = 6  // an IPv6 socket on Addr (::, any, with IPV6_V6ONLY): IPv6 only
	appBindV6Dual = 46 // an IPv6 socket on :: without IPV6_V6ONLY: IPv4 and IPv6, any address
)

// ipv6Taken is the IPv6 address a note's socket takes calls on, as the
// lookup matches it (ingress_bound_here), and whether it takes any: an IPv6
// note's address (:: for any), :: for a dual-stack one, and none for an IPv4
// note, which is an IPv4 socket's or an IPv6 socket's on a v4-mapped
// address (Java's bind of 127.0.0.1 on a dual-stack host).
func ipv6Taken(k appBindKey) (netip.Addr, bool) {
	switch k.Kind {
	case appBindV6:
		return netip.AddrFrom16(k.Addr), true
	case appBindV6Dual:
		return netip.IPv6Unspecified(), true
	}
	return netip.Addr{}, false
}

// ingressLookupStats indices (ingress_lookup.c, INGRESS_LOOKUP_*). They
// count listener lookups, not connections: the kernel looks a listener up
// for each SYN it receives, a retransmitted one again, so under load a
// connection can count more than once. What the park accepted is the
// ingress manager's own count.
const (
	ingressStatParked = iota
	ingressStatForwarded
	ingressStatOthers
	ingressStatNoPark
	ingressStatParkFailed
	ingressStatElsewhere
	ingressStatYielded
	ingressStatForwardedOver
	ingressStatOwn
	ingressStatOwnLost
	ingressStatAgentExiting
	ingressStatCount
)

// ingressStatNames name the lookup's counts, by index, for the Debug line
// at the end of recording.
var ingressStatNames = [ingressStatCount]string{
	ingressStatParked:     "parked",
	ingressStatForwarded:  "to_forwarder",
	ingressStatOthers:     "another_program_chose",
	ingressStatNoPark:     "no_park",
	ingressStatParkFailed: "park_refused",
	ingressStatElsewhere:  "not_where_the_app_bound",
	ingressStatYielded:    "yielded_to_another_listener",
	// An agent attached before this one parked it, and this one's forwarder
	// listens on the port: two keploy runs whose apps both use the port.
	ingressStatForwardedOver: "to_forwarder_over_another_agents_park",
	// The agent's own connection to the application, forwarding a steered
	// call (ingress_own): the kernel gives it the application's socket.
	ingressStatOwn: "the_agents_own_forward",
	// Not a lookup: one of the agent's own connections k_ingress_own_connect
	// could not note (an allocation failed); the agent closed it.
	ingressStatOwnLost: "own_forwards_not_noted",
	// The agent's process was exiting: its park was about to go, so the
	// connection was the kernel's (the application's own socket's).
	ingressStatAgentExiting: "agent_exiting",
}

// ingressShared are the main object's maps the ingress object uses in place
// of its own (MapReplacements): the moved ports, and the bind hooks' notes
// of what the app bound.
var ingressShared = []string{"app_port_info", "app_port_binds", "app_bind_socks", "pending_holds"}

// appPortTranslate mirrors struct app_port_translate: the proxy port and the
// port the app's bind was moved to.
type appPortTranslate struct {
	ProxyPort       uint16
	ApplicationPort uint16
}

// startIngressHold loads the ingress-lookup object beside the main one
// (sharing its app_port_info and the bind hooks' notes), opens the park
// listener, puts it in the park sockmap, attaches k_ingress_release to the
// app's cgroup (cgroupPath, where the bind hooks are) and the lookup to this
// process's network namespace, which is the application's (the host's
// natively; the app's own in Docker, which shares it). The caller then sets
// structs.FlagIngressHold in agent_info (ingressHoldFlag), before any app
// binds. It is best-effort: on any failure it logs one WARN that says what
// the window is without it, leaves the hold off (keploy forwards as it did
// before, and the bind hooks note nothing), and the agent records on. ctx
// closes the hold.
func (h *Hooks) startIngressHold(ctx context.Context, cgroupPath string) {
	hold, err := h.loadIngressHold(cgroupPath)
	if err != nil {
		h.logger.Warn("keploy cannot hold the application's port from its bind on this kernel: a call that reaches the application's port after it listens and before keploy logs \"Started ingress forwarding\" is refused (reset, through a port Docker publishes) and not recorded. "+movedOutage,
			zap.String("kernel", kernelName()), zap.Error(err),
			zap.String("next_step", "a kernel with BPF sk_lookup and cgroup/sock_release (Linux 5.9 or newer) and BPF sockmaps (CONFIG_BPF_STREAM_PARSER on Linux 5.10) closes this window; report this error to keploy otherwise"))
		return
	}
	h.ingressMu.Lock()
	h.ingress = hold
	h.ingressMu.Unlock()
	go func() {
		<-ctx.Done()
		h.closeIngressHold()
	}()
	h.logger.Debug("Holding the application's moved ports from their bind (BPF sk_lookup).")
}

func (h *Hooks) loadIngressHold(cgroupPath string) (*ingressHold, error) {
	h.objectsMutex.RLock()
	shared := map[string]*ebpf.Map{
		"app_port_info":  h.objects.AppPortInfo,
		"app_port_binds": h.objects.AppPortBinds,
		"app_bind_socks": h.objects.AppBindSocks,
		"pending_holds":  h.objects.PendingHolds,
	}
	h.objectsMutex.RUnlock()
	for _, name := range ingressShared {
		if shared[name] == nil {
			return nil, fmt.Errorf("the main object's %s is not loaded", name)
		}
	}

	spec, err := loadIngress()
	if err != nil {
		return nil, fmt.Errorf("read the ingress object: %w", err)
	}
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{MapReplacements: shared})
	if err != nil {
		return nil, fmt.Errorf("load the ingress object (BPF sk_lookup needs Linux 5.9): %w", err)
	}
	hold := &ingressHold{
		coll:       coll,
		forwarders: coll.Maps["ingress_forwarders"],
		parkMap:    coll.Maps["ingress_park"],
		yielded:    coll.Maps["ingress_yielded"],
		// The collection's own copies of the shared maps (it dups their
		// descriptors, and closes its copies with itself): every map the
		// hold reads is then closed under the hold's write lock, never by
		// unLoad's closing of the main objects during an accessor's read.
		appPort:  coll.Maps["app_port_info"],
		appBinds: coll.Maps["app_port_binds"],
		appSocks: coll.Maps["app_bind_socks"],
		own:      coll.Maps["ingress_own"],
		ownSocks: coll.Maps["ingress_own_socks"],
		stats:    coll.Maps["ingress_lookup_stats"],
	}
	hold.seed = coll.Programs["k_ingress_seed"]
	if hold.own == nil || hold.ownSocks == nil || coll.Programs["k_ingress_own_connect"] == nil || hold.seed == nil ||
		coll.Programs["k_ingress_agent_exit"] == nil || coll.Programs["k_ingress_agent_killed"] == nil || coll.Maps["ingress_agent"] == nil {
		coll.Close()
		return nil, errors.New("the ingress object lacks a program or map this agent needs: it is older than this agent")
	}

	// The park listener: plain TCP (a Go listener is MPTCP by default on a
	// kernel with MPTCP, and a sockmap refuses one), dual-stack so one socket
	// takes a moved port's IPv4 and IPv6 connections alike.
	park, err := plaintcp.Listen(context.Background(), "tcp", "[::]:0")
	if err != nil {
		coll.Close()
		return nil, fmt.Errorf("open the ingress park listener: %w", err)
	}
	hold.park = park
	// The park goes in the lookup's sockmap only once the ingress accepts on
	// it (ArmIngressPark).

	// The release program before the lookup: a note the bind hooks write
	// is taken back when its socket goes, so a port the app let go is not
	// held.
	rel, err := link.AttachCgroup(link.CgroupOptions{Path: cgroupPath, Attach: ebpf.AttachCgroupInetSockRelease, Program: coll.Programs["k_ingress_release"]})
	if err != nil {
		hold.close()
		return nil, fmt.Errorf("attach the ingress release to the cgroup %s: %w", cgroupPath, err)
	}
	hold.release = rel
	// The agent's own connections to the app it steers for are noted as the
	// kernel begins each (OwnDialControl): the agent's own process is in the
	// cgroup too, below the cgroup namespace's root that cgroupPath mounts.
	own, err := link.AttachCgroup(link.CgroupOptions{Path: cgroupPath, Attach: ebpf.AttachCGroupSockOps, Program: coll.Programs["k_ingress_own_connect"]})
	if err != nil {
		hold.close()
		return nil, fmt.Errorf("attach the ingress own-connection program to the cgroup %s: %w", cgroupPath, err)
	}
	hold.ownConnect = own
	// This agent's death stops the steering as it begins, before its memory
	// is torn down and its park closed: as a SIGKILL is sent to it
	// (k_ingress_agent_killed), or as its thread group begins to exit
	// (k_ingress_agent_exit).
	learnt, err := watchAgentExit(hold, coll)
	if err != nil {
		hold.close()
		return nil, err
	}
	if !learnt {
		h.logger.Debug("keploy could not learn its own process ID as the kernel sees it (a test run of a raw tracepoint program needs Linux 5.10): its death stops the steering of the application's ports as the first of its threads begins to exit, not as the SIGKILL is sent")
	}

	ns, err := os.Open("/proc/self/ns/net")
	if err != nil {
		hold.close()
		return nil, fmt.Errorf("open this process's network namespace: %w", err)
	}
	defer func() { _ = ns.Close() }()
	l, err := link.AttachNetNs(int(ns.Fd()), coll.Programs["k_ingress_lookup"])
	if err != nil {
		hold.close()
		return nil, fmt.Errorf("attach the ingress lookup to the network namespace: %w", err)
	}
	hold.netnsLink = l
	return hold, nil
}

// ingressHoldFlag sets structs.FlagIngressHold in agent_info's flags when the
// hold is on (both its programs attached), and structs.FlagIngressSteer with
// it when steer is set too, and clears both otherwise: the bind hooks note
// what the app binds only for a hold that reads the notes and takes them back,
// and leave the app's bind where it asked only for a lookup that steers its
// port. load steers whenever the hold is on and both post_bind hooks attached
// (they note each bind and tell the agent of it), outside observe-only mode:
// the app keeps its port, so the agent's death never takes it with it.
// Otherwise (a kernel with no sk_lookup) the bind hooks move the app's bind
// and keploy's forwarder listens on its port, the only way to take the app's
// calls there.
func ingressHoldFlag(flags uint32, held, steer bool) uint32 {
	flags &^= structs.FlagIngressHold | structs.FlagIngressSteer
	if held {
		flags |= structs.FlagIngressHold
		if steer {
			flags |= structs.FlagIngressSteer
		}
	}
	return flags
}

// closeIngressHold stops the hold, once: it takes the hold under the write
// lock, which waits out every call still using the hold's maps (each holds
// the read lock for the whole call), so later calls find no hold and none
// runs on a map closed under it.
func (h *Hooks) closeIngressHold() {
	h.ingressMu.Lock()
	hold := h.ingress
	h.ingress = nil
	h.ingressMu.Unlock()
	if hold != nil {
		hold.close()
	}
}

func (hold *ingressHold) close() {
	if hold.netnsLink != nil {
		_ = hold.netnsLink.Close()
	}
	if hold.release != nil {
		_ = hold.release.Close()
	}
	if hold.ownConnect != nil {
		_ = hold.ownConnect.Close()
	}
	if hold.agentKill != nil {
		_ = hold.agentKill.Close()
	}
	if hold.agentExit != nil {
		_ = hold.agentExit.Close()
	}
	if hold.park != nil {
		_ = hold.park.Close()
	}
	if hold.coll != nil {
		hold.coll.Close()
	}
}

// ingressAgent mirrors struct ingress_agent_t (keploy/ebpf ingress_lookup.c):
// the agent whose death stops the steering, by its PID namespace and its TGID
// there, whether it is exiting, and its TGID as the kernel sees it once
// learnt.
type ingressAgent struct {
	PidnsDev, PidnsIno uint64
	Tgid, Exiting      uint32
	KernelTgid, _      uint32
}

// watchAgentExit names this process the agent in coll's ingress_agent and
// attaches the programs that mark it exiting: k_ingress_agent_exit as its
// thread group begins to exit, and k_ingress_agent_killed as a SIGKILL is
// sent to it. A signal names the agent by its TGID as the kernel sees it,
// which this process does not know in a PID namespace of its own (a
// container): k_ingress_agent_exit, run here once (learnKernelTGID), reads
// it. It reports whether the TGID was learnt; without it the agent is marked
// only as its threads begin to exit.
func watchAgentExit(hold *ingressHold, coll *ebpf.Collection) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return false, fmt.Errorf("read this agent's PID namespace: %w", err)
	}
	m := coll.Maps["ingress_agent"]
	if err := m.Put(uint32(0), ingressAgent{PidnsDev: uint64(st.Dev), PidnsIno: st.Ino, Tgid: uint32(os.Getpid())}); err != nil {
		return false, fmt.Errorf("name this agent to the ingress lookup (ingress_agent): %w", err)
	}
	exit, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sched_process_exit", Program: coll.Programs["k_ingress_agent_exit"]})
	if err != nil {
		return false, fmt.Errorf("attach the ingress agent-exit program: %w", err)
	}
	hold.agentExit = exit
	kill, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "signal_generate", Program: coll.Programs["k_ingress_agent_killed"]})
	if err != nil {
		return false, fmt.Errorf("attach the ingress agent-kill program: %w", err)
	}
	hold.agentKill = kill
	return learnKernelTGID(coll), nil
}

// learnKernelTGID has k_ingress_agent_exit learn this agent's TGID as the
// kernel sees it: BPF_PROG_TEST_RUN runs a raw tracepoint program in the
// calling thread (Linux 5.10 or newer), where, in a live thread of the agent,
// it records that TGID and marks nothing. (Ending a thread of its own for the
// program to read at its exit is not certain in Go: a goroutine that ends
// locked to the process's main thread wedges the thread instead of ending
// it.) It reports whether the TGID was learnt.
func learnKernelTGID(coll *ebpf.Collection) bool {
	if _, err := coll.Programs["k_ingress_agent_exit"].Run(&ebpf.RunOptions{Context: []uint64{0}}); err != nil {
		return false
	}
	var a ingressAgent
	return coll.Maps["ingress_agent"].Lookup(uint32(0), &a) == nil && a.KernelTgid != 0 && a.Exiting == 0
}

// putListener puts ln's socket in m at key, by its file descriptor, as a
// sockmap or sockhash value.
func putListener(m *ebpf.Map, key uint32, ln net.Listener) error {
	if m == nil {
		return errors.New("the map is not loaded")
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("the listener is a %T, not a *net.TCPListener", ln)
	}
	rc, err := tl.SyscallConn()
	if err != nil {
		return err
	}
	var uerr error
	if err := rc.Control(func(fd uintptr) { uerr = m.Update(key, uint64(fd), ebpf.UpdateAny) }); err != nil {
		return err
	}
	return uerr
}

// IngressParkListener is the park listener keploy holds moved ports with, or
// nil when it holds none (the kernel has no sk_lookup, or a step failed): the
// ingress manager serves what the park accepts, each connection to the port
// its local address names. It satisfies incoming's IngressPortHolder.
func (h *Hooks) IngressParkListener() net.Listener {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	if h.ingress == nil {
		return nil
	}
	return h.ingress.park
}

// MovedPort is the port the app's bind of orig was moved to (app_port_info),
// and whether it is known: a parked connection to orig is forwarded there.
func (h *Hooks) MovedPort(orig uint16) (uint16, bool) {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.appPort == nil {
		return 0, false
	}
	var v appPortTranslate
	if err := hold.appPort.Lookup(orig, &v); err != nil || v.ApplicationPort == 0 {
		return 0, false
	}
	return v.ApplicationPort, true
}

// RegisterIngressForwarder puts keploy's forwarder for origPort in the
// sockhash, so the lookup steers the port's connections to it instead of the
// park once it listens. A no-op when nothing is held.
func (h *Hooks) RegisterIngressForwarder(origPort uint16, ln net.Listener) {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.forwarders == nil {
		return
	}
	if err := putListener(hold.forwarders, uint32(origPort), ln); err != nil {
		h.logger.Debug("could not register keploy's ingress forwarder for the lookup; the park serves the port until the forwarder's own listen does",
			zap.Uint16("port", origPort), zap.Error(err))
	}
	// The forwarder listens on the port, so no other listener holds it now:
	// take back a yield an earlier forwarder's failed listen left.
	if hold.yielded != nil {
		if err := hold.yielded.Delete(uint32(origPort)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			h.logger.Debug("could not take back the ingress hold's yield of the port", zap.Uint16("port", origPort), zap.Error(err))
		}
	}
}

// YieldIngressPort stops holding origPort: keploy's forwarder could not
// listen there, as another listener (not keploy's) holds it, so the lookup
// leaves the port's calls to the kernel, and so to that listener, as without
// keploy. A no-op when nothing is held.
func (h *Hooks) YieldIngressPort(origPort uint16) {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil {
		return
	}
	if hold.yielded == nil {
		h.logger.Warn("the ingress hold cannot yield the port to the listener that holds it: its yield map is not loaded", zap.Uint16("port", origPort))
		return
	}
	if err := hold.yielded.Put(uint32(origPort), uint8(1)); err != nil {
		h.logger.Warn("the ingress hold could not yield the port to the listener that holds it; keploy keeps taking the port's calls", zap.Uint16("port", origPort), zap.Error(err))
	}
}

// ArmIngressPark puts the park in the lookup's sockmap: from then on the
// lookup steers the calls it holds to it. The ingress manager calls it as its
// accept loop on the park starts (serveParkedConnections): a park nothing
// accepts on is never steered to, as its calls would complete their handshake
// and hang. Until then the lookup has no park, and a call is the kernel's: an
// agent restarted while the application runs, whose recorder has not
// connected yet, leaves the application's port to the application. A no-op
// when nothing is held.
func (h *Hooks) ArmIngressPark() error {
	if err := h.armIngressPark(); err != nil {
		return err
	}
	// keploy records the app's calls again from here: what the app opened
	// while no agent ran is counted (unseen.go).
	h.censusUnseen()
	return nil
}

func (h *Hooks) armIngressPark() error {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil {
		return nil
	}
	if err := putListener(hold.parkMap, 0, hold.park); err != nil {
		return fmt.Errorf("put the park listener in the sockmap: %w", err)
	}
	return nil
}

// StopIngressPark takes the park out of the lookup's sockmap and closes it,
// for an ingress manager whose park accept loop stopped: a call to a moved
// port that no forwarder takes is then refused, as without the hold, never
// held for a park nothing accepts. A no-op when nothing is held.
func (h *Hooks) StopIngressPark() {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil {
		return
	}
	if hold.parkMap != nil {
		if err := hold.parkMap.Delete(uint32(0)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			h.logger.Debug("could not take the ingress park out of the lookup's sockmap; closing it does", zap.Error(err))
		}
	}
	if hold.park != nil {
		_ = hold.park.Close()
	}
}

// UnregisterIngressForwarder takes keploy's forwarder for origPort out of the
// sockhash. A closed socket leaves a sockhash by itself, so this only tidies
// the entry; a no-op when nothing is held.
func (h *Hooks) UnregisterIngressForwarder(origPort uint16) {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.forwarders == nil {
		return
	}
	if err := hold.forwarders.Delete(uint32(origPort)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		h.logger.Debug("could not unregister keploy's ingress forwarder", zap.Uint16("port", origPort), zap.Error(err))
	}
}

// AppIPv6Binds is the IPv6 addresses the app's moved sockets of orig take
// calls on now, by the bind hooks' notes (app_port_binds), one per note: an
// IPv6 socket's address (:: for any, with IPV6_V6ONLY or without); none for
// an IPv4 socket, or an IPv6 one on a v4-mapped address, which takes IPv4
// only. The lookup holds the port's IPv6 calls only for these, so the
// ingress manager checks only these for a listener keploy does not own.
// Nothing when nothing is held; on an error (the notes cannot be read), what
// it read with the error.
func (h *Hooks) AppIPv6Binds(orig uint16) ([]netip.Addr, error) {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil {
		return nil, nil
	}
	if hold.appBinds == nil {
		return nil, errors.New("the ingress hold's notes map is not loaded")
	}
	var (
		addrs []netip.Addr
		k     appBindKey
		c     appBindCount
	)
	it := hold.appBinds.Iterate()
	for it.Next(&k, &c) {
		if k.Port != orig || c.N == 0 {
			continue
		}
		if a, ok := ipv6Taken(k); ok {
			addrs = append(addrs, a)
		}
	}
	return addrs, it.Err()
}

// IngressHeld reports whether keploy holds the application's moved ports from
// their bind: the ingress manager's fallback (binding the port as soon as the
// bind event arrives, which only shrinks the window) runs when it does not.
func (h *Hooks) IngressHeld() bool {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	return h.ingress != nil
}

// IngressLookupCounts is what the lookup did with the listener lookups for a
// moved port, by name (ingressStatNames), or nil when nothing is held:
// lookups, not connections, for the Debug line at the end of recording.
func (h *Hooks) IngressLookupCounts() map[string]uint64 {
	h.ingressMu.RLock()
	defer h.ingressMu.RUnlock()
	hold := h.ingress
	if hold == nil || hold.stats == nil {
		return nil
	}
	counts := make(map[string]uint64, ingressStatCount)
	for i, name := range ingressStatNames {
		var n uint64
		if err := hold.stats.Lookup(uint32(i), &n); err != nil {
			return nil
		}
		counts[name] = n
	}
	return counts
}
