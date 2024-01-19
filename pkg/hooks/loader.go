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
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/google/uuid"

	"go.uber.org/zap"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/clients"
	"go.keploy.io/server/pkg/clients/docker"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/utils"
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
	passthroughPorts *ebpf.Map

	platform.TestCaseDB

	logger                   *zap.Logger
	proxyPort                uint32
	localDb                  *localDb
	mu                       *sync.Mutex
	mutex                    sync.RWMutex
	userAppCmd               *exec.Cmd
	userAppShutdownInitiated bool
	mainRoutineId            int

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

	idc         clients.InternalDockerClient
	configMocks []*models.Mock
}

func NewHook(db platform.TestCaseDB, mainRoutineId int, logger *zap.Logger) (*Hook, error) {
	idc, err := docker.NewInternalDockerClient(logger)
	if err != nil {
		logger.Fatal("failed to create internal docker client", zap.Error(err))
	}

	schemaMap := map[string]map[string]string{
		mockTable: {
			mockTableIndex: mockTableIndexField,
		},
		configMockTable: {
			configMockTableIndex: configMockTableIndexField,
		},
	}

	ldb, err := NewLocalDb(schemaMap)
	if err != nil {
		return nil, fmt.Errorf("error while creating new LocalDb: %v", err)
	}

	return &Hook{
		logger:        logger,
		TestCaseDB:    db,
		localDb:       ldb,
		mu:            &sync.Mutex{},
		userIpAddress: make(chan string),
		idc:           idc,
		mainRoutineId: mainRoutineId,
	}, nil
}

func (h *Hook) SetProxyPort(port uint32) {
	h.proxyPort = port
}

func (h *Hook) GetProxyPort() uint32 {
	return h.proxyPort
}

func (h *Hook) SetUserCommand(appCmd *exec.Cmd) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.userAppCmd = appCmd
}

func (h *Hook) GetUserCommand() *exec.Cmd {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.userAppCmd
}

func (h *Hook) AppendMocks(m *models.Mock, ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.TestCaseDB.WriteMock(m, ctx)
	if err != nil {
		return err
	}
	return nil
}

func (h *Hook) SetTcsMocks(m []*models.Mock) error {
	h.localDb.deleteAll(mockTable, mockTableIndex)
	for _, mock := range m {
		mock.Id = uuid.NewString()
		err := h.localDb.insert(mockTable, mock)
		if err != nil {
			return fmt.Errorf("error while inserting tcs mock into localDb: %v", err)
		}
	}
	return nil
}

func (h *Hook) SetConfigMocks(m []*models.Mock) error {
	h.localDb.deleteAll(configMockTable, configMockTableIndex)
	for _, mock := range m {
		mock.Id = uuid.NewString()
		err := h.localDb.insert(configMockTable, mock)
		if err != nil {
			return fmt.Errorf("error while inserting config-mock into localDb: %v", err)
		}
	}
	return nil
}

func (h *Hook) GetTcsMocks() ([]*models.Mock, error) {
	it, err := h.localDb.getAll(mockTable, mockTableIndex)
	if err != nil {
		return nil, fmt.Errorf("error while getting all tcs mocks from localDb %v", err)
	}
	var mocks []*models.Mock
	for obj := it.Next(); obj != nil; obj = it.Next() {
		p := obj.(*models.Mock)
		mocks = append(mocks, p)
	}
	sort.Slice(mocks, func(i, j int) bool {
		return mocks[i].Spec.ReqTimestampMock.Before(mocks[j].Spec.ReqTimestampMock)
	})
	return mocks, nil
}

func (h *Hook) IsUsrAppTerminateInitiated() bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.userAppShutdownInitiated
}

func (h *Hook) SetUsrAppTerminateInitiated(appTerminated bool) {
	h.userAppShutdownInitiated = appTerminated
}

func (h *Hook) GetConfigMocks() ([]*models.Mock, error) {
	it, err := h.localDb.getAll(configMockTable, configMockTableIndex)
	if err != nil {
		return nil, fmt.Errorf("error while getting all config-mocks from localDb %v", err)
	}
	var mocks []*models.Mock
	for obj := it.Next(); obj != nil; obj = it.Next() {
		p := obj.(*models.Mock)
		mocks = append(mocks, p)
	}
	sort.Slice(mocks, func(i, j int) bool {
		return mocks[i].Spec.ReqTimestampMock.Before(mocks[j].Spec.ReqTimestampMock)
	})
	return mocks, nil
}

func (h *Hook) DeleteTcsMock(mock *models.Mock) (bool, error) {
	isDeleted, err := h.localDb.delete(mockTable, mock)
	if err != nil {
		return isDeleted, fmt.Errorf("error while deleting tcs mocks %v from localDb %v", mock, err)
	}
	return isDeleted, nil
}

