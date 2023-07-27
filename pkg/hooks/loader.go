package hooks

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type Hook struct {
	proxyInfoMap     *ebpf.Map
	appPidMap        *ebpf.Map
	inodeMap         *ebpf.Map
	redirectProxyMap *ebpf.Map
	keployModeMap    *ebpf.Map
	keployPid        *ebpf.Map

	db            platform.TestCaseDB
	logger        *zap.Logger
	proxyPortList []uint32
	deps          []*models.Mock
	mu            *sync.Mutex
	respChannel   chan *models.HttpResp
	mutex         sync.RWMutex
	userAppCmd    *exec.Cmd

	// ebpf objects and events
	stopper  chan os.Signal
	connect4 link.Link
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

	accept        link.Link
	acceptRet     link.Link
	accept4       link.Link
	accept4Ret    link.Link
	read          link.Link
	readRet       link.Link
	write         link.Link
	writeRet      link.Link
	close         link.Link
	closeRet      link.Link
	objects       bpfObjects
	userIpAddress string
}

func NewHook(db platform.TestCaseDB, logger *zap.Logger) *Hook {
	return &Hook{
		logger:      logger,
		db:          db,
		mu:          &sync.Mutex{},
		respChannel: make(chan *models.HttpResp),
	}
}

func (h *Hook) GetDepsSize() int {
	h.mu.Lock()
	size := len(h.deps)
	defer h.mu.Unlock()
	return size
}

func (h *Hook) AppendDeps(m *models.Mock) {
	h.mu.Lock()
	h.deps = append(h.deps, m)
	h.mu.Unlock()

}
func (h *Hook) SetDeps(m []*models.Mock) {
	h.mu.Lock()
	h.deps = m
	// fmt.Println("deps are set after aq ", h.deps)
	defer h.mu.Unlock()

}
func (h *Hook) PopFront() {
	h.mu.Lock()
	h.deps = h.deps[1:]
	h.mu.Unlock()
}
func (h *Hook) FetchDep(indx int) *models.Mock {
	h.mu.Lock()
	dep := h.deps[indx]
	// fmt.Println("deps in hooks: ", dep)
	// h.logger.Error("called FetchDep")

	defer h.mu.Unlock()
	return dep
}

func (h *Hook) GetDeps() []*models.Mock {
	h.mu.Lock()
	deps := h.deps
	// fmt.Println("deps in hooks: ", deps)
	// h.logger.Error("called GetDeps")
	defer h.mu.Unlock()
	return deps
}
func (h *Hook) ResetDeps() int {
	h.mu.Lock()
	h.deps = []*models.Mock{}
	// h.logger.Error("called ResetDeps", zap.Any("deps: ", h.deps))
	// fmt.Println("deps are reset")
	defer h.mu.Unlock()
	return 1
}
func (h *Hook) PutResp(resp *models.HttpResp) error {
	h.respChannel <- resp
	return nil
}
func (h *Hook) GetResp() *models.HttpResp {
	resp := <-h.respChannel
	return resp
}

