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

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
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
	redirectProxyMap      *ebpf.Map

	// eBPF C shared objectsobjects
	// ebpf objects and events
	socket   link.Link
	connect4 link.Link
	gp4      link.Link
	// tcpv4      link.Link
	// tcpv4Ret   link.Link
	connect6 link.Link
	gp6      link.Link
	// tcpv6      link.Link
	// tcpv6Ret   link.Link
	objects    bpfObjects
	cgBind4    link.Link
	cgBind6    link.Link
	bindEnter  link.Link
	BindEvents *ebpf.Map
	// tpClose    link.Link
	sockops  link.Link
	sendmsg4 link.Link
	recvmsg4 link.Link
	sendmsg6 link.Link
	recvmsg6 link.Link
}

const (
	AttachCGroupInet4Sendmsg = ebpf.AttachType(45) // BPF_CGROUP_INET4_SENDMSG
	AttachCGroupInet4Recvmsg = ebpf.AttachType(46) // BPF_CGROUP_INET4_RECVMSG
)

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
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		utils.LogError(h.logger, err, "failed to lock memory for eBPF resources")
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	optsa := &ebpf.CollectionOptions{
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
		name   string
		pType  ebpf.ProgramType
		aType  ebpf.AttachType
	}{
		{"k_sockops", ebpf.SockOps, ebpf.AttachCGroupSockOps},
		{"k_sendmsg4", ebpf.CGroupSockAddr, ebpf.AttachCGroupUDP4Sendmsg},
		{"k_recvmsg4", ebpf.CGroupSockAddr, ebpf.AttachCGroupUDP4Recvmsg},
		{"k_sendmsg6", ebpf.CGroupSockAddr, ebpf.AttachCGroupUDP6Sendmsg},
		{"k_recvmsg6", ebpf.CGroupSockAddr, ebpf.AttachCGroupUDP6Recvmsg},
		// {"k_connect4", ebpf.CGroupSockAddr, ebpf.AttachCGroupInet4Connect},
		// {"k_connect6", ebpf.CGroupSockAddr, ebpf.AttachCGroupInet6Connect},
		// {"k_getpeername4", ebpf.CGroupSockAddr, ebpf.AttachCGroupInet4GetPeername},
		// {"k_getpeername6", ebpf.CGroupSockAddr, ebpf.AttachCGroupInet6GetPeername},
	}

	for _, p := range programs {
		if prog, ok := spec.Programs[p.name]; ok {
			prog.Type = p.pType
			prog.AttachType = p.aType
		}
	}
	// 2. Explicitly force the correct ProgramType for k_sockops.
	// This tells the verifier: "Treat this function as a SockOps program"
	// which enables access to the `bpf_sock_ops` context and helper #59.
	// if prog, ok := spec.Programs["k_sockops"]; ok {
	// 	prog.Type = ebpf.SockOps
	// }
	// if prog, ok := spec.Programs["k_sendmsg4"]; ok {
    //     prog.Type = ebpf.CGroupSockAddr
    //     prog.AttachType = ebpf.AttachCGroupUDP4Sendmsg
    // }

    // // 3. FIX FOR RECVMSG4 (Add this block)
    // // We must tell the kernel: "This program is specifically for RecvMsg"
    // if prog, ok := spec.Programs["k_recvmsg4"]; ok {
    //     prog.Type = ebpf.CGroupSockAddr
    //     prog.AttachType = ebpf.AttachCGroupUDP4Recvmsg
    // }

	// 3. Now load and assign into the kernel with the corrected spec
	if err := spec.LoadAndAssign(&objs, optsa); err != nil {
		// ... (Keep your existing VerifierError handling logic here) ...
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			fmt.Printf("VERIFIER FAILURE:\n%s\n", strings.Join(ve.Log, "\n"))
		} else {
			fmt.Printf("SYSCALL FAILURE: %v\n", err)
		}
		return err
	}
	// if err != nil {
	// 	var ve *ebpf.VerifierError
	// 	if errors.As(err, &ve) {
	// 		// This will now print the FULL trace
	// 		fmt.Printf("Verifier Error:\n% +v\n", ve)
	// 	}
	// 	return err
	// }

	//getting all the ebpf maps with proper synchronization
	h.objectsMutex.Lock()
	h.clientRegistrationMap = objs.M_1769946249001
	h.agentRegistartionMap = objs.M_1769946249003
	h.objects = objs
	h.objectsMutex.Unlock()
	// ---------------

	// ----- used in case of wsl -----
	socket, err := link.Kprobe("sys_socket", objs.SyscallProbeEntrySocket, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_socket")
		return err
	}
	h.socket = socket

	h.redirectProxyMap = objs.RedirectProxyMap
	h.objects = objs

	// tcpC4, err := link.Kprobe("tcp_v4_connect", objs.SyscallProbeEntryTcpV4Connect, nil)
	// if err != nil {
	// 	utils.LogError(h.logger, err, "failed to attach the kprobe hook on tcp_v4_connect")
	// 	return err
	// }
	// h.tcpv4 = tcpC4

	// tcpRC4, err := link.Kretprobe("tcp_v4_connect", objs.SyscallProbeRetTcpV4Connect, &link.KprobeOptions{})
	// if err != nil {
	// 	utils.LogError(h.logger, err, "failed to attach the kretprobe hook on tcp_v4_connect")
	// 	return err
	// }
	// h.tcpv4Ret = tcpRC4

	// tp, err := link.Tracepoint("syscalls", "sys_enter_close", objs.TpClose, nil)
	// if err != nil {
	// 	utils.LogError(h.logger, err, "failed to attach tracepoint sys_enter_close")
	// 	return err
	// }
	// h.tpClose = tp

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
		utils.LogError(h.logger, err, "failed to attach SockOps") // Add detailed log
		return err
	}
	h.sockops = sockops

	sm4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupUDP4Sendmsg,
		Program: objs.K_sendmsg4,
	})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach SendMsg4 (Kernel might be too old)") // Add detailed log
		return err
	}
	h.sendmsg4 = sm4

	rm4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupUDP4Recvmsg,
		Program: objs.K_recvmsg4,
	})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach RecvMsg4") // Add detailed log
		return err
	}
	h.recvmsg4 = rm4

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

	// tcpC6, err := link.Kprobe("tcp_v6_connect", objs.SyscallProbeEntryTcpV6Connect, nil)
	// if err != nil {
	// 	utils.LogError(h.logger, err, "failed to attach the kprobe hook on tcp_v6_connect")
	// 	return err
	// }
	// h.tcpv6 = tcpC6

	// tcpRC6, err := link.Kretprobe("tcp_v6_connect", objs.SyscallProbeRetTcpV6Connect, &link.KprobeOptions{})
	// if err != nil {
	// 	utils.LogError(h.logger, err, "failed to attach the kretprobe hook on tcp_v6_connect")
	// 	return err
	// }
	// h.tcpv6Ret = tcpRC6

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

	h.sendmsg6, err = link.AttachCgroup(link.CgroupOptions{
		Path: cGroupPath, Attach: ebpf.AttachCGroupUDP6Sendmsg, Program: objs.K_sendmsg6,
	})
	if err != nil {
		h.logger.Warn("Failed to attach sendmsg6 (IPv6 UDP redirection disabled)", zap.Error(err))
	}
	h.recvmsg6, _ = link.AttachCgroup(link.CgroupOptions{
		Path: cGroupPath, Attach: ebpf.AttachCGroupUDP6Recvmsg, Program: objs.K_recvmsg6,
	})


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

	err = h.RegisterClient(ctx, setupOpts, opts.Rules)
	if err != nil {
		h.logger.Debug("Failed to register Client")
	}
	proxyInfo, err := h.GetProxyInfo(ctx, setupOpts)
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

	return nil
}

