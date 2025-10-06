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

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/agent/hooks/common"
	"go.keploy.io/server/v2/pkg/agent/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		BaseHooks:    common.NewBaseHooks(logger, cfg),
		objectsMutex: sync.RWMutex{},
	}
}

type Hooks struct {
	*common.BaseHooks
	objectsMutex sync.RWMutex // Protects eBPF objects during load/unload operations
	// eBPF C shared maps
	clientRegistrationMap *ebpf.Map
	agentRegistartionMap  *ebpf.Map
	redirectProxyMap      *ebpf.Map
	proxyInfoMap          *ebpf.Map
	e2eAppRegistrationMap *ebpf.Map

	// eBPF C shared objectsobjects
	// ebpf objects and events
	socket     link.Link
	connect4   link.Link
	gp4        link.Link
	udpp4      link.Link
	tcpv4      link.Link
	tcpv4Ret   link.Link
	connect6   link.Link
	gp6        link.Link
	tcpv6      link.Link
	tcpv6Ret   link.Link
	objects    bpfObjects
	appID      uint64
	cgBind4    link.Link
	cgBind6    link.Link
	bindEnter  link.Link
	BindEvents *ebpf.Map
}

func (h *Hooks) Load(ctx context.Context, id uint64, opts agent.HookCfg, setupOpts models.SetupOptions) error {

	h.Sess.Set(id, &agent.Session{
		ID: id,
	})

	// Set the app ID for this session with proper synchronization
	h.M.Lock()
	h.appID = id
	h.M.Unlock()

	// Reset the unload done channel for this new load
	h.UnloadDoneMutex.Lock()
	h.UnloadDone = make(chan struct{})
	h.UnloadDoneMutex.Unlock()

	err := h.load(ctx, opts, setupOpts)
	if err != nil {
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer utils.Recover(h.Logger)
		<-ctx.Done()
		h.unLoad(ctx, opts)

		//deleting in order to free the memory in case of rerecord.
		h.Sess.Delete(id)

		// Signal that unload is complete
		h.UnloadDoneMutex.Lock()
		close(h.UnloadDone)
		h.UnloadDoneMutex.Unlock()
		return nil
	})

	return nil
}

// GetUnloadDone returns a channel that will be closed when the hooks are completely unloaded
func (h *Hooks) GetUnloadDone() <-chan struct{} {
	h.UnloadDoneMutex.Lock()
	defer h.UnloadDoneMutex.Unlock()
	return h.UnloadDone
}

