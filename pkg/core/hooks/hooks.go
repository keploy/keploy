// Package hooks provides functionality for managing hooks.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg config.Config) *Hooks {
	return &Hooks{
		logger:    logger,
		sess:      core.NewSessions(),
		m:         sync.Mutex{},
		proxyIP:   "127.0.0.1",
		proxyPort: cfg.ProxyPort,
		dnsPort:   cfg.DNSPort,
	}
}

type Hooks struct {
	logger    *zap.Logger
	sess      *core.Sessions
	proxyIP   string
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
	DNSPort          *ebpf.Map
	// for test bench
	tbenchFilterPid  *ebpf.Map
	tbenchFilterPort *ebpf.Map
	//--------------

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

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer utils.Recover(h.logger)
		<-ctx.Done()
		h.unLoad(ctx)
		return nil
	})

	if opts.IsDocker {
		h.proxyIP = opts.KeployIPV4
	}

	proxyIP, err := IPv4ToUint32(h.proxyIP)
	if err != nil {
		return fmt.Errorf("failed to convert ip string:[%v] to 32-bit integer", opts.KeployIPV4)
	}

	err = h.SendProxyInfo(proxyIP, h.proxyPort, [4]uint32{0000, 0000, 0000, 0001})
	if err != nil {
		utils.LogError(h.logger, err, "failed to send proxy info to kernel", zap.Any("NewProxyIp", proxyIP))
		return err
	}

	err = h.SendDNSPort(h.dnsPort)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send dns port to kernel", zap.Any("DnsPort", h.dnsPort))
		return err
	}

	err = h.SendCmdType(opts.IsDocker)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the cmd type to kernel", zap.Bool("isDocker", opts.IsDocker))
		return err
	}

	return nil
}

func (h *Hooks) load(_ context.Context, opts core.HookCfg) error {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		utils.LogError(h.logger, err, "failed to lock memory for eBPF resources")
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		utils.LogError(h.logger, err, "failed to load eBPF objects")
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
	h.DNSPort = objs.DnsPortMap
	h.DockerCmdMap = objs.DockerCmdMap
	h.objects = objs
	// For test bench
	h.tbenchFilterPid = objs.TbenchFilterPid
	h.tbenchFilterPort = objs.TbenchFilterPort
	// ---------------

	// ----- used in case of wsl -----
	socket, err := link.Kprobe("sys_socket", objs.SyscallProbeEntrySocket, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_socket")
		return err
	}
	h.socket = socket

	// ------------ For Egress -------------

	bind, err := link.Kprobe("sys_bind", objs.SyscallProbeEntryBind, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_bind")
		return err
	}
	h.bind = bind

	udppC4, err := link.Kprobe("udp_pre_connect", objs.SyscallProbeEntryUdpPreConnect, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on udp_pre_connect")
		return err
	}
	h.udpp4 = udppC4

	// FOR IPV4
	tcppC4, err := link.Kprobe("tcp_v4_pre_connect", objs.SyscallProbeEntryTcpV4PreConnect, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on tcp_v4_pre_connect")
		return err
	}
	h.tcppv4 = tcppC4

	tcpC4, err := link.Kprobe("tcp_v4_connect", objs.SyscallProbeEntryTcpV4Connect, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on tcp_v4_connect")
		return err
	}
	h.tcpv4 = tcpC4

	tcpRC4, err := link.Kretprobe("tcp_v4_connect", objs.SyscallProbeRetTcpV4Connect, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on tcp_v4_connect")
		return err
	}
	h.tcpv4Ret = tcpRC4

	// Get the first-mounted cgroupv2 path.
	cGroupPath, err := detectCgroupPath(h.logger)
	if err != nil {
		utils.LogError(h.logger, err, "failed to detect the cgroup path")
		return err
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

	// FOR IPV6

	tcpPreC6, err := link.Kprobe("tcp_v6_pre_connect", objs.SyscallProbeEntryTcpV6PreConnect, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on tcp_v6_pre_connect")
		return err
	}
	h.tcppv6 = tcpPreC6

	tcpC6, err := link.Kprobe("tcp_v6_connect", objs.SyscallProbeEntryTcpV6Connect, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on tcp_v6_connect")
		return err
	}
	h.tcpv6 = tcpC6

	tcpRC6, err := link.Kretprobe("tcp_v6_connect", objs.SyscallProbeRetTcpV6Connect, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on tcp_v6_connect")
		return err
	}
	h.tcpv6Ret = tcpRC6

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

	//Open a kprobe at the entry of sendto syscall
	snd, err := link.Kprobe("sys_sendto", objs.SyscallProbeEntrySendto, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_sendto")
		return err
	}
	h.sendto = snd

	//Opening a kretprobe at the exit of sendto syscall
	sndr, err := link.Kretprobe("sys_sendto", objs.SyscallProbeRetSendto, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_sendto")
		return err
	}
	h.sendtoRet = sndr

	// ------------ For Ingress using Kprobes --------------

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac, err := link.Kprobe("sys_accept", objs.SyscallProbeEntryAccept, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_accept")
		return err
	}
	h.accept = ac

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	acRet, err := link.Kretprobe("sys_accept", objs.SyscallProbeRetAccept, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_accept")
		return err
	}
	h.acceptRet = acRet

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac4, err := link.Kprobe("sys_accept4", objs.SyscallProbeEntryAccept4, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_accept4")
		return err
	}
	h.accept4 = ac4

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac4Ret, err := link.Kretprobe("sys_accept4", objs.SyscallProbeRetAccept4, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_accept4")
		return err
	}
	h.accept4Ret = ac4Ret

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	rd, err := link.Kprobe("sys_read", objs.SyscallProbeEntryRead, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_read")
		return err
	}
	h.read = rd

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	rdRet, err := link.Kretprobe("sys_read", objs.SyscallProbeRetRead, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_read")
		return err
	}
	h.readRet = rdRet

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	wt, err := link.Kprobe("sys_write", objs.SyscallProbeEntryWrite, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_write")
		return err
	}
	h.write = wt

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	wtRet, err := link.Kretprobe("sys_write", objs.SyscallProbeRetWrite, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_write")
		return err
	}
	h.writeRet = wtRet

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program for writev.
	wtv, err := link.Kprobe("sys_writev", objs.SyscallProbeEntryWritev, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_writev")
		return err
	}
	h.writev = wtv

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program for writev.
	wtvRet, err := link.Kretprobe("sys_writev", objs.SyscallProbeRetWritev, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_writev")
		return err
	}
	h.writevRet = wtvRet

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	cl, err := link.Kprobe("sys_close", objs.SyscallProbeEntryClose, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_close")
		return err
	}
	h.close = cl

	//Attaching a kprobe at the entry of recvfrom syscall
	rcv, err := link.Kprobe("sys_recvfrom", objs.SyscallProbeEntryRecvfrom, nil)
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kprobe hook on sys_recvfrom")
		return err
	}
	h.recvfrom = rcv

	//Attaching a kretprobe at the exit of recvfrom syscall
	rcvr, err := link.Kretprobe("sys_recvfrom", objs.SyscallProbeRetRecvfrom, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_recvfrom")
		return err
	}
	h.recvfromRet = rcvr

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	clRet, err := link.Kretprobe("sys_close", objs.SyscallProbeRetClose, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		utils.LogError(h.logger, err, "failed to attach the kretprobe hook on sys_close")
		return err
	}
	h.closeRet = clRet

	h.logger.Info("keploy initialized and probes added to the kernel.")

	switch opts.Mode {
	case models.MODE_RECORD:
		err := h.SetKeployModeInKernel(1)
		if err != nil {
			utils.LogError(h.logger, nil, "failed to send the keploy mode to the ebpf program")
			return err
		}
	case models.MODE_TEST:
		err := h.SetKeployModeInKernel(2)
		if err != nil {
			utils.LogError(h.logger, nil, "failed to send the keploy mode to the ebpf program")
			return err
		}
	}

	//sending keploy pid to kernel to get filtered
	inode, err := getSelfInodeNumber()
	if err != nil {
		utils.LogError(h.logger, err, "failed to get inode of the keploy process")
		return err
	}
	h.logger.Debug("", zap.Any("Keploy Inode number", inode))
	err = h.SendNameSpaceID(1, inode)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the namespace id to the epbf program")
		return err
	}
	err = h.SendKeployPid(uint32(os.Getpid()))
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the keploy pid to the ebpf program")
		return err
	}
	h.logger.Debug("Keploy Pid sent successfully...")

	//send app pid to kernel to get filtered in case of mock record/test feature of unit test file
	// app pid here is the pid of the unit test file process or application pid
	if opts.Pid != 0 {
		err = h.SendAppPid(opts.Pid)
		if err != nil {
			utils.LogError(h.logger, err, "failed to send the app pid to the ebpf program")
			return err
		}
	}

	return nil
}

