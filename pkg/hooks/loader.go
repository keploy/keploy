package hooks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	sentry "github.com/getsentry/sentry-go"

	"go.uber.org/zap"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/clients"
	"go.keploy.io/server/pkg/clients/docker"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
)

var Emoji = "\U0001F430" + " Keploy:"

type Hook struct {
	proxyInfoMap     *ebpf.Map
	inodeMap         *ebpf.Map
	redirectProxyMap *ebpf.Map
	keployModeMap    *ebpf.Map
	keployPid        *ebpf.Map
	appPidMap        *ebpf.Map
	keployServerPort *ebpf.Map

	platform.TestCaseDB

	logger        *zap.Logger
	proxyPort     uint32
	tcsMocks      []*models.Mock
	configMocks   []*models.Mock
	mu            *sync.Mutex
	mutex         sync.RWMutex
	userAppCmd    *exec.Cmd
	mainRoutineId int

	// ebpf objects and events
	stopper  chan os.Signal
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
	sendto        link.Link
	sendtoRet     link.Link
	recvfrom      link.Link
	recvfromRet   link.Link
	objects       bpfObjects
	userIpAddress chan string
	writev        link.Link
	writevRet     link.Link

	idc clients.InternalDockerClient
}

func NewHook(db platform.TestCaseDB, mainRoutineId int, logger *zap.Logger) *Hook {
	idc, err := docker.NewInternalDockerClient(logger)
	if err != nil {
		logger.Fatal("failed to create internal docker client", zap.Error(err))
	}
	return &Hook{
		logger: logger,
		// db:          db,
		TestCaseDB:    db,
		mu:            &sync.Mutex{},
		userIpAddress: make(chan string),
		idc:           idc,
		mainRoutineId: mainRoutineId,
	}
}

func (h *Hook) SetProxyPort(port uint32) {
	h.proxyPort = port
}

func (h *Hook) GetProxyPort() uint32 {
	return h.proxyPort
}

func (h *Hook) GetDepsSize() int {
	h.mu.Lock()
	size := len(h.tcsMocks)
	defer h.mu.Unlock()
	return size
}

func (h *Hook) AppendMocks(m *models.Mock) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// h.tcsMocks = append(h.tcsMocks, m)
	err := h.TestCaseDB.WriteMock(m)
	if err != nil {
		return err
	}
	return nil
}
func (h *Hook) SetTcsMocks(m []*models.Mock) {
	h.mu.Lock()
	h.tcsMocks = m
	// fmt.Println("tcsMocks are set after aq ", h.tcsMocks)
	defer h.mu.Unlock()
}

func (h *Hook) SetConfigMocks(m []*models.Mock) {
	h.mu.Lock()
	h.configMocks = m
	// fmt.Println("tcsMocks are set after aq ", h.tcsMocks)
	defer h.mu.Unlock()
}

func (h *Hook) PopFront() {
	h.mu.Lock()
	h.tcsMocks = h.tcsMocks[1:]
	h.mu.Unlock()
}

func (h *Hook) PopIndex(index int) {
	h.mu.Lock()
	h.tcsMocks = append(h.tcsMocks[:index], h.tcsMocks[index+1:]...)
	h.mu.Unlock()
}

func (h *Hook) FetchDep(indx int) *models.Mock {
	h.mu.Lock()
	dep := h.tcsMocks[indx]
	// fmt.Println("tcsMocks in hooks: ", dep)
	// h.logger.Error("called FetchDep")

	defer h.mu.Unlock()
	return dep
}

func (h *Hook) GetTcsMocks() []*models.Mock {
	h.mu.Lock()
	tcsMocks := h.tcsMocks
	// fmt.Println("tcsMocks in hooks: ", tcsMocks)
	// h.logger.Error("called GetDeps")
	defer h.mu.Unlock()
	return tcsMocks
}

func (h *Hook) GetConfigMocks() []*models.Mock {
	h.mu.Lock()
	configMocks := h.configMocks
	// fmt.Println("tcsMocks in hooks: ", tcsMocks)
	// h.logger.Error("called GetDeps")
	defer h.mu.Unlock()
	return configMocks
}