func (h *Hooks) load(ctx context.Context, opts agent.HookCfg, setupOpts models.SetupOptions) error {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		utils.LogError(h.Logger, err, "failed to lock memory for eBPF resources")
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			errString := strings.Join(ve.Log, "\n")
			h.Logger.Debug("verifier log: ", zap.String("err", errString))
		}
		utils.LogError(h.Logger, err, "failed to load eBPF objects")
		return err
	}

	//getting all the ebpf maps with proper synchronization
	h.objectsMutex.Lock()
	h.clientRegistrationMap = objs.KeployClientRegistrationMap
	h.agentRegistartionMap = objs.KeployAgentRegistrationMap
	h.objects = objs
	h.objectsMutex.Unlock()
	// ---------------

	// ----- used in case of wsl -----
	socket, err := link.Kprobe("sys_socket", objs.SyscallProbeEntrySocket, nil)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to attach the kprobe hook on sys_socket")
		return err
	}
	h.socket = socket

	if !opts.E2E {
		h.redirectProxyMap = objs.RedirectProxyMap
		h.proxyInfoMap = objs.KeployProxyInfo
		// h.tbenchFilterPid = objs.TestbenchInfoMap
		h.objects = objs

		tcpC4, err := link.Kprobe("tcp_v4_connect", objs.SyscallProbeEntryTcpV4Connect, nil)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the kprobe hook on tcp_v4_connect")
			return err
		}
		h.tcpv4 = tcpC4

		tcpRC4, err := link.Kretprobe("tcp_v4_connect", objs.SyscallProbeRetTcpV4Connect, &link.KprobeOptions{})
		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the kretprobe hook on tcp_v4_connect")
			return err
		}
		h.tcpv4Ret = tcpRC4

		// Get the first-mounted cgroupv2 path.
		cGroupPath, err := agent.DetectCgroupPath(h.Logger)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to detect the cgroup path")
			return err
		}
		if opts.Mode != models.MODE_TEST {

			h.BindEvents = objs.BindEvents
			cg4, err := link.AttachCgroup(link.CgroupOptions{
				Path:    cGroupPath,
				Attach:  ebpf.AttachCGroupInet4Bind,
				Program: objs.K_bind4,
			})
			if err != nil {
				utils.LogError(h.Logger, err, "failed to attach the bind4 cgroup hook")
				return err
			}
			h.cgBind4 = cg4

			cg6, err := link.AttachCgroup(link.CgroupOptions{
				Path:    cGroupPath,
				Attach:  ebpf.AttachCGroupInet6Bind,
				Program: objs.K_bind6,
			})
			if err != nil {
				utils.LogError(h.Logger, err, "failed to attach the bind6 cgroup hook")
				return err
			}
			h.cgBind6 = cg6
			h.Logger.Debug("Attached ingress redirection hooks.")
		}

		c4, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cGroupPath,
			Attach:  ebpf.AttachCGroupInet4Connect,
			Program: objs.K_connect4,
		})

		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the connect4 cgroup hook")
			return err
		}
		h.connect4 = c4

		gp4, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cGroupPath,
			Attach:  ebpf.AttachCgroupInet4GetPeername,
			Program: objs.K_getpeername4,
		})

		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the GetPeername4 cgroup hook")
			return err
		}
		h.gp4 = gp4

		tcpC6, err := link.Kprobe("tcp_v6_connect", objs.SyscallProbeEntryTcpV6Connect, nil)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the kprobe hook on tcp_v6_connect")
			return err
		}
		h.tcpv6 = tcpC6

		tcpRC6, err := link.Kretprobe("tcp_v6_connect", objs.SyscallProbeRetTcpV6Connect, &link.KprobeOptions{})
		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the kretprobe hook on tcp_v6_connect")
			return err
		}
		h.tcpv6Ret = tcpRC6

		c6, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cGroupPath,
			Attach:  ebpf.AttachCGroupInet6Connect,
			Program: objs.K_connect6,
		})

		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the connect6 cgroup hook")
			return err
		}
		h.connect6 = c6

		gp6, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cGroupPath,
			Attach:  ebpf.AttachCgroupInet6GetPeername,
			Program: objs.K_getpeername6,
		})

		if err != nil {
			utils.LogError(h.Logger, err, "failed to attach the GetPeername6 cgroup hook")
			return err
		}
		h.gp6 = gp6
	}

	h.Logger.Debug("keploy initialized and probes added to the kernel.")

	if opts.E2E {
		pid, err := utils.GetPIDFromPort(ctx, h.Logger, int(opts.Port))
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get the keploy pid from the port in case of e2e")
			return err
		}
		err = h.SendE2EInfo(pid)
		if err != nil {
			h.Logger.Error("failed to send e2e info to the ebpf program", zap.Error(err))
		}
	}

	if opts.IsDocker {
		h.ProxyIP4 = opts.KeployIPV4
		ipv6, err := agent.ToIPv4MappedIPv6(opts.KeployIPV4)
		if err != nil {
			return fmt.Errorf("failed to convert ipv4:%v to ipv4 mapped ipv6 in docker env:%v", opts.KeployIPV4, err)
		}
		h.Logger.Debug(fmt.Sprintf("IPv4-mapped IPv6 for %s is: %08x:%08x:%08x:%08x\n", h.ProxyIP4, ipv6[0], ipv6[1], ipv6[2], ipv6[3]))
		h.ProxyIP6 = ipv6
	}

	h.Logger.Debug("proxy ips", zap.String("ipv4", h.ProxyIP4), zap.Any("ipv6", h.ProxyIP6))

	var agentInfo = structs.AgentInfo{}
	agentInfo.KeployAgentNsPid = uint32(os.Getpid())
	agentInfo.KeployAgentInode, _ = GetSelfInodeNumber()
	agentInfo.IsDocker = 0
	if opts.IsDocker {
		agentInfo.IsDocker = 1
	}
	agentInfo.DNSPort = int32(h.DNSPort)

	err = h.RegisterClient(ctx, setupOpts, opts.Rules)
	if err != nil {
		h.Logger.Debug("Failed to register Client")
	}

	err = h.SendAgentInfo(agentInfo)
	if err != nil {
		h.Logger.Error("failed to send agent info to the ebpf program", zap.Error(err))
		return err
	}

	return nil
}

func (h *Hooks) SendKeployClientInfo(clientInfo structs.ClientInfo) error {

	err := h.SendClientInfo(clientInfo)
	if err != nil {
		h.Logger.Error("failed to send client info to the ebpf program", zap.Error(err))
		return err
	}

	return nil
}