func (h *Hook) DeleteConfigMock(mock *models.Mock) (bool, error) {
	isDeleted, err := h.localDb.delete(configMockTable, configMockTableIndex)
	if err != nil {
		return isDeleted, fmt.Errorf("error while deleting config-mocks %v from localDb %v", mock, err)
	}
	return isDeleted, nil
}

func (h *Hook) ResetDeps() int {
	h.localDb.deleteAll(mockTable, mockTableIndex)
	return 1
}

// SendPassThroughPorts sends the destination ports of the server which should not be intercepted by keploy proxy.
func (h *Hook) SendPassThroughPorts(filterPorts []uint) error {
	portsSize := len(filterPorts)
	if portsSize > 10 {
		h.logger.Error("can not send more than 10 ports to be filtered to the ebpf program")
		return fmt.Errorf("passthrough ports limit exceeded")
	}

	var ports [10]int32

	for i := 0; i < 10; i++ {
		if i < portsSize {
			// Convert uint to int32
			ports[i] = int32(filterPorts[i])
		} else {
			// Fill the remaining elements with -1
			ports[i] = -1
		}
	}

	for i, v := range ports {
		h.logger.Debug(fmt.Sprintf("PassthroughPort(%v):[%v]", i, v))
		err := h.passthroughPorts.Update(uint32(i), &v, ebpf.UpdateAny)
		if err != nil {
			h.logger.Error("failed to send the passthrough ports to the ebpf program", zap.Any("error thrown by ebpf map", err.Error()))
			return err
		}
	}
	return nil
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
		err := syscall.Kill(childPID, syscall.SIGTERM)
		if err != nil {
			h.logger.Error("failed to set kill child pid", zap.Any("error killing child process", err.Error()))
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
	userAppCmd := h.GetUserCommand()
	h.mutex.Lock()
	h.SetUsrAppTerminateInitiated(true)
	var process *os.Process
	if userAppCmd != nil && userAppCmd.Process != nil {
		process = userAppCmd.Process
	}

	h.mutex.Unlock()

	if userAppCmd != nil && process != nil {

		// Stop Docker Container and Remove it if Keploy ran using docker.
		containerID := h.idc.GetContainerID()
		if len(containerID) != 0 {
			err := h.idc.StopAndRemoveDockerContainer()
			if err != nil {
				h.logger.Error(fmt.Sprintf("Failed to stop/remove the docker container %s. Please stop and remove the application container manually.", containerID), zap.Error(err))
			}
		}
		if process != nil {
			pid := process.Pid

			h.killProcessesAndTheirChildren(pid)
		}
	}
}

func (h *Hook) Recover(id int) {

	if r := recover(); r != nil {
		// Get the stack trace
		stackTrace := debug.Stack()

		h.logger.Debug("Recover from panic in go routine", zap.Any("current routine id", id), zap.Any("main routine id", h.mainRoutineId), zap.Any("stack trace", string(stackTrace)))
		h.Stop(true)
		// stop the user application cmd
		h.StopUserApplication()
		if id != h.mainRoutineId {
			log.Panic(r)
			os.Exit(1)
		}
	}
}

func deleteFileIfExists(filename string, logger *zap.Logger) error {
	// Check if file exists
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		// File exists, delete it
		err = os.Remove(filename)
		if err != nil {
			return fmt.Errorf("failed to delete the file: %v", err)
		}
		logger.Debug(fmt.Sprintf("File %s deleted successfully", filename))
	} else {
		logger.Debug(fmt.Sprintf("File %s doesn't exist", filename))
	}
	return nil
}

func (h *Hook) Stop(forceStop bool) {

	if !forceStop {
		h.logger.Info("Received signal to exit keploy program..")
		h.StopUserApplication()
	} else {
		h.logger.Info("Exiting keploy program gracefully.")
	}

	//deleting kdocker-compose.yaml file if made during the process in case of docker-compose env
	deleteFileIfExists("kdocker-compose.yaml", h.logger)

	// closing all readers.
	for _, reader := range PerfEventReaders {
		if err := reader.Close(); err != nil {
			h.logger.Error("failed to close the eBPF perf reader", zap.Error(err))
		}
	}
	for _, reader := range RingEventReaders {
		if err := reader.Close(); err != nil {
			h.logger.Error("failed to close the eBPF ringbuf reader", zap.Error(err))
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
func (h *Hook) LoadHooks(appCmd, appContainer string, pid uint32, ctx context.Context, filters *models.Filters) error {
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
	h.passthroughPorts = objs.PassThroughPorts

	h.stopper = stopper
	h.objects = objs

	connectionFactory := connection.NewFactory(time.Minute, h.logger)
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer utils.HandlePanic()
		for {
			connectionFactory.HandleReadyConnections(h.TestCaseDB, ctx, filters)
			// time.Sleep(1 * time.Second)
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