// This function sends the IP and Port of the running proxy in the eBPF program.
func (h *Hook) SendProxyInfo(ip4, port uint32, ip6 [4]uint32) error {
	key := 0
	err := h.proxyInfoMap.Update(uint32(key), structs.ProxyInfo{IP4: ip4, Ip6: ip6, Port: port}, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error(Emoji+"failed to send the proxy IP & Port to the epbf program", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

// This function is helpful when user application in running inside a docker container.
func (h *Hook) SendNameSpaceId(key uint32, inode uint64) error {
	err := h.inodeMap.Update(uint32(key), &inode, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error(Emoji+"failed to send the namespace id to the epbf program", zap.Any("error thrown by ebpf map", err.Error()), zap.Any("key", key), zap.Any("Inode", inode))
		return err
	}
	return nil
}

func (h *Hook) CleanProxyEntry(srcPort uint16) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	err := h.redirectProxyMap.Delete(srcPort)
	if err != nil {
		h.logger.Error(Emoji+"no such key present in the redirect proxy map", zap.Any("error thrown by ebpf map", err.Error()))
	}
	h.logger.Debug(Emoji+"successfully removed entry from redirect proxy map", zap.Any("(Key)/SourcePort", srcPort))
}

// // printing the whole map
func (h *Hook) PrintRedirectProxyMap() {
	println("------Redirect Proxy map-------")
	h.logger.Debug(Emoji + "--------Redirect Proxy Map-------")
	itr := h.redirectProxyMap.Iterate()
	var key uint16
	dest := structs.DestInfo{}

	for itr.Next(&key, &dest) {
		h.logger.Debug(Emoji + fmt.Sprintf("Redirect Proxy:  [key:%v] || [value:%v]\n", key, dest))
	}
	h.logger.Debug(Emoji + "--------Redirect Proxy Map-------")
}

func (h *Hook) GetDestinationInfo(srcPort uint16) (*structs.DestInfo, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	destInfo := structs.DestInfo{}
	if err := h.redirectProxyMap.Lookup(srcPort, &destInfo); err != nil {
		// h.logger.Error("failed to fetch the destination info", zap.Error(err))
		return nil, err
	}
	return &destInfo, nil
}

func (h *Hook) SendApplicationPIDs(appPids [15]int32) error {
	for i, v := range appPids {
		err := h.appPidMap.Update(uint32(i), &v, ebpf.UpdateAny)
		if err != nil {
			// h.logger.Error("failed to send the application pids to the ebpf program", zap.Any("error thrown by ebpf map", err.Error()))
			return err
		}
	}
	return nil
}

func (h *Hook) SendKeployPid(kPid uint32) error {
	h.logger.Debug(Emoji+"Sending keploy pid to kernel", zap.Any("pid", kPid))
	err := h.keployPid.Update(uint32(0), &kPid, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error(Emoji+"failed to send the keploy pid to the ebpf program", zap.Any("Keploy Pid", kPid), zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

func (h *Hook) SetKeployModeInKernel(mode uint32) {
	key := 0
	err := h.keployModeMap.Update(uint32(key), &mode, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error(Emoji+"failed to set keploy mode in the epbf program", zap.Any("error thrown by ebpf map", err.Error()))
	}
}

func (h *Hook) Stop(forceStop bool) {
	if !forceStop {
		<-h.stopper
		h.logger.Info(Emoji + "Received signal, exiting program..")

	}
	h.logger.Info(Emoji + "Received signal, exiting program..")

	// closing all readers.
	for _, reader := range PerfEventReaders {
		if err := reader.Close(); err != nil {
			h.logger.Error(Emoji+"failed to close the eBPF perf reader", zap.Error(err))
			// log.Fatalf("closing perf reader: %s", err)
		}
	}
	for _, reader := range RingEventReaders {
		if err := reader.Close(); err != nil {
			h.logger.Error(Emoji+"failed to close the eBPF ringbuf reader", zap.Error(err))
			// log.Fatalf("closing ringbuf reader: %s", err)
		}
	}

	// stop the user application cmd
	if h.userAppCmd != nil && h.userAppCmd.Process != nil {
		err := h.userAppCmd.Process.Kill()
		if err != nil {
			h.logger.Error(Emoji+"failed to stop user application", zap.Error(err))
		} else {
			h.logger.Info(Emoji + "User application stopped successfully...")
		}
	}

	// closing all events
	//egress
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
	h.objects.Close()

	h.logger.Info(Emoji + "eBPF resources released successfully...")
}

// LoadHooks is used to attach the eBPF hooks into the linux kernel. Hooks are attached for outgoing and incoming network requests.
//
// proxyPorts is used for redirecting outgoing network calls to the unoccupied proxy server.
//
// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS -no-global-types -target $TARGET bpf keploy_ebpf.c -- -I./headers
func (h *Hook) LoadHooks(appCmd, appContainer string) error {
	// k := keploy.KeployInitializer()

	if err := settings.InitRealTimeOffset(); err != nil {
		h.logger.Error(Emoji+"failed to fix the BPF clock", zap.Error(err))
		return err
	}

	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		h.logger.Error(Emoji+"failed to lock memory for eBPF resources", zap.Error(err))
		return err
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		h.logger.Error(Emoji+"failed to load eBPF objects", zap.Error(err))
		return err
	}

	h.proxyInfoMap = objs.ProxyInfoMap
	h.appPidMap = objs.AppPidMap
	h.inodeMap = objs.InodeMap
	h.redirectProxyMap = objs.RedirectProxyMap
	h.keployModeMap = objs.KeployModeMap
	h.keployPid = objs.KeployPidMap

	h.stopper = stopper
	h.objects = objs

	connectionFactory := connection.NewFactory(time.Minute, h.respChannel, h.logger)
	go func() {
		for {
			connectionFactory.HandleReadyConnections(h.db, h.GetDeps, h.ResetDeps)
			time.Sleep(1 * time.Second)
		}
	}()
	// ------------ For Egress -------------

	udpp_c4, err := link.Kprobe("udp_pre_connect", objs.SyscallProbeEntryUdpPreConnect, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening udp_pre_connect kprobe: %s", err)
	}
	h.udpp4 = udpp_c4

	// FOR IPV4
	tcpp_c4, err := link.Kprobe("tcp_v4_pre_connect", objs.SyscallProbeEntryTcpV4PreConnect, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening tcp_v4_pre_connect kprobe: %s", err)
	}
	h.tcppv4 = tcpp_c4

	tcp_c4, err := link.Kprobe("tcp_v4_connect", objs.SyscallProbeEntryTcpV4Connect, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening tcp_v4_connect kprobe: %s", err)
	}
	h.tcpv4 = tcp_c4

	tcp_r_c4, err := link.Kretprobe("tcp_v4_connect", objs.SyscallProbeRetTcpV4Connect, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		log.Fatalf(Emoji, "opening tcp_v4_connect kretprobe: %s", err)
	}
	h.tcpv4Ret = tcp_r_c4

	// Get the first-mounted cgroupv2 path.
	cgroupPath, err := detectCgroupPath()
	if err != nil {
		h.logger.Error(Emoji+"failed to detect the cgroup path", zap.Error(err))
		return err
	}

	c4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: objs.K_connect4,
	})

	if err != nil {
		h.logger.Error(Emoji+"failed to attach the connect4 cgroup hook", zap.Error(err))
		return err
	}
	h.connect4 = c4
	// defer c4.Close()

	gp4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCgroupInet4GetPeername,
		Program: objs.K_getpeername4,
	})

	if err != nil {
		h.logger.Error(Emoji+"failed to attach GetPeername4 cgroup hook", zap.Error(err))
		return err
	}
	h.gp4 = gp4
	// defer gp4.Close()

	// FOR IPV6

	tcpp_c6, err := link.Kprobe("tcp_v6_pre_connect", objs.SyscallProbeEntryTcpV6PreConnect, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening tcp_v6_pre_connect kprobe: %s", err)
	}
	h.tcppv6 = tcpp_c6

	tcp_c6, err := link.Kprobe("tcp_v6_connect", objs.SyscallProbeEntryTcpV6Connect, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening tcp_v6_connect kprobe: %s", err)
	}
	h.tcpv6 = tcp_c6

	tcp_r_c6, err := link.Kretprobe("tcp_v6_connect", objs.SyscallProbeRetTcpV6Connect, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		log.Fatalf(Emoji, "opening tcp_v6_connect kretprobe: %s", err)
	}
	h.tcpv6Ret = tcp_r_c6

	c6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInet6Connect,
		Program: objs.K_connect6,
	})

	if err != nil {
		h.logger.Error(Emoji+"failed to attach the connect6 cgroup hook", zap.Error(err))
		return err
	}
	h.connect6 = c6
	// defer c6.Close()

	gp6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCgroupInet6GetPeername,
		Program: objs.K_getpeername6,
	})

	if err != nil {
		h.logger.Error(Emoji+"failed to attach GetPeername6 cgroup hook", zap.Error(err))
		return err
	}
	h.gp6 = gp6
	// defer gp4.Close()

	// ------------ For Ingress using Kprobes --------------

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac, err := link.Kprobe("sys_accept", objs.SyscallProbeEntryAccept, nil)
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kprobe hook on sys_accept", zap.Error(err))
		return err
	}
	h.accept = ac
	// defer ac.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac_, err := link.Kretprobe("sys_accept", objs.SyscallProbeRetAccept, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kretprobe hook on sys_accept", zap.Error(err))
		return err
	}
	h.acceptRet = ac_
	// defer ac_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac4, err := link.Kprobe("sys_accept4", objs.SyscallProbeEntryAccept4, nil)
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kprobe hook on sys_accept4", zap.Error(err))
		return err
	}
	h.accept4 = ac4
	// defer ac4.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac4_, err := link.Kretprobe("sys_accept4", objs.SyscallProbeRetAccept4, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kretprobe hook on sys_accept4", zap.Error(err))
		return err
	}
	h.accept4Ret = ac4_
	// defer ac4_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	rd, err := link.Kprobe("sys_read", objs.SyscallProbeEntryRead, nil)
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kprobe hook on sys_read", zap.Error(err))
		return err
	}
	h.read = rd
	// defer rd.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	rd_, err := link.Kretprobe("sys_read", objs.SyscallProbeRetRead, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kretprobe hook on sys_read", zap.Error(err))
		return err
	}
	h.readRet = rd_
	// defer rd_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	wt, err := link.Kprobe("sys_write", objs.SyscallProbeEntryWrite, nil)
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.write = wt
	// defer wt.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	wt_, err := link.Kretprobe("sys_write", objs.SyscallProbeRetWrite, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kretprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.writeRet = wt_
	// defer wt_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	cl, err := link.Kprobe("sys_close", objs.SyscallProbeEntryClose, nil)
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.close = cl
	// defer cl.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	cl_, err := link.Kretprobe("sys_close", objs.SyscallProbeRetClose, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error(Emoji+"failed to attach the kretprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.closeRet = cl_
	// defer cl_.Close()

	LaunchPerfBufferConsumers(objs, connectionFactory, stopper, h.logger)

	h.logger.Info(Emoji + "keploy initialized and probes added to the kernel.")

	switch models.GetMode() {
	case models.MODE_RECORD:
		h.SetKeployModeInKernel(1)
	case models.MODE_TEST:
		h.SetKeployModeInKernel(2)
	}

	//sending keploy pid to kernel to get filtered
	k_inode := getSelfInodeNumber()
	h.logger.Debug(Emoji, zap.Any("Keploy Inode number", k_inode))
	h.SendNameSpaceId(1, k_inode)
	h.SendKeployPid(uint32(os.Getpid()))
	h.logger.Debug(Emoji + "Keploy Pid sent successfully...")

	return nil
}

