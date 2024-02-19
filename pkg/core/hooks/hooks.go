package hooks

import (
	"context"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"os"
	"sync"
)

func NewHooks() *Hooks {
	return &Hooks{}
}

type Hooks struct {
	logger    *zap.Logger
	sess      core.Sessions
	proxyPort uint32
	m         sync.Mutex
	// eBPF C shared maps
	proxyInfoMap     *ebpf.Map
	inodeMap         *ebpf.Map
	redirectProxyMap *ebpf.Map
	keployModeMap    *ebpf.Map
	keployPid        *ebpf.Map
	appPidMap        *ebpf.Map
	keployServerPort *ebpf.Map
	passthroughPorts *ebpf.Map
	DnsPort          *ebpf.Map

	// eBPF C shared objectsobjects
	// ebpf objects and events
	socket   link.Link
	connect4 link.Link
	bind     link.Link
	gp4      link.Link
	udpp4    link.Link
	tcppv4   link.Link
	tcpv4    link.Link
	tcpv4Ret link.Link
	connect6 link.Link
	gp6      link.Link
	tcppv6   link.Link
	tcpv6    link.Link
	tcpv6Ret link.Link

	accept      link.Link
	acceptRet   link.Link
	accept4     link.Link
	accept4Ret  link.Link
	read        link.Link
	readRet     link.Link
	write       link.Link
	writeRet    link.Link
	close       link.Link
	closeRet    link.Link
	sendto      link.Link
	sendtoRet   link.Link
	recvfrom    link.Link
	recvfromRet link.Link
	objects     bpfObjects
	writev      link.Link
	writevRet   link.Link
}

func (h *Hooks) Get(ctx context.Context, srcPort uint16) (*core.NetworkAddress, error) {
	d, err := h.GetDestinationInfo(srcPort)
	if err != nil {
		return nil, err
	}
	return &core.NetworkAddress{
		Version:  d.IpVersion,
		IPv4Addr: d.DestIp4,
		IPv6Addr: d.DestIp6,
		Port:     d.DestPort,
	}, nil
}

func (h *Hooks) Delete(ctx context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) Load(ctx context.Context, id uint64, opts core.HookOptions) error {
	err := h.load(ctx, opts)
	if err != nil {
		return err
	}
	proxyIp, err := IPv4ToUint32(opts.KeployIPV4)
	if err != nil {
		return fmt.Errorf("failed to convert ip string:[%v] to 32-bit integer", opts.KeployIPV4)
	}
	err = h.SendProxyInfo(proxyIp, h.proxyPort, [4]uint32{0000, 0000, 0000, 0001})
	if err != nil {
		h.logger.Error("failed to send new proxy ip to kernel", zap.Any("NewProxyIp", proxyIp))
		return err
	}
	return nil
}

