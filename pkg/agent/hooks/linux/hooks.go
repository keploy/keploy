//go:build linux

// Package hooks provides functionality for managing hooks.
package linux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:       logger,
		sess:         agent.NewSessions(),
		m:            sync.Mutex{},
		proxyIP4:     "127.0.0.1",
		proxyIP6:     [4]uint32{0000, 0000, 0000, 0001},
		proxyPort:    cfg.ProxyPort,
		dnsPort:      cfg.DNSPort,
		conf:         cfg,
		objectsMutex: sync.RWMutex{},
	}
}

type Hooks struct {
	// Common fields shared across all platforms
	logger    *zap.Logger
	sess      *agent.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32
	conf      *config.Config
	m         sync.Mutex

	// Linux-specific fields
	objectsMutex sync.RWMutex // Protects eBPF objects during load/unload operations
	// proxyMarked is set once MarkProxyListener has marked the proxy's
	// listener: proxy_conns_established counts nothing before (proxyconns.go).
	proxyMarked atomic.Bool
	// app is the application's side of that count (appconns.go), started
	// once, by the first mark (appOnce); appMu guards its lost count.
	app     appConnState
	appOnce sync.Once
	appMu   sync.Mutex
	// unix is the AF_UNIX connect report (unixconn.go), under unixMu.
	unixMu sync.Mutex
	unix   *unixConn
	// udp is the proven switch's UDP watch (udpwatch.go), under udpMu; nil
	// with the switch off.
	udpMu sync.Mutex
	udp   *udpWatch
	// ready is the proven switch's readiness watch (readywatch.go), under
	// readyMu; nil with the switch off.
	readyMu sync.Mutex
	ready   *readyWatch
	// clientPIDNS is the pid namespace the registered client said it runs
	// in (config.Agent.ClientPIDNS), beside its pid in the kernel's
	// registration, under clientMu: the readiness watch reads that pid
	// through it (utils.ClientPIDIn).
	clientMu    sync.Mutex
	clientPIDNS string
	// eBPF C shared maps
	clientRegistrationMap *ebpf.Map
	agentRegistrationMap  *ebpf.Map
	redirectProxyMap      *ebpf.Map

	// eBPF C shared objectsobjects
	// ebpf objects and events
	socket      link.Link
	processExit link.Link
	connect4    link.Link
	udp4Sendmsg link.Link
	udp4Recvmsg link.Link
	gp4         link.Link
	connect6    link.Link
	udp6Sendmsg link.Link
	udp6Recvmsg link.Link
	gp6         link.Link
	objects     bpfObjects
	cgBind4     link.Link
	cgBind6     link.Link
	cgPostBind4 link.Link
	cgPostBind6 link.Link
	// kernelPortAlloc records that BOTH post_bind hooks attached, which is
	// the precondition for telling bind4/6 to let the kernel pick the
	// application's relocated port (structs.FlagKernelPortAlloc).
	kernelPortAlloc bool

	BindEvents *ebpf.Map
	sockops    link.Link
	// sockCreate is k_sock_create's link: sys_enter_socket's backstop,
	// attached where it is.
	sockCreate link.Link
	// skips reads the runs of the hooks' programs the kernel skipped
	// (skipwatch.go), from their load on.
	skips *skipWatch
}

func (h *Hooks) Load(ctx context.Context, opts agent.HookCfg, setupOpts config.Agent) error {

	h.sess.Set(uint64(0), &agent.Session{
		ID: uint64(0), // need to check this one
	})
	err := h.load(ctx, opts, setupOpts)
	if err != nil {
		return privilegeFailure(err)
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// A process the kernel refuses a place in the PID maps is not tracked,
	// and nothing else would say so (pidmaps.go). Its last read, as ctx
	// ends, comes before unLoad closes the counters.
	watched := make(chan struct{})
	g.Go(func() error {
		defer close(watched)
		defer utils.Recover(h.logger)
		watchPIDMapRefusals(ctx, h.logger, h.pidMapCounts, pidMapPollInterval)
		return nil
	})

	// The entries a process leaves in the PID maps when the kernel skips
	// k_process_exit's run for its exit: deleted once the process is gone,
	// whenever the program's skip count grew (skipwatch.go).
	swept := make(chan struct{})
	g.Go(func() error {
		defer close(swept)
		defer utils.Recover(h.logger)
		t := time.NewTicker(pidMapPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				h.sweepIfExitsSkipped()
			}
		}
	})

	// The application's AF_UNIX connects to sockets no member of it owns
	// (unixconn.go): calls keploy's proxy never sees.
	h.startUnixConn(ctx)

	g.Go(func() error {
		defer utils.Recover(h.logger)
		<-ctx.Done()
		<-watched
		<-swept
		h.unLoad(ctx, opts)

		// deleting in order to free the memory on shutdown.
		h.sess.Delete(uint64(0))
		return nil
	})

	return nil
}