func (h *Hook) ResetDeps() int {
	h.mu.Lock()
	h.tcsMocks = []*models.Mock{}
	// h.logger.Error("called ResetDeps", zap.Any("tcsMocks: ", h.tcsMocks))
	// fmt.Println("tcsMocks are reset")
	defer h.mu.Unlock()
	return 1
}

// SendKeployServerPort sends the keploy graphql server port to be filtered in the eBPF program.
func (h *Hook) SendKeployServerPort(port uint32) error {
	h.logger.Debug("sending keploy server port", zap.Any("port", port))
	key := 0
	err := h.keployServerPort.Update(uint32(key), &port, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send keploy server port to the epbf program", zap.Any("Keploy server port", port), zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

// This function sends the IP and Port of the running proxy in the eBPF program.
func (h *Hook) SendProxyInfo(ip4, port uint32, ip6 [4]uint32) error {
	key := 0
	err := h.proxyInfoMap.Update(uint32(key), structs.ProxyInfo{IP4: ip4, Ip6: ip6, Port: port}, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the proxy IP & Port to the epbf program", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

// This function is helpful when user application in running inside a docker container.
func (h *Hook) SendNameSpaceId(key uint32, inode uint64) error {
	err := h.inodeMap.Update(uint32(key), &inode, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the namespace id to the epbf program", zap.Any("error thrown by ebpf map", err.Error()), zap.Any("key", key), zap.Any("Inode", inode))
		return err
	}
	return nil
}

func (h *Hook) CleanProxyEntry(srcPort uint16) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	err := h.redirectProxyMap.Delete(srcPort)
	if err != nil {
		h.logger.Error("no such key present in the redirect proxy map", zap.Any("error thrown by ebpf map", err.Error()))
	}
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.Any("(Key)/SourcePort", srcPort))
}

// // printing the whole map
func (h *Hook) PrintRedirectProxyMap() {
	h.logger.Debug("--------Redirect Proxy Map-------")
	itr := h.redirectProxyMap.Iterate()
	var key uint16
	dest := structs.DestInfo{}

	for itr.Next(&key, &dest) {
		h.logger.Debug(fmt.Sprintf("Redirect Proxy:  [key:%v] || [value:%v]\n", key, dest))
	}
	h.logger.Debug("--------Redirect Proxy Map-------")
}

// GetDestinationInfo retrieves destination information associated with a source port.
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

// SendAppPid sends the application's process ID (PID) to the kernel.
// This function is used when running Keploy tests along with unit tests of the application.
func (h *Hook) SendAppPid(pid uint32) error {
	h.logger.Debug("Sending app pid to kernel", zap.Any("app Pid", pid))
	err := h.appPidMap.Update(uint32(0), &pid, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the app pid to the ebpf program", zap.Any("app Pid", pid), zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

func (h *Hook) SendKeployPid(kPid uint32) error {
	h.logger.Debug("Sending keploy pid to kernel", zap.Any("pid", kPid))
	err := h.keployPid.Update(uint32(0), &kPid, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to send the keploy pid to the ebpf program", zap.Any("Keploy Pid", kPid), zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	return nil
}

func (h *Hook) SetKeployModeInKernel(mode uint32) {
	key := 0
	err := h.keployModeMap.Update(uint32(key), &mode, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to set keploy mode in the epbf program", zap.Any("error thrown by ebpf map", err.Error()))
	}
}
func (h *Hook) killProcessesAndTheirChildren(parentPID int) {

	pids := []int{}

	h.findAndCollectChildProcesses(fmt.Sprintf("%d", parentPID), &pids)

	for _, childPID := range pids {
		if h.userAppCmd.ProcessState == nil {
			err := syscall.Kill(childPID, syscall.SIGKILL)
			if err != nil {
				h.logger.Error("failed to set kill child pid", zap.Any("error killing child process", err.Error()))
			}
		}
	}
}

func (h *Hook) findAndCollectChildProcesses(parentPID string, pids *[]int) {

	cmd := exec.Command("pgrep", "-P", parentPID)
	parentIDint, err := strconv.Atoi(parentPID)
	if err != nil {
		h.logger.Error("failed to convert parent PID to int", zap.Any("error converting parent PID to int", err.Error()))
	}

	*pids = append(*pids, parentIDint)

	output, err := cmd.Output()
	if err != nil {
		return
	}

	outputStr := string(output)
	childPIDs := strings.Split(outputStr, "\n")
	childPIDs = childPIDs[:len(childPIDs)-1]

	for _, childPID := range childPIDs {
		if childPID != "" {
			h.findAndCollectChildProcesses(childPID, pids)
		}
	}
}

// StopUserApplication stops the user application
func (h *Hook) StopUserApplication() {
	if h.userAppCmd != nil && h.userAppCmd.Process != nil {
		h.logger.Debug("the process state for the user process", zap.String("state", h.userAppCmd.ProcessState.String()), zap.Any("processState", h.userAppCmd.ProcessState))
		if h.userAppCmd.ProcessState != nil && h.userAppCmd.ProcessState.Exited() {
			return
		}

		// Stop Docker Container and Remove it if Keploy ran using docker.
		containerID := h.idc.GetContainerID()
		if len(containerID) != 0  {
			err := h.idc.StopAndRemoveDockerContainer()
			if err != nil {
				h.logger.Error(fmt.Sprintf("Failed to stop/remove the docker container %s. Please stop and remove the application container manually.", containerID), zap.Error(err))
			}
		}

		h.killProcessesAndTheirChildren(h.userAppCmd.Process.Pid)
	}
}

func (h *Hook) Recover(id int) {

	if r := recover(); r != nil {
		h.logger.Debug("Recover from panic in go routine", zap.Any("current routine id", id), zap.Any("main routine id", h.mainRoutineId))
		h.Stop(true, nil)
		// stop the user application cmd
		h.StopUserApplication()
		if id != h.mainRoutineId {
			log.Panic(r)
			os.Exit(1)
		}
	}
}

func (h *Hook) Stop(forceStop bool, close chan bool) {

	if close == nil {
		close = make(chan bool)
	}

	if !forceStop {
		select {
		case <-close:
			return
		case <-h.stopper:
			h.logger.Info("Received signal to exit keploy program..")
			h.StopUserApplication()
		}
	} else {
		h.logger.Info("Exiting keploy program gracefully.")
	}

	// closing all readers.
	for _, reader := range PerfEventReaders {
		if err := reader.Close(); err != nil {
			h.logger.Error("failed to close the eBPF perf reader", zap.Error(err))
			// log.Fatalf("closing perf reader: %s", err)
		}
	}
	for _, reader := range RingEventReaders {
		if err := reader.Close(); err != nil {
			h.logger.Error("failed to close the eBPF ringbuf reader", zap.Error(err))
			// log.Fatalf("closing ringbuf reader: %s", err)
		}
	}

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
	h.objects.Close()
	h.writev.Close()
	h.writevRet.Close()
	h.logger.Info("eBPF resources released successfully...")
}

// LoadHooks is used to attach the eBPF hooks into the linux kernel. Hooks are attached for outgoing and incoming network requests.
//
// proxyPorts is used for redirecting outgoing network calls to the unoccupied proxy server.
//
// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS -no-global-types -target $TARGET bpf keploy_ebpf.c -- -I./headers -I./headers/$TARGET
func (h *Hook) LoadHooks(appCmd, appContainer string, pid uint32, ctx context.Context) error {
	// k := keploy.KeployInitializer()

	if err := settings.InitRealTimeOffset(); err != nil {
		h.logger.Error("failed to fix the BPF clock", zap.Error(err))
		return err
	}

	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

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

	h.stopper = stopper
	h.objects = objs

	connectionFactory := connection.NewFactory(time.Minute, h.logger)
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer sentry.Recover()
		for {
			connectionFactory.HandleReadyConnections(h.TestCaseDB, ctx)
			time.Sleep(1 * time.Second)
		}
	}()

	// ----- used in case of wsl -----
	socket, err := link.Kprobe("sys_socket", objs.SyscallProbeEntrySocket, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening sys_socket kprobe: %s", err)
	}
	h.socket = socket

	// ------------ For Egress -------------

	bind, err := link.Kprobe("sys_bind", objs.SyscallProbeEntryBind, nil)
	if err != nil {
		log.Fatalf(Emoji, "opening sys_bind kprobe: %s", err)
	}
	h.bind = bind

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
		h.logger.Error("failed to detect the cgroup path", zap.Error(err))
		return err
	}

	c4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: objs.K_connect4,
	})

	if err != nil {
		h.logger.Error("failed to attach the connect4 cgroup hook", zap.Error(err))
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
		h.logger.Error("failed to attach GetPeername4 cgroup hook", zap.Error(err))
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
		h.logger.Error("failed to attach the connect6 cgroup hook", zap.Error(err))
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
		h.logger.Error("failed to attach GetPeername6 cgroup hook", zap.Error(err))
		return err
	}
	h.gp6 = gp6
	// defer gp4.Close()

	//Open a kprobe at the entry of sendto syscall
	snd, err := link.Kprobe("sys_sendto", objs.SyscallProbeEntrySendto, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_sendto", zap.Error(err))
		return err
	}
	h.sendto = snd
	// defer snd.Close()

	//Opening a kretprobe at the exit of sendto syscall
	sndr, err := link.Kretprobe("sys_sendto", objs.SyscallProbeRetSendto, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_sendto", zap.Error(err))
		return err
	}
	h.sendtoRet = sndr
	// defer sndr.Close()

	// ------------ For Ingress using Kprobes --------------

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac, err := link.Kprobe("sys_accept", objs.SyscallProbeEntryAccept, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_accept", zap.Error(err))
		return err
	}
	h.accept = ac
	// defer ac.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac_, err := link.Kretprobe("sys_accept", objs.SyscallProbeRetAccept, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_accept", zap.Error(err))
		return err
	}
	h.acceptRet = ac_
	// defer ac_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac4, err := link.Kprobe("sys_accept4", objs.SyscallProbeEntryAccept4, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_accept4", zap.Error(err))
		return err
	}
	h.accept4 = ac4
	// defer ac4.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac4_, err := link.Kretprobe("sys_accept4", objs.SyscallProbeRetAccept4, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_accept4", zap.Error(err))
		return err
	}
	h.accept4Ret = ac4_
	// defer ac4_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	rd, err := link.Kprobe("sys_read", objs.SyscallProbeEntryRead, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_read", zap.Error(err))
		return err
	}
	h.read = rd
	// defer rd.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	rd_, err := link.Kretprobe("sys_read", objs.SyscallProbeRetRead, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_read", zap.Error(err))
		return err
	}
	h.readRet = rd_
	// defer rd_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	wt, err := link.Kprobe("sys_write", objs.SyscallProbeEntryWrite, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.write = wt
	// defer wt.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	wt_, err := link.Kretprobe("sys_write", objs.SyscallProbeRetWrite, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.writeRet = wt_
	// defer wt_.Close()

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
	// defer cl.Close()

	//Attaching a kprobe at the entry of recvfrom syscall
	rcv, err := link.Kprobe("sys_recvfrom", objs.SyscallProbeEntryRecvfrom, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_recvfrom", zap.Error(err))
		return err
	}
	h.recvfrom = rcv
	// defer rcv.Close()

	//Attaching a kretprobe at the exit of recvfrom syscall
	rcvr, err := link.Kretprobe("sys_recvfrom", objs.SyscallProbeRetRecvfrom, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_recvfrom", zap.Error(err))
		return err
	}
	h.recvfromRet = rcvr
	// defer rcvr.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	cl_, err := link.Kretprobe("sys_close", objs.SyscallProbeRetClose, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.closeRet = cl_
	// defer cl_.Close()

	h.LaunchPerfBufferConsumers(connectionFactory)

	h.logger.Info("keploy initialized and probes added to the kernel.")

	switch models.GetMode() {
	case models.MODE_RECORD:
		h.SetKeployModeInKernel(1)
	case models.MODE_TEST:
		h.SetKeployModeInKernel(2)
	}

	//sending keploy pid to kernel to get filtered
	k_inode := getSelfInodeNumber()
	h.logger.Debug("", zap.Any("Keploy Inode number", k_inode))
	h.SendNameSpaceId(1, k_inode)
	h.SendKeployPid(uint32(os.Getpid()))
	h.logger.Debug("Keploy Pid sent successfully...")

	//send app pid to kernel to get filtered in case of integration with unit test file
	// app pid here is the pid of the unit test file process or application pid
	if pid != 0 {
		h.SendAppPid(pid)
	}

	return nil
}

// to access the IP address of the hook
func (h *Hook) GetUserIP() string {
	h.logger.Debug("getting user ip address...")
	return <-h.userIpAddress
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
