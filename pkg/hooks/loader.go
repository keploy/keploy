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
	// "go.keploy.io/server/pkg/hooks/keploy"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
)

type Dest_info struct {
	Dest_ip   uint32
	Dest_port uint32
}

type Bpf_spin_lock struct{ Val uint32 }

type Vaccant_port struct {
	Port      uint32
	Occupied  uint32
	Dest_ip   uint32
	Dest_port uint32
	Lock      Bpf_spin_lock
}

var objs = bpfObjects{}

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS -no-global-types -target $TARGET bpf keploy_ebpf.c -- -I../headers
func LoadHooks() {
	// k := keploy.KeployInitializer()

	if err := settings.InitRealTimeOffset(); err != nil {
		log.Printf("Failed fixing BPF clock, timings will be offseted: %v", err)
	}

	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatal(err)
	}

	// Load pre-compiled programs and maps into the kernel.
	// objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Fatalf("loading objects: %v", err)
	}
	defer objs.Close()

	connectionFactory := connection.NewFactory(time.Minute)
	go func() {
		for {
			connectionFactory.HandleReadyConnections(k)
			time.Sleep(1 * time.Second)
		}
	}()

	// // Get the first-mounted cgroupv2 path.
	// cgroupPath, err := detectCgroupPath()
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// // println("cgroup path: ", cgroupPath)

	// // Link the count_egress_packets program to the cgroup.
	// l, err := link.AttachCgroup(link.CgroupOptions{
	// 	Path:    cgroupPath,
	// 	Attach:  ebpf.AttachCGroupInetEgress,
	// 	Program: objs.CountEgressPackets,
	// })

	// if err != nil {
	// 	log.Fatal(err)
	// }
	// defer l.Close()

	// ------------ For Egress -------------

	// Get the first-mounted cgroupv2 path.
	cgroupPath, err := detectCgroupPath()
	if err != nil {
		log.Fatal(err)
	}

	println("cgroup path: ", cgroupPath)

	// Link the count_egress_packets program to the cgroup.
	// l, err := link.AttachCgroup(link.CgroupOptions{
	// 	Path:    cgroupPath,
	// 	Attach:  ebpf.AttachCGroupInetEgress,
	// 	Program: objs.CountEgressPackets,
	// })

	// if err != nil {
	// 	log.Fatal(err)
	// }
	// defer l.Close()

	c4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: objs.K_connect4,
	})

	if err != nil {
		log.Fatal(err)
	}
	defer c4.Close()

	c6, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInet6Connect,
		Program: objs.K_connect6,
	})

	if err != nil {
		log.Fatal(err)
	}
	defer c6.Close()

	gp4, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCgroupInet4GetPeername,
		Program: objs.K_getpeername4,
	})

	if err != nil {
		log.Fatal(err)
	}
	defer gp4.Close()

	log.Println("Counting packets...")

	// Read loop reporting the total amount of times the kernel
	// function was entered, once per second.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// objs.PortMapping.Update(uint32(1), Dest_info{Dest_ip: 10, Dest_port: 11}, ebpf.UpdateAny)

	for i, v := range runningPorts {
		// log.Printf("setting the vPorts at i: %v and port: %v, sizeof(vaccantPorts): %v", i, v, unsafe.Sizeof(Vaccant_port{Port: v})) // Occupied: false

		// inf, _ := objs.VaccantPorts.Info()

		err = objs.VaccantPorts.Update(uint32(i), Vaccant_port{Port: v}, // Occupied: false,
			ebpf.UpdateLock)

		// err := objs.VaccantPorts.Update(uint32(i), []Vaccant_port{{Port: v, Occupied: false}}, ebpf.UpdateAny)
		// ports := []uint32{}
		// for i := 0; i < runtime.NumCPU(); i++ {
		// 	ports = append(ports, v)
		// }
		// err := objs.VaccantPorts.Put(uint32(i), ports)

		// err := objs.VaccantPorts.Put(uint32(i), []Vaccant_port{{Port: v, Occupied: false}, {Port: v, Occupied: false}})

		if err != nil {
			log.Printf("failed to update the vaccantPorts array at userspace. error: %v", err)
		}
		// log.Printf("info about VaccantPorts: %v, flags: %v", inf, objs.VaccantPorts.Flags())
	}
	go func() {
		for range ticker.C {
			// var value uint64
			// if err := objs.PktCount.Lookup(uint32(0), &value); err != nil {
			// 	log.Fatalf("reading map: %v", err)
			// }

			var port = Vaccant_port{}
			zero := uint32(0)
			// var all_cpu_value []uint32
			// if err := objs.VaccantPorts.Lookup(&zero, &port); err != nil {
			if err := objs.VaccantPorts.LookupWithFlags(&zero, &port, ebpf.LookupLock); err != nil {
				log.Fatalf("reading map: %v", err)
			}
			// for cpuid, cpuvalue := range all_cpu_value {
			// 	log.Printf("%s called %d times on CPU%v\n", "connect4", cpuvalue, cpuid)
			// }
			// log.Printf("reading map: %v", port)

			// var dest Dest_info
			// var key uint32 = 1

			// iter := objs.PortMapping.Iterate()
			// for iter.Next(&key,&dest){
			// 	log.Printf("Key: %v || Value: %v",key,dest)
			// }

			// if err := objs.PortMapping.Lookup(key, &dest); err != nil {
			// 	log.Printf("/proxy: reading Port map: %v", err)
			// } else {
			// 	log.Printf("/proxy: Value for key:[%v]: %v", key, dest)
			// }

			// objs.
			// log.Printf("number of packets: %d\n", value)
			// }
		}
	}()

	// ------------ For Ingress using Kprobes --------------

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac, err := link.Kprobe("sys_accept", objs.SyscallProbeEntryAccept, nil)
	if err != nil {
		log.Fatalf("opening accept kprobe: %s", err)
	}
	defer ac.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac_, err := link.Kretprobe("sys_accept", objs.SyscallProbeRetAccept, nil)
	if err != nil {
		log.Fatalf("opening accept kretprobe: %s", err)
	}
	defer ac_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	ac4, err := link.Kprobe("sys_accept4", objs.SyscallProbeEntryAccept4, nil)
	if err != nil {
		log.Fatalf("opening accept4 kprobe: %s", err)
	}
	defer ac4.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	ac4_, err := link.Kretprobe("sys_accept4", objs.SyscallProbeRetAccept4, nil)
	if err != nil {
		log.Fatalf("opening accept4 kretprobe: %s", err)
	}
	defer ac4_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	rd, err := link.Kprobe("sys_read", objs.SyscallProbeEntryRead, nil)
	if err != nil {
		log.Fatalf("opening read kprobe: %s", err)
	}
	defer rd.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	rd_, err := link.Kretprobe("sys_read", objs.SyscallProbeRetRead, &link.KprobeOptions{RetprobeMaxActive: 1024})
	if err != nil {
		log.Fatalf("opening read kretprobe: %s", err)
	}
	defer rd_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	wt, err := link.Kprobe("sys_write", objs.SyscallProbeEntryWrite, nil)
	if err != nil {
		log.Fatalf("opening write kprobe: %s", err)
	}
	defer wt.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	wt_, err := link.Kretprobe("sys_write", objs.SyscallProbeRetWrite, nil)
	if err != nil {
		log.Fatalf("opening write kretprobe: %s", err)
	}
	defer wt_.Close()

	// Open a Kprobe at the entry point of the kernel function and attach the
	// pre-compiled program.
	cl, err := link.Kprobe("sys_close", objs.SyscallProbeEntryClose, nil)
	if err != nil {
		log.Fatalf("opening write kprobe: %s", err)
	}
	defer cl.Close()

	// Open a Kprobe at the exit point of the kernel function and attach the
	// pre-compiled program.
	cl_, err := link.Kretprobe("sys_close", objs.SyscallProbeRetClose, nil)
	if err != nil {
		log.Fatalf("opening write kretprobe: %s", err)
	}
	defer cl_.Close()

	//------------------------------------------------------------------------
	// // Read loop reporting the total amount of times the kernel
	// // function was entered, once per second.
	// ticker := time.NewTicker(1 * time.Second)
	// defer ticker.Stop()

	// log.Println("Running...")

	// // objs.PktCount.Update(uint32(0),11,ebpf.UpdateAny)

	// // type Dest_info struct {
	// // 	Dest_ip   uint32
	// // 	Dest_port uint32
	// // }

	// // objs.PortMapping.Update(uint32(1), Dest_info{Dest_ip: 10, Dest_port: 11}, ebpf.UpdateAny)

	// for range ticker.C {

	// 	// var dest Dest_info
	// 	// var key uint32 = 1

	// 	// iter := objs.PortMapping.Iterate()
	// 	// for iter.Next(&key, &dest) {
	// 	// 	log.Printf("Key: %v || Value: %v", key, dest)
	// 	// }
	// 	// log.Println("waiting for events...")

	// }
	//-------------------------------------------------------------------------

	LaunchPerfBufferConsumers(objs, connectionFactory, stopper)

	// k := keploy.KeployInitializer()

	log.Printf("keploy initialized and probes added to the kernel.\n")

	<-stopper
	log.Println("Received signal, exiting program..")

	// closing all readers.
	for _, reader := range PerfEventReaders {
		if err := reader.Close(); err != nil {
			log.Fatalf("closing perf reader: %s", err)
		}
	}
	for _, reader := range RingEventReaders {
		if err := reader.Close(); err != nil {
			log.Fatalf("closing ringbuf reader: %s", err)
		}
	}

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