func (h *Hooks) attachIngress(cGroupPath string, objs bpfObjects) error {
	h.BindEvents = objs.BindEvents
	cg4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet4Bind,
		Program: objs.K_bind4,
	})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the bind4 cgroup hook")
		return err
	}
	h.cgBind4 = cg4

	cg6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet6Bind,
		Program: objs.K_bind6,
	})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the bind6 cgroup hook")
		return err
	}
	h.cgBind6 = cg6

	// post_bind4/6 are what make the kernel-allocated relocation port
	// usable: bind4/6 set user_port = 0 so the kernel's allocator picks a
	// port that is genuinely free (BPF cannot determine that — see
	// find_free_port's comment in keploy/ebpf), and these hooks read the
	// assigned port back out and publish the bind event.
	//
	// Non-fatal on purpose, and both-or-nothing: the flag below is only
	// set when both attach, and without the flag bind4/6 keep their old
	// guess-a-port behaviour. So a cgroup setup that rejects them records
	// exactly as it did before rather than failing startup, while everyone
	// else stops hitting "That port is already in use" on the relocated
	// port. This covers ATTACH failure only — a program the kernel refuses
	// to LOAD already fails LoadAndAssign above and aborts the agent, the
	// same as every other program in this object.
	pb4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet4PostBind,
		Program: objs.K_postBind4,
	})
	if err != nil {
		h.logger.Warn("failed to attach the post_bind4 cgroup hook; falling back to BPF-side port selection for the application's relocated port (rare: the app may fail to start with 'address already in use')", zap.Error(err))
	} else {
		pb6, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cGroupPath,
			Attach:  ebpf.AttachCGroupInet6PostBind,
			Program: objs.K_postBind6,
		})
		if err != nil {
			// Detach the v4 half too. With the flag off it would never do
			// any work, and leaving it attached runs a guaranteed no-op
			// program on every AF_INET bind on the machine for the whole
			// session.
			if cerr := pb4.Close(); cerr != nil {
				h.logger.Debug("failed to detach post_bind4 after post_bind6 failed", zap.Error(cerr))
			}
			h.logger.Warn("failed to attach the post_bind6 cgroup hook; falling back to BPF-side port selection for the application's relocated port (rare: the app may fail to start with 'address already in use')", zap.Error(err))
		} else {
			h.cgPostBind4 = pb4
			h.cgPostBind6 = pb6
			h.kernelPortAlloc = true
		}
	}

	h.logger.Debug("Attached ingress redirection hooks.",
		zap.Bool("kernel_port_alloc", h.kernelPortAlloc))
	return nil
}

// hookLoadOptions are the options load() loads hookSpec() with: no verifier
// log. The kernel formats the log while it verifies, and at the instruction
// and branch level the agent asked for, that log (and verifying again each
// program whose log outgrew its buffer: cilium/ebpf grows the buffer on
// ENOSPC and loads the program again) was nearly all of the load:
// keploy's hook object (bpf_x86_bpfel.o) loaded in 3.5-6.5 s with it and in
// 0.09-0.24 s without on 5.10.240, and in 0.84-1.5 s against 0.09-0.11 s on
// 6.12.40, on 4-vCPU KVM guests, every agent start, and for longer on a host
// short of CPU. A program the verifier rejects is loaded again by
// cilium/ebpf with a log (LogLevelBranch, in a buffer it grows until the log
// fits), so a rejection's *ebpf.VerifierError still carries the verifier's
// account of the failing path, which load() prints.
func hookLoadOptions() *ebpf.CollectionOptions {
	return &ebpf.CollectionOptions{}
}

// hookSpec is the collection load() loads into the kernel: the compiled
// objects, with k_sockops's program and attach types set.
func hookSpec() (*ebpf.CollectionSpec, error) {
	spec, err := loadBpf()
	if err != nil {
		return nil, err
	}

	programs := []struct {
		name  string
		pType ebpf.ProgramType
		aType ebpf.AttachType
	}{
		{"k_sockops", ebpf.SockOps, ebpf.AttachCGroupSockOps},
	}

	for _, p := range programs {
		if prog, ok := spec.Programs[p.name]; ok {
			prog.Type = p.pType
			prog.AttachType = p.aType
		}
	}
	shrinkUnswitchedMaps(spec)
	return spec, nil
}