func (h *Hooks) unLoad(_ context.Context, opts agent.HookCfg) {
	// closing all events
	//other
	if h.socket != nil {
		if err := h.socket.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the socket")
		}
	}

	if h.connect4 != nil {
		if err := h.connect4.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the connect4")
		}
	}

	if h.gp4 != nil {
		if err := h.gp4.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the gp4")
		}
	}

	// if h.tcpv4 != nil {
	// 	if err := h.tcpv4.Close(); err != nil {
	// 		utils.LogError(h.logger, err, "failed to close the tcpv4")
	// 	}
	// }

	// if h.tcpv4Ret != nil {
	// 	if err := h.tcpv4Ret.Close(); err != nil {
	// 		utils.LogError(h.logger, err, "failed to close the tcpv4Ret")
	// 	}
	// }

	if h.connect6 != nil {
		if err := h.connect6.Close(); err != nil {
			utils.LogError(h.logger, err, "failed to close the connect6")
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
	if h.sendmsg4 != nil {
		h.sendmsg4.Close()
	}
	if h.recvmsg4 != nil {
		h.recvmsg4.Close()
	}

	// if h.tcpv6 != nil {
	// 	if err := h.tcpv6.Close(); err != nil {
	// 		utils.LogError(h.logger, err, "failed to close the tcpv6")
	// 	}
	// }
	// if h.tcpv6Ret != nil {
	// 	if err := h.tcpv6Ret.Close(); err != nil {
	// 		utils.LogError(h.logger, err, "failed to close the tcpv6Ret")
	// 	}
	// }
	// if h.tpClose != nil {
	// 	if err := h.tpClose.Close(); err != nil {
	// 		utils.LogError(h.logger, err, "failed to close tpClose")
	// 	}
	// }

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
		if h.bindEnter != nil {
			if err := h.bindEnter.Close(); err != nil {
				utils.LogError(h.logger, err, "failed to close the bind enter kprobe")
			}
		}
	}
	h.logger.Debug("eBPF resources released successfully...")
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
	for i := 0; i < 10; i++ {
		if len(ports) <= i {
			clientInfo.PassThroughPorts[i] = -1
			continue
		}
		// Copy the port, casting from uint32 to int32
		clientInfo.PassThroughPorts[i] = int32(rules[i].Port)
	}
	clientInfo.ClientNSPID = opts.ClientNSPID

	return h.SendClientInfo(clientInfo)
}

func (h *Hooks) GetProxyInfo(ctx context.Context, opts config.Agent) (structs.ProxyInfo, error) {
	if !opts.IsDocker {
		proxyIP, err := IPv4ToUint32("127.0.0.1")
		if err != nil {
			return structs.ProxyInfo{}, err
		}
		proxyInfo := structs.ProxyInfo{
			IP4:  proxyIP,
			IP6:  [4]uint32{0, 0, 0, 0},
			Port: opts.ProxyPort,
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
		ipv6, err := ToIPv4MappedIPv6(AgentIP)
		if err != nil {
			return structs.ProxyInfo{}, fmt.Errorf("failed to convert ipv4:%v to ipv4 mapped ipv6 in docker env:%v", ipv4, err)
		}
		h.logger.Debug(fmt.Sprintf("IPv4-mapped IPv6 for %s is: %08x:%08x:%08x:%08x\n", AgentIP, ipv6[0], ipv6[1], ipv6[2], ipv6[3]))

	}

	proxyInfo := structs.ProxyInfo{
		IP4:  ipv4,
		IP6:  ipv6,
		Port: opts.ProxyPort,
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