// to access the IP address of the hook
func (h *Hook) GetUserIP() string {
	return h.userIpAddress
}

// detectCgroupPath returns the first-found mount point of type cgroup2
// and stores it in the cgroupPath global variable.
func detectCgroupPath() (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// example fields: cgroup2 /sys/fs/cgroup/unified cgroup2 rw,nosuid,nodev,noexec,relatime 0 0
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1], nil
		}
	}

	return "", errors.New("cgroup2 not mounted")
}

func platformPrefix(symbol string) string {
	// per https://github.com/golang/go/blob/master/src/go/build/syslist.go
	// and https://github.com/libbpf/libbpf/blob/master/src/libbpf.c#L10047
	var prefix string
	switch runtime.GOARCH {
	case "386":
		prefix = "ia32"
	case "amd64", "amd64p32":
		prefix = "x64"

	case "arm", "armbe":
		prefix = "arm"
	case "arm64", "arm64be":
		prefix = "arm64"

	case "mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le":
		prefix = "mips"

	case "s390":
		prefix = "s390"
	case "s390x":
		prefix = "s390x"

	case "riscv", "riscv64":
		prefix = "riscv"

	case "ppc":
		prefix = "powerpc"
	case "ppc64", "ppc64le":
		prefix = "powerpc64"

	default:
		return symbol
	}

	return fmt.Sprintf("__%s_%s", prefix, symbol)
}