// unswitchedMaps are the maps only the proven switch's UDP and readiness
// watches use, which
// the kernel allocates whole at load (an LRU hash, a ring buffer): with the
// switch off nothing uses them, so they are loaded at their smallest, and
// the watch costs nothing (udpwatch.go).
var unswitchedMaps = []string{"udp_peer_first", "udp_member_addrs", "udp_reply_events", "ready_socks", "ready_mains", "ready_live", "ready_listen_events", "ready_seen", "ready_holders"}

// shrinkUnswitchedMaps loads unswitchedMaps at their smallest when the
// proven switch is off: one entry, or one page for a ring buffer.
func shrinkUnswitchedMaps(spec *ebpf.CollectionSpec) {
	if provenswitch.On() {
		return
	}
	for _, name := range unswitchedMaps {
		m, ok := spec.Maps[name]
		if !ok {
			continue
		}
		if m.Type == ebpf.RingBuf {
			m.MaxEntries = uint32(os.Getpagesize())
		} else {
			m.MaxEntries = 1
		}
	}
}

func (h *Hooks) load(ctx context.Context, opts agent.HookCfg, setupOpts config.Agent) error {
	// The pass-through list is checked before anything is loaded: a run whose
	// ports the kernel cannot hold is refused at once, with nothing to undo.
	clientInfo, err := clientInfoFor(ctx, setupOpts, opts.Rules)
	if err != nil {
		return err
	}

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		utils.LogError(h.logger, err, "failed to lock memory for eBPF resources")
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}

	spec, err := hookSpec()
	if err != nil {
		utils.LogError(h.logger, err, "failed to load BPF spec")
		return err
	}

	// Now load and assign into the kernel with the corrected spec.
	//
	// BPF_PROG_LOAD is uninterruptible while it waits in the kernel, so a load
	// stuck there leaves the agent with no log output at all and the container
	// merely "not ready". watchedCall makes that state describe itself, from
	// the thread making the call. Scoped to this one call: the rest of load()
	// attaches tracepoints and resolves cgroups, and a watchdog left running
	// across that would blame BPF_PROG_LOAD for whatever else was slow.
	err = watchedCall(h.logger, "bpf(BPF_PROG_LOAD) via ebpf.CollectionSpec.LoadAndAssign", func() error {
		return spec.LoadAndAssign(&objs, hookLoadOptions())
	})
	if err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			fmt.Printf("VERIFIER FAILURE:\n%s\n", strings.Join(ve.Log, "\n"))
		} else {
			fmt.Printf("SYSCALL FAILURE: %v\n", err)
		}
		return err
	}
	//getting all the ebpf maps with proper synchronization
	h.objectsMutex.Lock()
	h.clientRegistrationMap = objs.KeployClientRegistrationMap
	h.agentRegistrationMap = objs.KeployAgentRegistrationMap
	h.objects = objs
	// The runs of these programs the kernel skips, from here on, before
	// any attaches (skipwatch.go).
	h.skips = newSkipWatch(h.logger, objs.SocketClassifyFailures)
	h.skips.watch(programsByName(&objs.bpfPrograms))
	h.objectsMutex.Unlock()
	// ---------------

	// sys_enter_socket and k_sock_create add to target_namespace_pids and
	// excluded_pids (a classification replaces a dead process's entries
	// under the TGID, track_process); k_process_exit takes a process out of
	// both when its last thread exits, and the sweep (skipwatch.go) what a
	// skipped run of it left. Without it the maps only grow, and once
	// target_namespace_pids is full (10240 TGIDs: seconds of process churn,
	// and it takes every process in the agent's PID namespace, the whole host
	// for a native agent) no new process is intercepted, so its calls go to the real dependency with
	// nothing recorded or mocked. (A refused insert is counted and reported,
	// pidmaps.go; with this hook it takes more live processes than the map
	// holds.) Attached in every mode,
	// DaemonSet and per-container targeting included: it only removes TGIDs
	// of processes that no longer exist, whoever added them. A raw
	// tracepoint, so it needs no tracefs. Attached before sys_enter_socket so
	// that no process it adds can exit unseen.
	processExit, err := link.AttachRawTracepoint(link.RawTracepointOptions{
		Name:    "sched_process_exit",
		Program: objs.K_processExit,
	})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the sched_process_exit hook that removes exited processes from the PID maps")
		return err
	}
	h.processExit = processExit

	// In DaemonSet mode the CRD-scoped SessionReconciler is the SOLE owner of
	// target_namespace_pids — it arms exactly the recorded pods' TGIDs, and the
	// proxyless capture reads that map. The sys_enter_socket tracepoint, by
	// contrast, AUTO-REGISTERS any process doing socket() that passes the
	// in-eBPF namespace check into target_namespace_pids. On a hostPID +
	// hostNetwork DS agent that namespace check matches HOST processes
	// (init/PID 1, node daemons, short-lived forks), so the tracepoint pollutes
	// the shared map and the capture records non-target host traffic — the
	// "test-set-0 / node-daemon traffic" leak. The reconciler-armed TGIDs
	// already capture the app's ingress (no auto-detection needed), so skip the
	// tracepoint entirely in DS mode.
	// Low-latency per-container targeting (KEPLOY_TARGET_CONTAINERS set by the
	// webhook) is the same situation as DaemonSet: the enterprise exec-barrier
	// owns target_namespace_pids and scopes it to the TARGET container's cgroup.
	// sys_enter_socket would auto-register EVERY process in the pod's PID ns
	// (both target and sibling containers, keyed by host TGID), leaking sibling
	// traffic past the cgroup targeting — the same pollution the DS skip prevents.
	if os.Getenv("KEPLOY_DAEMONSET_ENABLED") == "true" || os.Getenv("KEPLOY_TARGET_CONTAINERS") != "" {
		h.logger.Info("daemonset/targeting mode: skipping sys_enter_socket PID auto-registration (target_namespace_pids is owned/scoped externally)")
	} else {
		// A perf-event attach locks the kernel's tracing event_mutex
		// (perf_trace_init), which every other tracer's attach and detach on
		// the host holds too: watched like the load.
		var socket link.Link
		err := watchedCall(h.logger, "perf_event_open (tracepoint syscalls/sys_enter_socket) via link.Tracepoint", func() error {
			var err error
			socket, err = link.Tracepoint("syscalls", "sys_enter_socket", objs.SyscallProbeEntrySocket, nil)
			return err
		})
		if err != nil {
			utils.LogError(h.logger, err, "failed to attach the tracepoint hook on sys_socket")
			return tracepointFailure(err, tracefsMounted())
		}
		h.socket = socket
	}

	h.redirectProxyMap = objs.RedirectProxyMap
	h.objects = objs

	// If an eBPF loaded hook is registered, provide a lookup so it can
	// access the maps it needs.
	if agent.EbpfLoadedHook != nil {
		if err := agent.EbpfLoadedHook(objs.lookupMap); err != nil {
			utils.LogError(h.logger, err, "EbpfLoadedHook failed; verify hook configuration/capabilities or disable the hook if not required")
			return err
		}
	}

	// Get the first-mounted cgroupv2 path.
	cGroupPath, err := agent.DetectCgroupPath(h.logger)
	if err != nil {
		utils.LogError(h.logger, err, "failed to detect the cgroup path")
		return err
	}

	// Detect and clean orphaned BPF programs from crashed previous
	// runs. This must happen BEFORE attaching our new programs to
	// avoid "program already attached" failures. Safety is enforced
	// inside the helper: it name-matches keployBPFProgNames and
	// gates on HasOtherKeployProcesses(os.Getpid()) so it cannot
	// detach programs belonging to a live keploy sibling. The helper
	// also emits its own Info log when it cleans anything, so no
	// log is needed here.
	DetectAndCleanOrphanedCgroupPrograms(h.logger, cGroupPath)

	h.logger.Debug("Attaching SockOps...")
	sockops, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupSockOps,
		Program: objs.K_sockops,
	})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach SockOps")
		return err
	}
	h.sockops = sockops

	// sys_enter_socket's backstop, where sys_enter_socket is attached: the
	// kernel skips runs of a tracepoint's program, and from 7.2 a syscall
	// tracepoint's programs run preemptible, so under load it misses
	// processes' socket() calls, and such a process is no target.
	// k_sock_create, a cgroup program the kernel runs for every inet
	// socket's creation, classifies by the same rule before the process's
	// first connect, send or bind reaches pid_filter (keploy/ebpf
	// keploy_ebpf.c).
	if h.socket != nil {
		sc, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cGroupPath,
			Attach:  ebpf.AttachCGroupInetSockCreate,
			Program: objs.K_sockCreate,
		})
		if err != nil {
			utils.LogError(h.logger, err, "failed to attach the sock_create cgroup hook that classifies a process sys_enter_socket missed")
			return err
		}
		h.sockCreate = sc
	}

	// Ingress is captured while recording, and in mock mode whenever requests are on, so a replay can
	// compare the app's actual responses with the recorded cases. In mock mode the wrapped process is a
	// test runner, so a failure to attach here only means no requests are recorded; the egress capture
	// the run depends on goes ahead.
	if (opts.Mode == models.MODE_RECORD && !setupOpts.MockMode) || (setupOpts.MockMode && setupOpts.RecordRequests && len(agent.GetPortToSendToKernel(ctx, opts.Rules)) > 0) {
		if err := h.attachIngress(cGroupPath, objs); err != nil {
			if !setupOpts.MockMode {
				return err
			}
			h.logger.Warn("the ingress hooks did not attach; the app's incoming requests will not be recorded as test cases", zap.Error(err))
		}
	}

	c4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: objs.K_connect4,
	})

	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the connect4 cgroup hook")
		return err
	}
	h.connect4 = c4

	udp4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupUDP4Sendmsg,
		Program: objs.K_udp4Sendmsg,
	})
	if err != nil {
		h.logger.Error("failed to attach the udp4 sendmsg cgroup hook (unconnected UDP DNS won't be intercepted)", zap.Error(err))
	} else {
		h.udp4Sendmsg = udp4
	}

	// Partner to udp4_sendmsg: rewrites the source of the DNS reply back to
	// the original nameserver address. Without it, strict DNS clients (RFC
	// 5452 source validation — dnspython, raw recvfrom-based clients, glibc
	// res_send on the unconnected path) see the reply coming from keploy's
	// DNS port, reject it as spoofed, and retransmit until they time out
	// with EAI_AGAIN / "Temporary failure in name resolution".
	//
	// Attach is non-fatal on purpose. `cgroup/recvmsg4` requires kernel
	// >= 5.11; if it is not available the non-strict client paths (glibc
	// res_send's connected branch, Go's pure resolver, dig, getent) still
	// work via the existing getpeername4 hook, so a missing recvmsg4
	// attach is a strict-subset degradation of the pre-fix behaviour
	// rather than a regression.
	udp4Recv, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupUDP4Recvmsg,
		Program: objs.K_udp4Recvmsg,
	})
	if err != nil {
		h.logger.Warn("failed to attach the udp4 recvmsg cgroup hook (requires kernel >= 5.11; strict DNS clients may fail with EAI_AGAIN, other clients unaffected)", zap.Error(err))
	} else {
		h.udp4Recvmsg = udp4Recv
	}

	gp4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCgroupInet4GetPeername,
		Program: objs.K_getpeername4,
	})

	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the GetPeername4 cgroup hook")
		return err
	}
	h.gp4 = gp4

	c6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet6Connect,
		Program: objs.K_connect6,
	})

	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the connect6 cgroup hook")
		return err
	}
	h.connect6 = c6

	udp6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupUDP6Sendmsg,
		Program: objs.K_udp6Sendmsg,
	})
	if err != nil {
		h.logger.Error("failed to attach the udp6 sendmsg cgroup hook (unconnected UDP DNS won't be intercepted)", zap.Error(err))
	} else {
		h.udp6Sendmsg = udp6
	}

	// IPv6 partner to udp6_sendmsg — see the equivalent comment on the
	// udp4_recvmsg attach above for the full rationale. Same attach-is-
	// non-fatal trade-off: strict DNS clients lose source-address rewriting
	// if the hook is missing, but every other client path continues to
	// work via getpeername6.
	udp6Recv, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupUDP6Recvmsg,
		Program: objs.K_udp6Recvmsg,
	})
	if err != nil {
		h.logger.Warn("failed to attach the udp6 recvmsg cgroup hook (requires kernel >= 5.11; strict DNS clients may fail with EAI_AGAIN, other clients unaffected)", zap.Error(err))
	} else {
		h.udp6Recvmsg = udp6Recv
	}

	gp6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCgroupInet6GetPeername,
		Program: objs.K_getpeername6,
	})

	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the GetPeername6 cgroup hook")
		return err
	}
	h.gp6 = gp6

	h.logger.Debug("keploy initialized and probes added to the kernel.")

	var agentInfo = structs.AgentInfo{}
	agentInfo.KeployAgentNsPid = uint32(os.Getpid())
	agentInfo.KeployAgentInode, agentInfo.KeployAgentDev, err = GetSelfInodeNumber()
	if err != nil {
		h.logger.Error("failed to read inode and dev of /proc/self/ns/pid for the keploy process; ensure /proc is mounted and accessible (not running under a restrictive procfs / sandbox that hides namespace files)", zap.Error(err))
		return err
	}
	agentInfo.IsDocker = 0
	if opts.IsDocker {
		agentInfo.IsDocker = 1
	}
	agentInfo.DNSPort = int32(setupOpts.DnsPort)

	// When the recording started in this PID namespace, for the eBPF
	// tracepoint to exclude the processes that were there before it: this
	// agent's load time, unless an agent before it in the same PID namespace
	// loaded first (recording_start.go).
	if setupOpts.IsDocker {
		agentInfo.RecordingStartTime = recordingStart(h.logger, hostRecordingStart())
	}

	// Allow downstream builds to mutate AgentInfo (e.g. set the Flags
	// slot consumed by the BPF cgroup hooks) before it is written.
	if agent.AgentInfoCustomizer != nil {
		agent.AgentInfoCustomizer(&agentInfo)
	}

	// OR'd in after the customizer so a downstream build setting its own
	// flags cannot accidentally clear it, and only when both post_bind
	// hooks are attached — bind4/6 must not hand the port choice to the
	// kernel unless something is listening for the kernel's answer.
	if h.kernelPortAlloc {
		agentInfo.Flags |= structs.FlagKernelPortAlloc
	}
	agentInfo.Flags = classifyFlag(agentInfo.Flags, h.socket != nil)
	// Under the proven switch, the application's non-DNS UDP is watched for
	// replies, and QUIC refused (udpwatch.go).
	udpFlags, err := h.startProvenUDP(cGroupPath)
	if err != nil {
		utils.LogError(h.logger, err, "failed to start the proven switch's UDP watch")
		return err
	}
	agentInfo.Flags &^= structs.FlagProvenUDP
	agentInfo.Flags |= udpFlags
	// Under the proven switch, the application's listens and the progress
	// of its start, for keploy test's readiness (readywatch.go).
	readyFlags, err := h.startProvenReady(cGroupPath, opts.IsDocker, opts.Mode)
	if err != nil {
		utils.LogError(h.logger, err, "failed to start the proven switch's readiness watch")
		return err
	}
	agentInfo.Flags &^= structs.FlagProvenReady
	agentInfo.Flags |= readyFlags

	// A client keploy cannot register would have none of its pass-through
	// ports bypassed, so the start fails here rather than going on without them.
	if err := h.registerClient(clientInfo, setupOpts); err != nil {
		return fmt.Errorf("failed to register the client's pass-through ports with the kernel: %w", err)
	}
	proxyInfo, err := h.resolveProxyInfo(ctx, setupOpts, opts)
	if err != nil {
		return err
	}
	h.logger.Debug("proxy ips", zap.String("ipv4", h.proxyIP4), zap.Any("ipv6", h.proxyIP6))

	agentInfo.Proxy = proxyInfo
	err = h.SendAgentInfo(agentInfo)
	if err != nil {
		h.logger.Error("failed to send agent info to the ebpf program", zap.Error(err))
		return err
	}

	// With the flag set: the kernel records the agent's own mount namespace
	// (its container), so what runs in its container is not the application's progress
	// (readywatch.go). Failing, the watch says so at each read.
	h.readyMu.Lock()
	ready := h.ready
	h.readyMu.Unlock()
	if ready != nil {
		if err := ready.selfRegistered(); err != nil {
			h.logger.Warn("the readiness watch could not record the agent's own mount namespace; keploy test cannot tell whether the application is ready", zap.Error(err))
		}
	}

	return nil
}