func (h *Hooks) unLoad(_ context.Context, opts agent.HookCfg) {
	// closing all events
	//other
	if err := h.socket.Close(); err != nil {
		utils.LogError(h.Logger, err, "failed to close the socket")
	}

	h.M.Lock()
	h.appID = 0
	h.M.Unlock()

	if !opts.E2E {
		if err := h.udpp4.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the udpp4")
		}

		if err := h.connect4.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the connect4")
		}

		if err := h.gp4.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the gp4")
		}

		// if err := h.tcppv4.Close(); err != nil {
		// 	utils.LogError(h.Logger, err, "failed to close the tcppv4")
		// }

		if err := h.tcpv4.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the tcpv4")
		}

		if err := h.tcpv4Ret.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the tcpv4Ret")
		}

		if err := h.connect6.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the connect6")
		}
		if err := h.gp6.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the gp6")
		}
		// if err := h.tcppv6.Close(); err != nil {
		// 	utils.LogError(h.Logger, err, "failed to close the tcppv6")
		// }
		if err := h.tcpv6.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the tcpv6")
		}
		if err := h.tcpv6Ret.Close(); err != nil {
			utils.LogError(h.Logger, err, "failed to close the tcpv6Ret")
		}
	}

	// Close eBPF objects with proper synchronization
	h.objectsMutex.Lock()
	if err := h.objects.Close(); err != nil {
		utils.LogError(h.Logger, err, "failed to close the objects")
	}
	h.objectsMutex.Unlock()

	if opts.Mode != models.MODE_TEST {
		if h.cgBind4 != nil {
			if err := h.cgBind4.Close(); err != nil {
				utils.LogError(h.Logger, err, "failed to close the cgBind4")
			}
		}
		if h.cgBind6 != nil {
			if err := h.cgBind6.Close(); err != nil {
				utils.LogError(h.Logger, err, "failed to close the cgBind6")
			}
		}
		if h.bindEnter != nil {
			if err := h.bindEnter.Close(); err != nil {
				utils.LogError(h.Logger, err, "failed to close the bind enter kprobe")
			}
		}
	}
	h.Logger.Info("eBPF resources released successfully...")
}

func (h *Hooks) RegisterClient(ctx context.Context, opts models.SetupOptions, rules []config.BypassRule) error {
	h.Logger.Info("Registering the client Info with keploy")
	// Register the client and start processing

	// send the network info to the kernel
	err := h.SendNetworkInfo(ctx, opts)
	if err != nil {
		h.Logger.Error("failed to send network info to the kernel", zap.Error(err))
		return err
	}
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
		clientInfo.PassThroughPorts[i] = int32(ports[i])
	}
	clientInfo.ClientNSPID = opts.ClientNSPID
	return h.SendKeployClientInfo(clientInfo)
}

func (h *Hooks) SendNetworkInfo(ctx context.Context, opts models.SetupOptions) error {
	if !opts.IsDocker {
		proxyIP, err := IPv4ToUint32("127.0.0.1")
		if err != nil {
			return err
		}
		proxyInfo := structs.ProxyInfo{
			IP4:  proxyIP,
			IP6:  [4]uint32{0, 0, 0, 0},
			Port: opts.ProxyPort,
		}
		err = h.SendClientProxyInfo(uint64(0), proxyInfo)
		if err != nil {
			return err
		}
		return nil
	}
	opts.AgentIP, _ = GetContainerIP()
	ipv4, err := IPv4ToUint32(opts.AgentIP)
	if err != nil {
		return err
	}

	var ipv6 [4]uint32
	if opts.IsDocker {
		ipv6, err := ToIPv4MappedIPv6(opts.AgentIP)
		if err != nil {
			return fmt.Errorf("failed to convert ipv4:%v to ipv4 mapped ipv6 in docker env:%v", ipv4, err)
		}
		h.Logger.Debug(fmt.Sprintf("IPv4-mapped IPv6 for %s is: %08x:%08x:%08x:%08x\n", opts.AgentIP, ipv6[0], ipv6[1], ipv6[2], ipv6[3]))

	}

	proxyInfo := structs.ProxyInfo{
		IP4:  ipv4,
		IP6:  ipv6,
		Port: opts.ProxyPort,
	}

	err = h.SendClientProxyInfo(uint64(0), proxyInfo)
	if err != nil {
		return err
	}
	return nil
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

func GetContainerIP() (string, error) {
	// Get all network interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	// Iterate over the interfaces
	for _, i := range interfaces {
		// Skip down or loopback interfaces
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}

		// Get the addresses for the current interface
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}

		// Iterate over the addresses
		for _, addr := range addrs {
			var ip net.IP
			// The address can be of type *net.IPNet or *net.IPAddr
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				// Check if it's an IPv4 address
				if ipnet.IP.To4() != nil {
					ip = ipnet.IP
				}
			}

			if ip != nil {
				// Found a valid IPv4 address, return it
				return ip.String(), nil
			}
		}
	}

	return "", fmt.Errorf("could not find a non-loopback IP for the container")
}