func (h *Hooks) Record(ctx context.Context, _ uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	// TODO use the session to get the app id
	// and then use the app id to get the test cases chan
	// and pass that to eBPF consumers/listeners
	return conn.ListenSocket(ctx, h.logger, h.objects.SocketOpenEvents, h.objects.SocketDataEvents, h.objects.SocketCloseEvents, opts)
}

func (h *Hooks) unLoad(_ context.Context) {
	// closing all events
	//other
	if err := h.socket.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the socket")
	}

	if err := h.bind.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the bind")
	}
	if err := h.udpp4.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the udpp4")
	}

	if err := h.connect4.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the connect4")
	}

	if err := h.gp4.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the gp4")
	}

	if err := h.tcppv4.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the tcppv4")
	}

	if err := h.tcpv4.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the tcpv4")
	}

	if err := h.tcpv4Ret.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the tcpv4Ret")
	}

	if err := h.connect6.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the connect6")
	}
	if err := h.gp6.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the gp6")
	}
	if err := h.tcppv6.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the tcppv6")
	}
	if err := h.tcpv6.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the tcpv6")
	}
	if err := h.tcpv6Ret.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the tcpv6Ret")
	}
	if err := h.accept.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the accept")
	}
	if err := h.acceptRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the acceptRet")
	}
	if err := h.accept4.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the accept4")
	}
	if err := h.accept4Ret.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the accept4Ret")
	}
	if err := h.read.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the read")
	}
	if err := h.readRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the readRet")
	}
	if err := h.write.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the write")
	}
	if err := h.writeRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the writeRet")
	}
	if err := h.writev.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the writev")
	}
	if err := h.writevRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the writevRet")
	}
	if err := h.close.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the close")
	}
	if err := h.closeRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the closeRet")
	}
	if err := h.sendto.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the sendto")
	}
	if err := h.sendtoRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the sendtoRet")
	}
	if err := h.recvfrom.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the recvfrom")
	}
	if err := h.recvfromRet.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the recvfromRet")
	}
	if err := h.objects.Close(); err != nil {
		utils.LogError(h.logger, err, "failed to close the objects")
	}
	h.logger.Info("eBPF resources released successfully...")
}