// resolveProxyInfo is GetProxyInfo, keeping the addresses it picked: where the
// kernel is told to send the application's connections is where the
// application reaches the proxy, and so what ProxyIPv4 reports. Looking the
// address up a second time could come back with another interface's.
func (h *Hooks) resolveProxyInfo(ctx context.Context, setupOpts config.Agent, opts agent.HookCfg) (structs.ProxyInfo, error) {
	info, err := h.GetProxyInfo(ctx, setupOpts, opts)
	if err != nil {
		return structs.ProxyInfo{}, err
	}
	var ip4 [4]byte
	binary.BigEndian.PutUint32(ip4[:], info.IP4)
	h.proxyIP4 = net.IP(ip4[:]).String()
	h.proxyIP6 = info.IP6
	return info, nil
}

// ProxyIPv4 is the IPv4 address the application reaches the proxy at, once
// Load has returned: loopback for a native agent, which shares the
// application's network namespace, and for one started with --is-docker, its
// container's own address, as GetProxyInfo decides.
func (h *Hooks) ProxyIPv4() string {
	return h.proxyIP4
}

// Compile-time proof that the Linux hooks report where the proxy is reached:
// were the method renamed, the agent's type assertion would stop matching
// and an --is-docker agent's DNS server would silently answer loopback.
var _ agent.ProxyAddressReporter = (*Hooks)(nil)

