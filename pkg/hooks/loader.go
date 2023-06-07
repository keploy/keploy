package hooks

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/models/spec"
	"go.keploy.io/server/pkg/platform"
	"go.uber.org/zap"
)

type Bpf_spin_lock struct{ Val uint32 }

type PortState struct {
	Port      uint32
	Occupied  uint32
	Dest_ip   uint32
	Dest_port uint32
	Lock      Bpf_spin_lock
}

type Hook struct {
	proxyStateMap      *ebpf.Map
	db  			platform.TestCaseDB
	logger 			*zap.Logger
	proxyPortList 		[]uint32
	deps 			[]*models.Mock
	respChannel 	chan *spec.HttpRespYaml

	// ebpf objects and events
	stopper 		chan os.Signal
	connect4 		link.Link
	connect6 		link.Link
	gp4 			link.Link
	accept 			link.Link
	acceptRet 		link.Link
	accept4 		link.Link
	accept4Ret 		link.Link
	read 			link.Link
	readRet 		link.Link
	write 			link.Link
	writeRet 		link.Link
	close 			link.Link
	closeRet 		link.Link
	objects 		bpfObjects
}

func NewHook(proxyPorts []uint32, db platform.TestCaseDB, logger *zap.Logger) *Hook {
	return &Hook{
		// ProxyPorts: proxyPorts,
		logger: logger,
		proxyPortList: proxyPorts,
		db: db,
		respChannel: make(chan *spec.HttpRespYaml),
	}
}

func (h *Hook) AppendDeps(m *models.Mock)  {
	h.deps = append(h.deps, m)
}

func (h *Hook) FetchDep (indx int) *models.Mock {
	return h.deps[indx]
}

func (h *Hook) GetDeps () []*models.Mock {
	return h.deps
}
func (h *Hook) ResetDeps() int {
	h.deps = []*models.Mock{}
	return 1
}
func (h *Hook) PutResp(resp *spec.HttpRespYaml) error {
	h.respChannel <- resp
	return nil
}
func (h *Hook) GetResp() *spec.HttpRespYaml  {
	resp := <-h.respChannel
	return resp
}

func (h *Hook) UpdateProxyState (indx uint32, ps *PortState) {
	err := h.proxyStateMap.Update(uint32(indx), *ps, ebpf.UpdateLock)
	if err != nil {
		h.logger.Error("failed to release the occupied proxy", zap.Error(err), zap.Any("proxy port", ps.Port))
		return
	}
}

func (h *Hook) GetProxyState(i uint32) (*PortState, error) {
	proxyState := PortState{}
	if err := h.proxyStateMap.LookupWithFlags(uint32(i), &proxyState, ebpf.LookupLock); err != nil {
		// h.logger.Error("failed to fetch the state of proxy", zap.Error(err))
		return nil, err
	}
	return &proxyState, nil
}

func (h *Hook) Stop () {
	<-h.stopper
	log.Println("Received signal, exiting program..")

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
	h.accept.Close()
	h.acceptRet.Close()
	h.accept4.Close()
	h.accept4Ret.Close()
	h.close.Close()
	h.closeRet.Close()
	h.connect4.Close()
	h.connect6.Close()
	h.gp4.Close()
	h.read.Close()
	h.readRet.Close()
	h.write.Close()
	h.writeRet.Close()
	h.objects.Close()
}

// LoadHooks is used to attach the eBPF hooks into the linux kernel. Hooks are attached for outgoing and incoming network requests.
//
// proxyPorts is used for redirecting outgoing network calls to the unoccupied proxy server.
//
// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS -no-global-types -target $TARGET bpf keploy_ebpf.c -- -I./headers
func (h *Hook) LoadHooks(pid uint32) error {
	// k := keploy.KeployInitializer()
	
	if err := settings.InitRealTimeOffset(); err != nil {
		h.logger.Error("failed to fix the BPF clock", zap.Error(err))
		return err
	}
	
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	
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
	// defer objs.Close()
	// hook := newHook(objs.ProxyPorts, stopper, db)
	err := objs.UserPid.Update(uint32(0), &pid, ebpf.UpdateAny)
	if err != nil {
		h.logger.Error("failed to update the process id of the user application", zap.Any("error thrown by ebpf map", err.Error()))
		return err
	}
	h.proxyStateMap = objs.ProxyPorts
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

	gp4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCgroupInet4GetPeername,
		Program: objs.K_getpeername4,
	})

	if err != nil {
		h.logger.Error("faled to attach GetPeername cgroup hook", zap.Error(err))
		return err
	}
	h.gp4 = gp4
	// defer gp4.Close()


	// objs.PortMapping.Update(uint32(1), Dest_info{Dest_ip: 10, Dest_port: 11}, ebpf.UpdateAny)
	// log.Println("the ports on which proxy is running: ", h.proxyPortList)
	for i, v := range h.proxyPortList {
		// log.Printf("setting the vPorts at i: %v and port: %v, sizeof(vaccantPorts): %v", i, v, unsafe.Sizeof(PortState{Port: v})) // Occupied: false

		// inf, _ := objs.VaccantPorts.Info()

		err = objs.ProxyPorts.Update(uint32(i), PortState{Port: v}, ebpf.UpdateLock)
		if err != nil {
			h.logger.Error("failed to update the proxy state in the ebpf map", zap.Error(err))
			return err
		}
		// err := objs.VaccantPorts.Update(uint32(i), []PortState{{Port: v, Occupied: false}}, ebpf.UpdateAny)
		// ports := []uint32{}
		// for i := 0; i < runtime.NumCPU(); i++ {
		// 	ports = append(ports, v)
		// }
		// err := objs.VaccantPorts.Put(uint32(i), ports)

		// err := objs.VaccantPorts.Put(uint32(i), []PortState{{Port: v, Occupied: false}, {Port: v, Occupied: false}})

		// log.Printf("info about VaccantPorts: %v, flags: %v", inf, objs.VaccantPorts.Flags())
	}
	
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
	ac_, err := link.Kretprobe("sys_accept", objs.SyscallProbeRetAccept, nil)
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
	ac4_, err := link.Kretprobe("sys_accept4", objs.SyscallProbeRetAccept4, nil)
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
	wt_, err := link.Kretprobe("sys_write", objs.SyscallProbeRetWrite, nil)
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_write", zap.Error(err))
		return err
	}
	h.writeRet = wt_
	// defer wt_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	cl, err := link.Kprobe("sys_close", objs.SyscallProbeEntryClose, nil)
	if err != nil {
		h.logger.Error("failed to attach the kprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.close = cl
	// defer cl.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	cl_, err := link.Kretprobe("sys_close", objs.SyscallProbeRetClose, nil)
	if err != nil {
		h.logger.Error("failed to attach the kretprobe hook on sys_close", zap.Error(err))
		return err
	}
	h.closeRet = cl_
	// defer cl_.Close()

	LaunchPerfBufferConsumers(objs, connectionFactory, stopper, h.logger)

	h.logger.Info("keploy initialized and probes added to the kernel.")

	// need to add this stopper in the service function
	// <-stopper
	// log.Println("Received signal, exiting program..")

	// // closing all readers.
	// for _, reader := range PerfEventReaders {
	// 	if err := reader.Close(); err != nil {
	// 		log.Fatalf("closing perf reader: %s", err)
	// 	}
	// }
	// for _, reader := range RingEventReaders {
	// 	if err := reader.Close(); err != nil {
	// 		log.Fatalf("closing ringbuf reader: %s", err)
	// 	}
	// }
	return nil
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