func (h *Hooks) load(ctx context.Context, opts core.HookOptions) error {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		h.logger.Error("failed to lock memory for eBPF resources", zap.Error(err))
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		h.logger.Error("failed to load eBPF objects", zap.Error(err))
		return err
	}

	//getting all the ebpf maps
	h.proxyInfoMap = objs.ProxyInfoMap
	h.inodeMap = objs.InodeMap
	h.redirectProxyMap = objs.RedirectProxyMap
	h.keployModeMap = objs.KeployModeMap
	h.keployPid = objs.KeployNamespacePidMap
	h.appPidMap = objs.AppNsPidMap
	h.keployServerPort = objs.KeployServerPort
	h.passthroughPorts = objs.PassThroughPorts
	h.objects = objs

	// ----- used in case of wsl -----
	socket, err := link.Kprobe("sys_socket", objs.SyscallProbeEntrySocket, nil)
	if err != nil {
		h.logger.Error("opening sys_socket kprobe: %s", zap.Error(err))
		return err
	}
	h.socket = socket

	// ------------ For Egress -------------

	bind, err := link.Kprobe("sys_bind", objs.SyscallProbeEntryBind, nil)
	if err != nil {
		h.logger.Error("opening sys_bind kprobe: %s", zap.Error(err))
		return err
	}
	h.bind = bind

	udppC4, err := link.Kprobe("udp_pre_connect", objs.SyscallProbeEntryUdpPreConnect, nil)
	if err != nil {
		h.logger.Error("opening udp_pre_connect kprobe: %s", zap.Error(err))
		return err
	}
	h.udpp4 = udppC4

	// FOR IPV4
	tcppC4, err := link.Kprobe("tcp_v4_pre_connect", objs.SyscallProbeEntryTcpV4PreConnect, nil)
	if err != nil {
		h.logger.Error("opening tcp_v4_pre_connect kprobe: %s", zap.Error(err))
		return err
	}
	h.tcppv4 = tcppC4

	tcpC4, err := link.Kprobe("tcp_v4_connect", objs.SyscallProbeEntryTcpV4Connect, nil)
	if err != nil {
		h.logger.Error("opening tcp_v4_connect kprobe: %s", zap.Error(err))
		return err
	}
	h.tcpv4 = tcpC4

	tcpRC4, err := link.Kretprobe("tcp_v4_connect", objs.SyscallProbeRetTcpV4Connect, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("opening tcp_v4_connect kretprobe: %s", zap.Error(err))
		return err
	}
	h.tcpv4Ret = tcpRC4

	// Get the first-mounted cgroupv2 path.
	cGroupPath, err := detectCgroupPath()
	if err != nil {
		h.logger.Error("failed to detect the cgroup path", zap.Error(err))
		return err
	}

	c4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: objs.K_connect4,
	})

	if err != nil {
		h.logger.Error("failed to attach the connect4 cgroup hook", zap.Error(err))
		return err
	}
	h.connect4 = c4

	gp4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCgroupInet4GetPeername,
		Program: objs.K_getpeername4,
	})

	if err != nil {
		h.logger.Error("failed to attach GetPeername4 cgroup hook", zap.Error(err))
		return err
	}
	h.gp4 = gp4

	// FOR IPV6

	tcpPreC6, err := link.Kprobe("tcp_v6_pre_connect", objs.SyscallProbeEntryTcpV6PreConnect, nil)
	if err != nil {
		h.logger.Error("opening tcp_v6_pre_connect kprobe: %s", zap.Error(err))
		return err
	}
	h.tcppv6 = tcpPreC6

	tcpC6, err := link.Kprobe("tcp_v6_connect", objs.SyscallProbeEntryTcpV6Connect, nil)
	if err != nil {
		h.logger.Error("opening tcp_v6_connect kprobe: %s", zap.Error(err))
		return err
	}
	h.tcpv6 = tcpC6

	tcpRC6, err := link.Kretprobe("tcp_v6_connect", objs.SyscallProbeRetTcpV6Connect, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("opening tcp_v6_connect kretprobe: %s", zap.Error(err))
		return err
	}
	h.tcpv6Ret = tcpRC6

	c6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCGroupInet6Connect,
		Program: objs.K_connect6,
	})

	if err != nil {
		h.logger.Error("failed to attach the connect6 cgroup hook", zap.Error(err))
		return err
	}
	h.connect6 = c6

	gp6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cGroupPath,
		Attach:  ebpf.AttachCgroupInet6GetPeername,
		Program: objs.K_getpeername6,
	})

	if err != nil {
		h.logger.Error("failed to attach GetPeername6 cgroup hook", zap.Error(err))
		return err
	}
	h.gp6 = gp6

	//Open a kprobe at the entry of sendto syscall
	snd, err := link.Kprobe("sys_sendto", objs.SyscallProbeEntrySendto, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_sendto", zap.Error(err))
		return err
	}
	h.sendto = snd

	//Opening a kretprobe at the exit of sendto syscall
	sndr, err := link.Kretprobe("sys_sendto", objs.SyscallProbeRetSendto, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_sendto", zap.Error(err))
		return err
	}
	h.sendtoRet = sndr

	// ------------ For Ingress using Kprobes --------------

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac, err := link.Kprobe("sys_accept", objs.SyscallProbeEntryAccept, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_accept", zap.Error(err))
		return err
	}
	h.accept = ac

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac_, err := link.Kretprobe("sys_accept", objs.SyscallProbeRetAccept, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_accept", zap.Error(err))
		return err
	}
	h.acceptRet = ac_

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac4, err := link.Kprobe("sys_accept4", objs.SyscallProbeEntryAccept4, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_accept4", zap.Error(err))
		return err
	}
	h.accept4 = ac4

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac4_, err := link.Kretprobe("sys_accept4", objs.SyscallProbeRetAccept4, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_accept4", zap.Error(err))
		return err
	}
	h.accept4Ret = ac4_

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	rd, err := link.Kprobe("sys_read", objs.SyscallProbeEntryRead, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_read", zap.Error(err))
		return err
	}
	h.read = rd

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	rd_, err := link.Kretprobe("sys_read", objs.SyscallProbeRetRead, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_read", zap.Error(err))
		return err
	}
	h.readRet = rd_

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	wt, err := link.Kprobe("sys_write", objs.SyscallProbeEntryWrite, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.write = wt

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	wt_, err := link.Kretprobe("sys_write", objs.SyscallProbeRetWrite, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.writeRet = wt_

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program for writev.
	wtv, err := link.Kprobe("sys_writev", objs.SyscallProbeEntryWritev, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_writev", zap.Error(err))
		return err
	}
	h.writev = wtv

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program for writev.
	wtv_, err := link.Kretprobe("sys_writev", objs.SyscallProbeRetWritev, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_writev", zap.Error(err))
		return err
	}
	h.writevRet = wtv_

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	cl, err := link.Kprobe("sys_close", objs.SyscallProbeEntryClose, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.close = cl

	//Attaching a kprobe at the entry of recvfrom syscall
	rcv, err := link.Kprobe("sys_recvfrom", objs.SyscallProbeEntryRecvfrom, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_recvfrom", zap.Error(err))
		return err
	}
	h.recvfrom = rcv

	//Attaching a kretprobe at the exit of recvfrom syscall
	rcvr, err := link.Kretprobe("sys_recvfrom", objs.SyscallProbeRetRecvfrom, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_recvfrom", zap.Error(err))
		return err
	}
	h.recvfromRet = rcvr

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	cl_, err := link.Kretprobe("sys_close", objs.SyscallProbeRetClose, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.closeRet = cl_

	h.logger.Info("keploy initialized and probes added to the kernel.")

	switch models.GetMode() {
	case models.MODE_RECORD:
		h.SetKeployModeInKernel(1)
	case models.MODE_TEST:
		h.SetKeployModeInKernel(2)
	}

	//sending keploy pid to kernel to get filtered
	k_inode, err := getSelfInodeNumber()
	if err != nil {
		h.logger.Error("failed to get inode of the keploy process", zap.Error(err))
		return err
	}
	h.logger.Debug("", zap.Any("Keploy Inode number", k_inode))
	err = h.SendNameSpaceId(1, k_inode)
	if err != nil {
		h.logger.Error("failed to send the namespace id to the epbf program", zap.Error(err))
		return err

	}
	err = h.SendKeployPid(uint32(os.Getpid()))
	if err != nil {
		h.logger.Error("failed to send the keploy pid to the ebpf program", zap.Error(err))
		return err
	}
	h.logger.Debug("Keploy Pid sent successfully...")

	//send app pid to kernel to get filtered in case of integration with unit test file
	// app pid here is the pid of the unit test file process or application pid
	if opts.Pid != 0 {
		err = h.SendAppPid(opts.Pid)
		if err != nil {
			h.logger.Error("failed to send the app pid to the ebpf program", zap.Error(err))
			return err
		}
	}

	return nil
}
func (h *Hooks) SetKeployModeInKernel(mode uint32) {
	key := 0
	err := h.keployModeMap.Update(uint32(key), &mode, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to set keploy mode in the epbf program", zap.Any("error thrown by ebpf map", err.Error()))
	}
}

func (h *Hooks) Record(ctx context.Context, id uint64) (<-chan *models.TestCase, <-chan error) {
	return conn.ListenSocket(ctx, h.logger, h.objects.SocketOpenEvents, h.objects.SocketDataEvents, h.objects.SocketCloseEvents)
}

// This function sends the IP and Port of the running proxy in the eBPF program.
func (h *Hooks) SendProxyInfo(ip4, port uint32, ip6 [4]uint32) error {
	key := 0
	err := h.proxyInfoMap.Update(uint32(key), structs.ProxyInfo{IP4: ip4, Ip6: ip6, Port: port}, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the proxy IP & Port to the epbf program", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

// This function is helpful when user application in running inside a docker container.
func (h *Hooks) SendNameSpaceId(key uint32, inode uint64) error {
	err := h.inodeMap.Update(uint32(key), &inode, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the namespace id to the epbf program", zap.Any("error thrown by ebpf map", err.Error()), zap.Any("key", key), zap.Any("Inode", inode))
		return err
	}
	return nil
}

func (h *Hooks) SendKeployPid(kPid uint32) error {
	h.logger.Debug("Sending keploy pid to kernel", zap.Any("pid", kPid))
	err := h.keployPid.Update(uint32(0), &kPid, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the keploy pid to the ebpf program", zap.Any("Keploy Pid", kPid), zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

// SendAppPid sends the application's process ID (PID) to the kernel.
// This function is used when running Keploy tests along with unit tests of the application.
func (h *Hooks) SendAppPid(pid uint32) error {
	h.logger.Debug("Sending app pid to kernel", zap.Any("app Pid", pid))
	err := h.appPidMap.Update(uint32(0), &pid, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the app pid to the ebpf program", zap.Any("app Pid", pid), zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

// GetDestinationInfo retrieves destination information associated with a source port.
func (h *Hooks) GetDestinationInfo(srcPort uint16) (*structs.DestInfo, error) {
	h.m.Lock()
	defer h.m.Unlock()
	destInfo := structs.DestInfo{}
	if err := h.redirectProxyMap.Lookup(srcPort, &destInfo); err != nil {
		return nil, err
	}
	return &destInfo, nil
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	h.m.Lock()
	defer h.m.Unlock()
	err := h.redirectProxyMap.Delete(srcPort)
	if err != nil {
		h.logger.Error("no such key present in the redirect proxy map", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.Any("(Key)/SourcePort", srcPort))
	return nil
}