func (h *Hooks) unLoad(_ context.Context, opts agent.HookCfg) {
	// closing all events
	//other

	if h.socket != nil {
		if err := h.socket.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the tracepoint hook on sys_socket")
		}
	}

	if h.processExit != nil {
		if err := h.processExit.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the sched_process_exit hook")
		}
	}

	if h.connect4 != nil {
		if err := h.connect4.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the connect4")
		}
	}

	if h.udp4Sendmsg != nil {
		if err := h.udp4Sendmsg.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the udp4 sendmsg hook")
		}
	}

	if h.udp4Recvmsg != nil {
		if err := h.udp4Recvmsg.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the udp4 recvmsg hook")
		}
	}

	if h.gp4 != nil {
		if err := h.gp4.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the gp4")
		}
	}

	if h.connect6 != nil {
		if err := h.connect6.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the connect6")
		}
	}

	if h.udp6Sendmsg != nil {
		if err := h.udp6Sendmsg.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the udp6 sendmsg hook")
		}
	}
	if h.udp6Recvmsg != nil {
		if err := h.udp6Recvmsg.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the udp6 recvmsg hook")
		}
	}
	if h.gp6 != nil {
		if err := h.gp6.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the gp6")
		}
	}

	if h.sockops != nil {
		h.sockops.Close()
	}
	if h.sockCreate != nil {
		if err := h.sockCreate.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the sock_create hook")
		}
	}
	// What each program skipped in the session, read before they close.
	if h.skips != nil {
		h.objectsMutex.RLock()
		h.skips.close()
		h.objectsMutex.RUnlock()
	}

	// The UDP watch reads the objects' ring buffer: closed before them.
	// The watch stays, failed: a read after the hooks unloaded is unknown,
	// never 0.
	h.udpMu.Lock()
	if h.udp != nil {
		h.udp.fail(errors.New("the UDP watch was closed with the hooks"))
		h.udp.close()
	}
	h.udpMu.Unlock()
	h.readyMu.Lock()
	if h.ready != nil {
		h.ready.fail(errors.New("the readiness watch was closed with the hooks"))
		h.ready.close()
	}
	h.readyMu.Unlock()

	// Close eBPF objects with proper synchronization
	h.objectsMutex.Lock()
	h.stopAppConns()
	if err := h.objects.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the objects")
	}
	h.objectsMutex.Unlock()

	if h.cgBind4 != nil || h.cgBind6 != nil {
		if h.cgBind4 != nil {
			if err := h.cgBind4.Close(); err != nil {
				utils.LogError(h.logger, err, "failed to close the cgBind4")
			}
		}
		if h.cgBind6 != nil {
			if err := h.cgBind6.Close(); err != nil {
				utils.LogError(h.logger, err, "failed to close the cgBind6")
			}
		}
		if h.cgPostBind4 != nil {
			if err := h.cgPostBind4.Close(); err != nil {
				utils.LogError(h.logger, err, "failed to close the cgPostBind4")
			}
		}
		if h.cgPostBind6 != nil {
			if err := h.cgPostBind6.Close(); err != nil {
				utils.LogError(h.logger, err, "failed to close the cgPostBind6")
			}
		}

	}
	h.logger.Debug("eBPF resources released successfully...")

}

