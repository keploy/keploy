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

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	agentSvc "go.keploy.io/server/v3/pkg/service/agent"
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
	// eBPF C shared maps
	clientRegistrationMap *ebpf.Map
	agentRegistartionMap  *ebpf.Map
	agentKernelPidMap     *ebpf.Map
	redirectProxyMap      *ebpf.Map

	// eBPF C shared objectsobjects
	// ebpf objects and events
	socket      link.Link
	connect4    link.Link
	udp4Sendmsg link.Link
	gp4         link.Link
	connect6    link.Link
	udp6Sendmsg link.Link
	gp6         link.Link
	objects     bpfObjects
	cgBind4     link.Link
	cgBind6     link.Link

	BindEvents *ebpf.Map
	sockops    link.Link

	// mapPinCleanup is set by the EbpfMapPinHook to unpin maps on shutdown.
	mapPinCleanup func()

	// sockmapCleanup is set by the enterprise SockmapLoadHook to unload sockmap BPF on shutdown.
	sockmapCleanup func()
}

func (h *Hooks) Load(ctx context.Context, opts agent.HookCfg, setupOpts config.Agent) error {

	h.sess.Set(uint64(0), &agent.Session{
		ID: uint64(0), // need to check this one
	})
	err := h.load(ctx, opts, setupOpts)
	if err != nil {
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer utils.Recover(h.logger)
		<-ctx.Done()
		h.unLoad(ctx, opts)

		//deleting in order to free the memory in case of rerecord.
		h.sess.Delete(uint64(0))
		return nil
	})

	return nil
}

