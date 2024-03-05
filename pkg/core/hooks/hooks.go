package hooks

import (
	"context"
	"fmt"
	"os"
	"sync"

	"go.keploy.io/server/v2/config"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// TODO: do we need the config here, if not then how can i set the proxyPort
func NewHooks(logger *zap.Logger, cfg config.Config) *Hooks {
	return &Hooks{
		logger:    logger,
		sess:      core.NewSessions(),
		m:         sync.Mutex{},
		proxyIp:   "127.0.0.1",
		proxyPort: cfg.ProxyPort,
		dnsPort:   cfg.DnsPort,
	}
}

type Hooks struct {
	logger    *zap.Logger
	sess      *core.Sessions
	proxyIp   string
	proxyPort uint32
	dnsPort   uint32

	m sync.Mutex
	// eBPF C shared maps
	proxyInfoMap     *ebpf.Map
	inodeMap         *ebpf.Map
	redirectProxyMap *ebpf.Map
	keployModeMap    *ebpf.Map
	keployPid        *ebpf.Map
	appPidMap        *ebpf.Map
	keployServerPort *ebpf.Map
	passthroughPorts *ebpf.Map
	DockerCmdMap     *ebpf.Map
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

func (h *Hooks) Load(ctx context.Context, id uint64, opts core.HookCfg) error {

	h.sess.Set(id, &core.Session{
		ID: id,
	})


	err := h.load(ctx, opts)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		h.unLoad(ctx)
	}()

	if opts.IsDocker {
		h.proxyIp = opts.KeployIPV4
	}

	proxyIp, err := IPv4ToUint32(h.proxyIp)
	if err != nil {
		return fmt.Errorf("failed to convert ip string:[%v] to 32-bit integer", opts.KeployIPV4)
	}

	err = h.SendProxyInfo(proxyIp, h.proxyPort, [4]uint32{0000, 0000, 0000, 0001})
	if err != nil {
		h.logger.Error("failed to send new proxy ip to kernel", zap.Any("NewProxyIp", proxyIp))
		return err
	}

	err = h.SendDnsPort(h.dnsPort)
	if err != nil {
		h.logger.Error("failed to send dns port to kernel", zap.Any("DnsPort", h.dnsPort))
		return err
	}

	err = h.SendCmdType(opts.IsDocker)
	if err != nil {
		h.logger.Error("failed to send the cmd type to kernel", zap.Bool("isDocker", opts.IsDocker))
		return err
	}

	return nil
}

func (h *Hooks) load(ctx context.Context, opts core.HookCfg) error {
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
	h.DnsPort = objs.DnsPortMap
	h.DockerCmdMap = objs.DockerCmdMap
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

	//send app pid to kernel to get filtered in case of mock record/test feature of unit test file
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

func (h *Hooks) Record(ctx context.Context, id uint64) (<-chan *models.TestCase, <-chan error) {
	// TODO use the session to get the app id
	// and then use the app id to get the test cases chan
	// and pass that to eBPF consumers/listeners
	return conn.ListenSocket(ctx, h.logger, h.objects.SocketOpenEvents, h.objects.SocketDataEvents, h.objects.SocketCloseEvents)
}

func (h *Hooks) unLoad(ctx context.Context) {
	// closing all events
	//other
	h.socket.Close()
	//egress
	h.bind.Close()
	h.udpp4.Close()
	//ipv4
	h.connect4.Close()
	h.gp4.Close()
	h.tcppv4.Close()
	h.tcpv4.Close()
	h.tcpv4Ret.Close()
	//ipv6
	h.connect6.Close()
	h.gp6.Close()
	h.tcppv6.Close()
	h.tcpv6.Close()
	h.tcpv6Ret.Close()
	//ingress
	h.accept.Close()
	h.acceptRet.Close()
	h.accept4.Close()
	h.accept4Ret.Close()
	h.close.Close()
	h.closeRet.Close()
	h.read.Close()
	h.readRet.Close()
	h.write.Close()
	h.writeRet.Close()
	h.writev.Close()
	h.writevRet.Close()
	h.sendto.Close()
	h.sendtoRet.Close()
	h.recvfrom.Close()
	h.recvfromRet.Close()
	h.objects.Close()
	h.logger.Info("eBPF resources released successfully...")
}