// registerClient hands the kernel the client info clientInfoFor built.
func (h *Hooks) registerClient(clientInfo structs.ClientInfo, opts config.Agent) error {
	h.logger.Debug("Registering the client Info with keploy")
	h.clientMu.Lock()
	h.clientPIDNS = opts.ClientPIDNS
	h.clientMu.Unlock()

	return h.SendClientInfo(clientInfo)
}

// clientInfoFor is the client info registerClient hands the kernel: the mode,
// the client's pid, and the pass-through list (kernelPassThroughList), with
// keploy's own ports in it before the ports of the user's bypassRules.
func clientInfoFor(ctx context.Context, opts config.Agent, rules []models.BypassRule) (structs.ClientInfo, error) {
	clientInfo := structs.ClientInfo{}

	switch opts.Mode {
	case models.MODE_RECORD:
		clientInfo.Mode = uint32(1)
	case models.MODE_TEST:
		clientInfo.Mode = uint32(2)
	default:
		clientInfo.Mode = uint32(0)
	}

	var own []uint
	if agent.ExtraPassThroughPortsHook != nil {
		own = append(own, agent.ExtraPassThroughPortsHook()...)
	}

	// Mock mode: the wrapped test runner calls the agent's own HTTP server
	// (the /agent/scope/* API) at localhost:<AgentPort>. That process is inside
	// the intercepted PID tree, so without bypassing this port the runner's
	// scope calls would be redirected into the proxy instead of reaching the
	// agent. Kernel-level bypass the agent's own control port.
	if opts.MockMode && opts.AgentPort != 0 {
		own = append(own, uint(opts.AgentPort))
	}

	list, err := kernelPassThroughList(own, agent.GetPortToSendToKernel(ctx, rules))
	if err != nil {
		return structs.ClientInfo{}, err
	}
	clientInfo.PassThroughPorts = list
	clientInfo.ClientNSPID = opts.ClientNSPID
	return clientInfo, nil
}