func (h *Hooks) load(ctx context.Context, opts agent.HookCfg, setupOpts config.Agent) error {
	// Ensure /sys/fs/bpf is a writable bpffs. When running inside Docker,
	// the host's bpffs may be bind-mounted with restrictive permissions
	// (mode=700). In that case, mount a fresh container-local bpffs.
	ensureBPFFS(h.logger)

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		utils.LogError(h.logger, err, "failed to lock memory for eBPF resources")
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	bpfopts := &ebpf.CollectionOptions{
		Programs: ebpf.ProgramOptions{
			LogLevel:     ebpf.LogLevelInstruction | ebpf.LogLevelBranch,
			LogSizeStart: 1 * 1024 * 1024,
		},
	}

	spec, err := loadBpf()
	if err != nil {
		utils.LogError(h.logger, err, "failed to load BPF spec")
		return err
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

	// Now load and assign into the kernel with the corrected spec
	if err := spec.LoadAndAssign(&objs, bpfopts); err != nil {
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
	h.clientRegistrationMap = objs.M_1773927248001
	h.agentRegistartionMap = objs.M_1773927248002
	h.agentKernelPidMap = objs.KeployAgentKernelPidMap
	h.objects = objs
	h.objectsMutex.Unlock()
	// ---------------

	socket, err := link.Tracepoint("syscalls", "sys_enter_socket", objs.SyscallProbeEntrySocket, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the tracepoint hook on sys_socket")
		return err
	}
	h.socket = socket

	h.redirectProxyMap = objs.RedirectProxyMap
	h.objects = objs

	// If enterprise registered a map-pin hook, expose maps for pinning.
	if agentSvc.MapPinHook != nil {
		pinnableMaps := map[string]agentSvc.Pinnable{
			"redirect_proxy_map":    objs.RedirectProxyMap,
			"target_namespace_pids": objs.TargetNamespacePids,
		}
		cleanup, pinErr := agentSvc.MapPinHook(pinnableMaps)
		if pinErr != nil {
			h.logger.Error(
				"EbpfMapPinHook failed; cross-process eBPF map access will be disabled. Ensure the container has CAP_SYS_ADMIN or is run with --enable-docker-unconfined, and verify that bpffs is mounted and writable.",
				zap.Error(pinErr),
			)
		} else {
			h.mapPinCleanup = cleanup
		}
	}

	// Get the first-mounted cgroupv2 path.
	cGroupPath, err := agent.DetectCgroupPath(h.logger)
	if err != nil {
		utils.LogError(h.logger, err, "failed to detect the cgroup path")
		return err
	}
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

	if opts.Mode == models.MODE_RECORD {

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
		h.logger.Debug("Attached ingress redirection hooks.")
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
	agentInfo.KeployAgentInode, err = GetSelfInodeNumber()
	if err != nil {
		h.logger.Error("failed to get the inode number of the keploy process", zap.Error(err))
		return err
	}
	agentInfo.IsDocker = 0
	if opts.IsDocker {
		agentInfo.IsDocker = 1
	}
	agentInfo.DNSPort = int32(setupOpts.DnsPort)

	// Set recording start time using CLOCK_BOOTTIME so the eBPF tracepoint
	// can compare process start_boottime and auto-exclude pre-existing PIDs.
	if setupOpts.IsDocker {
		var ts unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
			h.logger.Warn("failed to read CLOCK_BOOTTIME; pre-existing PID exclusion disabled", zap.Error(err))
		} else {
			agentInfo.RecordingStartTime = uint64(ts.Sec)*1e9 + uint64(ts.Nsec)
			h.logger.Info("recording start boottime set", zap.Uint64("ns", agentInfo.RecordingStartTime))
		}
	}

	err = h.RegisterClient(ctx, setupOpts, opts.Rules)
	if err != nil {
		h.logger.Debug("Failed to register Client")
	}
	proxyInfo, err := h.GetProxyInfo(ctx, setupOpts, opts)
	if err != nil {
		return err
	}

	if opts.IsDocker {
		h.proxyIP4, err = utils.GetContainerIPv4()
		if err != nil {
			h.logger.Error("Failed to get the container IP", zap.Error(err))
			return err
		}
		ipv6, err := ToIPv4MappedIPv6(h.proxyIP4)
		if err != nil {
			return fmt.Errorf("failed to convert ipv4:%v to ipv4 mapped ipv6 in docker env:%v", h.proxyIP4, err)
		}
		h.logger.Debug(fmt.Sprintf("IPv4-mapped IPv6 for %s is: %08x:%08x:%08x:%08x\n", h.proxyIP4, ipv6[0], ipv6[1], ipv6[2], ipv6[3]))
		h.proxyIP6 = ipv6
	}
	h.logger.Debug("proxy ips", zap.String("ipv4", h.proxyIP4), zap.Any("ipv6", h.proxyIP6))

	agentInfo.Proxy = proxyInfo
	err = h.SendAgentInfo(agentInfo)
	if err != nil {
		h.logger.Error("failed to send agent info to the ebpf program", zap.Error(err))
		return err
	}

	// If enterprise registered a sockmap load hook, invoke it to load
	// sk_skb BPF programs for low-latency mode.
	if agentSvc.SockmapHook != nil {
		cleanup, smErr := agentSvc.SockmapHook(h.logger)
		if smErr != nil {
			h.logger.Warn("SockmapLoadHook failed (low-latency mode unavailable)", zap.Error(smErr))
		} else {
			h.sockmapCleanup = cleanup
			h.logger.Info("Sockmap BPF programs loaded via enterprise hook")
		}
	}

	return nil
}

func (h *Hooks) unLoad(_ context.Context, opts agent.HookCfg) {
	// closing all events
	//other

	if h.socket != nil {
		if err := h.socket.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the tracepoint hook on sys_socket")
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
	if h.gp6 != nil {
		if err := h.gp6.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the gp6")
		}
	}

	if h.sockops != nil {
		h.sockops.Close()
	}

	// Close eBPF objects with proper synchronization
	h.objectsMutex.Lock()
	if err := h.objects.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the objects")
	}
	h.objectsMutex.Unlock()

	if opts.Mode == models.MODE_RECORD {
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

	}
	h.logger.Debug("eBPF resources released successfully...")

	// Clean up pinned eBPF maps (if enterprise pinned any)
	if h.mapPinCleanup != nil {
		h.mapPinCleanup()
		h.mapPinCleanup = nil
	}

	// Clean up sockmap BPF resources (loaded by enterprise hook)
	if h.sockmapCleanup != nil {
		h.sockmapCleanup()
		h.sockmapCleanup = nil
	}
}

func (h *Hooks) RegisterClient(ctx context.Context, opts config.Agent, rules []models.BypassRule) error {
	h.logger.Debug("Registering the client Info with keploy")
	// Register the client and start processing

	clientInfo := structs.ClientInfo{}

	switch opts.Mode {
	case models.MODE_RECORD:
		clientInfo.Mode = uint32(1)
	case models.MODE_TEST:
		clientInfo.Mode = uint32(2)
	default:
		clientInfo.Mode = uint32(0)
	}

	ports := agent.GetPortToSendToKernel(ctx, rules)

	// Allow enterprise to add extra pass-through ports (e.g. JSSE capture port
	// for low-latency mode).
	if agentSvc.ExtraPassThroughPortsHook != nil {
		ports = append(ports, agentSvc.ExtraPassThroughPortsHook()...)
	}

	for i := 0; i < 10; i++ {
		if len(ports) <= i {
			clientInfo.PassThroughPorts[i] = -1
			continue
		}
		clientInfo.PassThroughPorts[i] = int32(ports[i])
	}
	clientInfo.ClientNSPID = opts.ClientNSPID

	return h.SendClientInfo(clientInfo)
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
		// Use IPv4-mapped IPv6 address (::ffff:127.0.0.1) so that IPv6 connections
		// (e.g. from Java JVM which prefers IPv6) get routed through the IPv4 stack
		// to the proxy listening on 0.0.0.0.
		proxyIPv6, err := ToIPv4MappedIPv6("127.0.0.1")
		if err != nil {
			return structs.ProxyInfo{}, err
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

// ensureBPFFS checks that /sys/fs/bpf is a bpffs filesystem. When running
// inside Docker, /sys/fs/bpf may be a tmpfs or a bind-mounted host bpffs
// with restrictive permissions. In either case, mount a fresh bpffs on top.
func ensureBPFFS(logger *zap.Logger) {
	const bpfFSPath = "/sys/fs/bpf"

	// Check if /sys/fs/bpf is already a bpffs (magic = 0xcafe4a11).
	var stat unix.Statfs_t
	if err := unix.Statfs(bpfFSPath, &stat); err == nil && stat.Type == 0xcafe4a11 {
		// Already a bpffs — check if it's writable.
		testPath := bpfFSPath + "/.keploy_write_test"
		f, err := os.Create(testPath)
		if err == nil {
			f.Close()
			os.Remove(testPath)
			return // bpffs is writable
		}
		logger.Debug("bpffs exists but is not writable, remounting",
			zap.String("path", bpfFSPath), zap.Error(err))
	}

	// Ensure the mount point directory exists.
	_ = os.MkdirAll(bpfFSPath, 0755)

	// Mount a fresh bpffs. This requires CAP_SYS_ADMIN + no AppArmor restriction.
	if err := unix.Mount("bpf", bpfFSPath, "bpf", 0, ""); err != nil {
		logger.Warn("Failed to mount bpffs — BPF map pinning will not work",
			zap.String("path", bpfFSPath), zap.Error(err))
		return
	}

	logger.Info("Mounted fresh bpffs for BPF map pinning", zap.String("path", bpfFSPath))
}