func (h *Hooks) GetProxyInfo(ctx context.Context, opts config.Agent, cfg agent.HookCfg) (structs.ProxyInfo, error) {
	port := opts.ProxyPort
	if cfg.Port != 0 {
		port = cfg.Port
	}

	if !opts.IsDocker {
		proxyIP, err := IPv4ToUint32("127.0.0.1")
		if err != nil {
			return structs.ProxyInfo{}, err
		}

		// Non-docker v6 redirect: when EnableIPv6Redirect is set (the
		// default) we publish ::ffff:127.0.0.1 so the BPF cgroup program
		// rewrites ::1 and other IPv6 destinations to the v4-mapped proxy
		// address. Without this, modern Linux distros (glibc resolves
		// localhost → ::1 before 127.0.0.1) would have their traffic
		// silently bypass the proxy because the cgroup program cannot
		// redirect to an all-zero address.
		//
		// The explicit cfg.Port override path kept the pre-existing
		// behaviour (non-zero when a per-app port override was given) as
		// a safety net; with the default flipped, that branch is now
		// subsumed by the flag but we honour the override unconditionally.
		var proxyIPv6 [4]uint32
		if opts.EnableIPv6Redirect || cfg.Port != 0 {
			proxyIPv6, err = ToIPv4MappedIPv6("127.0.0.1")
			if err != nil {
				return structs.ProxyInfo{}, err
			}
		}

		proxyInfo := structs.ProxyInfo{
			IP4:  proxyIP,
			IP6:  proxyIPv6,
			Port: port,
		}

		return proxyInfo, nil
	}
	AgentIP, err := utils.GetContainerIPv4() // in case of docker we will get the container's IP fron within the container
	if err != nil {
		return structs.ProxyInfo{}, err
	}
	ipv4, err := IPv4ToUint32(AgentIP)
	if err != nil {
		return structs.ProxyInfo{}, err
	}

	var ipv6 [4]uint32
	if opts.IsDocker {
		ipv6, err = ToIPv4MappedIPv6(AgentIP)
		if err != nil {
			return structs.ProxyInfo{}, fmt.Errorf("failed to convert ipv4:%v to ipv4 mapped ipv6 in docker env:%v", ipv4, err)
		}
		h.logger.Debug(fmt.Sprintf("IPv4-mapped IPv6 for %s is: %08x:%08x:%08x:%08x\n", AgentIP, ipv6[0], ipv6[1], ipv6[2], ipv6[3]))
	}

	proxyInfo := structs.ProxyInfo{
		IP4:  ipv4,
		IP6:  ipv6,
		Port: port,
	}

	return proxyInfo, nil
}

// IPv4ToUint32 converts a string representation of an IPv4 address to a 32-bit integer.
func IPv4ToUint32(ipStr string) (uint32, error) {
	ipAddr := net.ParseIP(ipStr)
	if ipAddr != nil {
		ipAddr = ipAddr.To4()
		if ipAddr != nil {
			return binary.BigEndian.Uint32(ipAddr), nil
		}
		return 0, errors.New("not a valid IPv4 address")
	}
	return 0, errors.New("failed to parse IP address")
}

// ToIPv4MappedIPv6 converts an IPv4 address to an IPv4-mapped IPv6 address.
func ToIPv4MappedIPv6(ipv4 string) ([4]uint32, error) {
	var result [4]uint32

	// Parse the input IPv4 address
	ip := net.ParseIP(ipv4)
	if ip == nil {
		return result, errors.New("invalid IPv4 address")
	}

	// Check if the input is an IPv4 address
	ip = ip.To4()
	if ip == nil {
		return result, errors.New("not a valid IPv4 address")
	}

	// Convert IPv4 address to IPv4-mapped IPv6 address
	// IPv4-mapped IPv6 address is ::ffff:a.b.c.d
	ipv6 := "::ffff:" + ipv4

	// Parse the resulting IPv6 address
	ip6 := net.ParseIP(ipv6)
	if ip6 == nil {
		return result, errors.New("failed to parse IPv4-mapped IPv6 address")
	}

	// Convert the IPv6 address to a 16-byte representation
	ip6Bytes := ip6.To16()
	if ip6Bytes == nil {
		return result, errors.New("failed to convert IPv6 address to bytes")
	}

	// Populate the result array
	for i := 0; i < 4; i++ {
		result[i] = uint32(ip6Bytes[i*4])<<24 | uint32(ip6Bytes[i*4+1])<<16 | uint32(ip6Bytes[i*4+2])<<8 | uint32(ip6Bytes[i*4+3])
	}

	return result, nil
}
